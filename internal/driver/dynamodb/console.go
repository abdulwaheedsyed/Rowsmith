package dynamodb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"rowsmith/internal/driver"
)

// call is one console request: a PartiQL statement, or a DynamoDB API
// operation with its input in DynamoDB JSON, as the AWS CLI takes it:
//
//	Query
//	{"TableName": "Orders", "KeyConditionExpression": "customer_id = :c",
//	 "ExpressionAttributeValues": {":c": {"S": "C001"}}}
type call struct {
	op         string // the API operation; empty for PartiQL
	body       []byte // the operation's input, compacted
	text       string
	start, end int
	line       int
}

// operations are the API calls the console runs, with what they do.
var operations = map[string]driver.StatementKind{
	"Scan": driver.StmtRead, "Query": driver.StmtRead, "GetItem": driver.StmtRead, "BatchGetItem": driver.StmtRead,
	"TransactGetItems": driver.StmtRead, "DescribeTable": driver.StmtRead, "ListTables": driver.StmtRead,
	"DescribeTimeToLive": driver.StmtRead, "DescribeContinuousBackups": driver.StmtRead, "ListTagsOfResource": driver.StmtRead,
	"DescribeLimits": driver.StmtRead,
	"PutItem":        driver.StmtWrite, "UpdateItem": driver.StmtWrite, "DeleteItem": driver.StmtWrite, "BatchWriteItem": driver.StmtWrite,
	"TransactWriteItems": driver.StmtWrite,
	"CreateTable":        driver.StmtDDL, "UpdateTable": driver.StmtDDL, "DeleteTable": driver.StmtDDL, "UpdateTimeToLive": driver.StmtDDL,
	"TagResource": driver.StmtDDL, "UntagResource": driver.StmtDDL, "UpdateContinuousBackups": driver.StmtDDL,
}

// opName finds an operation by name in any letter case.
func opName(s string) string {
	for name := range operations {
		if strings.EqualFold(name, s) {
			return name
		}
	}
	return ""
}

var (
	opLine      = regexp.MustCompile(`^([A-Za-z]+)\s*(\{.*)?$`)
	partiqlVerb = regexp.MustCompile(`^(?i)(SELECT|INSERT|UPDATE|DELETE)\b`)
)

// SyntaxError reports where a script stops parsing.
type SyntaxError struct {
	Line int
	Msg  string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

func comment(trimmed string) bool {
	return strings.HasPrefix(trimmed, "--") || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#")
}

// endsStatement reports whether a PartiQL line ends with a semicolon
// outside quotes, given whether the statement so far left a quote open.
func endsStatement(line string, quote byte) (bool, byte) {
	last := byte(0)
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			}
		case ch == '\'' || ch == '"':
			quote = ch
		case ch == '-' && i+1 < len(line) && line[i+1] == '-':
			return last == ';', 0
		case ch != ' ' && ch != '\t' && ch != '\r' && ch != '\n':
			last = ch
		}
	}
	return quote == 0 && last == ';', quote
}

// parseScript splits a script into calls. A PartiQL statement ends at a
// semicolon or a blank line; an operation's input runs to the next
// operation or statement.
func parseScript(script string) ([]call, error) {
	var out []call
	var cur *call
	var body []string
	var quote byte
	bodyLine := 0
	flush := func(end int) error {
		if cur == nil {
			return nil
		}
		cur.end = end
		cur.text = strings.TrimRight(script[cur.start:end], " \t\r\n")
		if cur.op != "" {
			text := strings.TrimSpace(strings.Join(body, ""))
			if text == "" {
				text = "{}"
			}
			var b bytes.Buffer
			if err := json.Compact(&b, []byte(text)); err != nil {
				return &SyntaxError{Line: bodyLine, Msg: "the input is not valid JSON: " + err.Error()}
			}
			if b.Bytes()[0] != '{' {
				return &SyntaxError{Line: bodyLine, Msg: "the input is a JSON object"}
			}
			cur.body = b.Bytes()
		} else {
			cur.text = strings.TrimSuffix(cur.text, ";")
			cur.end = cur.start + len(cur.text)
		}
		out = append(out, *cur)
		cur, body, quote = nil, nil, 0
		return nil
	}
	offset := 0
	for i, line := range strings.SplitAfter(script, "\n") {
		trimmed := strings.TrimSpace(line)
		lead := offset + strings.Index(line, strings.TrimLeft(line, " \t"))
		inPartiQL := cur != nil && cur.op == ""
		m := opLine.FindStringSubmatch(trimmed)
		switch {
		case inPartiQL && quote != 0:
			_, quote = endsStatement(line, quote)
		case m != nil && opName(m[1]) != "":
			if err := flush(offset); err != nil {
				return nil, err
			}
			cur = &call{op: opName(m[1]), start: lead, line: i + 1}
			bodyLine = i + 1
			if m[2] != "" {
				body = append(body, m[2])
			} else {
				bodyLine = i + 2
			}
		case trimmed == "":
			if inPartiQL {
				if err := flush(offset); err != nil {
					return nil, err
				}
			}
		case comment(trimmed):
		case partiqlVerb.MatchString(trimmed) && (cur == nil || cur.op != "" && jsonDone(body)):
			if err := flush(offset); err != nil {
				return nil, err
			}
			cur = &call{start: lead, line: i + 1}
			var done bool
			if done, quote = endsStatement(line, 0); done {
				if err := flush(offset + len(line)); err != nil {
					return nil, err
				}
			}
		case cur == nil:
			return nil, &SyntaxError{Line: i + 1, Msg: "expected a PartiQL statement (SELECT, INSERT, UPDATE or DELETE) or an operation such as Scan or Query"}
		case cur.op != "":
			body = append(body, line)
		default:
			var done bool
			if done, quote = endsStatement(line, 0); done {
				if err := flush(offset + len(line)); err != nil {
					return nil, err
				}
			}
		}
		offset += len(line)
	}
	if err := flush(len(script)); err != nil {
		return nil, err
	}
	return out, nil
}

// jsonDone reports whether an operation's input so far is a whole JSON
// value (or empty), so a statement may follow it.
func jsonDone(lines []string) bool {
	text := strings.TrimSpace(strings.Join(lines, ""))
	return text == "" || json.Valid([]byte(text))
}

func (r call) kind() driver.StatementKind {
	if r.op != "" {
		return operations[r.op]
	}
	switch strings.ToUpper(partiqlVerb.FindString(strings.TrimSpace(r.text))) {
	case "SELECT":
		return driver.StmtRead
	case "":
		return driver.StmtUnknown
	}
	return driver.StmtWrite
}

func (r call) danger(k driver.StatementKind) driver.Danger {
	switch {
	case k == driver.StmtRead:
		return driver.Danger{}
	case r.op == "DeleteTable":
		return driver.Danger{Level: "destructive", Reason: "deletes the table and all its items"}
	case r.op == "UpdateTable":
		return driver.Danger{Level: "caution", Reason: "changes the table's capacity or indexes"}
	case k == driver.StmtDDL:
		return driver.Danger{Level: "caution", Reason: "changes the table's settings"}
	case r.op == "BatchWriteItem" && bytes.Contains(r.body, []byte(`"DeleteRequest"`)):
		return driver.Danger{Level: "caution", Reason: "deletes items"}
	case r.op == "DeleteItem" || r.op == "" && strings.EqualFold(partiqlVerb.FindString(strings.TrimSpace(r.text)), "DELETE"):
		return driver.Danger{Level: "caution", Reason: "deletes an item"}
	}
	return driver.Danger{Level: "caution", Reason: "modifies data"}
}

// SplitScript implements driver.ScriptSplitter for the console language.
func (ddbDriver) SplitScript(script string) ([]driver.ScriptStatement, error) {
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
