// Package elastic implements the Elasticsearch and OpenSearch driver over
// their REST API: indices, data streams and aliases with their mappings, a
// grid that searches with typed filters and edits documents checked by
// sequence number, a console in Kibana Dev Tools syntax, search profiles as
// plans, and tasks as processes.
package elastic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"rowsmith/internal/driver"
)

type esDriver struct {
	id, name string
	order    int
}

func init() {
	driver.Register(esDriver{id: "elasticsearch", name: "Elasticsearch", order: 72})
	driver.Register(esDriver{id: "opensearch", name: "OpenSearch", order: 73})
}

var kinds = []driver.KindInfo{
	{Kind: "table", Label: "Indices", Icon: "table", Browse: true},
	{Kind: "data_stream", Label: "Data streams", Icon: "stream", Browse: true},
	{Kind: "alias", Label: "Aliases", Icon: "synonym", Browse: true},
}

var fieldTypes = []string{
	"keyword", "text", "match_only_text", "wildcard", "long", "integer", "short", "byte", "double", "float", "half_float",
	"scaled_float", "unsigned_long", "boolean", "date", "date_nanos", "ip", "binary", "object", "nested", "flattened",
	"geo_point", "geo_shape", "dense_vector", "version",
}

func (d esDriver) Info() driver.Info {
	fields := []driver.Field{
		{Key: "host", Label: "Host", Type: driver.FieldText, Required: true, Placeholder: "search.example.com", Span: 4},
		{Key: "port", Label: "Port", Type: driver.FieldNumber, Default: 9200, Span: 2},
		{Key: "auth", Label: "Sign in with", Type: driver.FieldSelect, Default: "basic", Section: "auth", Options: []driver.Option{
			{Value: "basic", Label: "User and password"}, {Value: "apikey", Label: "API key"}, {Value: "none", Label: "Nothing (security off)"}}},
		{Key: "user", Label: "User", Type: driver.FieldText, Section: "auth", Span: 3, Placeholder: "elastic", ShowIf: map[string][]string{"auth": {"basic"}}},
		{Key: "password", Label: "Password", Type: driver.FieldPassword, Secret: true, Section: "auth", Span: 3, ShowIf: map[string][]string{"auth": {"basic"}}},
		{Key: "apiKey", Label: "API key", Type: driver.FieldPassword, Secret: true, Section: "auth", ShowIf: map[string][]string{"auth": {"apikey"}},
			Help: "The encoded key (base64 of id:key), as Kibana shows it."},
		{Key: "pathPrefix", Label: "Path prefix", Type: driver.FieldText, Section: "advanced", Placeholder: "/",
			Help: "For a cluster behind a proxy at a sub-path."},
	}
	fields = append(fields, driver.TLSFields("disable")...)
	fields = append(fields, driver.Field{Key: "timeout", Label: "Request timeout (seconds)", Type: driver.FieldNumber, Default: 60, Section: "advanced", Span: 3})
	desc := "Elasticsearch 8 and 9, and Elastic Cloud"
	if d.id == "opensearch" {
		desc = "OpenSearch 2 and 3, and Amazon OpenSearch Service"
	}
	return driver.Info{
		ID: d.id, Name: d.name, Order: d.order, Description: desc,
		Dialect: "elasticsearch", DefaultPort: 9200, SSH: true, QuoteChar: "",
		Fields: fields,
		Caps:   driver.Caps{EditRows: true, DDL: true, Explain: true, Processes: true, Variables: true},
		Kinds:  kinds, Types: fieldTypes,
		Design: &driver.TableDesign{
			Columns: true, PrimaryKey: true,
			Options: []driver.Field{
				{Key: "number_of_shards", Label: "Primary shards", Type: driver.FieldNumber, Placeholder: "1"},
				{Key: "number_of_replicas", Label: "Replicas", Type: driver.FieldNumber, Placeholder: "1"},
				{Key: "refresh_interval", Label: "Refresh interval", Type: driver.FieldText, Placeholder: "1s"},
			},
			Note: "Fields can be added to an index but not changed or removed; to change a mapping, create a new index and reindex into it. A primary key of one field becomes each document's _id.",
		},
	}
}

type conn struct {
	base    string // scheme://host:port[/prefix]
	auth    string // Authorization header
	hc      *http.Client
	ro      bool
	product string // Elasticsearch or OpenSearch
	version string
	major   int
	cluster string
	user    string
}

func (d esDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	host, port := driver.HostPort(p, 9200)
	if host == "" {
		return nil, errors.New("host is required")
	}
	tc, err := driver.TLSConfig(p, host)
	if err != nil {
		return nil, err
	}
	scheme := "http"
	if tc != nil {
		scheme = "https"
	}
	tr := &http.Transport{TLSClientConfig: tc, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second, ForceAttemptHTTP2: tc != nil}
	if p.Dial != nil {
		dial := p.Dial
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) { return dial(ctx, network, addr) }
	}
	c := &conn{
		base: scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/" + strings.Trim(strings.TrimSpace(p.String("pathPrefix")), "/"),
		hc:   &http.Client{Transport: tr, Timeout: time.Duration(p.Int("timeout", 60)) * time.Second},
		ro:   p.ReadOnly,
	}
	c.base = strings.TrimSuffix(c.base, "/")
	switch p.String("auth") {
	case "", "basic":
		if u := strings.TrimSpace(p.String("user")); u != "" {
			c.user = u
			c.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+p.Secret("password")))
		}
	case "apikey":
		key := strings.TrimSpace(p.Secret("apiKey"))
		if key == "" {
			return nil, errors.New("enter the API key")
		}
		c.auth = "ApiKey " + key
	}
	var root struct {
		ClusterName string `json:"cluster_name"`
		Version     struct {
			Number       string `json:"number"`
			Distribution string `json:"distribution"`
		} `json:"version"`
	}
	if err := c.do(ctx, "GET", "/", nil, &root); err != nil {
		return nil, err
	}
	c.product, c.version, c.cluster = "Elasticsearch", root.Version.Number, root.ClusterName
	if root.Version.Distribution == "opensearch" {
		c.product = "OpenSearch"
	}
	c.major, _ = strconv.Atoi(strings.SplitN(c.version, ".", 2)[0])
	if c.user == "" {
		var who struct {
			Username string `json:"username"`
			UserName string `json:"user_name"`
		}
		path := "/_security/_authenticate"
		if c.product == "OpenSearch" {
			path = "/_plugins/_security/authinfo"
		}
		if c.do(ctx, "GET", path, nil, &who) == nil {
			c.user = who.Username + who.UserName
		}
	}
	return c, nil
}

func (c *conn) Close() error {
	c.hc.CloseIdleConnections()
	return nil
}

func (c *conn) Ping(ctx context.Context) error { return c.do(ctx, "HEAD", "/", nil, nil) }

// ---- requests -------------------------------------------------------------------

// apiError is an error reply: {"error": {"type", "reason", "root_cause"}, "status"}.
type apiError struct {
	Status int
	Type   string
	Reason string
	Body   string
}

func (e *apiError) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
	if e.Body != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// raw sends a request and returns the reply's status and body; replies of
// 400 and above become errors. body is JSON bytes, a value to encode, or
// NDJSON text for _bulk.
func (c *conn) raw(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	ctype := "application/json"
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case ndjson:
		rd, ctype = strings.NewReader(string(b)), "application/x-ndjson"
	default:
		enc, err := json.Marshal(b)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(enc)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if rd != nil {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Accept", "application/json")
	if c.auth != "" {
		req.Header.Set("Authorization", c.auth)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && ue.Timeout() {
			return 0, nil, fmt.Errorf("the request timed out: %w", err)
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, data, replyError(resp.StatusCode, data)
	}
	return resp.StatusCode, data, nil
}

type ndjson string

func replyError(status int, data []byte) error {
	var r struct {
		Error json.RawMessage `json:"error"`
	}
	e := &apiError{Status: status}
	if json.Unmarshal(data, &r) == nil && len(r.Error) > 0 {
		var detail struct {
			Type      string `json:"type"`
			Reason    string `json:"reason"`
			RootCause []struct {
				Type   string `json:"type"`
				Reason string `json:"reason"`
			} `json:"root_cause"`
			CausedBy *struct {
				Reason string `json:"reason"`
			} `json:"caused_by"`
		}
		if json.Unmarshal(r.Error, &detail) == nil {
			e.Type, e.Reason = detail.Type, detail.Reason
			if len(detail.RootCause) > 0 && detail.RootCause[0].Reason != "" && detail.RootCause[0].Reason != e.Reason {
				e.Reason += ": " + detail.RootCause[0].Reason
			} else if detail.CausedBy != nil && detail.CausedBy.Reason != "" {
				e.Reason += ": " + detail.CausedBy.Reason
			}
		} else {
			var s string
			json.Unmarshal(r.Error, &s)
			e.Reason = s
		}
	} else {
		e.Body = strings.TrimSpace(string(data))
		if len(e.Body) > 300 {
			e.Body = e.Body[:300] + "…"
		}
	}
	switch status {
	case 401:
		e.Reason = "Sign-in failed: check the user and password or API key. " + e.Reason
	case 403:
		if e.Reason == "" {
			e.Reason = "Not allowed for this user"
		}
	}
	return e
}

// do sends a request and decodes a JSON reply into out (numbers kept exact
// when out is an any or a map).
func (c *conn) do(ctx context.Context, method, path string, body, out any) error {
	_, data, err := c.raw(ctx, method, path, body)
	if err != nil || out == nil || len(data) == 0 {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(out)
}

func esc(name string) string { return url.PathEscape(name) }

func mapError(err error) error {
	var ae *apiError
	if errors.As(err, &ae) {
		return &driver.QueryError{Message: ae.Error(), Code: ae.Type}
	}
	return err
}

// ---- catalog ---------------------------------------------------------------------

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	info := &driver.ServerInfo{Product: c.product, Version: c.version, User: c.user, Extras: map[string]string{"Cluster": c.cluster}}
	var health struct {
		Status string `json:"status"`
		Nodes  int    `json:"number_of_nodes"`
	}
	if c.do(ctx, "GET", "/_cluster/health", nil, &health) == nil {
		info.Extras["Health"] = health.Status
		info.Extras["Nodes"] = strconv.Itoa(health.Nodes)
	}
	if c.ro {
		info.Extras["Session"] = "read-only"
	}
	return info, nil
}

func (c *conn) Databases(context.Context) ([]driver.Database, error)     { return nil, nil }
func (c *conn) Schemas(context.Context, string) ([]driver.Schema, error) { return nil, nil }

// Objects lists indices (not hidden ones, such as data streams' backing
// indices), data streams and aliases.
func (c *conn) Objects(ctx context.Context, _ driver.Scope) ([]driver.Object, error) {
	var idx []struct {
		Index  string `json:"index"`
		Health string `json:"health"`
		Status string `json:"status"`
		Docs   string `json:"docs.count"`
		Store  string `json:"store.size"`
	}
	if err := c.do(ctx, "GET", "/_cat/indices?format=json&bytes=b&expand_wildcards=open&h=index,health,status,docs.count,store.size", nil, &idx); err != nil {
		return nil, mapError(err)
	}
	var out []driver.Object
	for _, i := range idx {
		if strings.HasPrefix(i.Index, ".") {
			continue
		}
		o := driver.Object{Name: i.Index, Kind: "table", Engine: i.Health}
		if n, err := strconv.ParseInt(i.Docs, 10, 64); err == nil {
			o.Rows = &n
		}
		if n, err := strconv.ParseInt(i.Store, 10, 64); err == nil {
			o.Size = &n
		}
		out = append(out, o)
	}
	var ds struct {
		DataStreams []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"data_streams"`
	}
	if c.do(ctx, "GET", "/_data_stream", nil, &ds) == nil {
		for _, d := range ds.DataStreams {
			if !strings.HasPrefix(d.Name, ".") {
				out = append(out, driver.Object{Name: d.Name, Kind: "data_stream", Engine: strings.ToLower(d.Status)})
			}
		}
	}
	var aliases map[string]struct {
		Aliases map[string]json.RawMessage `json:"aliases"`
	}
	if c.do(ctx, "GET", "/_alias", nil, &aliases) == nil {
		of := map[string][]string{}
		for index, a := range aliases {
			for name := range a.Aliases {
				if !strings.HasPrefix(name, ".") {
					of[name] = append(of[name], index)
				}
			}
		}
		for name, indices := range of {
			sort.Strings(indices)
			out = append(out, driver.Object{Name: name, Kind: "alias", Extra: "→ " + strings.Join(indices, ", ")})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *conn) Classify(stmt string) driver.StatementKind {
	reqs, err := parseScript(stmt)
	if err != nil || len(reqs) == 0 {
		return driver.StmtUnknown
	}
	return reqs[0].kind()
}
