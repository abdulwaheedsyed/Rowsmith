package dynamodb

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

// Integration tests run against DynamoDB Local from the dev stack
// (dev/compose.yaml, seeded by dev/seed/dynamodb-seed.sh) when
// ROWSMITH_TEST_DYNAMODB_ENDPOINT is set, such as http://dynamodb:8000.
// They create and delete their own it_ tables.

func open(t *testing.T) *conn {
	t.Helper()
	endpoint := os.Getenv("ROWSMITH_TEST_DYNAMODB_ENDPOINT")
	if endpoint == "" {
		t.Skip("ROWSMITH_TEST_DYNAMODB_ENDPOINT not set")
	}
	d, _ := driver.Get("dynamodb")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cn, err := d.Open(ctx, driver.OpenParams{Params: map[string]any{"region": "us-east-1", "endpoint": endpoint, "accessKeyId": "rowsmith"},
		Secrets: map[string]string{"secretAccessKey": "rowsmith"}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cn.Close() })
	return cn.(*conn)
}

type sink struct {
	stmts    []driver.StatementInfo
	errs     []error
	cols     [][]driver.ResultColumn
	rows     [][][]any
	notices  []string
	affected []int64
}

func (s *sink) BeginStatement(i driver.StatementInfo) error { s.stmts = append(s.stmts, i); return nil }
func (s *sink) Columns(c []driver.ResultColumn) error {
	s.cols = append(s.cols, c)
	s.rows = append(s.rows, nil)
	return nil
}
func (s *sink) Rows(r [][]any) error {
	s.rows[len(s.rows)-1] = append(s.rows[len(s.rows)-1], r...)
	return nil
}
func (s *sink) EndResult(sum driver.ResultSummary) error {
	if sum.RowsAffected != nil {
		s.affected = append(s.affected, *sum.RowsAffected)
	}
	return nil
}
func (s *sink) Notice(_, text string) error  { s.notices = append(s.notices, text); return nil }
func (s *sink) EndStatement(err error) error { s.errs = append(s.errs, err); return nil }

func run(t *testing.T, sess driver.Session, script string, maxRows int) *sink {
	t.Helper()
	out := &sink{}
	if err := sess.Execute(context.Background(), script, driver.ExecOptions{StopOnError: true, MaxRows: maxRows}, out); err != nil {
		t.Fatalf("execute: %v", err)
	}
	for i, err := range out.errs {
		if err != nil {
			t.Fatalf("%s: %v", out.stmts[i].SQL, err)
		}
	}
	return out
}

func colNames(cols []driver.ResultColumn) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

func column(t *testing.T, cols []driver.ResultColumn, name string) int {
	t.Helper()
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	t.Fatalf("no column %s in %v", name, colNames(cols))
	return -1
}

func TestIntegrationCatalog(t *testing.T) {
	c := open(t)
	ctx := context.Background()
	objs, err := c.Objects(ctx, driver.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]*int64{}
	for _, o := range objs {
		counts[o.Name] = o.Rows
	}
	for name, want := range map[string]int64{"Customers": 50, "Orders": 300, "Sessions": 20} {
		if counts[name] == nil || *counts[name] != want {
			t.Errorf("%s: %v items, want %d", name, counts[name], want)
		}
	}

	cust, err := c.Describe(ctx, driver.ObjectRef{Name: "Customers"})
	if err != nil {
		t.Fatal(err)
	}
	if cust.Columns[0].Name != "customer_id" || !slices.Equal(cust.PrimaryKey, []string{"customer_id"}) {
		t.Errorf("key: %v %v", colNames(nil), cust.PrimaryKey)
	}
	types := map[string]string{}
	for _, col := range cust.Columns {
		types[col.Name] = col.Type
	}
	for name, want := range map[string]string{"address": "M", "tags": "SS", "balance": "N", "active": "BOOL", "tier": "S", "signed_up": "S"} {
		if types[name] != want {
			t.Errorf("%s is %q, want %s", name, types[name], want)
		}
	}
	var gsi *driver.Index
	for i := range cust.Indexes {
		if cust.Indexes[i].Name == "ByTier" {
			gsi = &cust.Indexes[i]
		}
	}
	if gsi == nil || gsi.Type != "global" || !slices.Equal(gsi.Columns, []string{"tier", "signed_up"}) || gsi.Definition != "projects ALL" {
		t.Errorf("ByTier: %+v", gsi)
	}
	if cust.Options["billing"] != "PAY_PER_REQUEST" || !strings.HasPrefix(cust.DDL, "CreateTable\n") || !strings.Contains(cust.DDL, `"ByTier"`) {
		t.Errorf("options %v, DDL:\n%s", cust.Options, cust.DDL)
	}

	sess, _ := c.Describe(ctx, driver.ObjectRef{Name: "Sessions"})
	if sess.Options["billing"] != "PROVISIONED" || sess.Options["read_capacity"] != "5" || sess.Options["ttl_attribute"] != "expires_at" {
		t.Errorf("Sessions options: %v", sess.Options)
	}
	orders, _ := c.Describe(ctx, driver.ObjectRef{Name: "Orders"})
	if !slices.Equal(orders.PrimaryKey, []string{"customer_id", "order_id"}) || !strings.Contains(orders.DDL, "LocalSecondaryIndexes") {
		t.Errorf("Orders: %v\n%s", orders.PrimaryKey, orders.DDL)
	}
}

func TestIntegrationBrowse(t *testing.T) {
	c := open(t)
	ctx := context.Background()
	ref := driver.ObjectRef{Name: "Orders"}

	// The partition key with = reads by Query, in sort key order.
	res, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 10, Filters: []driver.Filter{{Column: "customer_id", Op: "=", Value: "C0002"}},
		Sort: []driver.Sort{{Column: "order_id", Desc: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.SQL, "Query\n") || len(res.Rows) != 6 || res.Truncated {
		t.Fatalf("query: %d rows, truncated %v\n%s", len(res.Rows), res.Truncated, res.SQL)
	}
	oi := column(t, res.Columns, "order_id")
	for i := 1; i < len(res.Rows); i++ {
		if res.Rows[i-1][oi].(string) < res.Rows[i][oi].(string) {
			t.Fatalf("not descending: %v", res.Rows)
		}
	}
	// Offsets page past items; the sort key narrows the key condition.
	res, err = c.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 2, Offset: 2, Filters: []driver.Filter{{Column: "customer_id", Op: "=", Value: "C0002"},
		{Column: "order_id", Op: "startswith", Value: "2025"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 || !res.Truncated || !strings.Contains(res.SQL, "begins_with") {
		t.Fatalf("offset: %d rows, truncated %v\n%s", len(res.Rows), res.Truncated, res.SQL)
	}

	// Anything else scans with a filter.
	res, err = c.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 500, Filters: []driver.Filter{{Column: "total", Op: ">", Value: "100"}, {Column: "status", Op: "in", Values: []any{"paid", "shipped"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.SQL, "Scan\n") || len(res.Rows) == 0 {
		t.Fatalf("scan: %d rows\n%s", len(res.Rows), res.SQL)
	}
	ti, si := column(t, res.Columns, "total"), column(t, res.Columns, "status")
	for _, r := range res.Rows {
		if s := r[si].(string); s != "paid" && s != "shipped" {
			t.Fatalf("status %v", s)
		}
		if tot := r[ti].(string); len(tot) < 5 && tot < "100" {
			t.Fatalf("total %v", tot)
		}
	}
	n, err := c.Count(ctx, driver.BrowseRequest{Ref: ref, Filters: []driver.Filter{{Column: "total", Op: ">", Value: "100"}, {Column: "status", Op: "in", Values: []any{"paid", "shipped"}}}})
	if err != nil || n.Rows != int64(len(res.Rows)) || !n.Exact {
		t.Fatalf("count %+v (%v), browse %d", n, err, len(res.Rows))
	}
	if n, err := c.Count(ctx, driver.BrowseRequest{Ref: ref}); err != nil || n.Rows != 300 {
		t.Fatalf("count all %+v %v", n, err)
	}

	// A global index that projects everything serves = on its partition key.
	res, err = c.Browse(ctx, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "Customers"}, Limit: 100, Filters: []driver.Filter{{Column: "tier", Op: "=", Value: "pro"}}})
	if err != nil || !strings.Contains(res.SQL, `"IndexName": "ByTier"`) || len(res.Rows) != 13 {
		t.Fatalf("index: %v %d\n%s", err, len(res.Rows), res.SQL)
	}
	// Search looks in text attributes and string sets.
	res, err = c.Browse(ctx, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "Customers"}, Limit: 100, Search: "wholesale"})
	if err != nil || len(res.Rows) == 0 {
		t.Fatalf("search: %v %d", err, len(res.Rows))
	}
	if _, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 5, Sort: []driver.Sort{{Column: "total"}}}); err == nil {
		t.Fatal("sorting a scan should be refused")
	}
}

const itDDL = `CreateTable
{"TableName": "it_items", "BillingMode": "PAY_PER_REQUEST",
 "AttributeDefinitions": [{"AttributeName": "pk", "AttributeType": "S"}, {"AttributeName": "sk", "AttributeType": "N"}],
 "KeySchema": [{"AttributeName": "pk", "KeyType": "HASH"}, {"AttributeName": "sk", "KeyType": "RANGE"}]}`

func fresh(t *testing.T, c *conn, sess driver.Session) {
	t.Helper()
	out := &sink{}
	sess.Execute(context.Background(), "DeleteTable\n{\"TableName\": \"it_items\"}", driver.ExecOptions{}, out)
	run(t, sess, itDDL, 0)
	t.Cleanup(func() { sess.Execute(context.Background(), "DeleteTable\n{\"TableName\": \"it_items\"}", driver.ExecOptions{}, &sink{}) })
}

func TestIntegrationConsole(t *testing.T) {
	c := open(t)
	ctx := context.Background()
	sess, _ := c.NewSession(ctx, driver.Scope{})
	fresh(t, c, sess)

	out := run(t, sess, `INSERT INTO "it_items" VALUE {'pk': 'a', 'sk': 1, 'name': 'first', 'tags': <<'x', 'y'>>};
INSERT INTO "it_items" VALUE {'pk': 'a', 'sk': 2, 'name': 'second'}

PutItem
{"TableName": "it_items", "Item": {"pk": {"S": "b"}, "sk": {"N": "1"}, "price": {"N": "12.50"}, "doc": {"M": {"k": {"L": [{"N": "1"}, {"S": "two"}]}}}}}

BatchWriteItem
{"RequestItems": {"it_items": [
  {"PutRequest": {"Item": {"pk": {"S": "c"}, "sk": {"N": "1"}}}},
  {"PutRequest": {"Item": {"pk": {"S": "c"}, "sk": {"N": "2"}}}}]}}`, 100)
	if len(out.stmts) != 4 || !slices.Equal(out.affected, []int64{1, 1, 1, 2}) {
		t.Fatalf("writes: %d statements, affected %v", len(out.stmts), out.affected)
	}

	out = run(t, sess, `SELECT * FROM "it_items" WHERE pk = 'a'`, 100)
	if len(out.rows) != 1 || len(out.rows[0]) != 2 || !slices.Equal(colNames(out.cols[0])[:2], []string{"pk", "sk"}) {
		t.Fatalf("select: %v %v", colNames(out.cols[0]), out.rows)
	}
	if tags := out.rows[0][0][column(t, out.cols[0], "tags")]; tags == nil {
		t.Fatalf("tags missing: %v", out.rows[0][0])
	}

	out = run(t, sess, "Query\n"+`{"TableName": "it_items", "KeyConditionExpression": "pk = :p", "ExpressionAttributeValues": {":p": {"S": "c"}}, "ScanIndexForward": false}`, 100)
	if len(out.rows[0]) != 2 || out.rows[0][0][1] != "2" {
		t.Fatalf("query: %v", out.rows)
	}
	out = run(t, sess, "Scan\n"+`{"TableName": "it_items", "Limit": 2}`, 100)
	if len(out.rows[0]) != 2 || len(out.notices) != 1 || !strings.Contains(out.notices[0], "ExclusiveStartKey") {
		t.Fatalf("scan with limit: %v %v", out.rows, out.notices)
	}
	out = run(t, sess, "Scan {\"TableName\": \"it_items\", \"Select\": \"COUNT\"}", 100)
	if out.rows[0][0][0] != int64(5) && out.rows[0][0][0] != driver.EncodeInt(5) {
		t.Fatalf("count: %v", out.rows)
	}
	out = run(t, sess, "GetItem\n"+`{"TableName": "it_items", "Key": {"pk": {"S": "b"}, "sk": {"N": "1"}}}`, 100)
	if len(out.rows[0]) != 1 || out.rows[0][0][column(t, out.cols[0], "price")] != "12.5" {
		t.Fatalf("get: %v", out.rows)
	}
	out = run(t, sess, "DescribeTable {\"TableName\": \"it_items\"}\n\nListTables", 100)
	if len(out.rows) != 2 || len(out.rows[1]) < 4 {
		t.Fatalf("describe/list: %v", out.rows)
	}
	// Errors carry DynamoDB's message.
	bad := &sink{}
	sess.Execute(ctx, "GetItem\n{\"TableName\": \"it_nope\", \"Key\": {\"pk\": {\"S\": \"x\"}}}", driver.ExecOptions{}, bad)
	if bad.errs[0] == nil || !strings.Contains(bad.errs[0].Error(), "not") {
		t.Fatalf("missing table: %v", bad.errs)
	}

	// A whole read with no row limit streams every page.
	if _, err := c.db.ListTables(ctx, nil); err != nil {
		t.Fatal(err)
	}
	all := &sink{}
	if err := sess.Execute(ctx, "Scan {\"TableName\": \"Orders\", \"ProjectionExpression\": \"customer_id, order_id, #t\", \"ExpressionAttributeNames\": {\"#t\": \"total\"}}",
		driver.ExecOptions{}, all); err != nil || all.errs[0] != nil {
		t.Fatal(err, all.errs)
	}
	if len(all.rows[0]) != 300 || !slices.Equal(colNames(all.cols[0]), []string{"customer_id", "order_id", "total"}) {
		t.Fatalf("whole scan: %d rows, %v", len(all.rows[0]), colNames(all.cols[0]))
	}
}

func TestIntegrationEdits(t *testing.T) {
	c := open(t)
	ctx := context.Background()
	sess, _ := c.NewSession(ctx, driver.Scope{})
	fresh(t, c, sess)
	ref := driver.ObjectRef{Name: "it_items"}
	res, err := c.ApplyEdits(ctx, ref, []driver.RowEdit{
		{Op: "insert", Values: map[string]any{"pk": "e", "sk": "1", "note": "hello", "n": 3.25}},
		{Op: "insert", Values: map[string]any{"pk": "e", "sk": "2", "obj": `{"a": [1, "b"]}`}},
	})
	if err != nil || res.Applied != 2 {
		t.Fatalf("insert: %v %+v", err, res)
	}
	// An insert never overwrites; the whole save is refused.
	if _, err := c.ApplyEdits(ctx, ref, []driver.RowEdit{{Op: "insert", Values: map[string]any{"pk": "e", "sk": "9"}},
		{Op: "insert", Values: map[string]any{"pk": "e", "sk": "1"}}}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate insert: %v", err)
	}
	if _, err := c.ApplyEdits(ctx, ref, []driver.RowEdit{{Op: "update", Key: map[string]any{"pk": "e", "sk": "1"}, Values: map[string]any{"note": nil, "n": "4"}},
		{Op: "delete", Key: map[string]any{"pk": "e", "sk": "2"}}}); err != nil {
		t.Fatal(err)
	}
	// An update or delete of an item that is gone fails.
	if _, err := c.ApplyEdits(ctx, ref, []driver.RowEdit{{Op: "update", Key: map[string]any{"pk": "e", "sk": "2"}, Values: map[string]any{"n": "1"}}}); err == nil {
		t.Fatal("updating a deleted item should fail")
	}
	if _, err := c.ApplyEdits(ctx, ref, []driver.RowEdit{{Op: "update", Key: map[string]any{"pk": "e", "sk": "1"}, Values: map[string]any{"sk": "5"}}}); err == nil {
		t.Fatal("changing a key should be refused")
	}
	out := run(t, sess, `SELECT * FROM "it_items"`, 100)
	if len(out.rows[0]) != 1 {
		t.Fatalf("items: %v", out.rows)
	}
	cols := colNames(out.cols[0])
	if slices.Contains(cols, "note") || out.rows[0][0][column(t, out.cols[0], "n")] != "4" {
		t.Fatalf("after update: %v %v", cols, out.rows[0])
	}
	if typ := out.cols[0][column(t, out.cols[0], "n")].Type; typ != "N" {
		t.Fatalf("n stored as %s", typ)
	}
}

func TestIntegrationDDLAndImport(t *testing.T) {
	c := open(t)
	ctx := context.Background()
	sess, _ := c.NewSession(ctx, driver.Scope{})
	drop := func() { sess.Execute(ctx, "DeleteTable {\"TableName\": \"it_made\"}", driver.ExecOptions{}, &sink{}) }
	drop()
	t.Cleanup(drop)
	col := func(name, typ string) driver.ColumnDef {
		return driver.ColumnDef{Column: driver.Column{Name: name, Type: typ, Nullable: true}}
	}
	def := driver.TableDef{Ref: driver.ObjectRef{Name: "it_made"}, PrimaryKey: []string{"id"},
		Columns: []driver.ColumnDef{col("id", "S"), col("amount", "N"), col("meta", "M"), col("labels", "SS"), col("cat", "S")},
		Indexes: []driver.Index{{Name: "ByCat", Type: "global", Columns: []string{"cat", "amount"}, Definition: "projects KEYS_ONLY"}},
		Options: map[string]string{"billing": "PROVISIONED", "read_capacity": "3", "write_capacity": "2", "ttl_attribute": "expires"}}
	stmts, err := c.CreateTableSQL(def)
	if err != nil {
		t.Fatal(err)
	}
	run(t, sess, strings.Join(stmts, "\n\n"), 0)
	tbl, err := c.Describe(ctx, def.Ref)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, col := range tbl.Columns {
		types[col.Name] = col.Type
	}
	// The types outside the keys come back from the table's tags, which
	// DynamoDB Local doesn't keep: there only the key types are known.
	local := strings.Contains(tbl.Options["arn"], ":ddblocal:")
	for name, want := range map[string]string{"id": "S", "amount": "N", "meta": "M", "labels": "SS", "cat": "S"} {
		if types[name] != want && !(local && types[name] == "") {
			t.Errorf("%s is %q, want %s (%v)", name, types[name], want, types)
		}
	}
	if tbl.Options["read_capacity"] != "3" || tbl.Options["ttl_attribute"] != "expires" || len(tbl.Indexes) != 2 || tbl.Indexes[1].Definition != "projects KEYS_ONLY" {
		t.Errorf("created: %v %+v", tbl.Options, tbl.Indexes)
	}

	// Imports take the recorded types, or those they are given: "7" is a
	// number, JSON text a map.
	given := *tbl
	given.Columns = nil
	for _, cd := range def.Columns {
		given.Columns = append(given.Columns, cd.Column)
	}
	imp, err := c.BeginImport(ctx, &given, []string{"id", "amount", "meta", "labels", "cat"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]any
	for i := range 60 {
		rows = append(rows, []any{"r" + strings.Repeat("0", 3-len(itoa(i))) + itoa(i), itoa(i), `{"k": ` + itoa(i) + `}`, []any{"x"}, []string{"a", "b"}[i%2]})
	}
	if err := imp.Insert(ctx, rows); err != nil {
		t.Fatal(err)
	}
	imp.Commit()
	q, args, err := c.BrowseQuery(ctx, tbl, driver.BrowseRequest{Ref: def.Ref, Filters: []driver.Filter{{Column: "cat", Op: "=", Value: "a"}}})
	if err != nil {
		t.Fatal(err)
	}
	all := &sink{}
	if err := sess.Execute(ctx, q, driver.ExecOptions{Params: args}, all); err != nil || all.errs[0] != nil {
		t.Fatal(err, all.errs, q)
	}
	if len(all.rows[0]) != 30 {
		t.Fatalf("export of cat a: %d rows\n%s", len(all.rows[0]), q)
	}
	ai, mi := column(t, all.cols[0], "amount"), column(t, all.cols[0], "meta")
	if _, ok := all.rows[0][0][mi].(driver.Doc); !ok || all.cols[0][ai].Type != "N" {
		t.Fatalf("typed values: %T %v", all.rows[0][0][mi], all.cols[0])
	}

	// Change capacity, the TTL attribute, an index and the recorded types.
	to := def
	to.Columns = []driver.ColumnDef{}
	for _, cd := range def.Columns {
		if cd.Name != "labels" {
			cd.OriginalName = cd.Name
			to.Columns = append(to.Columns, cd)
		}
	}
	to.Columns = append(to.Columns, col("flag", "BOOL"))
	to.Indexes = []driver.Index{{Name: "ByCat2", Type: "global", Columns: []string{"cat"}}}
	to.Options = map[string]string{"billing": "PAY_PER_REQUEST", "ttl_attribute": "gone_at"}
	stmts, err = c.AlterTableSQL(tbl, to)
	if err != nil {
		t.Fatal(err)
	}
	run(t, sess, strings.Join(stmts, "\n\n"), 0)
	tbl, _ = c.Describe(ctx, def.Ref)
	types = map[string]string{}
	for _, col := range tbl.Columns {
		types[col.Name] = col.Type
	}
	if tbl.Options["billing"] != "PAY_PER_REQUEST" || tbl.Options["ttl_attribute"] != "gone_at" || types["flag"] != "BOOL" && !local ||
		len(tbl.Indexes) != 2 || tbl.Indexes[1].Name != "ByCat2" {
		t.Fatalf("altered: %v %v %+v\n%s", tbl.Options, types, tbl.Indexes, strings.Join(stmts, "\n\n"))
	}
	if _, err := c.AlterTableSQL(tbl, driver.TableDef{Ref: def.Ref, PrimaryKey: []string{"cat"}}); err == nil {
		t.Fatal("changing the key should be refused")
	}

	// Replacing empties the table first.
	imp, err = c.BeginImport(ctx, tbl, []string{"id", "amount"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := imp.Insert(ctx, [][]any{{"only", "1"}}); err != nil {
		t.Fatal(err)
	}
	imp.Commit()
	if n, err := c.Count(ctx, driver.BrowseRequest{Ref: def.Ref, Filters: []driver.Filter{{Column: "id", Op: "notnull"}}}); err != nil || n.Rows != 1 {
		t.Fatalf("after replace: %+v %v", n, err)
	}
	drops, _ := c.DropObjectSQL(def.Ref, false)
	run(t, sess, drops[0], 0)
}

func itoa(i int) string { return strconv.Itoa(i) }
