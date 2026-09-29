package mssql

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

// Integration tests run against a real server when ROWSMITH_TEST_MSSQL_HOST
// is set (see dev/compose.yaml). They create and drop their own databases.
//
//	ROWSMITH_TEST_MSSQL_HOST, _PORT (1433), _USER (sa), _PASSWORD

func testParams(t *testing.T, database string) driver.OpenParams {
	t.Helper()
	host := os.Getenv("ROWSMITH_TEST_MSSQL_HOST")
	if host == "" {
		t.Skip("ROWSMITH_TEST_MSSQL_HOST not set")
	}
	user := os.Getenv("ROWSMITH_TEST_MSSQL_USER")
	if user == "" {
		user = "sa"
	}
	port := os.Getenv("ROWSMITH_TEST_MSSQL_PORT")
	if port == "" {
		port = "1433"
	}
	return driver.OpenParams{
		Params: map[string]any{"host": host, "port": port, "user": user, "database": database,
			"encrypt": "true", "trustServerCertificate": true},
		Secrets: map[string]string{"password": os.Getenv("ROWSMITH_TEST_MSSQL_PASSWORD")},
		AppName: "rowsmith-test",
	}
}

func openConn(t *testing.T, p driver.OpenParams) *conn {
	t.Helper()
	d, ok := driver.Get("mssql")
	if !ok {
		t.Fatal("driver not registered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dc, err := d.Open(ctx, p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { dc.Close() })
	return dc.(*conn)
}

var fixture = []string{
	`CREATE SCHEMA sales`,
	`CREATE TABLE dbo.customers (
		id int IDENTITY(1,1) NOT NULL CONSTRAINT PK_customers PRIMARY KEY,
		name nvarchar(100) NOT NULL,
		email varchar(200) COLLATE Latin1_General_CS_AS NULL CONSTRAINT UQ_customers_email UNIQUE,
		created datetime NOT NULL CONSTRAINT DF_customers_created DEFAULT (getdate())
	)`,
	`EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'People who buy things',
		@level0type = N'SCHEMA', @level0name = N'dbo', @level1type = N'TABLE', @level1name = N'customers'`,
	`EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'Display name',
		@level0type = N'SCHEMA', @level0name = N'dbo', @level1type = N'TABLE', @level1name = N'customers', @level2type = N'COLUMN', @level2name = N'name'`,
	`CREATE TABLE dbo.orders (
		id int IDENTITY(1,1) NOT NULL CONSTRAINT PK_orders PRIMARY KEY,
		customer_id int NOT NULL CONSTRAINT FK_orders_customers REFERENCES dbo.customers (id) ON DELETE CASCADE,
		total decimal(12,2) NOT NULL CONSTRAINT DF_orders_total DEFAULT ((0)),
		tax AS (total * 0.2) PERSISTED,
		price money NULL,
		token uniqueidentifier NOT NULL CONSTRAINT DF_orders_token DEFAULT (newid()),
		note nvarchar(max) NULL,
		placed datetimeoffset(3) NULL,
		shipped_on date NULL,
		flag bit NOT NULL CONSTRAINT DF_orders_flag DEFAULT (0),
		location geometry NULL,
		area geography NULL,
		ver rowversion,
		CONSTRAINT CK_orders_total CHECK (total >= 0)
	)`,
	`CREATE INDEX IX_orders_customer ON dbo.orders (customer_id DESC) INCLUDE (total) WHERE total > 0`,
	`CREATE TABLE dbo.audit_log (id int IDENTITY PRIMARY KEY, msg nvarchar(200))`,
	// No SET NOCOUNT ON: its row counts must not confuse grid edits.
	`CREATE TRIGGER dbo.trg_orders_audit ON dbo.orders AFTER INSERT, UPDATE AS
		INSERT dbo.audit_log (msg) SELECT N'order ' + CAST(id AS nvarchar(20)) FROM inserted`,
	`CREATE TABLE dbo.keyless (a int NULL, b nvarchar(20) NULL)`,
	`CREATE VIEW dbo.big_orders AS SELECT id, customer_id, total FROM dbo.orders WHERE total > 100`,
	`CREATE PROCEDURE dbo.greet @who nvarchar(50) AS
	BEGIN
		PRINT N'hello ' + @who;
		SELECT @who AS who;
	END`,
	`CREATE FUNCTION dbo.add_tax (@x decimal(12,2)) RETURNS decimal(12,2) AS BEGIN RETURN @x * 1.2 END`,
	`CREATE SEQUENCE dbo.seq_invoice AS bigint START WITH 1000 INCREMENT BY 1`,
	`CREATE SYNONYM dbo.clients FOR dbo.customers`,
	`CREATE TYPE dbo.email_address FROM nvarchar(320) NOT NULL`,
	`CREATE TYPE dbo.id_list AS TABLE (id int NOT NULL PRIMARY KEY, label nvarchar(20) NULL)`,
	`CREATE TABLE sales.regions (code char(2) NOT NULL PRIMARY KEY, name nvarchar(50) NOT NULL)`,
	`INSERT dbo.customers (name, email) VALUES (N'Ada', 'ada@example.com'), (N'Grace', 'grace@example.com'),
		(N'Linus', NULL), (N'Bob [admin] 100%', 'bob@example.com')`,
	`INSERT dbo.orders (customer_id, total, price, token, note, placed, shipped_on, flag, location, area) VALUES
		(1, 150.25, 10.5, '6F9619FF-8B86-D011-B42D-00C04FC964FF', N'first ☕', '2024-03-01 10:00:00.123 +02:00', '2024-03-02', 1,
			geometry::STGeomFromText('POINT(1 2)', 0), geography::STGeomFromText('POINT(13.4 52.5)', 4326)),
		(1, 20, NULL, NEWID(), NULL, NULL, NULL, 0, NULL, NULL),
		(2, 120, 1, NEWID(), N'second', NULL, NULL, 0, NULL, NULL)`,
	`INSERT dbo.keyless VALUES (1, N'x'), (1, N'x'), (2, N'y')`,
	`CREATE USER tester WITHOUT LOGIN`,
	`GRANT SELECT ON dbo.customers TO tester`,
	`GRANT SELECT, INSERT ON SCHEMA::sales TO tester`,
	`DENY DELETE ON dbo.orders TO tester`,
	`ALTER ROLE db_datareader ADD MEMBER tester`,
}

func TestIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	boot := openConn(t, testParams(t, ""))
	dbName := fmt.Sprintf("rowsmith_it_%d", time.Now().UnixNano()%1_000_000_000)
	copyDB := dbName + "_copy"
	for _, db := range []string{dbName, copyDB} {
		if _, err := boot.db.ExecContext(ctx, "CREATE DATABASE "+quote(db)); err != nil {
			t.Fatalf("create database: %v", err)
		}
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, _ = boot.db.ExecContext(cctx, "ALTER DATABASE "+quote(db)+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
			if _, err := boot.db.ExecContext(cctx, "DROP DATABASE "+quote(db)); err != nil {
				t.Logf("drop %s: %v", db, err)
			}
		})
	}
	c := openConn(t, testParams(t, dbName))
	for _, q := range fixture {
		if _, err := c.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("fixture %.60q: %v", q, err)
		}
	}
	ref := func(name string) driver.ObjectRef {
		return driver.ObjectRef{Database: dbName, Schema: "dbo", Name: name}
	}

	t.Run("Server", func(t *testing.T) {
		info, err := c.Server(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.Product != "SQL Server" || info.Version == "" || info.Database != dbName || info.Extras["Build"] == "" {
			t.Errorf("server info: %+v", info)
		}
	})

	t.Run("Databases", func(t *testing.T) {
		dbs, err := c.Databases(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]driver.Database{}
		for _, d := range dbs {
			found[d.Name] = d
		}
		if d, ok := found[dbName]; !ok || d.System || d.Size == nil || *d.Size <= 0 || d.Owner == "" {
			t.Errorf("test database: %+v", d)
		}
		if !found["master"].System {
			t.Error("master should be a system database")
		}
	})

	t.Run("Schemas", func(t *testing.T) {
		ss, err := c.Schemas(ctx, dbName)
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]driver.Schema{}
		for _, s := range ss {
			byName[s.Name] = s
		}
		if ss[0].Name != "dbo" || byName["sales"].System || !byName["sys"].System || !byName["db_owner"].System {
			t.Errorf("schemas: %+v", ss)
		}
	})

	t.Run("Objects", func(t *testing.T) {
		objs, err := c.Objects(ctx, driver.Scope{Database: dbName, Schema: "dbo"})
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]driver.Object{}
		for _, o := range objs {
			byName[o.Name] = o
		}
		want := map[string]string{"customers": "table", "orders": "table", "keyless": "table", "big_orders": "view",
			"greet": "procedure", "add_tax": "function", "trg_orders_audit": "trigger", "seq_invoice": "sequence",
			"clients": "synonym", "email_address": "type"}
		for name, kind := range want {
			if byName[name].Kind != kind {
				t.Errorf("%s: kind %q, want %q", name, byName[name].Kind, kind)
			}
		}
		if o := byName["customers"]; o.Rows == nil || *o.Rows != 4 || o.Size == nil || o.Comment != "People who buy things" {
			t.Errorf("customers: %+v", o)
		}
		checks := map[string]string{"add_tax": "scalar", "trg_orders_audit": "AFTER INSERT, UPDATE ON orders",
			"clients": "[dbo].[customers]", "email_address": "nvarchar(320)", "id_list": "table type"}
		for name, extra := range checks {
			if byName[name].Extra != extra {
				t.Errorf("%s extra %q, want %q", name, byName[name].Extra, extra)
			}
		}
	})

	t.Run("Describe", func(t *testing.T) {
		tb, err := c.Describe(ctx, ref("orders"))
		if err != nil {
			t.Fatal(err)
		}
		cols := map[string]driver.Column{}
		for _, col := range tb.Columns {
			cols[col.Name] = col
		}
		expect := func(name, typ string, kind driver.ValueKind) {
			t.Helper()
			if c := cols[name]; c.Type != typ || c.Kind != kind {
				t.Errorf("%s: %s/%s, want %s/%s", name, c.Type, c.Kind, typ, kind)
			}
		}
		expect("id", "int", driver.KindInt)
		expect("total", "decimal(12,2)", driver.KindDecimal)
		expect("price", "money", driver.KindDecimal)
		expect("token", "uniqueidentifier", driver.KindUUID)
		expect("note", "nvarchar(max)", driver.KindText)
		expect("placed", "datetimeoffset(3)", driver.KindTimestamp)
		expect("shipped_on", "date", driver.KindDate)
		expect("flag", "bit", driver.KindBool)
		expect("location", "geometry", driver.KindGeometry)
		expect("area", "geography", driver.KindGeometry)
		expect("ver", "rowversion", driver.KindBinary)
		if !cols["id"].AutoIncrement || !cols["id"].PrimaryKey || cols["id"].Nullable {
			t.Errorf("id: %+v", cols["id"])
		}
		if cols["tax"].Generated != "[total]*(0.2)" || cols["ver"].Generated == "" {
			t.Errorf("generated: tax %q ver %q", cols["tax"].Generated, cols["ver"].Generated)
		}
		if d := cols["total"].Default; d == nil || *d != "0" {
			t.Errorf("total default %v", d)
		}
		if cols["area"].SRID != 4326 || cols["location"].GeometryType != "geometry" {
			t.Errorf("spatial: %+v %+v", cols["area"], cols["location"])
		}
		if strings.Join(tb.PrimaryKey, ",") != "id" || tb.RowKeyKind != "primary" || !tb.Editable {
			t.Errorf("keys: %v %s", tb.PrimaryKey, tb.RowKeyKind)
		}
		var ix *driver.Index
		for i := range tb.Indexes {
			if tb.Indexes[i].Name == "IX_orders_customer" {
				ix = &tb.Indexes[i]
			}
		}
		if ix == nil || ix.Where != "[total]>(0)" || len(ix.Desc) != 1 || !ix.Desc[0] || !strings.Contains(ix.Definition, "INCLUDE ([total])") {
			t.Errorf("index: %+v", ix)
		}
		if len(tb.ForeignKeys) != 1 || tb.ForeignKeys[0].OnDelete != "CASCADE" || tb.ForeignKeys[0].RefTable.Name != "customers" {
			t.Errorf("fks: %+v", tb.ForeignKeys)
		}
		if len(tb.Checks) != 1 || tb.Checks[0].Expression != "[total]>=(0)" {
			t.Errorf("checks: %+v", tb.Checks)
		}
		if len(tb.Triggers) != 1 || tb.Triggers[0].Timing != "AFTER" || tb.Triggers[0].Event != "INSERT, UPDATE" {
			t.Errorf("triggers: %+v", tb.Triggers)
		}
		for _, part := range []string{"CREATE TABLE [dbo].[orders]", "[id] int IDENTITY(1,1) NOT NULL", "[tax] AS ([total]*(0.2)) PERSISTED",
			"CONSTRAINT [DF_orders_total] DEFAULT ((0))", "CONSTRAINT [PK_orders] PRIMARY KEY CLUSTERED ([id] ASC)",
			"REFERENCES [dbo].[customers] ([id]) ON DELETE CASCADE", "WHERE [total]>(0);", "GO\nCREATE TRIGGER"} {
			if !strings.Contains(tb.DDL, part) {
				t.Errorf("DDL lacks %q:\n%s", part, tb.DDL)
			}
		}

		cu, err := c.Describe(ctx, ref("customers"))
		if err != nil {
			t.Fatal(err)
		}
		if cu.Comment != "People who buy things" || cu.Columns[1].Comment != "Display name" || len(cu.Referenced) != 1 ||
			cu.Columns[2].Collation != "Latin1_General_CS_AS" || !strings.Contains(cu.DDL, "COLLATE Latin1_General_CS_AS") ||
			!strings.Contains(cu.DDL, "CONSTRAINT [UQ_customers_email] UNIQUE NONCLUSTERED ([email] ASC)") {
			t.Errorf("customers: %+v\n%s", cu, cu.DDL)
		}
		kl, err := c.Describe(ctx, ref("keyless"))
		if err != nil || kl.RowKeyKind != "all" || !kl.Editable {
			t.Errorf("keyless: %+v %v", kl, err)
		}
		v, err := c.Describe(ctx, ref("big_orders"))
		if err != nil || v.Kind != "view" || !strings.Contains(v.Definition, "CREATE VIEW dbo.big_orders") || v.Editable {
			t.Errorf("view: %+v %v", v, err)
		}
		if _, err := c.Describe(ctx, ref("missing")); err == nil {
			t.Error("describe of a missing table should fail")
		}

		// The reconstructed DDL recreates the tables elsewhere.
		s, err := c.NewSession(ctx, driver.Scope{Database: copyDB})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		al, err := c.Describe(ctx, ref("audit_log"))
		if err != nil {
			t.Fatal(err)
		}
		sink := &recSink{}
		if err := s.Execute(ctx, al.DDL+"\nGO\n"+cu.DDL+"\nGO\n"+tb.DDL, driver.ExecOptions{StopOnError: true}, sink); err != nil {
			t.Fatal(err)
		}
		for _, st := range sink.runs() {
			if st.err != nil {
				t.Errorf("recreating %q: %v", st.info.SQL, st.err)
			}
		}
		copied, err := c.Describe(ctx, driver.ObjectRef{Database: copyDB, Schema: "dbo", Name: "orders"})
		if err != nil || len(copied.Columns) != len(tb.Columns) || len(copied.Indexes) != len(tb.Indexes) || len(copied.Triggers) != 1 {
			t.Errorf("copy: %+v %v", copied, err)
		}
	})

	t.Run("Definition", func(t *testing.T) {
		want := map[string][2]string{
			"greet":            {"procedure", "PRINT N'hello '"},
			"big_orders":       {"view", "CREATE VIEW"},
			"trg_orders_audit": {"trigger", "CREATE TRIGGER dbo.trg_orders_audit"},
			"add_tax":          {"function", "RETURNS decimal(12,2)"},
			"seq_invoice":      {"sequence", "CREATE SEQUENCE [dbo].[seq_invoice] AS bigint START WITH 1000 INCREMENT BY 1"},
			"clients":          {"synonym", "CREATE SYNONYM [dbo].[clients] FOR [dbo].[customers];"},
			"email_address":    {"type", "CREATE TYPE [dbo].[email_address] FROM nvarchar(320) NOT NULL;"},
			"customers":        {"table", "CREATE TABLE [dbo].[customers]"},
			"id_list":          {"type", "CREATE TYPE [dbo].[id_list] AS TABLE (\n    [id] int NOT NULL,\n    [label] nvarchar(20) NULL,\n    PRIMARY KEY CLUSTERED ([id] ASC)\n);"},
		}
		for name, w := range want {
			r := ref(name)
			r.Kind = w[0]
			def, err := c.Definition(ctx, r)
			if err != nil || !strings.Contains(def, w[1]) {
				t.Errorf("%s: %q %v", name, def, err)
			}
		}
		if _, err := c.Definition(ctx, driver.ObjectRef{Database: dbName, Schema: "dbo", Name: "nope", Kind: "procedure"}); err == nil {
			t.Error("missing module should fail")
		}
	})

	t.Run("CatalogColumns", func(t *testing.T) {
		cat, err := c.CatalogColumns(ctx, driver.Scope{Database: dbName, Schema: "dbo"})
		if err != nil {
			t.Fatal(err)
		}
		var orders *driver.CatalogTable
		for i := range cat {
			if cat[i].Name == "orders" {
				orders = &cat[i]
			}
		}
		if orders == nil || orders.Kind != "table" || !orders.Columns[0].PK || orders.Columns[2].Type != "decimal(12,2)" ||
			len(orders.FKs) != 1 || orders.FKs[0].RefTable.Name != "customers" {
			t.Errorf("orders: %+v", orders)
		}
	})

	t.Run("Browse", func(t *testing.T) {
		res, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref("orders"), Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) != 3 || res.Truncated {
			t.Fatalf("rows %d truncated %v\n%s", len(res.Rows), res.Truncated, res.SQL)
		}
		row := cells(res, 0)
		if row["total"] != "150.25" || row["price"] != "10.5000" || row["token"] != "6f9619ff-8b86-d011-b42d-00c04fc964ff" ||
			row["placed"] != "2024-03-01 10:00:00.123+02:00" || row["shipped_on"] != "2024-03-02" || row["flag"] != true ||
			row["note"] != "first ☕" {
			t.Errorf("cells: %#v", row)
		}
		loc, _ := row["location"].(map[string]any)
		area, _ := row["area"].(map[string]any)
		if loc == nil || loc["wkt"] != "POINT (1 2)" || area == nil || area["srid"] != 4326 {
			t.Errorf("geometry cells: %#v %#v", row["location"], row["area"])
		}
		if _, ok := row["ver"].(map[string]any)["$bin"]; !ok {
			t.Errorf("rowversion: %#v", row["ver"])
		}

		page, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref("orders"), Columns: []string{"id", "total"},
			Sort: []driver.Sort{{Column: "total", Desc: true}}, Offset: 1, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Rows) != 1 || !page.Truncated || cells(page, 0)["total"] != "120.00" {
			t.Errorf("page: %+v", page)
		}
		f, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref("orders"), Limit: 10,
			Filters: []driver.Filter{{Column: "total", Op: ">", Value: 50}, {Column: "note", Op: "notnull"}}})
		if err != nil || len(f.Rows) != 2 {
			t.Errorf("filters: %v %v", f, err)
		}
		for _, search := range []string{"[admin]", "100%"} {
			s, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref("customers"), Search: search, Limit: 10})
			if err != nil || len(s.Rows) != 1 {
				t.Errorf("search %q: %v %v", search, s, err)
			}
		}
		v, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref("big_orders"), Limit: 1})
		if err != nil || len(v.Rows) != 1 || !v.Truncated || !strings.Contains(v.SQL, "ORDER BY (SELECT NULL)") {
			t.Errorf("view: %+v %v", v, err)
		}
		n, err := c.Count(ctx, driver.BrowseRequest{Ref: ref("orders"), Filters: []driver.Filter{{Column: "customer_id", Op: "=", Value: 1}}})
		if err != nil || n.Rows != 2 || !n.Exact {
			t.Errorf("count: %+v %v", n, err)
		}
	})

	t.Run("ApplyEdits", func(t *testing.T) {
		res, err := c.ApplyEdits(ctx, ref("orders"), []driver.RowEdit{
			{Op: "update", Key: map[string]any{"id": 1.0}, Values: map[string]any{"total": "175.50", "flag": false}},
			{Op: "insert", Values: map[string]any{"customer_id": 2.0, "total": 5.0, "token": "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11",
				"placed": "2025-01-02 03:04:05+01:00", "location": map[string]any{"$geo": map[string]any{"type": "Point", "coordinates": []any{5.0, 6.0}}},
				"area": "SRID=4326;POINT(10 20)"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Applied != 2 {
			t.Errorf("applied %d", res.Applied)
		}
		got, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref("orders"), Limit: 10, Filters: []driver.Filter{{Column: "total", Op: "=", Value: 5}}})
		if err != nil || len(got.Rows) != 1 {
			t.Fatalf("inserted row: %v %v", got, err)
		}
		row := cells(got, 0)
		loc, _ := row["location"].(map[string]any)
		area, _ := row["area"].(map[string]any)
		if row["token"] != "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11" || loc == nil || loc["wkt"] != "POINT (5 6)" || area == nil || area["wkt"] != "POINT (10 20)" {
			t.Errorf("inserted: %#v", row)
		}

		// Datetime values are bound typed, independent of the session language.
		if _, err := c.ApplyEdits(ctx, ref("customers"), []driver.RowEdit{
			{Op: "update", Key: map[string]any{"id": 2.0}, Values: map[string]any{"created": "2024-01-13 10:11:12.123"}},
			{Op: "delete", Key: map[string]any{"id": 3.0}},
		}); err != nil {
			t.Fatal(err)
		}
		cu, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref("customers"), Limit: 10, Filters: []driver.Filter{{Column: "id", Op: "=", Value: 2}}})
		if err != nil || cells(cu, 0)["created"] != "2024-01-13 10:11:12.123" {
			t.Errorf("datetime: %v %v", cu, err)
		}
		// Rows of a keyless table are matched on every column, one at a time.
		if _, err := c.ApplyEdits(ctx, ref("keyless"), []driver.RowEdit{
			{Op: "update", Key: map[string]any{"a": 1.0, "b": "x"}, Values: map[string]any{"b": "z"}},
			{Op: "delete", Key: map[string]any{"a": 2.0, "b": "y"}},
		}); err != nil {
			t.Fatal(err)
		}
		var z, x, y int
		if err := c.db.QueryRowContext(ctx, `SELECT SUM(CASE WHEN b = N'z' THEN 1 ELSE 0 END), SUM(CASE WHEN b = N'x' THEN 1 ELSE 0 END),
			SUM(CASE WHEN b = N'y' THEN 1 ELSE 0 END) FROM dbo.keyless`).Scan(&z, &x, &y); err != nil || z != 1 || x != 1 || y != 0 {
			t.Errorf("keyless after edits: z=%d x=%d y=%d %v", z, x, y, err)
		}
		// A key that matches nothing rolls the batch back.
		_, err = c.ApplyEdits(ctx, ref("orders"), []driver.RowEdit{{Op: "update", Key: map[string]any{"id": 999.0}, Values: map[string]any{"total": 1.0}}})
		if err == nil || !strings.Contains(err.Error(), "no longer exists") {
			t.Errorf("stale edit: %v", err)
		}
		// Server errors surface as QueryErrors.
		_, err = c.ApplyEdits(ctx, ref("orders"), []driver.RowEdit{{Op: "update", Key: map[string]any{"id": 1.0}, Values: map[string]any{"total": -1.0}}})
		var qe *driver.QueryError
		if !errors.As(err, &qe) || qe.Code != "547" || !strings.HasPrefix(qe.Message, "change 1: ") {
			t.Errorf("check violation: %#v", err)
		}

		ro := testParams(t, dbName)
		ro.ReadOnly = true
		rc := openConn(t, ro)
		if _, err := rc.ApplyEdits(ctx, ref("orders"), nil); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only edits: %v", err)
		}
		if info, err := rc.Server(ctx); err != nil || info.Extras["Session"] != "read-only intent" {
			t.Errorf("read-only server info: %+v %v", info, err)
		}
	})

	t.Run("Session", func(t *testing.T) {
		s, err := c.NewSession(ctx, driver.Scope{Database: dbName})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		script := "SELECT 1 AS a; SELECT 2 AS b\nPRINT 'hi'\nUPDATE dbo.keyless SET b = b\n" +
			"GO\n" +
			"SELECT 1 AS ok\nSELECT * FROM dbo.nope\n" +
			"GO\n" +
			"EXEC dbo.greet @who = N'Ada'"
		sink := &recSink{}
		if err := s.Execute(ctx, script, driver.ExecOptions{MaxRows: 100}, sink); err != nil {
			t.Fatal(err)
		}
		runs := sink.runs()
		if len(runs) != 3 {
			t.Fatalf("statements: %d", len(runs))
		}
		b0 := runs[0]
		if b0.err != nil || len(b0.sets) != 3 || b0.sets[0].cols[0].Name != "a" || b0.sets[1].rows[0][0] != int64(2) ||
			b0.sets[2].sum.RowsAffected == nil || *b0.sets[2].sum.RowsAffected != 2 || strings.Join(b0.notices, "|") != "info:hi" {
			t.Errorf("batch 0: %+v", b0)
		}
		var qe *driver.QueryError
		if b1 := runs[1]; !errors.As(b1.err, &qe) || qe.Code != "208" || qe.Line != 6 || b1.info.Line != 5 {
			t.Errorf("batch 1: %+v %#v", b1, b1.err)
		}
		if b2 := runs[2]; b2.err != nil || strings.Join(b2.notices, "|") != "info:hello Ada" || len(b2.sets) == 0 || b2.sets[0].rows[0][0] != "Ada" {
			t.Errorf("batch 2: %+v", b2)
		}

		// Rows beyond MaxRows are counted, and the rest of the batch still runs.
		sink = &recSink{}
		if err := s.Execute(ctx, "SELECT TOP (10) name FROM sys.all_objects\nSELECT 42 AS after", driver.ExecOptions{MaxRows: 3}, sink); err != nil {
			t.Fatal(err)
		}
		if r := sink.runs()[0]; len(r.sets) != 2 || len(r.sets[0].rows) != 3 || !r.sets[0].sum.Truncated || r.sets[0].sum.RowCount != 10 ||
			r.sets[1].rows[0][0] != int64(42) {
			t.Errorf("truncated: %+v", r)
		}

		// Long values are not cut at a default TEXTSIZE.
		sink = &recSink{}
		if err := s.Execute(ctx, "SELECT REPLICATE(CAST(N'x' AS nvarchar(max)), 10000) AS v", driver.ExecOptions{}, sink); err != nil {
			t.Fatal(err)
		}
		if v, _ := sink.runs()[0].sets[0].rows[0][0].(string); len(v) != 10000 {
			t.Errorf("long text: %d chars", len(v))
		}

		// Spatial values decode for the map view outside browse queries too.
		sink = &recSink{}
		if err := s.Execute(ctx, "SELECT location, area FROM dbo.orders WHERE id = 1", driver.ExecOptions{}, sink); err != nil {
			t.Fatal(err)
		}
		geoRow := sink.runs()[0].sets[0].rows[0]
		if loc, _ := geoRow[0].(map[string]any); loc == nil || loc["wkt"] != "POINT (1 2)" {
			t.Errorf("console geometry: %#v", geoRow[0])
		}
		if area, _ := geoRow[1].(map[string]any); area == nil || area["wkt"] != "POINT (13.4 52.5)" || area["srid"] != 4326 {
			t.Errorf("console geography: %#v", geoRow[1])
		}

		// Cancelling stops a running batch and leaves the session usable.
		cctx, ccancel := context.WithTimeout(ctx, time.Second)
		sink = &recSink{}
		started := time.Now()
		err = s.Execute(cctx, "WAITFOR DELAY '00:00:30'\nSELECT 1", driver.ExecOptions{}, sink)
		ccancel()
		if r := sink.runs(); err != nil || time.Since(started) > 10*time.Second || r[0].err == nil || !strings.Contains(r[0].err.Error(), "cancelled") {
			t.Errorf("cancel: %v %+v after %s", err, r, time.Since(started))
		}

		// A transaction spans Execute calls until committed or rolled back.
		run := func(sql string, opts driver.ExecOptions) stmtRun {
			t.Helper()
			sink := &recSink{}
			if err := s.Execute(ctx, sql, opts, sink); err != nil {
				t.Fatal(err)
			}
			return sink.runs()[0]
		}
		if r := run("BEGIN TRANSACTION\nUPDATE dbo.customers SET name = N'Ada L' WHERE id = 1", driver.ExecOptions{}); r.err != nil || !s.InTransaction() {
			t.Fatalf("begin: %+v in tx %v", r, s.InTransaction())
		}
		if r := run("SELECT name FROM dbo.customers WHERE id = 1", driver.ExecOptions{}); r.sets[0].rows[0][0] != "Ada L" {
			t.Errorf("inside tx: %+v", r)
		}
		if r := run("ROLLBACK", driver.ExecOptions{}); r.err != nil || s.InTransaction() {
			t.Errorf("rollback: %+v in tx %v", r, s.InTransaction())
		}
		var name string
		if err := c.db.QueryRowContext(ctx, "SELECT name FROM dbo.customers WHERE id = 1").Scan(&name); err != nil || name != "Ada" {
			t.Errorf("after rollback: %q %v", name, err)
		}
		if r := run("DELETE FROM dbo.keyless", driver.ExecOptions{ReadOnly: true}); r.err == nil || !strings.Contains(r.err.Error(), "read-only") {
			t.Errorf("read-only: %+v", r)
		}
		if r := run("SELECT 1/0 AS boom", driver.ExecOptions{}); r.err == nil || r.err.(*driver.QueryError).Code != "8134" {
			t.Errorf("divide by zero: %+v", r)
		}

		// Closing a session rolls back its open transaction.
		s2, err := c.NewSession(ctx, driver.Scope{Database: dbName})
		if err != nil {
			t.Fatal(err)
		}
		sink = &recSink{}
		if err := s2.Execute(ctx, "BEGIN TRAN\nDELETE FROM dbo.audit_log", driver.ExecOptions{}, sink); err != nil || !s2.InTransaction() {
			t.Fatalf("s2: %v", err)
		}
		s2.Close()
		var n int
		if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dbo.audit_log").Scan(&n); err != nil || n == 0 {
			t.Errorf("audit_log after close: %d %v", n, err)
		}
	})

	t.Run("Explain", func(t *testing.T) {
		scope := driver.Scope{Database: dbName}
		p, err := c.Explain(ctx, scope, "SELECT o.id, c.name FROM dbo.orders o JOIN dbo.customers c ON c.id = o.customer_id WHERE c.id = 1", false)
		if err != nil {
			t.Fatal(err)
		}
		objects := map[string]bool{}
		walkPlan(p.Root, func(n *driver.PlanNode) { objects[n.Object] = true })
		if p.Format != "xml" || !strings.HasPrefix(p.Raw, "<ShowPlanXML") || p.Root.Operation != "SELECT" || p.Root.Cost == nil ||
			len(p.Root.Children) == 0 || !objects["dbo.orders o"] || !objects["dbo.customers c"] {
			t.Errorf("estimated plan: %+v objects %v", p.Root, objects)
		}
		p, err = c.Explain(ctx, scope, "SELECT COUNT(*) FROM dbo.orders", true)
		if err != nil {
			t.Fatal(err)
		}
		actual := false
		walkPlan(p.Root, func(n *driver.PlanNode) { actual = actual || n.ActualRows != nil })
		if !actual || p.Totals["Execution Time"] == nil {
			t.Errorf("actual plan: %+v", p)
		}
		// Analyzed writes are rolled back.
		if _, err := c.Explain(ctx, scope, "UPDATE dbo.customers SET name = N'zzz'", true); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dbo.customers WHERE name = N'zzz'").Scan(&n); err != nil || n != 0 {
			t.Errorf("analyze persisted a write: %d %v", n, err)
		}
		if _, err := c.Explain(ctx, scope, "SELECT * FROM dbo.nope", false); err == nil {
			t.Error("plan for a missing table should fail")
		}
	})

	t.Run("Processes", func(t *testing.T) {
		s, err := c.NewSession(ctx, driver.Scope{Database: dbName})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		sink := &recSink{}
		if err := s.Execute(ctx, "SELECT @@SPID AS spid", driver.ExecOptions{}, sink); err != nil {
			t.Fatal(err)
		}
		spid := fmt.Sprint(sink.runs()[0].sets[0].rows[0][0])
		res, err := c.Processes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for i := range res.Rows {
			if fmt.Sprint(cells(res, i)["session_id"]) == spid {
				found = true
			}
		}
		if res.Columns[0].Name != "session_id" || !found {
			t.Errorf("session %s not listed: %+v", spid, res.Columns)
		}
		if err := c.KillProcess(ctx, "1; DROP TABLE x"); err == nil {
			t.Error("non-numeric id accepted")
		}
		if err := c.KillProcess(ctx, spid); err != nil {
			t.Fatal(err)
		}
		sink = &recSink{}
		_ = s.Execute(ctx, "SELECT 1", driver.ExecOptions{}, sink)
		if r := sink.runs(); len(r) == 1 && r[0].err == nil {
			t.Error("killed session still works")
		}
	})

	t.Run("Variables", func(t *testing.T) {
		v, err := c.Variables(ctx, "variables")
		if err != nil || len(v.Rows) < 10 {
			t.Fatalf("variables: %v %v", v, err)
		}
		st, err := c.Variables(ctx, "status")
		if err != nil {
			t.Fatal(err)
		}
		counters := map[string]bool{}
		for i := range st.Rows {
			counters[fmt.Sprint(cells(st, i)["counter"])] = true
		}
		if !counters["User Connections"] || !counters["Buffer cache hit ratio"] {
			t.Errorf("status counters: %v", counters)
		}
	})

	t.Run("Users", func(t *testing.T) {
		res, err := c.Users(ctx)
		if err != nil {
			t.Fatal(err)
		}
		scopes := map[string]string{}
		for i := range res.Rows {
			r := cells(res, i)
			scopes[fmt.Sprint(r["name"])] = fmt.Sprint(r["scope"])
		}
		if scopes["sa"] != "server" || scopes["tester"] != dbName {
			t.Errorf("users: %v", scopes)
		}
		g, err := c.UserGrants(ctx, "tester")
		if err != nil {
			t.Fatal(err)
		}
		all := strings.Join(g, "\n")
		for _, want := range []string{"USE [" + dbName + "];", "GRANT CONNECT TO [tester];", "GRANT SELECT ON [dbo].[customers] TO [tester];",
			"GRANT INSERT, SELECT ON SCHEMA::[sales] TO [tester];", "DENY DELETE ON [dbo].[orders] TO [tester];",
			"ALTER ROLE [db_datareader] ADD MEMBER [tester];"} {
			if !strings.Contains(all, want) {
				t.Errorf("grants lack %q:\n%s", want, all)
			}
		}
		g, err = c.UserGrants(ctx, "sa")
		if err != nil || !strings.Contains(strings.Join(g, "\n"), "ALTER SERVER ROLE [sysadmin] ADD MEMBER [sa];") {
			t.Errorf("sa grants: %v %v", g, err)
		}
	})

	t.Run("Tunnel", func(t *testing.T) {
		p := testParams(t, "")
		var calls atomic.Int32
		var addr atomic.Value
		p.Dial = func(ctx context.Context, network, a string) (net.Conn, error) {
			calls.Add(1)
			addr.Store(a)
			var d net.Dialer
			return d.DialContext(ctx, network, a)
		}
		tc := openConn(t, p)
		if err := tc.Ping(ctx); err != nil {
			t.Fatal(err)
		}
		want := net.JoinHostPort(p.String("host"), p.String("port"))
		if calls.Load() == 0 || addr.Load() != want {
			t.Errorf("dialed %v (%d calls), want %s unresolved", addr.Load(), calls.Load(), want)
		}
	})
}

func cells(res *driver.Result, i int) map[string]any {
	m := map[string]any{}
	for j, c := range res.Columns {
		m[c.Name] = res.Rows[i][j]
	}
	return m
}

func walkPlan(n *driver.PlanNode, fn func(*driver.PlanNode)) {
	if n == nil {
		return
	}
	fn(n)
	for _, c := range n.Children {
		walkPlan(c, fn)
	}
}

// recSink records execution events; runs groups them like the web client.
type recSink struct{ events []any }

type evCols []driver.ResultColumn
type evRows [][]any
type evNotice [2]string
type evEnd struct{ err error }

func (s *recSink) BeginStatement(st driver.StatementInfo) error {
	s.events = append(s.events, st)
	return nil
}
func (s *recSink) Columns(c []driver.ResultColumn) error {
	s.events = append(s.events, evCols(c))
	return nil
}
func (s *recSink) Rows(r [][]any) error { s.events = append(s.events, evRows(r)); return nil }
func (s *recSink) EndResult(sum driver.ResultSummary) error {
	s.events = append(s.events, sum)
	return nil
}
func (s *recSink) Notice(level, text string) error {
	s.events = append(s.events, evNotice{level, text})
	return nil
}
func (s *recSink) EndStatement(err error) error { s.events = append(s.events, evEnd{err}); return nil }

type resultSet struct {
	cols []driver.ResultColumn
	rows [][]any
	sum  driver.ResultSummary
	done bool
}

type stmtRun struct {
	info    driver.StatementInfo
	sets    []resultSet
	notices []string
	err     error
}

func (s *recSink) runs() []stmtRun {
	var out []stmtRun
	for _, e := range s.events {
		var cur *stmtRun
		if len(out) > 0 {
			cur = &out[len(out)-1]
		}
		switch x := e.(type) {
		case driver.StatementInfo:
			out = append(out, stmtRun{info: x})
		case evCols:
			cur.sets = append(cur.sets, resultSet{cols: x})
		case evRows:
			cur.sets[len(cur.sets)-1].rows = append(cur.sets[len(cur.sets)-1].rows, x...)
		case driver.ResultSummary:
			if n := len(cur.sets); n > 0 && !cur.sets[n-1].done && cur.sets[n-1].cols != nil {
				cur.sets[n-1].sum, cur.sets[n-1].done = x, true
			} else {
				cur.sets = append(cur.sets, resultSet{sum: x, done: true})
			}
		case evNotice:
			cur.notices = append(cur.notices, x[0]+":"+x[1])
		case evEnd:
			cur.err = x.err
		}
	}
	return out
}
