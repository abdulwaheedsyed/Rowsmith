package bigquery

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

// recSink records execution events for assertions.
type recSink struct {
	stmts []*recStmt
}

type recStmt struct {
	info    driver.StatementInfo
	cols    []driver.ResultColumn
	rows    [][]any
	sums    []driver.ResultSummary
	notices []string
	err     error
	ended   bool
}

func (s *recSink) cur() *recStmt { return s.stmts[len(s.stmts)-1] }

func (s *recSink) BeginStatement(i driver.StatementInfo) error {
	s.stmts = append(s.stmts, &recStmt{info: i})
	return nil
}
func (s *recSink) Columns(c []driver.ResultColumn) error { s.cur().cols = c; return nil }
func (s *recSink) Rows(r [][]any) error                  { s.cur().rows = append(s.cur().rows, r...); return nil }
func (s *recSink) EndResult(sum driver.ResultSummary) error {
	s.cur().sums = append(s.cur().sums, sum)
	return nil
}
func (s *recSink) Notice(level, text string) error {
	s.cur().notices = append(s.cur().notices, level+": "+text)
	return nil
}
func (s *recSink) EndStatement(err error) error { s.cur().err, s.cur().ended = err, true; return nil }

// exec runs a script and fails the test on statement errors unless allowed.
func exec(t *testing.T, sess driver.Session, script string, opts driver.ExecOptions) *recSink {
	t.Helper()
	sink := &recSink{}
	if err := sess.Execute(context.Background(), script, opts, sink); err != nil {
		t.Fatalf("Execute(%q): %v", script, err)
	}
	if len(sink.stmts) == 0 {
		t.Fatalf("Execute(%q): no statements reported", script)
	}
	for _, st := range sink.stmts {
		if !st.ended {
			t.Fatalf("Execute(%q): statement %d not ended", script, st.info.Index)
		}
	}
	return sink
}

func mustOK(t *testing.T, sink *recSink) {
	t.Helper()
	for _, st := range sink.stmts {
		if st.err != nil {
			t.Fatalf("statement %q failed: %v", st.info.SQL, st.err)
		}
	}
}

// TestIntegration runs against the goccy BigQuery emulator:
//
//	ROWSMITH_TEST_BQ_ENDPOINT=http://bigquery:9050 go test ./internal/driver/bigquery/ -run Integration
func TestIntegration(t *testing.T) {
	ep := os.Getenv("ROWSMITH_TEST_BQ_ENDPOINT")
	if ep == "" {
		t.Skip("set ROWSMITH_TEST_BQ_ENDPOINT to run against the BigQuery emulator")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	d, ok := driver.Get("bigquery")
	if !ok {
		t.Fatal("driver not registered")
	}
	cn, err := d.Open(ctx, driver.OpenParams{AppName: "Rowsmith", Params: map[string]any{
		"project": "rowsmith-dev", "dataset": "analytics", "endpoint": ep, "maxBilledGB": float64(1)}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cn.Close()

	srv, err := cn.Server(ctx)
	if err != nil || srv.Product != "BigQuery" || srv.Database != "rowsmith-dev" {
		t.Fatalf("Server = %+v, %v", srv, err)
	}

	sess, err := cn.NewSession(ctx, driver.Scope{Schema: "analytics"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()
	opts := driver.ExecOptions{MaxRows: 1000}

	// The emulator ignores the default dataset, so statements qualify names.
	table := "it_people_" + strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 36)
	qt := "analytics." + table
	ref := driver.ObjectRef{Schema: "analytics", Name: table}
	mustOK(t, exec(t, sess, "CREATE TABLE "+qt+` (
		id INT64 NOT NULL OPTIONS(description = "person id"),
		name STRING,
		score NUMERIC,
		big BIGNUMERIC,
		tags ARRAY<STRING>,
		address STRUCT<city STRING, zip INT64>,
		loc GEOGRAPHY,
		created TIMESTAMP,
		birthday DATE,
		payload BYTES
	)`, opts))
	// The emulator's DROP TABLE leaves the table listed; delete through the API.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := cn.(*conn).client.Dataset("analytics").Table(table).Delete(ctx); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()

	ins := exec(t, sess, "INSERT INTO "+qt+` (id, name, score, big, tags, address, loc, created, birthday, payload) VALUES
		(1, 'Ada', 12.5, BIGNUMERIC '1.123456789012345678901234567890', ['math', 'code'], STRUCT('London', 1815), ST_GEOGPOINT(-0.1276, 51.5072), TIMESTAMP '2024-01-02 03:04:05.123456+00', DATE '1815-12-10', b'\x00\x01'),
		(2, 'Bob', 3.25, NULL, [], STRUCT('Paris', 75001), NULL, NULL, NULL, NULL),
		(3, 'Cleo', 99.999999999, NULL, ['x'], NULL, NULL, NULL, NULL, NULL)`, opts)
	mustOK(t, ins)

	t.Run("Catalog", func(t *testing.T) {
		schemas, err := cn.Schemas(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if !hasSchema(schemas, "analytics") {
			t.Fatalf("analytics missing from %v", schemas)
		}
		objs, err := cn.Objects(ctx, driver.Scope{Schema: "analytics"})
		if err != nil {
			t.Fatal(err)
		}
		var found *driver.Object
		for i := range objs {
			if objs[i].Name == table {
				found = &objs[i]
			}
		}
		if found == nil || found.Kind != "table" {
			t.Fatalf("table %s not listed: %+v", table, objs)
		}
		if cat, ok := cn.(driver.Catalog); !ok {
			t.Fatal("conn does not implement driver.Catalog")
		} else if tabs, err := cat.CatalogColumns(ctx, driver.Scope{Schema: "analytics"}); err != nil || len(tabs) == 0 {
			t.Fatalf("CatalogColumns = %v, %v", tabs, err)
		}
	})

	t.Run("Describe", func(t *testing.T) {
		tb, err := cn.Describe(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if tb.Kind != "table" || len(tb.Columns) != 10 {
			t.Fatalf("Describe = %s with %d columns", tb.Kind, len(tb.Columns))
		}
		want := map[string]struct {
			typ  string
			kind driver.ValueKind
		}{
			"id": {"INT64", driver.KindInt}, "score": {"NUMERIC", driver.KindDecimal}, "tags": {"ARRAY<STRING>", driver.KindArray},
			"address": {"STRUCT<city STRING, zip INT64>", driver.KindObject}, "loc": {"GEOGRAPHY", driver.KindGeometry},
			"created": {"TIMESTAMP", driver.KindTimestamp}, "birthday": {"DATE", driver.KindDate}, "payload": {"BYTES", driver.KindBinary},
		}
		for _, c := range tb.Columns {
			if w, ok := want[c.Name]; ok && (c.Type != w.typ || c.Kind != w.kind) {
				t.Errorf("column %s = %s/%s, want %s/%s", c.Name, c.Type, c.Kind, w.typ, w.kind)
			}
		}
		// The emulator drops NOT NULL and column OPTIONS from DDL, so
		// nullability and descriptions are covered by the unit tests.
		if tb.DDL == "" || !strings.Contains(tb.DDL, table) {
			t.Errorf("DDL = %q", tb.DDL)
		}
		if def, err := cn.(driver.Definer).Definition(ctx, ref); err != nil || def == "" {
			t.Errorf("Definition = %q, %v", def, err)
		}
	})

	t.Run("BrowseFree", func(t *testing.T) {
		res, err := cn.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if res.SQL != freeReadSQL || len(res.Rows) != 2 || !res.Truncated {
			t.Fatalf("free browse: sql=%q rows=%d truncated=%v", res.SQL, len(res.Rows), res.Truncated)
		}
		all, err := cn.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 10, Columns: []string{"id", "address", "tags"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(all.Rows) != 3 || all.Truncated || len(all.Columns) != 3 {
			t.Fatalf("free browse all: rows=%d truncated=%v cols=%d", len(all.Rows), all.Truncated, len(all.Columns))
		}
		// The emulator ignores tabledata.list startIndex, so paging by
		// offset is only checked on the query path below.
		checkAda(t, cn, ref)
	})

	t.Run("BrowseFiltered", func(t *testing.T) {
		res, err := cn.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 10, Filters: []driver.Filter{{Column: "name", Op: "=", Value: "Bob"}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) != 1 || !strings.Contains(res.SQL, "@p1") {
			t.Fatalf("filtered browse: %d rows, sql %q", len(res.Rows), res.SQL)
		}
		sorted, err := cn.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 2, Offset: 1, Columns: []string{"id", "name"},
			Sort: []driver.Sort{{Column: "id", Desc: true}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(sorted.Rows) != 2 || sorted.Rows[0][0] != int64(2) || sorted.Rows[1][0] != int64(1) || sorted.Truncated {
			t.Fatalf("sorted page = %v (truncated %v)", sorted.Rows, sorted.Truncated)
		}
		search, err := cn.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 10, Search: "cle"})
		if err != nil || len(search.Rows) != 1 {
			t.Fatalf("search = %v, %v", search, err)
		}
		gt, err := cn.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 10, Filters: []driver.Filter{
			{Column: "score", Op: ">", Value: "10"}, {Column: "birthday", Op: "null"}}})
		if err != nil || len(gt.Rows) != 1 {
			t.Fatalf("score/birthday filter = %v, %v", gt, err)
		}
		if _, err := cn.Browse(ctx, driver.BrowseRequest{Ref: ref, Sort: []driver.Sort{{Column: "address"}}}); err == nil {
			t.Error("sorting by a STRUCT column should fail")
		}
	})

	t.Run("Count", func(t *testing.T) {
		n, err := cn.Count(ctx, driver.BrowseRequest{Ref: ref})
		if err != nil || n.Rows != 3 || !n.Exact {
			t.Fatalf("Count = %+v, %v", n, err)
		}
		f, err := cn.Count(ctx, driver.BrowseRequest{Ref: ref, Filters: []driver.Filter{{Column: "id", Op: ">=", Value: float64(2)}}})
		if err != nil || f.Rows != 2 || !f.Exact {
			t.Fatalf("filtered Count = %+v, %v", f, err)
		}
	})

	t.Run("Execute", func(t *testing.T) {
		// Unqualified: resolved through the session's default dataset.
		sink := exec(t, sess, "-- people\nSELECT id, name, address FROM "+table+" ORDER BY id", driver.ExecOptions{MaxRows: 2})
		mustOK(t, sink)
		st := sink.stmts[0]
		if len(st.cols) != 3 || len(st.rows) != 2 || len(st.sums) != 1 || !st.sums[0].Truncated || st.sums[0].RowCount != 2 {
			t.Fatalf("result: cols=%d rows=%d sums=%+v", len(st.cols), len(st.rows), st.sums)
		}
		if st.sums[0].BytesProcessed == nil || st.sums[0].CacheHit == nil {
			t.Errorf("summary lacks bytes/cache info: %+v", st.sums[0])
		}
		if !hasNotice(st, "will process") {
			t.Errorf("no estimate notice in %v", st.notices)
		}
		if addr, ok := st.rows[0][2].(map[string]any); !ok || addr["city"] != "London" {
			t.Errorf("struct cell = %#v", st.rows[0][2])
		}

		dry := exec(t, sess, "SELECT * FROM "+qt, driver.ExecOptions{DryRun: true})
		mustOK(t, dry)
		if d := dry.stmts[0]; len(d.rows) != 0 || len(d.sums) != 1 || d.sums[0].BytesProcessed == nil {
			t.Errorf("dry run reported %+v", d)
		}

		multi := exec(t, sess, "SELECT 1 AS a;\nSELECT name FROM "+qt+" WHERE id = 3", opts)
		mustOK(t, multi)

		last := multi.stmts[len(multi.stmts)-1]
		if len(last.rows) != 1 || last.rows[0][0] != "Cleo" {
			t.Errorf("script result = %+v", last)
		}
	})

	t.Run("Errors", func(t *testing.T) {
		bad := exec(t, sess, "SELECT 1;\nSELEC oops", opts)
		var qe *driver.QueryError
		if err := bad.stmts[0].err; !errors.As(err, &qe) || qe.Line != 2 {
			t.Fatalf("syntax error = %#v", err)
		}
		missing := exec(t, sess, "SELECT * FROM no_such_table_here", opts)
		if missing.stmts[0].err == nil {
			t.Fatal("missing table did not fail")
		}
		guard := exec(t, sess, "SELECT * FROM "+qt, driver.ExecOptions{MaxBytesBilled: 1})
		if err := guard.stmts[0].err; !errors.As(err, &qe) || qe.Code != "bytesBilledLimitExceeded" {
			t.Fatalf("cost guard = %#v", err)
		}
		ro := exec(t, sess, "DELETE FROM "+qt+" WHERE TRUE", driver.ExecOptions{ReadOnly: true})
		if err := ro.stmts[0].err; err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("read-only DELETE = %v", err)
		}
		if n, _ := cn.Count(ctx, driver.BrowseRequest{Ref: ref}); n.Rows != 3 {
			t.Fatalf("rows after blocked DELETE = %d", n.Rows)
		}
		if _, err := cn.ApplyEdits(ctx, ref, nil); !errors.Is(err, driver.ErrNotSupported) {
			t.Fatalf("ApplyEdits = %v", err)
		}
	})

	t.Run("Explain", func(t *testing.T) {
		plan, err := cn.(driver.Explainer).Explain(ctx, driver.Scope{Schema: "analytics"}, "SELECT name FROM "+qt, false)
		if err != nil || plan.Root == nil || plan.Format != "json" {
			t.Fatalf("Explain = %+v, %v", plan, err)
		}
		if _, ok := plan.Totals["Bytes processed"]; !ok {
			t.Errorf("totals = %v", plan.Totals)
		}
		an, err := cn.(driver.Explainer).Explain(ctx, driver.Scope{Schema: "analytics"}, "SELECT COUNT(*) FROM "+qt, true)
		if err != nil || an.Root == nil {
			t.Fatalf("Explain analyze = %+v, %v", an, err)
		}
		if _, err := cn.(driver.Explainer).Explain(ctx, driver.Scope{Schema: "analytics"}, "DELETE FROM "+qt+" WHERE TRUE", true); err == nil {
			t.Fatal("analyzing a DELETE should be refused")
		}
	})

	t.Run("Processes", func(t *testing.T) {
		res, err := cn.(driver.ProcessManager).Processes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Columns) == 0 || res.Columns[0].Name != "id" {
			t.Fatalf("columns = %+v", res.Columns)
		}
	})
}

func hasSchema(list []driver.Schema, name string) bool {
	for _, s := range list {
		if s.Name == name {
			return true
		}
	}
	return false
}

func hasNotice(st *recStmt, sub string) bool {
	for _, n := range st.notices {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// checkAda verifies the cell encoding of a fully populated row.
func checkAda(t *testing.T, cn driver.Conn, ref driver.ObjectRef) {
	t.Helper()
	res, err := cn.Browse(context.Background(), driver.BrowseRequest{Ref: ref, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	idx := map[string]int{}
	for i, c := range res.Columns {
		idx[c.Name] = i
	}
	var row []any
	for _, r := range res.Rows {
		if r[idx["id"]] == int64(1) {
			row = r
		}
	}
	if row == nil {
		t.Fatalf("row 1 missing from %v", res.Rows)
	}
	checks := map[string]any{"name": "Ada", "score": "12.5", "big": "1.12345678901234567890123456789", "birthday": "1815-12-10",
		"created": "2024-01-02 03:04:05.123456+00:00"}
	for col, want := range checks {
		if got := row[idx[col]]; got != want {
			t.Errorf("%s = %#v, want %#v", col, got, want)
		}
	}
	if tags, ok := row[idx["tags"]].([]any); !ok || len(tags) != 2 || tags[0] != "math" {
		t.Errorf("tags = %#v", row[idx["tags"]])
	}
	if addr, ok := row[idx["address"]].(map[string]any); !ok || addr["city"] != "London" || addr["zip"] != int64(1815) {
		t.Errorf("address = %#v", row[idx["address"]])
	}
	if g, ok := row[idx["loc"]].(map[string]any); !ok || g["srid"] != geographySRID || g["$geo"] == nil {
		t.Errorf("loc = %#v", row[idx["loc"]])
	}
	if b, ok := row[idx["payload"]].(map[string]any); !ok || b["$bin"] != "AAE=" {
		t.Errorf("payload = %#v", row[idx["payload"]])
	}
}

// TestIntegrationDDL creates a table from generated DDL on the emulator and
// checks that its described structure round-trips without statements. The
// emulator accepts most ALTER TABLE actions without applying them, rejects
// RENAME COLUMN, SET DATA TYPE and DROP SCHEMA, and ignores DROP TABLE, so
// changes are covered by the unit tests.
func TestIntegrationDDL(t *testing.T) {
	ep := os.Getenv("ROWSMITH_TEST_BQ_ENDPOINT")
	if ep == "" {
		t.Skip("set ROWSMITH_TEST_BQ_ENDPOINT to run against the BigQuery emulator")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, _ := driver.Get("bigquery")
	cn, err := d.Open(ctx, driver.OpenParams{Params: map[string]any{"project": "rowsmith-dev", "dataset": "analytics", "endpoint": ep}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cn.Close()
	gen := cn.(driver.DDLGenerator)
	sess, err := cn.NewSession(ctx, driver.Scope{Schema: "analytics"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	table := "it_ddl_" + strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 36)
	ref := driver.ObjectRef{Schema: "analytics", Name: table, Kind: "table"}
	stmts, err := gen.CreateTableSQL(driver.TableDef{Ref: ref, Comment: "Generated",
		Columns: []driver.ColumnDef{
			{Column: driver.Column{Name: "id", Type: "INT64", Comment: "key"}},
			{Column: driver.Column{Name: "name", Type: "STRING", Nullable: true, Default: strp("'anon'")}},
			{Column: driver.Column{Name: "amount", Type: "NUMERIC(12, 2)", Nullable: true}},
			{Column: driver.Column{Name: "tags", Type: "ARRAY<STRING>"}},
			{Column: driver.Column{Name: "address", Type: "STRUCT<city STRING, zip INT64>", Nullable: true}},
			{Column: driver.Column{Name: "day", Type: "DATE", Nullable: true}},
		},
		PrimaryKey: []string{"id"},
		Options:    map[string]string{"partition_by": "day", "cluster_by": "id", "labels": "env=test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustOK(t, exec(t, sess, strings.Join(stmts, ";\n"), driver.ExecOptions{}))
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := cn.(*conn).client.Dataset("analytics").Table(table).Delete(ctx); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()

	from, err := cn.Describe(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	// The emulator keeps names and types but drops NOT NULL, defaults,
	// descriptions, parameterized precision and keys.
	if len(from.Columns) != 6 || from.Columns[4].Type != "STRUCT<city STRING, zip INT64>" {
		t.Fatalf("described columns = %+v", from.Columns)
	}
	if again, err := gen.AlterTableSQL(from, defFromTable(from)); err != nil || len(again) != 0 {
		t.Fatalf("unchanged table produced %q, %v", again, err)
	}

	trunc, err := gen.TruncateSQL(ref)
	if err != nil {
		t.Fatal(err)
	}
	mustOK(t, exec(t, sess, "INSERT INTO `rowsmith-dev.analytics."+table+"` (id, tags) VALUES (1, ['a'])", driver.ExecOptions{}))
	mustOK(t, exec(t, sess, strings.Join(trunc, ";\n"), driver.ExecOptions{}))
	if n, err := cn.Count(ctx, driver.BrowseRequest{Ref: ref}); err != nil || n.Rows != 0 {
		t.Errorf("rows after TRUNCATE = %+v, %v", n, err)
	}
}
