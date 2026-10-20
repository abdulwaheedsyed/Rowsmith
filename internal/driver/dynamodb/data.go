package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"rowsmith/internal/driver"
)

var (
	_ driver.BrowseQuerier = (*conn)(nil)
	_ driver.BulkImporter  = (*conn)(nil)
)

// expr collects an expression's attribute names (#a0) and values (:v0).
type expr struct {
	names  map[string]string
	values map[string]types.AttributeValue
	byName map[string]string
}

func newExpr() *expr {
	return &expr{names: map[string]string{}, values: map[string]types.AttributeValue{}, byName: map[string]string{}}
}

func (e *expr) name(attr string) string {
	if n, ok := e.byName[attr]; ok {
		return n
	}
	n := "#a" + strconv.Itoa(len(e.byName))
	e.byName[attr], e.names[n] = n, attr
	return n
}

func (e *expr) value(v types.AttributeValue) string {
	n := ":v" + strconv.Itoa(len(e.values))
	e.values[n] = v
	return n
}

// request is a Query or Scan for a browse request.
type request struct {
	query            bool
	index            string
	keyCond, filter  string
	e                *expr
	forward          *bool
	projection       string
}

func elemType(t string) string {
	switch t {
	case tSS:
		return tS
	case tNS:
		return tN
	case tBS:
		return tB
	}
	return ""
}

// plan chooses a Query when the partition key of the table, or of an index
// that projects every attribute, is filtered with =; otherwise a Scan.
// Conditions on the sort key go into the key condition, the rest into the
// filter, which DynamoDB applies after reading.
func plan(t *driver.Table, l *layout, req driver.BrowseRequest) (*request, error) {
	r := &request{e: newExpr()}
	cols := map[string]*driver.Column{}
	for i := range t.Columns {
		cols[t.Columns[i].Name] = &t.Columns[i]
	}
	typeOfCol := func(name string) string {
		if c := cols[name]; c != nil {
			return c.Type
		}
		return ""
	}
	eq := map[string]bool{}
	for _, f := range req.Filters {
		if f.Op == "=" {
			eq[f.Column] = true
		}
	}
	hash, rng := "", ""
	switch {
	case eq[l.hash]:
		hash, rng, r.query = l.hash, l.rng, true
	default:
		for _, ix := range l.indexes {
			if ix.all && eq[ix.hash] {
				hash, rng, r.query, r.index = ix.hash, ix.rng, true, ix.name
				break
			}
		}
	}
	val := func(col string, v any) (string, error) {
		a, err := attribute(typeOfCol(col), v)
		if err != nil {
			return "", fmt.Errorf("%s: %w", col, err)
		}
		return r.e.value(a), nil
	}
	var keyConds, filters []string
	usedHash, usedRange := false, false
	for _, f := range req.Filters {
		n := r.e.name(f.Column)
		var cond string
		switch f.Op {
		case "=", "!=", "<", "<=", ">", ">=":
			v, err := val(f.Column, f.Value)
			if err != nil {
				return nil, err
			}
			op := map[string]string{"!=": "<>"}[f.Op]
			if op == "" {
				op = f.Op
			}
			cond = n + " " + op + " " + v
		case "between":
			if len(f.Values) != 2 {
				return nil, fmt.Errorf("%s: between needs two values", f.Column)
			}
			a, err := val(f.Column, f.Values[0])
			if err != nil {
				return nil, err
			}
			b, err := val(f.Column, f.Values[1])
			if err != nil {
				return nil, err
			}
			cond = n + " BETWEEN " + a + " AND " + b
		case "in", "notin":
			var vs []string
			for _, x := range f.Values {
				v, err := val(f.Column, x)
				if err != nil {
					return nil, err
				}
				vs = append(vs, v)
			}
			if len(vs) == 0 || len(vs) > 100 {
				return nil, fmt.Errorf("%s: in takes 1 to 100 values", f.Column)
			}
			cond = n + " IN (" + strings.Join(vs, ", ") + ")"
			if f.Op == "notin" {
				cond = "NOT (" + cond + ")"
			}
		case "startswith":
			v, err := val(f.Column, f.Value)
			if err != nil {
				return nil, err
			}
			cond = "begins_with(" + n + ", " + v + ")"
		case "contains":
			et := elemType(typeOfCol(f.Column))
			if typeOfCol(f.Column) == tS || typeOfCol(f.Column) == "" {
				et = tS
			}
			a, err := attribute(et, f.Value)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.Column, err)
			}
			cond = "contains(" + n + ", " + r.e.value(a) + ")"
		case "notnull":
			cond = "attribute_exists(" + n + ") AND NOT attribute_type(" + n + ", " + r.e.value(&types.AttributeValueMemberS{Value: tNULL}) + ")"
		case "null":
			cond = "(attribute_not_exists(" + n + ") OR attribute_type(" + n + ", " + r.e.value(&types.AttributeValueMemberS{Value: tNULL}) + "))"
		default:
			return nil, fmt.Errorf("%s: DynamoDB filters with =, ≠, <, ≤, >, ≥, between, in, begins with, contains and is (not) empty", f.Column)
		}
		switch {
		case r.query && f.Column == hash && f.Op == "=" && !usedHash:
			keyConds, usedHash = append(keyConds, cond), true
		case r.query && f.Column == rng && rng != "" && !usedRange && (f.Op == "=" || f.Op == "<" || f.Op == "<=" || f.Op == ">" || f.Op == ">=" || f.Op == "between" || f.Op == "startswith"):
			keyConds, usedRange = append(keyConds, cond), true
		default:
			filters = append(filters, cond)
		}
	}
	if s := strings.TrimSpace(req.Search); s != "" {
		var any []string
		for _, c := range t.Columns {
			if c.Type == tS || c.Type == tSS {
				any = append(any, "contains("+r.e.name(c.Name)+", "+r.e.value(&types.AttributeValueMemberS{Value: s})+")")
			}
		}
		if len(any) == 0 {
			return nil, errors.New("no text attributes to search")
		}
		filters = append(filters, "("+strings.Join(any, " OR ")+")")
	}
	if strings.TrimSpace(req.Where) != "" {
		return nil, errors.New("DynamoDB takes filters, not a written condition; use the console for PartiQL")
	}
	r.keyCond, r.filter = strings.Join(keyConds, " AND "), strings.Join(filters, " AND ")
	for _, s := range req.Sort {
		if !r.query || s.Column != rng || rng == "" || len(req.Sort) > 1 {
			return nil, errors.New("DynamoDB returns items in sort key order within one partition: filter the partition key with =, then sort by the sort key")
		}
		r.forward = aws.Bool(!s.Desc)
	}
	if len(req.Columns) > 0 {
		var ps []string
		for _, n := range req.Columns {
			ps = append(ps, r.e.name(n))
		}
		r.projection = strings.Join(ps, ", ")
	}
	return r, nil
}

func (r *request) scanInput(table string, start map[string]types.AttributeValue, limit int32) *dynamodb.ScanInput {
	in := &dynamodb.ScanInput{TableName: aws.String(table), ExclusiveStartKey: start, Limit: aws.Int32(limit)}
	if r.filter != "" {
		in.FilterExpression = aws.String(r.filter)
	}
	if r.projection != "" {
		in.ProjectionExpression = aws.String(r.projection)
	}
	if len(r.e.names) > 0 {
		in.ExpressionAttributeNames = r.e.names
	}
	if len(r.e.values) > 0 {
		in.ExpressionAttributeValues = r.e.values
	}
	return in
}

func (r *request) queryInput(table string, start map[string]types.AttributeValue, limit int32) *dynamodb.QueryInput {
	in := &dynamodb.QueryInput{TableName: aws.String(table), ExclusiveStartKey: start, Limit: aws.Int32(limit),
		KeyConditionExpression: aws.String(r.keyCond), ScanIndexForward: r.forward, ExpressionAttributeNames: r.e.names}
	if r.index != "" {
		in.IndexName = aws.String(r.index)
	}
	if r.filter != "" {
		in.FilterExpression = aws.String(r.filter)
	}
	if r.projection != "" {
		in.ProjectionExpression = aws.String(r.projection)
	}
	if len(r.e.values) > 0 {
		in.ExpressionAttributeValues = r.e.values
	}
	return in
}

// page reads one page: items, the key to continue from, and how many items
// DynamoDB read (before its filter).
func (c *conn) page(ctx context.Context, table string, r *request, start map[string]types.AttributeValue, limit int32) ([]map[string]types.AttributeValue, map[string]types.AttributeValue, int32, error) {
	if r.query {
		out, err := c.db.Query(ctx, r.queryInput(table, start, limit))
		if err != nil {
			return nil, nil, 0, mapError(err)
		}
		return out.Items, out.LastEvaluatedKey, out.ScannedCount, nil
	}
	out, err := c.db.Scan(ctx, r.scanInput(table, start, limit))
	if err != nil {
		return nil, nil, 0, mapError(err)
	}
	return out.Items, out.LastEvaluatedKey, out.ScannedCount, nil
}

// requestText shows a request as the console would run it.
func (r *request) text(table string) string {
	op, body := "Scan", map[string]any{"TableName": table}
	if r.query {
		op = "Query"
		body["KeyConditionExpression"] = r.keyCond
		if r.index != "" {
			body["IndexName"] = r.index
		}
		if r.forward != nil {
			body["ScanIndexForward"] = *r.forward
		}
	}
	if r.filter != "" {
		body["FilterExpression"] = r.filter
	}
	if r.projection != "" {
		body["ProjectionExpression"] = r.projection
	}
	if len(r.e.names) > 0 {
		body["ExpressionAttributeNames"] = r.e.names
	}
	if len(r.e.values) > 0 {
		body["ExpressionAttributeValues"] = itemJSON(r.e.values)
	}
	b, _ := json.MarshalIndent(body, "", "  ")
	return op + "\n" + string(b)
}

func (c *conn) Browse(ctx context.Context, req driver.BrowseRequest) (*driver.Result, error) {
	start := time.Now()
	t, l, err := c.describe(ctx, req.Ref, false)
	if err != nil {
		return nil, err
	}
	r, err := plan(t, l, req)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	cols := t.Columns
	if len(req.Columns) > 0 {
		cols = nil
		for _, n := range req.Columns {
			for _, col := range t.Columns {
				if col.Name == n {
					cols = append(cols, col)
				}
			}
		}
	}
	idx := map[string]int{}
	res := &driver.Result{Rows: [][]any{}, SQL: r.text(t.Ref.Name)}
	for i, col := range cols {
		idx[col.Name] = i
		res.Columns = append(res.Columns, driver.ResultColumn{Name: col.Name, Type: col.Type, Kind: col.Kind})
	}
	// No offset in DynamoDB: page past the earlier items, one more to tell
	// whether others follow.
	var skipped int64
	var key map[string]types.AttributeValue
	deadline := time.Now().Add(30 * time.Second)
	for {
		items, next, _, err := c.page(ctx, t.Ref.Name, r, key, int32(min(int64(limit)+req.Offset+1, 1000)))
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if skipped < req.Offset {
				skipped++
				continue
			}
			if len(res.Rows) == limit {
				res.Truncated = true
				break
			}
			row := make([]any, len(res.Columns))
			for name, v := range item {
				i, ok := idx[name]
				if !ok {
					if len(req.Columns) > 0 {
						continue
					}
					// An attribute the sample missed becomes a column.
					i = len(res.Columns)
					idx[name] = i
					res.Columns = append(res.Columns, driver.ResultColumn{Name: name, Type: typeOf(v), Kind: kindOf(typeOf(v))})
					for j := range res.Rows {
						res.Rows[j] = append(res.Rows[j], nil)
					}
					row = append(row, nil)
				}
				row[i] = cell(v)
			}
			driver.PreviewRow(row)
			res.Rows = append(res.Rows, row)
		}
		if res.Truncated || next == nil {
			break
		}
		if time.Now().After(deadline) {
			res.Truncated = true // more may follow; DynamoDB read for 30 seconds
			break
		}
		key = next
	}
	res.Duration = time.Since(start)
	res.DurationMS = float64(res.Duration.Microseconds()) / 1000
	return res, nil
}

// Count gives DynamoDB's item count when unfiltered (refreshed about every
// six hours), or counts the matching items, for up to 30 seconds.
func (c *conn) Count(ctx context.Context, req driver.BrowseRequest) (driver.Count, error) {
	t, l, err := c.describe(ctx, req.Ref, false)
	if err != nil {
		return driver.Count{}, err
	}
	if len(req.Filters) == 0 && strings.TrimSpace(req.Search) == "" && t.RowEstimate != nil {
		return driver.Count{Rows: *t.RowEstimate, Exact: c.endpoint != ""}, nil
	}
	r, err := plan(t, l, req)
	if err != nil {
		return driver.Count{}, err
	}
	var n int64
	var key map[string]types.AttributeValue
	deadline := time.Now().Add(30 * time.Second)
	for {
		var count int32
		var next map[string]types.AttributeValue
		if r.query {
			in := r.queryInput(t.Ref.Name, key, 0)
			in.Limit, in.Select = nil, types.SelectCount
			out, err := c.db.Query(ctx, in)
			if err != nil {
				return driver.Count{}, mapError(err)
			}
			count, next = out.Count, out.LastEvaluatedKey
		} else {
			in := r.scanInput(t.Ref.Name, key, 0)
			in.Limit, in.Select = nil, types.SelectCount
			out, err := c.db.Scan(ctx, in)
			if err != nil {
				return driver.Count{}, mapError(err)
			}
			count, next = out.Count, out.LastEvaluatedKey
		}
		n += int64(count)
		if next == nil {
			return driver.Count{Rows: n, Exact: true}, nil
		}
		if time.Now().After(deadline) {
			return driver.Count{Rows: n}, nil
		}
		key = next
	}
}

// ---- edits ------------------------------------------------------------------------------

func keyOf(l *layout, key map[string]any) (map[string]types.AttributeValue, error) {
	out := map[string]types.AttributeValue{}
	for _, k := range []string{l.hash, l.rng} {
		if k == "" {
			continue
		}
		v, ok := key[k]
		if !ok || v == nil {
			return nil, fmt.Errorf("missing key attribute %s", k)
		}
		a, err := attribute(l.types[k], v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = a
	}
	return out, nil
}

// ApplyEdits writes the changes in one transaction (100 at a time): an
// insert only when no item has its key, updates and deletes only of items
// that still exist. One failed condition saves nothing of its transaction.
func (c *conn) ApplyEdits(ctx context.Context, ref driver.ObjectRef, edits []driver.RowEdit) (*driver.EditResult, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	_, l, err := c.describe(ctx, ref, false)
	if err != nil {
		return nil, err
	}
	defer c.forget(ref.Name)
	table := aws.String(ref.Name)
	res := &driver.EditResult{}
	for start := 0; start < len(edits); start += 100 {
		batch := edits[start:min(start+100, len(edits))]
		var items []types.TransactWriteItem
		var texts []string
		for i, ed := range batch {
			n := start + i + 1
			e := newExpr()
			exists := "attribute_exists(" + e.name(l.hash) + ")"
			switch ed.Op {
			case "insert":
				item := map[string]types.AttributeValue{}
				for k, v := range ed.Values {
					a, err := attribute(l.types[k], v)
					if err != nil {
						return nil, fmt.Errorf("change %d: %s: %w", n, k, err)
					}
					if _, isNull := a.(*types.AttributeValueMemberNULL); isNull && (k == l.hash || k == l.rng) {
						return nil, fmt.Errorf("change %d: the key attribute %s needs a value", n, k)
					}
					item[k] = a
				}
				if _, err := keyOf(l, ed.Values); err != nil {
					return nil, fmt.Errorf("change %d: %w", n, err)
				}
				notExists := "attribute_not_exists(" + e.name(l.hash) + ")"
				items = append(items, types.TransactWriteItem{Put: &types.Put{TableName: table, Item: item,
					ConditionExpression: aws.String(notExists), ExpressionAttributeNames: e.names}})
				b, _ := json.Marshal(itemJSON(item))
				texts = append(texts, "PutItem "+string(b)+" if "+notExists)
			case "update":
				key, err := keyOf(l, ed.Key)
				if err != nil {
					return nil, fmt.Errorf("change %d: %w", n, err)
				}
				var sets, removes []string
				names := make([]string, 0, len(ed.Values))
				for k := range ed.Values {
					names = append(names, k)
				}
				sort.Strings(names)
				for _, k := range names {
					if k == l.hash || k == l.rng {
						return nil, fmt.Errorf("change %d: %s is part of the key, which can't change; insert a new item and delete this one", n, k)
					}
					v := ed.Values[k]
					if m, ok := v.(map[string]any); v == nil || ok && m["$null"] == true {
						removes = append(removes, e.name(k)) // an empty cell removes the attribute
						continue
					}
					a, err := attribute(l.types[k], v)
					if err != nil {
						return nil, fmt.Errorf("change %d: %s: %w", n, k, err)
					}
					sets = append(sets, e.name(k)+" = "+e.value(a))
				}
				if len(sets)+len(removes) == 0 {
					continue
				}
				var parts []string
				if len(sets) > 0 {
					parts = append(parts, "SET "+strings.Join(sets, ", "))
				}
				if len(removes) > 0 {
					parts = append(parts, "REMOVE "+strings.Join(removes, ", "))
				}
				up := &types.Update{TableName: table, Key: key, UpdateExpression: aws.String(strings.Join(parts, " ")),
					ConditionExpression: aws.String(exists), ExpressionAttributeNames: e.names}
				if len(e.values) > 0 {
					up.ExpressionAttributeValues = e.values
				}
				items = append(items, types.TransactWriteItem{Update: up})
				b, _ := json.Marshal(itemJSON(key))
				texts = append(texts, "UpdateItem "+string(b)+" "+strings.Join(parts, " ")+" if "+exists)
			case "delete":
				key, err := keyOf(l, ed.Key)
				if err != nil {
					return nil, fmt.Errorf("change %d: %w", n, err)
				}
				items = append(items, types.TransactWriteItem{Delete: &types.Delete{TableName: table, Key: key,
					ConditionExpression: aws.String(exists), ExpressionAttributeNames: e.names}})
				b, _ := json.Marshal(itemJSON(key))
				texts = append(texts, "DeleteItem "+string(b)+" if "+exists)
			default:
				return nil, fmt.Errorf("change %d: unknown edit operation %q", n, ed.Op)
			}
		}
		if len(items) == 0 {
			continue
		}
		_, err := c.db.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: items})
		if err != nil {
			var tc *types.TransactionCanceledException
			if errors.As(err, &tc) {
				for i, reason := range tc.CancellationReasons {
					if aws.ToString(reason.Code) == "ConditionalCheckFailed" {
						msg := "the item no longer exists or was modified by someone else"
						if batch[i].Op == "insert" {
							msg = "an item with this key already exists"
						}
						err = fmt.Errorf("change %d: %s; nothing in this save was written", start+i+1, msg)
						break
					}
				}
			}
			if res.Applied > 0 {
				err = fmt.Errorf("%w (the %d changes before them were saved)", mapError(err), res.Applied)
			}
			return nil, mapError(err)
		}
		res.Statements = append(res.Statements, texts...)
		res.Applied += len(items)
	}
	return res, nil
}

// ---- exports and imports ------------------------------------------------------------------

// BrowseQuery is the whole (filtered) table as a console Scan or Query,
// which the console reads page by page for exports and migrations.
func (c *conn) BrowseQuery(ctx context.Context, t *driver.Table, req driver.BrowseRequest) (string, []any, error) {
	_, l, err := c.describe(ctx, t.Ref, false)
	if err != nil {
		return "", nil, err
	}
	req.Sort = nil
	r, err := plan(t, l, req)
	if err != nil {
		return "", nil, err
	}
	return r.text(t.Ref.Name), nil, nil
}

// BeginImport writes items with BatchWriteItem, 25 at a time, retrying what
// DynamoDB leaves unprocessed. A put replaces an item with the same key.
func (c *conn) BeginImport(ctx context.Context, t *driver.Table, cols []string, empty bool) (driver.RowImporter, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	_, l, err := c.describe(ctx, t.Ref, false)
	if err != nil {
		return nil, err
	}
	for _, k := range t.PrimaryKey {
		found := false
		for _, n := range cols {
			found = found || n == k
		}
		if !found {
			return nil, fmt.Errorf("map the key attribute %s: every item needs it", k)
		}
	}
	// Types the caller knows (a migration's target columns) count where the
	// table records none.
	for _, col := range t.Columns {
		if _, known := l.types[col.Name]; !known && slices.Contains(attributeTypes, col.Type) {
			l.types[col.Name] = col.Type
		}
	}
	im := &importer{c: c, table: t.Ref.Name, cols: cols, l: l}
	if empty {
		if err := im.deleteAll(ctx); err != nil {
			return nil, err
		}
	}
	return im, nil
}

type importer struct {
	c     *conn
	table string
	cols  []string
	l     *layout
}

func (im *importer) write(ctx context.Context, reqs []types.WriteRequest) error {
	pending := map[string][]types.WriteRequest{im.table: reqs}
	for wait := 50 * time.Millisecond; len(pending[im.table]) > 0; wait = min(wait*2, 5*time.Second) {
		out, err := im.c.db.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: pending})
		if err != nil {
			return mapError(err)
		}
		pending = out.UnprocessedItems
		if len(pending[im.table]) > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}

func (im *importer) Insert(ctx context.Context, rows [][]any) error {
	var reqs []types.WriteRequest
	for n, r := range rows {
		item := map[string]types.AttributeValue{}
		for j, name := range im.cols {
			if j >= len(r) || r[j] == nil {
				continue // DynamoDB has no columns: an empty value is no attribute
			}
			a, err := attribute(im.l.types[name], r[j])
			if err != nil {
				return fmt.Errorf("row %d: %s: %w", n+1, name, err)
			}
			item[name] = a
		}
		reqs = append(reqs, types.WriteRequest{PutRequest: &types.PutRequest{Item: item}})
		if len(reqs) == 25 {
			if err := im.write(ctx, reqs); err != nil {
				return fmt.Errorf("rows up to %d: %w", n+1, err)
			}
			reqs = nil
		}
	}
	if len(reqs) > 0 {
		return im.write(ctx, reqs)
	}
	return nil
}

// deleteAll empties the table item by item: DynamoDB has no truncate.
func (im *importer) deleteAll(ctx context.Context) error {
	e := newExpr()
	proj := e.name(im.l.hash)
	if im.l.rng != "" {
		proj += ", " + e.name(im.l.rng)
	}
	p := dynamodb.NewScanPaginator(im.c.db, &dynamodb.ScanInput{TableName: aws.String(im.table), ProjectionExpression: aws.String(proj), ExpressionAttributeNames: e.names})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return mapError(err)
		}
		for start := 0; start < len(page.Items); start += 25 {
			var reqs []types.WriteRequest
			for _, key := range page.Items[start:min(start+25, len(page.Items))] {
				reqs = append(reqs, types.WriteRequest{DeleteRequest: &types.DeleteRequest{Key: key}})
			}
			if err := im.write(ctx, reqs); err != nil {
				return err
			}
		}
	}
	return nil
}

func (im *importer) Commit() error {
	im.c.forget(im.table)
	return nil
}

func (im *importer) Rollback() error { return nil }
