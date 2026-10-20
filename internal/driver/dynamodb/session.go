package dynamodb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	"rowsmith/internal/driver"
)

// session is a console: calls carry no state between runs.
type session struct {
	c  *conn
	mu sync.Mutex
}

func (c *conn) NewSession(context.Context, driver.Scope) (driver.Session, error) {
	return &session{c: c}, nil
}

func (s *session) InTransaction() bool { return false }
func (s *session) Close() error        { return nil }

func (s *session) Execute(ctx context.Context, script string, opts driver.ExecOptions, sink driver.Sink) error {
	if !s.mu.TryLock() {
		return errors.New("this console is still running a previous request")
	}
	defer s.mu.Unlock()
	calls, err := parseScript(script)
	if err != nil {
		if e := sink.BeginStatement(driver.StatementInfo{SQL: strings.TrimSpace(script), Line: 1, Kind: driver.StmtUnknown}); e != nil {
			return e
		}
		qe := &driver.QueryError{Message: err.Error()}
		var se *SyntaxError
		if errors.As(err, &se) {
			qe = &driver.QueryError{Message: se.Msg, Line: se.Line}
		}
		return sink.EndStatement(qe)
	}
	for i, r := range calls {
		k := r.kind()
		if err := sink.BeginStatement(driver.StatementInfo{Index: i, SQL: r.text, Line: r.line, Kind: k}); err != nil {
			return err
		}
		var runErr error
		if (opts.ReadOnly || s.c.ro) && !k.Safe() {
			runErr = &driver.QueryError{Message: "Blocked: this connection is read-only for you, and the request may modify data (" + string(k) + ")."}
		} else if r.op == "" {
			runErr = s.c.partiql(ctx, r, opts, sink)
		} else {
			runErr = s.c.operation(ctx, r, opts, sink)
			// A new table takes no items until it's active; later calls in
			// the script wait for any table change.
			if runErr == nil && (r.op == "CreateTable" || i < len(calls)-1 && (r.op == "UpdateTable" || r.op == "DeleteTable")) {
				runErr = s.c.settle(ctx, r, sink)
			}
		}
		if runErr != nil {
			var qe *driver.QueryError
			if !errors.As(mapError(runErr), &qe) {
				qe = &driver.QueryError{Message: runErr.Error()}
			}
			qe.Line = r.line
			runErr = qe
		}
		if ctx.Err() != nil && runErr != nil {
			runErr = &driver.QueryError{Message: "Cancelled"}
		}
		if err := sink.EndStatement(runErr); err != nil {
			return err
		}
		if runErr != nil && (opts.StopOnError || ctx.Err() != nil) {
			break
		}
	}
	return nil
}

// settle waits, before the script goes on, for a table to be created,
// updated or deleted: DynamoDB takes one index change at a time, and a
// table in the making takes no items.
func (c *conn) settle(ctx context.Context, r call, sink driver.Sink) error {
	var in struct{ TableName string }
	json.Unmarshal(r.body, &in)
	if in.TableName == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	told := false
	for wait := 100 * time.Millisecond; ; wait = min(wait*2, 5*time.Second) {
		d, err := c.db.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(in.TableName)})
		var nf *types.ResourceNotFoundException
		switch {
		case errors.As(err, &nf):
			if r.op == "DeleteTable" {
				return nil
			}
		case err != nil:
			return mapError(err)
		case r.op != "DeleteTable" && ready(d.Table):
			return nil
		}
		if !told {
			told = true
			if err := sink.Notice("info", "Waiting for "+in.TableName+" to be ready before the next request…"); err != nil {
				return err
			}
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return fmt.Errorf("%s is still changing; run the rest of the script once it's ready", in.TableName)
		}
	}
}

func ready(t *types.TableDescription) bool {
	if t == nil || t.TableStatus != types.TableStatusActive {
		return false
	}
	for _, g := range t.GlobalSecondaryIndexes {
		if g.IndexStatus != types.IndexStatusActive {
			return false
		}
	}
	return true
}

// ---- items as rows ------------------------------------------------------------------

// keyOrder is a table's key attributes, which lead the columns.
func (c *conn) keyOrder(ctx context.Context, table string) []string {
	if table == "" {
		return nil
	}
	d, err := c.db.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)})
	if err != nil || d.Table == nil {
		return nil
	}
	h, r := keyNames(d.Table.KeySchema)
	if r == "" {
		return []string{h}
	}
	return []string{h, r}
}

// itemColumns lays out items: the named columns, or the keys and then
// every other attribute by name, each typed by its values.
func itemColumns(items []map[string]types.AttributeValue, keys, named []string) []driver.ResultColumn {
	names := named
	if len(names) == 0 {
		seen := map[string]bool{}
		for _, k := range keys {
			seen[k] = true
		}
		var rest []string
		for _, item := range items {
			for k := range item {
				if !seen[k] {
					seen[k] = true
					rest = append(rest, k)
				}
			}
		}
		sort.Strings(rest)
		for _, k := range keys {
			if items == nil || present(items, k) {
				names = append(names, k)
			}
		}
		names = append(names, rest...)
	}
	cols := make([]driver.ResultColumn, len(names))
	for i, n := range names {
		t := ""
		for _, item := range items {
			v, ok := item[n]
			if !ok {
				continue
			}
			switch vt := typeOf(v); {
			case t == "" || t == tNULL:
				t = vt
			case vt != t && vt != tNULL:
				t = tMixed
			}
		}
		cols[i] = driver.ResultColumn{Name: n, Type: t, Kind: kindOf(t)}
	}
	return cols
}

func present(items []map[string]types.AttributeValue, name string) bool {
	for _, item := range items {
		if _, ok := item[name]; ok {
			return true
		}
	}
	return false
}

func itemRow(item map[string]types.AttributeValue, cols []driver.ResultColumn) []any {
	row := make([]any, len(cols))
	for i, c := range cols {
		if v, ok := item[c.Name]; ok {
			row[i] = cell(v)
		}
	}
	return row
}

// pager reads the next page of items; more is false after the last.
type pager func(ctx context.Context) (items []map[string]types.AttributeValue, more bool, err error)

// emitItems sends what next reads. With a row limit (the console) it reads
// up to one item past it, for at most 30 seconds, and lays the columns out
// over all items; without one (exports and migrations) it streams every
// page, with the columns named or those of the first page.
func emitItems(ctx context.Context, sink driver.Sink, opts driver.ExecOptions, start time.Time, keys, named []string, next pager) error {
	if opts.MaxRows > 0 {
		var items []map[string]types.AttributeValue
		deadline := time.Now().Add(30 * time.Second)
		truncated := false
		for {
			page, more, err := next(ctx)
			if err != nil {
				return err
			}
			items = append(items, page...)
			if !more || len(items) > opts.MaxRows {
				break
			}
			if time.Now().After(deadline) {
				truncated = true
				break
			}
		}
		cols := itemColumns(items, keys, named)
		rows := make([][]any, len(items))
		for i, item := range items {
			rows[i] = itemRow(item, cols)
		}
		if truncated {
			if err := sink.Notice("info", fmt.Sprintf("Stopped reading after 30 seconds, at %d items; filter by key with Query to read fewer.", len(items))); err != nil {
				return err
			}
		}
		return driver.EmitRows(sink, cols, rows, opts.MaxRows, start)
	}
	full := driver.WantsFullValues(sink)
	var cols []driver.ResultColumn
	sum := driver.ResultSummary{}
	dropped := map[string]bool{}
	for {
		page, more, err := next(ctx)
		if err != nil {
			return err
		}
		if cols == nil {
			cols = itemColumns(page, keys, named)
			if err := sink.Columns(cols); err != nil {
				return err
			}
		}
		if len(page) > 0 {
			rows := make([][]any, len(page))
			for i, item := range page {
				rows[i] = itemRow(item, cols)
				if !full {
					driver.PreviewRow(rows[i])
				}
				if len(named) == 0 && len(item) > len(cols) {
					for k := range item {
						if !hasColumn(cols, k) {
							dropped[k] = true
						}
					}
				}
			}
			if err := sink.Rows(rows); err != nil {
				return err
			}
			sum.RowCount += int64(len(rows))
		}
		if !more {
			break
		}
	}
	if len(dropped) > 0 {
		names := make([]string, 0, len(dropped))
		for k := range dropped {
			names = append(names, k)
		}
		sort.Strings(names)
		if err := sink.Notice("warning", "Attributes first seen after the first page were left out: "+strings.Join(names, ", ")+". Name every attribute in ProjectionExpression to read them all."); err != nil {
			return err
		}
	}
	sum.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	return sink.EndResult(sum)
}

func hasColumn(cols []driver.ResultColumn, name string) bool {
	for _, c := range cols {
		if c.Name == name {
			return true
		}
	}
	return false
}

// ---- PartiQL ------------------------------------------------------------------------

var fromTable = regexp.MustCompile(`(?is)\bFROM\s+(?:"((?:[^"]|"")+)"|([A-Za-z0-9_.-]+))`)

func (c *conn) partiql(ctx context.Context, r call, opts driver.ExecOptions, sink driver.Sink) error {
	start := time.Now()
	in := &dynamodb.ExecuteStatementInput{Statement: aws.String(r.text)}
	for i, p := range opts.Params {
		a, err := infer(p)
		if err != nil {
			return fmt.Errorf("parameter %d: %w", i+1, err)
		}
		in.Parameters = append(in.Parameters, a)
	}
	if r.kind() != driver.StmtRead {
		if _, err := c.db.ExecuteStatement(ctx, in); err != nil {
			return mapError(err)
		}
		one := int64(1)
		return sink.EndResult(driver.ResultSummary{RowsAffected: &one, DurationMS: float64(time.Since(start).Microseconds()) / 1000})
	}
	var keys []string
	if m := fromTable.FindStringSubmatch(r.text); m != nil {
		table := m[2]
		if m[1] != "" {
			table = strings.ReplaceAll(m[1], `""`, `"`)
		}
		table, _, _ = strings.Cut(table, `"."`) // "Table"."Index"
		keys = c.keyOrder(ctx, table)
	}
	return emitItems(ctx, sink, opts, start, keys, nil, func(ctx context.Context) ([]map[string]types.AttributeValue, bool, error) {
		out, err := c.db.ExecuteStatement(ctx, in)
		if err != nil {
			return nil, false, mapError(err)
		}
		in.NextToken = out.NextToken
		return out.Items, out.NextToken != nil, nil
	})
}

// ---- API operations -----------------------------------------------------------------

// invoke sends an operation's input as written, signed, and decodes the
// reply: the console takes DynamoDB JSON just as the API does.
func (c *conn) invoke(ctx context.Context, op string, body []byte) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "DynamoDB_20120810."+op)
	creds, err := c.creds.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not get AWS credentials: %w", err)
	}
	sum := sha256.Sum256(body)
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), "dynamodb", c.region, time.Now()); err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
			Msg     string `json:"Message"`
		}
		json.Unmarshal(data, &e)
		code := e.Type[strings.LastIndex(e.Type, "#")+1:]
		msg := e.Message + e.Msg
		if code == "" && msg == "" {
			msg = fmt.Sprintf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
		}
		return nil, mapError(&smithy.GenericAPIError{Code: code, Message: msg})
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	out := map[string]any{}
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("unexpected reply: %v", err)
	}
	return out, nil
}

func items(v any) ([]map[string]types.AttributeValue, error) {
	arr, _ := v.([]any)
	out := make([]map[string]types.AttributeValue, 0, len(arr))
	for _, e := range arr {
		m, _ := e.(map[string]any)
		item, err := itemFromJSON(m)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// projected names the top-level attributes a ProjectionExpression reads.
func projected(in map[string]any) []string {
	p, _ := in["ProjectionExpression"].(string)
	if strings.TrimSpace(p) == "" {
		return nil
	}
	names, _ := in["ExpressionAttributeNames"].(map[string]any)
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(p, ",") {
		part = strings.TrimSpace(part)
		if i := strings.IndexAny(part, ".["); i >= 0 {
			part = part[:i]
		}
		if n, ok := names[part].(string); ok {
			part = n
		}
		if part != "" && !seen[part] {
			seen[part] = true
			out = append(out, part)
		}
	}
	return out
}

func (c *conn) operation(ctx context.Context, r call, opts driver.ExecOptions, sink driver.Sink) error {
	start := time.Now()
	in := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(r.body))
	dec.UseNumber()
	if err := dec.Decode(&in); err != nil {
		return err
	}
	table, _ := in["TableName"].(string)
	if operations[r.op] == driver.StmtDDL || r.op == "PutItem" || r.op == "UpdateItem" || r.op == "DeleteItem" || r.op == "BatchWriteItem" || r.op == "TransactWriteItems" {
		defer func() {
			c.mu.Lock()
			clear(c.samples)
			c.mu.Unlock()
		}()
	}
	switch r.op {
	case "Scan", "Query":
		_, limited := in["Limit"]
		_, from := in["ExclusiveStartKey"]
		once := limited || from
		if sel, _ := in["Select"].(string); sel == "COUNT" {
			var count, scanned int64
			for {
				out, err := c.invoke(ctx, r.op, r.body)
				if err != nil {
					return err
				}
				n1, _ := out["Count"].(json.Number).Int64()
				n2, _ := out["ScannedCount"].(json.Number).Int64()
				count, scanned = count+n1, scanned+n2
				lek, ok := out["LastEvaluatedKey"]
				if !ok || once {
					break
				}
				in["ExclusiveStartKey"] = lek
				if r.body, err = json.Marshal(in); err != nil {
					return err
				}
			}
			return driver.EmitRows(sink, []driver.ResultColumn{{Name: "count", Type: tN, Kind: driver.KindInt}, {Name: "scanned", Type: tN, Kind: driver.KindInt}},
				[][]any{{driver.EncodeInt(count), driver.EncodeInt(scanned)}}, 0, start)
		}
		body := r.body
		var lastKey any
		err := emitItems(ctx, sink, opts, start, c.keyOrder(ctx, table), projected(in), func(ctx context.Context) ([]map[string]types.AttributeValue, bool, error) {
			out, err := c.invoke(ctx, r.op, body)
			if err != nil {
				return nil, false, err
			}
			list, err := items(out["Items"])
			if err != nil {
				return nil, false, err
			}
			lek, ok := out["LastEvaluatedKey"]
			lastKey = nil
			if !ok {
				return list, false, nil
			}
			lastKey = lek
			if once {
				return list, false, nil
			}
			in["ExclusiveStartKey"] = lek
			body, err = json.Marshal(in)
			return list, true, err
		})
		if err != nil || lastKey == nil || !once {
			return err
		}
		b, _ := json.Marshal(lastKey)
		return sink.Notice("info", "More items follow: run again with \"ExclusiveStartKey\": "+string(b))
	case "GetItem":
		out, err := c.invoke(ctx, r.op, r.body)
		if err != nil {
			return err
		}
		var list []map[string]types.AttributeValue
		if m, ok := out["Item"].(map[string]any); ok {
			item, err := itemFromJSON(m)
			if err != nil {
				return err
			}
			list = append(list, item)
		}
		return emitItems(ctx, sink, opts, start, c.keyOrder(ctx, table), projected(in), func(context.Context) ([]map[string]types.AttributeValue, bool, error) {
			return list, false, nil
		})
	case "TransactGetItems":
		out, err := c.invoke(ctx, r.op, r.body)
		if err != nil {
			return err
		}
		var list []map[string]types.AttributeValue
		resp, _ := out["Responses"].([]any)
		for _, e := range resp {
			m, _ := e.(map[string]any)
			if raw, ok := m["Item"].(map[string]any); ok {
				item, err := itemFromJSON(raw)
				if err != nil {
					return err
				}
				list = append(list, item)
			}
		}
		return emitItems(ctx, sink, opts, start, nil, nil, func(context.Context) ([]map[string]types.AttributeValue, bool, error) {
			return list, false, nil
		})
	case "BatchGetItem":
		out, err := c.invoke(ctx, r.op, r.body)
		if err != nil {
			return err
		}
		resp, _ := out["Responses"].(map[string]any)
		tables := make([]string, 0, len(resp))
		for t := range resp {
			tables = append(tables, t)
		}
		sort.Strings(tables)
		for _, t := range tables {
			list, err := items(resp[t])
			if err != nil {
				return err
			}
			if err := sink.Notice("info", "Table "+t); err != nil {
				return err
			}
			if err := emitItems(ctx, sink, opts, start, c.keyOrder(ctx, t), nil, func(context.Context) ([]map[string]types.AttributeValue, bool, error) {
				return list, false, nil
			}); err != nil {
				return err
			}
		}
		if u, _ := out["UnprocessedKeys"].(map[string]any); len(u) > 0 {
			b, _ := json.Marshal(u)
			return sink.Notice("warning", "DynamoDB left keys unread for now; run again with these as RequestItems: "+string(b))
		}
		return nil
	case "ListTables":
		var names [][]any
		for {
			out, err := c.invoke(ctx, r.op, r.body)
			if err != nil {
				return err
			}
			list, _ := out["TableNames"].([]any)
			for _, n := range list {
				names = append(names, []any{n})
			}
			last, ok := out["LastEvaluatedTableName"]
			if _, limited := in["Limit"]; !ok || limited {
				break
			}
			in["ExclusiveStartTableName"] = last
			if r.body, err = json.Marshal(in); err != nil {
				return err
			}
		}
		return driver.EmitRows(sink, []driver.ResultColumn{{Name: "TableName", Type: tS, Kind: driver.KindString}}, names, opts.MaxRows, start)
	}
	out, err := c.invoke(ctx, r.op, r.body)
	if err != nil {
		return err
	}
	switch r.op {
	case "PutItem", "UpdateItem", "DeleteItem":
		if m, ok := out["Attributes"].(map[string]any); ok {
			item, err := itemFromJSON(m)
			if err != nil {
				return err
			}
			if err := sink.Notice("info", "The item's attributes as ReturnValues asked"); err != nil {
				return err
			}
			return emitItems(ctx, sink, opts, start, c.keyOrder(ctx, table), nil, func(context.Context) ([]map[string]types.AttributeValue, bool, error) {
				return []map[string]types.AttributeValue{item}, false, nil
			})
		}
		one := int64(1)
		return sink.EndResult(driver.ResultSummary{RowsAffected: &one, DurationMS: float64(time.Since(start).Microseconds()) / 1000})
	case "BatchWriteItem", "TransactWriteItems":
		n := int64(0)
		if reqs, ok := in["RequestItems"].(map[string]any); ok {
			for _, list := range reqs {
				l, _ := list.([]any)
				n += int64(len(l))
			}
		} else if list, ok := in["TransactItems"].([]any); ok {
			n = int64(len(list))
		}
		if u, _ := out["UnprocessedItems"].(map[string]any); len(u) > 0 {
			left := 0
			for _, list := range u {
				l, _ := list.([]any)
				left += len(l)
			}
			n -= int64(left)
			b, _ := json.Marshal(u)
			if err := sink.Notice("warning", fmt.Sprintf("DynamoDB left %d writes undone for now; run again with these as RequestItems: %s", left, b)); err != nil {
				return err
			}
		}
		return sink.EndResult(driver.ResultSummary{RowsAffected: &n, DurationMS: float64(time.Since(start).Microseconds()) / 1000})
	}
	return emitReply(sink, out, opts, start)
}

// emitReply shows any other reply as names and values; a reply of one
// object (DescribeTable's Table) shows that object's.
func emitReply(sink driver.Sink, out map[string]any, opts driver.ExecOptions, start time.Time) error {
	if len(out) == 1 {
		for _, v := range out {
			if m, ok := v.(map[string]any); ok {
				out = m
			}
		}
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([][]any, len(keys))
	for i, k := range keys {
		rows[i] = []any{k, plain(out[k])}
	}
	return driver.EmitRows(sink, []driver.ResultColumn{{Name: "name", Type: tS, Kind: driver.KindString}, {Name: "value", Type: "json", Kind: driver.KindObject}},
		rows, opts.MaxRows, start)
}

// plain encodes a JSON value of a reply.
func plain(v any) any {
	switch x := v.(type) {
	case json.Number:
		return number(x.String())
	case string:
		return driver.EncodeText(x)
	case map[string]any:
		d := driver.Doc{Values: map[string]any{}}
		for k, e := range x {
			d.Keys = append(d.Keys, k)
			d.Values[k] = plain(e)
		}
		sort.Strings(d.Keys)
		return d
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plain(e)
		}
		return out
	}
	return v
}
