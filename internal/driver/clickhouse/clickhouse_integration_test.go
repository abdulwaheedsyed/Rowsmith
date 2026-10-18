package clickhouse

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

// Integration tests run against a real server when ROWSMITH_TEST_CLICKHOUSE_HOST
// is set (the dev stack's clickhouse, seeded from dev/seed/clickhouse.sql).
//
//	ROWSMITH_TEST_CLICKHOUSE_HOST, _USER (default), _PASSWORD

func params(t *testing.T, protocol string, readOnly bool) driver.OpenParams {
	t.Helper()
	host := os.Getenv("ROWSMITH_TEST_CLICKHOUSE_HOST")
	if host == "" {
		t.Skip("ROWSMITH_TEST_CLICKHOUSE_HOST not set")
	}
	user := os.Getenv("ROWSMITH_TEST_CLICKHOUSE_USER")
	if user == "" {
		user = "default"
	}
	port := "9000"
	if protocol == "http" {
		port = "8123"
	}
	return driver.OpenParams{Params: map[string]any{"host": host, "port": port, "protocol": protocol, "user": user, "database": "analytics"},
		Secrets: map[string]string{"password": os.Getenv("ROWSMITH_TEST_CLICKHOUSE_PASSWORD")}, ReadOnly: readOnly, AppName: "rowsmith-test"}
}

func openConn(t *testing.T, protocol string, readOnly bool) *conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dc, err := chDriver{}.Open(ctx, params(t, protocol, readOnly))
	if err != nil {
		t.Fatalf("open %s: %v", protocol, err)
	}
	t.Cleanup(func() { dc.Close() })
	return dc.(*conn)
}

// sink records a console run.
type sink struct {
	cols  [][]driver.ResultColumn
	rows  [][][]any
	sums  []driver.ResultSummary
	errs  []error
	stmts []driver.StatementInfo
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
func (s *sink) EndResult(sum driver.ResultSummary) error { s.sums = append(s.sums, sum); return nil }
func (s *sink) Notice(string, string) error              { return nil }
func (s *sink) EndStatement(err error) error             { s.errs = append(s.errs, err); return nil }

func run(t *testing.T, sess driver.Session, script string) *sink {
	t.Helper()
	out := &sink{}
	if err := sess.Execute(context.Background(), script, driver.ExecOptions{MaxRows: 1000}, out); err != nil {
		t.Fatalf("execute %q: %v", script, err)
	}
	return out
}

func cellJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestIntegrationCatalogAndValues(t *testing.T) {
	ctx := context.Background()
	for _, proto := range []string{"native", "http"} {
		t.Run(proto, func(t *testing.T) {
			c := openConn(t, proto, false)
			info, err := c.Server(ctx)
			if err != nil || info.Product != "ClickHouse" || info.Version == "" {
				t.Fatalf("server: %+v %v", info, err)
			}
			dbs, err := c.Databases(ctx)
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(dbs, func(d driver.Database) bool { return d.Name == "analytics" })
			if i < 0 || dbs[i].Size == nil || *dbs[i].Size == 0 || dbs[i].Comment != "Product analytics" {
				t.Fatalf("databases: %+v", dbs)
			}
			objs, err := c.Objects(ctx, driver.Scope{Database: "analytics"})
			if err != nil {
				t.Fatal(err)
			}
			kind := map[string]driver.Object{}
			for _, o := range objs {
				kind[o.Name] = o
			}
			if kind["events"].Kind != "table" || kind["events"].Rows == nil || *kind["events"].Rows != 200000 || kind["events"].Size == nil ||
				kind["top_pages"].Kind != "view" || kind["daily_revenue_mv"].Kind != "materialized_view" ||
				kind["daily_revenue_mv"].Extra != "to analytics.daily_revenue" || kind["country_names"].Kind != "dictionary" ||
				kind["users"].Extra != "ReplacingMergeTree" {
				t.Fatalf("objects: %+v", objs)
			}

			tb, err := c.Describe(ctx, driver.ObjectRef{Database: "analytics", Name: "events"})
			if err != nil {
				t.Fatal(err)
			}
			col := func(name string) driver.Column {
				for _, c := range tb.Columns {
					if c.Name == name {
						return c
					}
				}
				t.Fatalf("no column %s", name)
				return driver.Column{}
			}
			if col("event_date").Generated != "toDate(event_time)" || !col("event_date").GeneratedStored ||
				col("event_type").Kind != driver.KindString || !col("event_type").PrimaryKey || col("duration_ms").Codec != "CODEC(T64, ZSTD(1))" ||
				col("revenue").Kind != driver.KindDecimal || col("device").Kind != driver.KindEnum || !slices.Equal(col("device").Enum, []string{"desktop", "mobile", "tablet"}) ||
				col("properties").Kind != driver.KindObject || col("tags").Kind != driver.KindArray {
				t.Fatalf("columns: %+v", tb.Columns)
			}
			if tb.Options["engine"] != "MergeTree" || tb.Options["partition_by"] != "toYYYYMM(event_time)" || tb.Options["order_by"] != "event_type, event_time" ||
				!strings.HasPrefix(tb.Options["ttl"], "toDateTime(event_time) + toIntervalYear(5)") || !slices.Equal(tb.PrimaryKey, []string{"event_type", "event_time"}) ||
				!strings.Contains(tb.DDL, "CREATE TABLE analytics.events") || !tb.Editable || tb.RowKeyKind != "all" {
				t.Fatalf("table: options %v pk %v ddl %q", tb.Options, tb.PrimaryKey, tb.DDL)
			}
			if !slices.ContainsFunc(tb.Indexes, func(ix driver.Index) bool { return ix.Name == "idx_page" && ix.Type == "bloom_filter" }) {
				t.Fatalf("indexes: %+v", tb.Indexes)
			}

			// Every type renders as a cell the grid understands.
			res, err := c.Browse(ctx, driver.BrowseRequest{Ref: driver.ObjectRef{Database: "analytics", Name: "zoo"}, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			row := map[string]string{}
			for i, rc := range res.Columns {
				row[rc.Name] = cellJSON(res.Rows[0][i])
			}
			want := map[string]string{
				"u64": `"18446744073709551615"`, "i128": `"-170141183460469231731687303715884105728"`, "f32": `0.1`, "dec": `"1234567890.0123456789"`,
				"s": `"hello\nworld"`, "lc": `"low"`, "n": `null`, "b": `true`, "d": `"2025-02-28"`, "d32": `"1925-01-01"`,
				"dt": `"2025-06-01 12:00:00"`, "dt64": `"2025-06-01 12:00:00.123456"`, "t": `"12:34:56"`, "u": `"7c9e6679-7425-40de-944b-e07fc1f90ae7"`,
				"ip4": `"192.168.1.10"`, "ip6": `"2001:db8::1"`, "e": `"b"`, "arr": `[1,null,3]`, "tup": `{"name":"ada","n":36}`, "m": `{"a":1,"b":2}`,
				"nested.k": `["x","y"]`,
			}
			for k, v := range want {
				// Older servers' seed leaves out the Time and JSON columns.
				if got, ok := row[k]; ok && got != v {
					t.Errorf("%s = %s, want %s", k, got, v)
				}
			}
			if _, ok := row["j"]; ok && !strings.Contains(row["j"], `seats`) || !strings.Contains(row["pt"], `"$geo"`) || !strings.Contains(row["poly"], `Polygon`) {
				t.Errorf("json/geo cells: %s %s %s", row["j"], row["pt"], row["poly"])
			}
			res2, _ := c.Browse(ctx, driver.BrowseRequest{Ref: driver.ObjectRef{Database: "analytics", Name: "zoo"}, Limit: 10, Sort: []driver.Sort{{Column: "id"}}})
			fs := ""
			for i, rc := range res2.Columns {
				if rc.Name == "fs" {
					fs = cellJSON(res2.Rows[1][i])
				}
			}
			if !strings.Contains(fs, `"$bin"`) {
				t.Errorf("a FixedString of NUL bytes should be binary, got %s", fs)
			}

			// Filters compare against each column's type.
			for _, f := range []driver.Filter{
				{Column: "revenue", Op: ">", Value: "300"}, {Column: "device", Op: "=", Value: "mobile"},
				{Column: "event_time", Op: "between", Values: []any{"2025-01-02 00:00:00", "2025-01-03 00:00:00"}},
				{Column: "session_id", Op: "notnull"}, {Column: "page", Op: "regexp", Value: "^/(docs|blog)$"},
				{Column: "is_bot", Op: "=", Value: "true"}, {Column: "ip", Op: "startswith", Value: "10.1."},
			} {
				n, err := c.Count(ctx, driver.BrowseRequest{Ref: driver.ObjectRef{Database: "analytics", Name: "events"}, Filters: []driver.Filter{f}})
				if err != nil || n.Rows == 0 {
					t.Errorf("filter %+v: %v rows, %v", f, n.Rows, err)
				}
			}
			// Maps keep their keys' order (a Go map wouldn't).
			ev, err := c.Browse(ctx, driver.BrowseRequest{Ref: driver.ObjectRef{Database: "analytics", Name: "events"}, Limit: 1, Columns: []string{"properties"}})
			if err != nil || !strings.HasPrefix(cellJSON(ev.Rows[0][0]), `{"utm_source":`) || ev.Columns[0].Type != "Map(String, String)" || ev.Columns[0].Kind != driver.KindObject {
				t.Fatalf("map order: %v %+v %v", ev, ev.Columns, err)
			}
			n, err := c.Count(ctx, driver.BrowseRequest{Ref: driver.ObjectRef{Database: "analytics", Name: "users"}, Search: "grace"})
			if err != nil || n.Rows == 0 {
				t.Errorf("search: %v %v", n, err)
			}
		})
	}
}

func TestIntegrationConsole(t *testing.T) {
	for _, proto := range []string{"native", "http"} {
		t.Run(proto, func(t *testing.T) {
			c := openConn(t, proto, false)
			sess, err := c.NewSession(context.Background(), driver.Scope{Database: "analytics"})
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			// Settings and the current database carry from one run to the next.
			run(t, sess, "SET max_threads = 3")
			out := run(t, sess, "SELECT getSetting('max_threads') AS t, currentDatabase() AS db; SELECT count() FROM events WHERE event_type = 'purchase'")
			if out.errs[0] != nil || cellJSON(out.rows[0][0]) != `[3,"analytics"]` {
				t.Fatalf("session state: %v %v", out.errs, out.rows)
			}
			// Only the native protocol reports progress.
			if proto == "native" && (out.sums[1].RowsRead == nil || *out.sums[1].RowsRead == 0 || out.sums[1].BytesProcessed == nil) {
				t.Errorf("rows read not reported: %+v", out.sums[1])
			}
			out = run(t, sess, "USE system; SELECT currentDatabase()")
			if cellJSON(out.rows[0][0]) != `["system"]` {
				t.Fatalf("USE: %v", out.rows)
			}
			// Syntax errors point at the statement's line and position.
			out = run(t, sess, "SELECT 1;\n\nSELECT * FORM analytics.events")
			var qe *driver.QueryError
			if !errors.As(out.errs[1], &qe) || qe.Line != 3 || qe.Position == 0 || qe.Code != "SYNTAX_ERROR" && qe.Code != "62" {
				t.Fatalf("syntax error: %#v", out.errs[1])
			}
			// Commands run; a big result stops at the row cap without hanging.
			out = run(t, sess, "CREATE TEMPORARY TABLE tmp (x UInt8); INSERT INTO tmp VALUES (1), (2); SELECT sum(x) FROM tmp; SELECT number FROM numbers(1000000000)")
			for i, err := range out.errs {
				if err != nil {
					t.Fatalf("statement %d: %v", i, err)
				}
			}
			if cellJSON(out.rows[len(out.rows)-2]) != `[[3]]` || !out.sums[len(out.sums)-1].Truncated {
				t.Fatalf("temporary table / truncation: %v %+v", out.rows, out.sums)
			}
			// The cut-off query was cancelled, not read to the end, and the
			// console's connection (and its temporary table) is still there.
			start := time.Now()
			out = run(t, sess, "SELECT number FROM numbers(100000000000); SELECT sum(x) FROM tmp")
			if out.errs[1] != nil || cellJSON(out.rows[1]) != `[[3]]` || time.Since(start) > 10*time.Second {
				t.Fatalf("after a cut-off query (%v): %v %v", time.Since(start), out.errs, out.rows)
			}
		})
	}
}

func TestIntegrationReadOnly(t *testing.T) {
	c := openConn(t, "native", true)
	sess, err := c.NewSession(context.Background(), driver.Scope{Database: "analytics"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out := &sink{}
	// Past Rowsmith's own check (ReadOnly unset), the server refuses too.
	_ = sess.Execute(context.Background(), "INSERT INTO notes (id, body) VALUES (99, 'nope'); SET max_threads = 2", driver.ExecOptions{}, out)
	if out.errs[0] == nil || out.errs[1] != nil {
		t.Fatalf("read-only session: %v", out.errs)
	}
	if _, err := c.ApplyEdits(context.Background(), driver.ObjectRef{Database: "analytics", Name: "notes"}, nil); !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("edits on a read-only connection: %v", err)
	}
}

// scratch makes a database the test owns.
func scratch(t *testing.T, c *conn) string {
	t.Helper()
	ctx := context.Background()
	db := "rs_test"
	_, _ = c.db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+db)
	if _, err := c.db.ExecContext(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = c.db.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+db) })
	return db
}

// applier runs generated statements.
func applier(t *testing.T, c *conn) func([]string, error) {
	return func(stmts []string, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range stmts {
			if _, err := c.db.ExecContext(context.Background(), s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
}

func TestIntegrationEditsAndDDL(t *testing.T) {
	ctx := context.Background()
	c := openConn(t, "native", false)
	db := scratch(t, c)
	apply := applier(t, c)

	// A table from the structure editor: engine, keys, partitions, TTL, an index.
	def := driver.TableDef{Ref: driver.ObjectRef{Database: db, Name: "items"}, PrimaryKey: []string{"id"}, Comment: "Things",
		Columns: []driver.ColumnDef{
			{Column: driver.Column{Name: "id", Type: "UInt64"}},
			{Column: driver.Column{Name: "name", Type: "String", Comment: "What it's called"}},
			{Column: driver.Column{Name: "price", Type: "Decimal(10, 2)", Default: ptr("0")}},
			{Column: driver.Column{Name: "tags", Type: "Array(String)"}},
			{Column: driver.Column{Name: "attrs", Type: "Map(String, String)"}},
			{Column: driver.Column{Name: "note", Type: "String", Nullable: true, Codec: "CODEC(ZSTD(1))"}},
			{Column: driver.Column{Name: "made", Type: "DateTime"}},
			{Column: driver.Column{Name: "upper_name", Type: "String", Generated: "upper(name)", GeneratedStored: true}},
		},
		Indexes: []driver.Index{{Name: "idx_name", Columns: []string{"name"}, Type: "bloom_filter", Definition: "GRANULARITY 2"}},
		Options: map[string]string{"engine": "MergeTree", "partition_by": "toYYYYMM(made)", "ttl": "made + INTERVAL 10 YEAR"}}
	apply(c.CreateTableSQL(def))
	ref := driver.ObjectRef{Database: db, Name: "items"}
	tb, err := c.Describe(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if tb.Options["order_by"] != "id" || tb.Options["partition_by"] != "toYYYYMM(made)" || tb.Comment != "Things" || !tb.Columns[5].Nullable ||
		tb.Columns[5].Codec != "CODEC(ZSTD(1))" || tb.Columns[7].Generated != "upper(name)" || tb.Columns[1].Comment != "What it's called" {
		t.Fatalf("created: %+v / %+v", tb.Options, tb.Columns)
	}

	// Row edits: insert, update and delete, each matched on the row's values.
	res, err := c.ApplyEdits(ctx, ref, []driver.RowEdit{
		{Op: "insert", Values: map[string]any{"id": float64(1), "name": "anvil", "price": "12.50", "tags": []any{"iron", "heavy"},
			"attrs": map[string]any{"colour": "black"}, "made": "2025-03-01 10:00:00", "note": nil}},
		{Op: "insert", Values: map[string]any{"id": float64(2), "name": "hammer", "price": 3, "tags": "[\"steel\"]", "made": "2025-04-01 00:00:00"}},
	})
	if err != nil || res.Applied != 2 {
		t.Fatalf("insert: %+v %v", res, err)
	}
	browse := func() map[string]map[string]string {
		r, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]string{}
		for _, row := range r.Rows {
			m := map[string]string{}
			for i, rc := range r.Columns {
				m[rc.Name] = cellJSON(row[i])
			}
			out[m["id"]] = m
		}
		return out
	}
	rows := browse()
	if rows["1"]["tags"] != `["iron","heavy"]` || rows["1"]["attrs"] != `{"colour":"black"}` || rows["1"]["price"] != `"12.5"` || rows["1"]["upper_name"] != `"ANVIL"` {
		t.Fatalf("inserted: %v", rows)
	}
	key := func(id string) map[string]any {
		var m map[string]any
		b, _ := json.Marshal(func() map[string]any {
			out := map[string]any{}
			for k, v := range rows[id] {
				var x any
				_ = json.Unmarshal([]byte(v), &x)
				out[k] = x
			}
			return out
		}())
		_ = json.Unmarshal(b, &m)
		return m
	}
	if _, err := c.ApplyEdits(ctx, ref, []driver.RowEdit{{Op: "update", Key: key("1"), Values: map[string]any{"price": "13.75", "note": "dented"}}}); err != nil {
		t.Fatal(err)
	}
	if r := browse(); r["1"]["price"] != `"13.75"` || r["1"]["note"] != `"dented"` {
		t.Fatalf("updated: %v", r["1"])
	}
	// A row that changed since it was read isn't touched, and neither is anything else in the batch.
	stale := key("2")
	stale["price"] = "999"
	_, err = c.ApplyEdits(ctx, ref, []driver.RowEdit{{Op: "delete", Key: key("1")}, {Op: "delete", Key: stale}})
	if err == nil || !strings.Contains(err.Error(), "nothing was saved") || len(browse()) != 2 {
		t.Fatalf("stale key: %v, %d rows", err, len(browse()))
	}
	rows = browse()
	if _, err := c.ApplyEdits(ctx, ref, []driver.RowEdit{{Op: "delete", Key: key("2")}}); err != nil || len(browse()) != 1 {
		t.Fatalf("delete: %v", err)
	}

	// Structure changes: add, rename, retype, comment, an index, TTL and the table comment.
	tb, _ = c.Describe(ctx, ref)
	to := driver.TableDef{Ref: ref, PrimaryKey: tb.PrimaryKey, Comment: "Things for sale", Options: tb.Options, Indexes: tb.Indexes}
	for _, col := range tb.Columns {
		cd := driver.ColumnDef{Column: col, OriginalName: col.Name}
		switch col.Name {
		case "name":
			cd.Comment = "Shown in the shop"
		case "note":
			cd.Name = "remark"
		case "price":
			cd.Type = "Decimal(12, 2)"
		}
		to.Columns = append(to.Columns, cd)
	}
	to.Columns = append(to.Columns, driver.ColumnDef{Column: driver.Column{Name: "stock", Type: "UInt32", Default: ptr("1")}})
	to.Options = map[string]string{}
	for k, v := range tb.Options {
		to.Options[k] = v
	}
	to.Options["ttl"] = "made + toIntervalYear(20)"
	to.Indexes = append(to.Indexes, driver.Index{Name: "idx_price", Columns: []string{"price"}, Type: "minmax"})
	stmts, err := c.AlterTableSQL(tb, to)
	if err != nil {
		t.Fatal(err)
	}
	apply(stmts, nil)
	tb, _ = c.Describe(ctx, ref)
	names := []string{}
	for _, col := range tb.Columns {
		names = append(names, col.Name+" "+col.Type)
	}
	if !slices.Contains(names, "remark String") || !slices.Contains(names, "price Decimal(12, 2)") || !slices.Contains(names, "stock UInt32") ||
		tb.Columns[1].Comment != "Shown in the shop" || tb.Comment != "Things for sale" || !strings.Contains(tb.Options["ttl"], "toIntervalYear(20)") ||
		!slices.ContainsFunc(tb.Indexes, func(ix driver.Index) bool { return ix.Name == "idx_price" }) {
		t.Fatalf("altered (%q): %v / %+v / %+v", stmts, names, tb.Options, tb.Indexes)
	}
	if again, _ := c.AlterTableSQL(tb, defOf(tb)); len(again) != 0 {
		t.Fatalf("an unchanged table still alters: %q", again)
	}
	if _, err := c.AlterTableSQL(tb, func() driver.TableDef { d := defOf(tb); d.Options["partition_by"] = "made"; return d }()); err == nil {
		t.Fatal("PARTITION BY changed on an existing table")
	}

	// Import: through a staging table, so a bad row leaves the target as it was.
	imp, err := c.BeginImport(ctx, tb, []string{"id", "name", "price", "made"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := imp.Insert(ctx, [][]any{{"10", "rake", "4.20", "2025-05-01 00:00:00"}, {"11", "shovel", "5", "2025-05-02 00:00:00"}}); err != nil {
		t.Fatal(err)
	}
	if err := imp.Insert(ctx, [][]any{{"12", "bad", "lots", "2025-05-03 00:00:00"}}); err == nil {
		t.Fatal("a non-number price was imported")
	}
	_ = imp.Rollback()
	// A value only the server rejects fails when the rows are sent, and leaves the table as it was.
	imp, _ = c.BeginImport(ctx, tb, []string{"id", "name", "price", "made"}, true)
	_ = imp.Insert(ctx, [][]any{{"13", "late", "1", "not a date"}})
	if err := imp.Commit(); err == nil {
		t.Fatal("an import with a bad date committed")
	}
	if n, _ := c.Count(ctx, driver.BrowseRequest{Ref: ref}); n.Rows != 1 {
		t.Fatalf("after a failed import the table has %d rows", n.Rows)
	}
	imp, _ = c.BeginImport(ctx, tb, []string{"id", "name", "price", "made"}, true)
	_ = imp.Insert(ctx, [][]any{{"10", "rake", "4.20", "2025-05-01 00:00:00"}, {"11", "shovel", "5", "2025-05-02 00:00:00"}})
	if err := imp.Commit(); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.Count(ctx, driver.BrowseRequest{Ref: ref}); n.Rows != 2 {
		t.Fatalf("after replacing with an import the table has %d rows", n.Rows)
	}
	objs, _ := c.Objects(ctx, driver.Scope{Database: db})
	if len(objs) != 1 {
		t.Fatalf("staging tables were left behind: %+v", objs)
	}

	// The plan shows index use; analyze runs the query and reports what it read.
	plan, err := c.Explain(ctx, driver.Scope{Database: "analytics"}, "SELECT count() FROM events WHERE event_type = 'purchase'", true)
	if err != nil || plan.Root == nil || plan.Totals["rows read"] == nil || plan.Totals["time"] == nil || !strings.Contains(plan.Raw, "PrimaryKey") {
		t.Fatalf("explain: %+v %v", plan, err)
	}
	if _, err := c.Explain(ctx, driver.Scope{Database: db}, "INSERT INTO items (id) VALUES (5)", true); err == nil {
		t.Fatal("an INSERT was analyzed")
	}

	// Drop, truncate and rename statements run.
	apply(c.RenameObjectSQL(ref, "goods"))
	apply(c.TruncateSQL(driver.ObjectRef{Database: db, Name: "goods"}))
	apply(c.DropObjectSQL(driver.ObjectRef{Database: db, Name: "goods", Kind: "table"}, false))
}

func ptr(s string) *string { return &s }

// defOf is the structure editor's starting point for a table.
func defOf(t *driver.Table) driver.TableDef {
	d := driver.TableDef{Ref: t.Ref, PrimaryKey: t.PrimaryKey, Comment: t.Comment, Indexes: t.Indexes, Options: map[string]string{}}
	for k, v := range t.Options {
		d.Options[k] = v
	}
	for _, col := range t.Columns {
		d.Columns = append(d.Columns, driver.ColumnDef{Column: col, OriginalName: col.Name})
	}
	return d
}

func TestIntegrationAccess(t *testing.T) {
	ctx := context.Background()
	c := openConn(t, "native", false)
	apply := applier(t, c)
	cleanup := func() {
		for _, s := range []string{"DROP USER IF EXISTS rs_acc_user", "DROP ROLE IF EXISTS rs_acc_readers"} {
			_, _ = c.db.ExecContext(ctx, s)
		}
	}
	cleanup()
	defer cleanup()
	if _, err := c.AccountSQL(driver.AccountOp{Op: "create", Kind: "user", Name: "rs_acc_user"}); err == nil {
		t.Fatal("created a user without a password")
	}
	apply(c.AccountSQL(driver.AccountOp{Op: "create", Kind: "user", Name: "rs_acc_user", Password: "Wild-Otter-Lantern-42", Attrs: map[string]string{"default_database": "analytics"}}))
	apply(c.AccountSQL(driver.AccountOp{Op: "create", Kind: "role", Name: "rs_acc_readers"}))
	apply(c.AccountSQL(driver.AccountOp{Op: "add_role", Name: "rs_acc_user", Role: "rs_acc_readers"}))
	apply(c.GrantSQL([]driver.GrantChange{
		{Grant: driver.Grant{Principal: "rs_acc_readers", Level: driver.LevelDatabase, Database: "analytics", Privilege: "SELECT"}},
		{Grant: driver.Grant{Principal: "rs_acc_user", Level: driver.LevelTable, Database: "analytics", Object: "notes", Privilege: "ALTER TABLE"}},
		{Grant: driver.Grant{Principal: "rs_acc_user", Level: driver.LevelServer, Privilege: "SHOW DATABASES"}, Kind: "user"},
	}))
	// A partial revoke takes one privilege back out of a group grant.
	apply(c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "rs_acc_user", Level: driver.LevelTable, Database: "analytics", Object: "notes", Privilege: "ALTER DELETE"}, Revoke: true}}))

	ps, err := c.Principals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ps, func(p driver.Principal) bool { return p.Name == "rs_acc_user" })
	d := slices.IndexFunc(ps, func(p driver.Principal) bool { return p.Name == "default" })
	if i < 0 || d < 0 || !slices.Equal(ps[i].MemberOf, []string{"rs_acc_readers"}) || ps[i].Attributes["default_database"] != "analytics" || ps[i].System || !ps[d].System {
		t.Fatalf("principals: %+v", ps)
	}
	gs, err := c.Access(ctx, "rs_acc_user", driver.Scope{Database: "analytics"})
	if err != nil {
		t.Fatal(err)
	}
	has := func(level, object, priv, via string) bool {
		return slices.ContainsFunc(gs, func(g driver.Grant) bool {
			return g.Level == level && g.Object == object && g.Privilege == priv && g.Via == via
		})
	}
	if !has(driver.LevelDatabase, "", "SELECT", "rs_acc_readers") || !has(driver.LevelTable, "notes", "ALTER UPDATE", "") ||
		has(driver.LevelTable, "notes", "ALTER DELETE", "") || !has(driver.LevelServer, "", "SHOW DATABASES", "") {
		t.Fatalf("access: %+v", gs)
	}
	og, err := c.ObjectAccess(ctx, driver.ObjectRef{Database: "analytics", Name: "notes"})
	if err != nil {
		t.Fatal(err)
	}
	holds := func(who, priv, via string) bool {
		return slices.ContainsFunc(og, func(g driver.Grant) bool { return g.Principal == who && g.Privilege == priv && g.Via == via })
	}
	if !holds("rs_acc_readers", "SELECT", "the whole database") || !holds("rs_acc_user", "ALTER UPDATE", "") {
		t.Fatalf("object access: %+v", og)
	}
	// The new account signs in, starts in its default database and reads through its role.
	p := params(t, "native", false)
	p.Params["user"], p.Params["database"], p.Secrets["password"] = "rs_acc_user", "", "Wild-Otter-Lantern-42"
	uc, err := chDriver{}.Open(ctx, p)
	if err != nil {
		t.Fatalf("signing in as the new user: %v", err)
	}
	var n uint64
	if err := uc.(*conn).db.QueryRowContext(ctx, "SELECT count() FROM notes").Scan(&n); err != nil || n == 0 {
		t.Fatalf("reading as the new user: %v", err)
	}
	uc.Close()
	if _, err := c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "x", Level: driver.LevelServer, Privilege: "ALL; DROP DATABASE x"}}}); err == nil {
		t.Fatal("an unknown privilege was accepted")
	}
	apply(c.AccountSQL(driver.AccountOp{Op: "drop", Kind: "user", Name: "rs_acc_user"}))
	apply(c.AccountSQL(driver.AccountOp{Op: "drop", Kind: "role", Name: "rs_acc_readers"}))
}
