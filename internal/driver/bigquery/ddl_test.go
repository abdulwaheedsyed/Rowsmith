package bigquery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"

	"rowsmith/internal/driver"
)

func strp(s string) *string { return &s }

// defFromTable builds the definition the structure editor submits for an
// existing table: everything Describe reported, columns linked by name.
func defFromTable(t *driver.Table) driver.TableDef {
	def := driver.TableDef{Ref: t.Ref, PrimaryKey: append([]string(nil), t.PrimaryKey...), Comment: t.Comment,
		Indexes: append([]driver.Index(nil), t.Indexes...), ForeignKeys: append([]driver.ForeignKey(nil), t.ForeignKeys...),
		Checks: append([]driver.Check(nil), t.Checks...), Options: map[string]string{}}
	for _, c := range t.Columns {
		def.Columns = append(def.Columns, driver.ColumnDef{Column: c, OriginalName: c.Name})
	}
	for k, v := range t.Options {
		def.Options[k] = v
	}
	return def
}

func testConn() *conn { return &conn{project: "p", dataset: "ds"} }

// describedEvents is realistic Describe output for a partitioned, clustered
// table with keys, defaults, descriptions and nested columns.
func describedEvents() *driver.Table {
	md := &bigquery.TableMetadata{
		Type: bigquery.RegularTable, Description: "Events", Location: "EU", NumRows: 42, NumBytes: 4096,
		CreationTime: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), LastModifiedTime: time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC),
		ExpirationTime: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		Labels:         map[string]string{"team": "data", "env": "prod"},
		Schema: bigquery.Schema{
			{Name: "id", Type: bigquery.IntegerFieldType, Required: true, Description: "event id"},
			{Name: "day", Type: bigquery.DateFieldType, DefaultValueExpression: "CURRENT_DATE()"},
			{Name: "amount", Type: bigquery.NumericFieldType, Precision: 12, Scale: 2},
			{Name: "user", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "name", Type: bigquery.StringFieldType}, {Name: "age", Type: bigquery.IntegerFieldType}}},
			{Name: "tags", Type: bigquery.StringFieldType, Repeated: true},
			{Name: "country", Type: bigquery.StringFieldType, MaxLength: 2, Collation: "und:ci"},
			{Name: "account_id", Type: bigquery.IntegerFieldType},
		},
		TimePartitioning:       &bigquery.TimePartitioning{Type: bigquery.MonthPartitioningType, Field: "day", Expiration: 30 * 24 * time.Hour},
		RequirePartitionFilter: true,
		Clustering:             &bigquery.Clustering{Fields: []string{"country", "id"}},
		TableConstraints: &bigquery.TableConstraints{
			PrimaryKey: &bigquery.PrimaryKey{Columns: []string{"id"}},
			ForeignKeys: []*bigquery.ForeignKey{{Name: "fk_account", ReferencedTable: &bigquery.Table{ProjectID: "p", DatasetID: "crm", TableID: "accounts"},
				ColumnReferences: []*bigquery.ColumnReference{{ReferencingColumn: "account_id", ReferencedColumn: "id"}}}},
		},
		StreamingBuffer: &bigquery.StreamingBuffer{EstimatedRows: 5},
	}
	return describeMetadata(driver.ObjectRef{Schema: "ds", Name: "events", Kind: "table"}, &bigquery.Table{ProjectID: "p", DatasetID: "ds", TableID: "events"}, md)
}

func TestBQDesign(t *testing.T) {
	d, _ := driver.Get("bigquery")
	info := d.Info()
	ds := info.Design
	if ds == nil || !ds.Columns || !ds.ColumnComments || !ds.TableComment || !ds.PrimaryKey || ds.Indexes || ds.Checks ||
		ds.ForeignKeys || ds.AutoIncrement || ds.ReorderColumns || ds.Note == "" || !info.Caps.DDL {
		t.Fatalf("design = %+v, caps = %+v", ds, info.Caps)
	}
	// Every option the editor offers is a key Describe reports.
	described := describedEvents().Options
	for _, f := range ds.Options {
		if _, ok := described[f.Key]; !ok {
			t.Errorf("option %s is not reported by Describe", f.Key)
		}
	}
}

func TestBQCreateTableSQL(t *testing.T) {
	c := testConn()
	stmts, err := c.CreateTableSQL(driver.TableDef{
		Ref: driver.ObjectRef{Schema: "ds", Name: "orders"},
		Columns: []driver.ColumnDef{
			{Column: driver.Column{Name: "id", Type: "INT64", Comment: `order "id"`}},
			{Column: driver.Column{Name: "placed", Type: "TIMESTAMP", Default: strp("CURRENT_TIMESTAMP()")}},
			{Column: driver.Column{Name: "status", Type: "STRING", Nullable: true, Default: strp("'new'"), Collation: "und:ci"}},
			{Column: driver.Column{Name: "tags", Type: "ARRAY<STRING>"}}, // arrays are never NOT NULL
			{Column: driver.Column{Name: "customer", Type: "STRUCT<id INT64, name STRING>", Nullable: true}},
			{Column: driver.Column{Name: "customer_id", Type: "INT64", Nullable: true}},
		},
		PrimaryKey: []string{"id"},
		ForeignKeys: []driver.ForeignKey{{Name: "fk_customer", Columns: []string{"customer_id"},
			RefTable: driver.ObjectRef{Schema: "crm", Name: "customers"}, RefColumns: []string{"id"}}},
		Comment: "Orders",
		Options: map[string]string{"partition_by": "DATE(placed)", "cluster_by": "status, customer_id", "partition_expiration_days": "90",
			"require_partition_filter": "true", "labels": "team=sales, env=prod", "expiration": "2030-01-01 00:00:00 UTC",
			"location": "EU", "type": "TABLE"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "CREATE TABLE `p.ds.orders` (\n" +
		"  `id` INT64 NOT NULL OPTIONS(description=\"order \\\"id\\\"\"),\n" +
		"  `placed` TIMESTAMP DEFAULT CURRENT_TIMESTAMP() NOT NULL,\n" +
		"  `status` STRING COLLATE \"und:ci\" DEFAULT 'new',\n" +
		"  `tags` ARRAY<STRING>,\n" +
		"  `customer` STRUCT<id INT64, name STRING>,\n" +
		"  `customer_id` INT64,\n" +
		"  PRIMARY KEY (`id`) NOT ENFORCED,\n" +
		"  CONSTRAINT `fk_customer` FOREIGN KEY (`customer_id`) REFERENCES `p.crm.customers`(`id`) NOT ENFORCED\n" +
		")\nPARTITION BY DATE(placed)\nCLUSTER BY status, customer_id\nOPTIONS(\n" +
		"  description=\"Orders\",\n  partition_expiration_days=90,\n  require_partition_filter=true,\n" +
		"  expiration_timestamp=TIMESTAMP \"2030-01-01 00:00:00 UTC\",\n  labels=[(\"env\", \"prod\"), (\"team\", \"sales\")]\n)"
	if len(stmts) != 1 || stmts[0] != want {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(stmts, "\n---\n"), want)
	}

	minimal, err := c.CreateTableSQL(driver.TableDef{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "STRING", Nullable: true}}}})
	if err != nil || minimal[0] != "CREATE TABLE `p.ds.t` (\n  `a` STRING\n)" {
		t.Fatalf("minimal = %q, %v", minimal, err)
	}

	for name, def := range map[string]driver.TableDef{
		"no name":    {Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "INT64"}}}},
		"no columns": {Ref: driver.ObjectRef{Name: "t"}},
		"no type":    {Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a"}}}},
		"index": {Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "INT64"}}},
			Indexes: []driver.Index{{Name: "ix", Columns: []string{"a"}}}},
		"check": {Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "INT64"}}},
			Checks: []driver.Check{{Name: "ck", Expression: "a > 0"}}},
		"duplicate": {Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "INT64"}}, {Column: driver.Column{Name: "A", Type: "INT64"}}}},
		"auto":      {Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "INT64", AutoIncrement: true}}}},
		"days": {Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "INT64"}}},
			Options: map[string]string{"partition_expiration_days": "soon"}},
		"labels": {Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "INT64"}}},
			Options: map[string]string{"labels": "team"}},
	} {
		if _, err := c.CreateTableSQL(def); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := (&conn{project: "p"}).CreateTableSQL(driver.TableDef{Ref: driver.ObjectRef{Name: "t"},
		Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "INT64"}}}}); err == nil || err.Error() != "choose a dataset" {
		t.Errorf("no dataset: %v", err)
	}
}

func TestBQAlterUnchanged(t *testing.T) {
	c := testConn()
	from := describedEvents()
	stmts, err := c.AlterTableSQL(from, defFromTable(from))
	if err != nil || len(stmts) != 0 {
		t.Fatalf("unchanged table produced %q, %v", stmts, err)
	}
	// Cosmetic differences in types and expressions are not changes.
	def := defFromTable(from)
	def.Columns[2].Type = "numeric(12,2)"
	def.Columns[3].Type = "struct<name string, age integer>"
	def.Options["cluster_by"] = "country, id"
	def.Options["partition_by"] = "date_trunc(day, month)"
	def.Options["labels"] = "team=data,env=prod"
	def.Options["partition_expiration_days"] = "30.0"
	def.Indexes = []driver.Index{{Name: "PRIMARY", Primary: true, Columns: []string{"id"}}}
	if stmts, err := c.AlterTableSQL(from, def); err != nil || len(stmts) != 0 {
		t.Fatalf("cosmetic edits produced %q, %v", stmts, err)
	}
}

func TestBQAlterTableSQL(t *testing.T) {
	c := testConn()
	from := describedEvents()
	def := defFromTable(from)
	cols := def.Columns
	// id: widened; day: default dropped; amount → total, relaxed, described; user: removed;
	// tags kept; country: default set; account_id kept; new note column.
	cols[0].Type = "NUMERIC"
	cols[1].Default = nil
	cols[2].Name, cols[2].Comment = "total", "gross"
	cols[5].Default = strp("'SA'")
	cols[5].Comment = "ISO code"
	cols[0].Nullable = true
	def.Columns = append(append([]driver.ColumnDef{}, cols[:3]...), cols[4:]...)
	def.Columns = append(def.Columns, driver.ColumnDef{Column: driver.Column{Name: "note", Type: "STRING", Nullable: true, Comment: "free text"}},
		driver.ColumnDef{Column: driver.Column{Name: "codes", Type: "ARRAY<INT64>"}})
	def.PrimaryKey = []string{"id", "total"}
	def.ForeignKeys = nil
	def.Comment = ""
	def.Options["labels"] = "team=data"
	def.Options["partition_expiration_days"] = ""
	def.Options["require_partition_filter"] = "false"
	def.Options["expiration"] = "TIMESTAMP_ADD(CURRENT_TIMESTAMP(), INTERVAL 7 DAY)"
	def.Ref.Name = "events_v2"

	stmts, err := c.AlterTableSQL(from, def)
	if err != nil {
		t.Fatal(err)
	}
	tbl := "ALTER TABLE `p.ds.events` "
	want := []string{
		tbl + "DROP PRIMARY KEY",
		tbl + "DROP CONSTRAINT `fk_account`",
		tbl + "DROP COLUMN `user`",
		tbl + "ALTER COLUMN `id` SET DATA TYPE NUMERIC",
		tbl + "ALTER COLUMN `id` DROP NOT NULL",
		tbl + "ALTER COLUMN `day` DROP DEFAULT",
		tbl + "ALTER COLUMN `amount` SET OPTIONS(description=\"gross\")",
		tbl + "ALTER COLUMN `country` SET DEFAULT 'SA'",
		tbl + "ALTER COLUMN `country` SET OPTIONS(description=\"ISO code\")",
		tbl + "RENAME COLUMN `amount` TO `total`",
		"ALTER TABLE `p.ds.events`\n  ADD COLUMN `note` STRING OPTIONS(description=\"free text\"),\n  ADD COLUMN `codes` ARRAY<INT64>",
		tbl + "ADD PRIMARY KEY (`id`, `total`) NOT ENFORCED",
		tbl + "SET OPTIONS(description=NULL, partition_expiration_days=NULL, require_partition_filter=false, " +
			"expiration_timestamp=TIMESTAMP_ADD(CURRENT_TIMESTAMP(), INTERVAL 7 DAY), labels=[(\"team\", \"data\")])",
		tbl + "RENAME TO `events_v2`",
	}
	if !reflect.DeepEqual(stmts, want) {
		t.Fatalf("got:\n%s\n\nwant:\n%s", strings.Join(stmts, "\n"), strings.Join(want, "\n"))
	}

	// Adding keys and options to a plain table.
	plain := &driver.Table{Ref: driver.ObjectRef{Schema: "ds", Name: "t"}, Kind: "table",
		Columns: []driver.Column{{Name: "a", Type: "INT64", Nullable: true}, {Name: "b", Type: "INT64", Nullable: true}}, Options: map[string]string{"type": "TABLE"}}
	pd := defFromTable(plain)
	pd.PrimaryKey = []string{"a"}
	pd.ForeignKeys = []driver.ForeignKey{{Name: "fk_b", Columns: []string{"b"}, RefTable: driver.ObjectRef{Name: "other"}, RefColumns: []string{"id"}}}
	pd.Comment = "Plain"
	pd.Options["labels"] = "k=v"
	pd.Options["partition_expiration_days"] = "7"
	stmts, err = c.AlterTableSQL(plain, pd)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		"ALTER TABLE `p.ds.t` ADD PRIMARY KEY (`a`) NOT ENFORCED",
		"ALTER TABLE `p.ds.t` ADD CONSTRAINT `fk_b` FOREIGN KEY (`b`) REFERENCES `p.ds.other`(`id`) NOT ENFORCED",
		"ALTER TABLE `p.ds.t` SET OPTIONS(description=\"Plain\", partition_expiration_days=7, labels=[(\"k\", \"v\")])",
	}
	if !reflect.DeepEqual(stmts, want) {
		t.Fatalf("got:\n%s\n\nwant:\n%s", strings.Join(stmts, "\n"), strings.Join(want, "\n"))
	}
}

func TestBQAlterErrors(t *testing.T) {
	c := testConn()
	cases := map[string]func(d *driver.TableDef){
		"required": func(d *driver.TableDef) { d.Columns[1].Nullable = false },
		"new required": func(d *driver.TableDef) {
			d.Columns = append(d.Columns, driver.ColumnDef{Column: driver.Column{Name: "x", Type: "INT64"}})
		},
		"partition":   func(d *driver.TableDef) { d.Options["partition_by"] = "id" },
		"unpartition": func(d *driver.TableDef) { delete(d.Options, "partition_by") },
		"cluster":     func(d *driver.TableDef) { d.Options["cluster_by"] = "id" },
		"collation":   func(d *driver.TableDef) { d.Columns[5].Collation = "" },
		"index": func(d *driver.TableDef) {
			d.Indexes = append(d.Indexes, driver.Index{Name: "ix", Columns: []string{"id"}})
		},
		"check":     func(d *driver.TableDef) { d.Checks = []driver.Check{{Name: "c", Expression: "id > 0"}} },
		"gone":      func(d *driver.TableDef) { d.Columns[0].OriginalName = "nope" },
		"twice":     func(d *driver.TableDef) { d.Columns[1].OriginalName = "id" },
		"duplicate": func(d *driver.TableDef) { d.Columns[1].Name = "ID" },
		"no type":   func(d *driver.TableDef) { d.Columns[1].Type = "" },
		"labels":    func(d *driver.TableDef) { d.Options["labels"] = "=x" },
	}
	for name, edit := range cases {
		def := defFromTable(describedEvents())
		edit(&def)
		if stmts, err := c.AlterTableSQL(describedEvents(), def); err == nil {
			t.Errorf("%s: expected an error, got %q", name, stmts)
		}
	}
	view := &driver.Table{Ref: driver.ObjectRef{Schema: "ds", Name: "v"}, Kind: "view"}
	if _, err := c.AlterTableSQL(view, defFromTable(view)); err == nil || !strings.Contains(err.Error(), "view") {
		t.Errorf("view: %v", err)
	}
	if _, err := c.AlterTableSQL(nil, driver.TableDef{}); err == nil {
		t.Error("nil table accepted")
	}
	// Arrays are never NOT NULL, so flipping the switch on one is ignored.
	def := defFromTable(describedEvents())
	def.Columns[4].Nullable = true
	if stmts, err := c.AlterTableSQL(describedEvents(), def); err != nil || len(stmts) != 0 {
		t.Errorf("array nullability: %q, %v", stmts, err)
	}
}

func TestNormType(t *testing.T) {
	same := [][2]string{
		{"numeric(10,2)", "NUMERIC(10, 2)"}, {"INTEGER", "INT64"}, {"bool", "BOOLEAN"}, {"float", "FLOAT64"},
		{"struct<a int64, b string>", "STRUCT<a INT64, b STRING>"}, {"ARRAY< STRUCT<`x y` INT64> >", "ARRAY<STRUCT<`x y` INT64>>"},
	}
	for _, p := range same {
		if normType(p[0]) != normType(p[1]) {
			t.Errorf("%q and %q should be the same type (%q vs %q)", p[0], p[1], normType(p[0]), normType(p[1]))
		}
	}
	differ := [][2]string{{"ARRAY<STRUCT<x INT64>>", "ARRAY<STRUCT<x STRING>>"}, {"STRING(10)", "STRING(20)"}, {"STRUCT<ab INT64>", "STRUCT<a bINT64>"}}
	for _, p := range differ {
		if normType(p[0]) == normType(p[1]) {
			t.Errorf("%q and %q should differ", p[0], p[1])
		}
	}
}

// routineServer answers routine metadata lookups for DropObjectSQL.
func routineServer(t *testing.T) *conn {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /projects/p/datasets", func(w http.ResponseWriter, r *http.Request) { reply(w, &bq.DatasetList{}) })
	mux.HandleFunc("GET /projects/p/datasets/ds/routines/{name}", func(w http.ResponseWriter, r *http.Request) {
		types := map[string]string{"inc": "SCALAR_FUNCTION", "agg": "AGGREGATE_FUNCTION", "tvf": "TABLE_VALUED_FUNCTION", "proc": "PROCEDURE"}
		typ, ok := types[r.PathValue("name")]
		if !ok {
			http.Error(w, `{"error":{"code":404,"message":"Not found: Routine p:ds.`+r.PathValue("name")+`"}}`, 404)
			return
		}
		reply(w, &bq.Routine{RoutineType: typ, RoutineReference: &bq.RoutineReference{ProjectId: "p", DatasetId: "ds", RoutineId: r.PathValue("name")}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	d, _ := driver.Get("bigquery")
	cn, err := d.Open(context.Background(), driver.OpenParams{Params: map[string]any{"project": "p", "dataset": "ds", "location": "EU", "endpoint": srv.URL}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cn.Close() })
	return cn.(*conn)
}

func TestBQObjectDDL(t *testing.T) {
	c := routineServer(t)
	one := func(stmts []string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if len(stmts) != 1 {
			t.Fatalf("statements = %q", stmts)
		}
		return stmts[0]
	}
	drops := map[driver.ObjectRef]string{
		{Schema: "ds", Name: "t", Kind: "table"}:                "DROP TABLE `p.ds.t`",
		{Name: "t"}:                                             "DROP TABLE `p.ds.t`",
		{Schema: "ds", Name: "v", Kind: "view"}:                 "DROP VIEW `p.ds.v`",
		{Schema: "ds", Name: "mv", Kind: "materialized_view"}:   "DROP MATERIALIZED VIEW `p.ds.mv`",
		{Schema: "ds", Name: "ext", Kind: "external_table"}:     "DROP EXTERNAL TABLE `p.ds.ext`",
		{Schema: "other.ds2", Name: "t", Kind: "table"}:         "DROP TABLE `other.ds2.t`",
		{Schema: "ds", Name: "inc", Kind: "routine"}:            "DROP FUNCTION `p.ds.inc`",
		{Schema: "ds", Name: "agg", Kind: "routine"}:            "DROP FUNCTION `p.ds.agg`",
		{Schema: "ds", Name: "tvf", Kind: "routine"}:            "DROP TABLE FUNCTION `p.ds.tvf`",
		{Schema: "ds", Name: "proc", Kind: "routine"}:           "DROP PROCEDURE `p.ds.proc`",
		{Schema: "ds", Name: "odd`name", Kind: "table"}:         "DROP TABLE `p.ds.odd\\`name`",
		{Schema: "ds", Name: "f", Kind: "function"}:             "DROP FUNCTION `p.ds.f`",
		{Schema: "ds", Name: "tf", Kind: "table_function"}:      "DROP TABLE FUNCTION `p.ds.tf`",
		{Schema: "ds", Name: "p", Kind: "procedure"}:            "DROP PROCEDURE `p.ds.p`",
		{Schema: "ds", Name: "snapshot_of_t", Kind: "table"}:    "DROP TABLE `p.ds.snapshot_of_t`",
		{Schema: "ds", Name: "events$20250101", Kind: "table"}:  "DROP TABLE `p.ds.events$20250101`",
		{Schema: "ds", Name: "with space", Kind: "view"}:        "DROP VIEW `p.ds.with space`",
		{Schema: "ds", Name: "t", Kind: "table", Database: "x"}: "DROP TABLE `p.ds.t`",
	}
	for ref, want := range drops {
		if got := one(c.DropObjectSQL(ref, true)); got != want {
			t.Errorf("drop %+v = %q, want %q", ref, got, want)
		}
	}
	if _, err := c.DropObjectSQL(driver.ObjectRef{Schema: "ds", Name: "missing", Kind: "routine"}, false); err == nil || !strings.Contains(err.Error(), "Not found") {
		t.Errorf("missing routine: %v", err)
	}
	if _, err := c.DropObjectSQL(driver.ObjectRef{Schema: "ds", Name: "x", Kind: "index"}, false); err == nil {
		t.Error("unknown kind accepted")
	}

	if got := one(c.TruncateSQL(driver.ObjectRef{Schema: "ds", Name: "t", Kind: "table"})); got != "TRUNCATE TABLE `p.ds.t`" {
		t.Errorf("truncate = %q", got)
	}
	if _, err := c.TruncateSQL(driver.ObjectRef{Schema: "ds", Name: "v", Kind: "view"}); err == nil {
		t.Error("truncating a view accepted")
	}
	if got := one(c.RenameObjectSQL(driver.ObjectRef{Schema: "ds", Name: "t", Kind: "table"}, " t2 ")); got != "ALTER TABLE `p.ds.t` RENAME TO `t2`" {
		t.Errorf("rename = %q", got)
	}
	for _, bad := range []driver.ObjectRef{{Schema: "ds", Name: "v", Kind: "view"}, {Schema: "ds", Name: "r", Kind: "routine"}} {
		if _, err := c.RenameObjectSQL(bad, "x"); err == nil {
			t.Errorf("renaming %s accepted", bad.Kind)
		}
	}
	if _, err := c.RenameObjectSQL(driver.ObjectRef{Schema: "ds", Name: "t"}, " "); err == nil {
		t.Error("empty new name accepted")
	}

	if got := one(c.CreateSchemaSQL("", "staging")); got != "CREATE SCHEMA `p.staging` OPTIONS(location=\"EU\")" {
		t.Errorf("create schema = %q", got)
	}
	c.location = ""
	if got := one(c.CreateSchemaSQL("", "other.staging")); got != "CREATE SCHEMA `other.staging`" {
		t.Errorf("create schema elsewhere = %q", got)
	}
	if got := one(c.DropSchemaSQL("", "staging", true)); got != "DROP SCHEMA `p.staging` CASCADE" {
		t.Errorf("drop schema = %q", got)
	}
	if got := one(c.DropSchemaSQL("", "staging", false)); got != "DROP SCHEMA `p.staging`" {
		t.Errorf("drop schema = %q", got)
	}
	if _, err := c.CreateSchemaSQL("", ""); err == nil {
		t.Error("empty dataset name accepted")
	}
	if _, err := c.CreateDatabaseSQL("x", nil); err == nil || !strings.Contains(err.Error(), "Google Cloud") {
		t.Errorf("create database: %v", err)
	}
	if _, err := c.DropDatabaseSQL("x"); err == nil {
		t.Error("drop database accepted")
	}
}

func TestBuildDDLDefaultBeforeNotNull(t *testing.T) {
	md := &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "a", Type: bigquery.StringFieldType, Required: true, DefaultValueExpression: "'x'"}}}
	tb := describeMetadata(driver.ObjectRef{Schema: "ds", Name: "t"}, &bigquery.Table{ProjectID: "p", DatasetID: "ds", TableID: "t"}, md)
	if ddl := buildDDL("`p.ds.t`", tb, md); !strings.Contains(ddl, "`a` STRING DEFAULT 'x' NOT NULL") {
		t.Errorf("DDL = %s", ddl)
	}
}
