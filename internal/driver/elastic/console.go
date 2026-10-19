package elastic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"rowsmith/internal/driver"
)

// request is one console request in Kibana Dev Tools syntax:
//
//	GET /orders/_search
//	{ "query": { "match": { "status": "paid" } } }
type request struct {
	method, path string
	body         []byte // JSON, or NDJSON for _bulk and _msearch
	text         string
	start, end   int
	line         int
}

var methodLine = regexp.MustCompile(`^(?i)(GET|POST|PUT|DELETE|HEAD|PATCH)\s+(\S+)\s*$`)

// SyntaxError reports where a script stops parsing.
type SyntaxError struct {
	Line int
	Msg  string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

// tripleQuoted turns Kibana's """multi-line strings""" into JSON strings.
var tripleQuoted = regexp.MustCompile(`(?s)"""(.*?)"""`)

func parseScript(script string) ([]request, error) {
	var out []request
	var cur *request
	var body []string
	bodyLine := 0
	flush := func(end int) error {
		if cur == nil {
			return nil
		}
		cur.end = end
		cur.text = strings.TrimRight(script[cur.start:end], " \t\r\n")
		text := strings.TrimSpace(strings.Join(body, "")) // lines keep their newlines
		text = tripleQuoted.ReplaceAllStringFunc(text, func(m string) string {
			b, _ := json.Marshal(m[3 : len(m)-3])
			return string(b)
		})
		if text != "" {
			b, err := jsonBody(text, cur.ndjson())
			if err != nil {
				return &SyntaxError{Line: bodyLine, Msg: err.Error()}
			}
			cur.body = b
		}
		out = append(out, *cur)
		cur, body = nil, nil
		return nil
	}
	offset := 0
	for i, line := range strings.SplitAfter(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if m := methodLine.FindStringSubmatch(trimmed); m != nil {
			if err := flush(offset); err != nil {
				return nil, err
			}
			cur = &request{method: strings.ToUpper(m[1]), path: m[2], start: offset + strings.Index(line, strings.TrimLeft(line, " \t")), line: i + 1}
			bodyLine = i + 2
		} else if cur == nil {
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "//") {
				return nil, &SyntaxError{Line: i + 1, Msg: "expected a request such as GET /orders/_search"}
			}
		} else if !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "//") {
			body = append(body, line)
		}
		offset += len(line)
	}
	if err := flush(len(script)); err != nil {
		return nil, err
	}
	return out, nil
}

// jsonBody checks a body: one JSON value, or one per line for NDJSON
// endpoints, compacted.
func jsonBody(text string, nd bool) ([]byte, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	var docs []json.RawMessage
	for dec.More() {
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("the body is not valid JSON: %v", err)
		}
		docs = append(docs, v)
	}
	if !nd {
		if len(docs) != 1 {
			return nil, fmt.Errorf("the body must be one JSON object (found %d)", len(docs))
		}
		var b bytes.Buffer
		json.Compact(&b, docs[0])
		return b.Bytes(), nil
	}
	var b bytes.Buffer
	for _, d := range docs {
		json.Compact(&b, d)
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

func (r request) ndjson() bool {
	p := r.endpoint()
	return strings.HasSuffix(p, "_bulk") || strings.HasSuffix(p, "_msearch") || strings.HasSuffix(p, "_msearch/template")
}

// endpoint is the path without its query string and leading slash.
func (r request) endpoint() string {
	p, _, _ := strings.Cut(r.path, "?")
	return strings.Trim(p, "/")
}

// readEndpoints answer POST without changing anything.
var readEndpoints = []string{"_search", "_count", "_msearch", "_mget", "_field_caps", "_validate/query", "_explain", "_analyze",
	"_termvectors", "_mtermvectors", "_rank_eval", "_search/template", "_msearch/template", "_render/template", "_sql", "_sql/translate",
	"_query", "_eql/search", "_plugins/_sql", "_plugins/_ppl", "_plugins/_sql/_explain", "_async_search", "_knn_search", "_terms_enum"}

func (r request) kind() driver.StatementKind {
	switch r.method {
	case "GET", "HEAD":
		return driver.StmtRead
	}
	ep := r.endpoint()
	if r.method == "POST" {
		for _, e := range readEndpoints {
			if strings.Contains("/"+ep+"/", "/"+e+"/") {
				return driver.StmtRead
			}
		}
	}
	segs := strings.Split(ep, "/")
	switch {
	case (r.method == "PUT" || r.method == "DELETE") && len(segs) == 1 && !strings.HasPrefix(segs[0], "_"):
		return driver.StmtDDL // an index
	case strings.HasSuffix(ep, "_mapping") || strings.HasSuffix(ep, "_settings") || strings.Contains(ep, "_alias") ||
		strings.HasPrefix(ep, "_index_template") || strings.HasPrefix(ep, "_component_template") || strings.HasPrefix(ep, "_data_stream") ||
		strings.HasPrefix(ep, "_ilm") || strings.HasPrefix(ep, "_ingest/pipeline") || strings.HasSuffix(ep, "_reindex"):
		return driver.StmtDDL
	case strings.HasPrefix(ep, "_security") || strings.HasPrefix(ep, "_plugins/_security"):
		return driver.StmtDCL
	}
	return driver.StmtWrite
}

func (r request) danger(k driver.StatementKind) driver.Danger {
	ep := r.endpoint()
	switch {
	case k == driver.StmtRead:
		return driver.Danger{}
	case r.method == "DELETE" && (ep == "" || ep == "_all" || ep == "*" || strings.Contains(ep, "*")):
		return driver.Danger{Level: "destructive", Reason: "deletes every index the pattern matches"}
	case r.method == "DELETE" && k == driver.StmtDDL:
		return driver.Danger{Level: "destructive", Reason: "deletes the index and all its documents"}
	case strings.HasSuffix(ep, "_delete_by_query"):
		return driver.Danger{Level: "destructive", Reason: "deletes every document the query matches"}
	case strings.HasSuffix(ep, "_update_by_query"):
		return driver.Danger{Level: "caution", Reason: "changes every document the query matches"}
	case k == driver.StmtDCL:
		return driver.Danger{Level: "caution", Reason: "changes users and permissions"}
	case k == driver.StmtDDL:
		return driver.Danger{Level: "caution", Reason: "changes indices or their settings"}
	}
	return driver.Danger{Level: "caution", Reason: "modifies data"}
}

// SplitScript implements driver.ScriptSplitter for the console language.
func (esDriver) SplitScript(script string) ([]driver.ScriptStatement, error) {
	reqs, err := parseScript(script)
	if err != nil {
		return nil, err
	}
	out := make([]driver.ScriptStatement, len(reqs))
	for i, r := range reqs {
		k := r.kind()
		out[i] = driver.ScriptStatement{SQL: r.text, Start: r.start, End: r.start + len(r.text), Line: r.line, Kind: k, Danger: r.danger(k)}
	}
	return out, nil
}
