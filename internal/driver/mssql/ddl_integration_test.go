package mssql

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

// TestDDLIntegration runs generated DDL on a real server (see testParams):
// every script goes through a console session as the web UI runs it, and
// the re-described table must round-trip to zero statements.
func TestDDLIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	boot := openConn(t, testParams(t, ""))
	stamp := time.Now().UnixNano() % 1_000_000_000
	dbName := fmt.Sprintf("rowsmith_ddl_%d", stamp)
	extraDB := dbName + "_new"
	if _, err := boot.db.ExecContext(ctx, "CREATE DATABASE "+quote(dbName)); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, db := range []string{dbName, extraDB} {
			_, _ = boot.db.ExecContext(cctx, "IF DB_ID("+literal(db)+") IS NOT NULL ALTER DATABASE "+quote(db)+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
			if _, err := boot.db.ExecContext(cctx, "IF DB_ID("+literal(db)+") IS NOT NULL DROP DATABASE "+quote(db)); err != nil {
				t.Logf("drop %s: %v", db, err)
			}
		}
	})
	c := openConn(t, testParams(t, dbName))
	for _, q := range append(slices.Clone(fixture[:6]),
		`ALTER TABLE dbo.orders ADD CONSTRAINT UQ_orders_token UNIQUE (token)`,
		`CREATE INDEX IX_orders_placed ON dbo.orders (placed, price)`,
		`EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'Free text',
			@level0type = N'SCHEMA', @level0name = N'dbo', @level1type = N'TABLE', @level1name = N'orders', @level2type = N'COLUMN', @level2name = N'note'`,
		`INSERT dbo.customers (name, email) VALUES (N'Ada', 'ada@example.com'), (N'Grace', NULL)`,
		`INSERT dbo.orders (customer_id, total, price, note, placed, flag) VALUES (1, 150.25, 10.5, N'first', '2024-03-01 10:00 +02:00', 1),
			(1, 20, NULL, NULL, NULL, 0), (2, 120, 1, N'second', NULL, 0)`,
	) {
		if _, err := c.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("fixture %q: %v", q, err)
		}
	}
	sess, err := c.NewSession(ctx, driver.Scope{Database: dbName})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// run executes statements as one console script, the way the UI does.
	run := func(t *testing.T, stmts []string) {
		t.Helper()
		sink := &recSink{}
		if err := sess.Execute(ctx, strings.Join(stmts, "\nGO\n"), driver.ExecOptions{StopOnError: true}, sink); err != nil {
			t.Fatal(err)
		}
		runs := sink.runs()
		for _, r := range runs {
			if r.err != nil {
				t.Fatalf("statement %d failed: %v\n%s\n\nscript:\n%s", r.info.Index, r.err, r.info.SQL, strings.Join(stmts, "\nGO\n"))
			}
		}
		if len(runs) != len(stmts) {
			t.Fatalf("ran %d batches for %d statements", len(runs), len(stmts))
		}
	}
	describe := func(t *testing.T, schema, name string) *driver.Table {
		t.Helper()
		tb, err := c.Describe(ctx, driver.ObjectRef{Schema: schema, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return tb
	}
	roundTrip := func(t *testing.T, tb *driver.Table) {
		t.Helper()
		stmts, err := c.AlterTableSQL(tb, defOf(tb))
		if err != nil {
			t.Fatal(err)
		}
		if len(stmts) > 0 {
			t.Fatalf("%s: unchanged definition produced\n%s", tb.Ref.Name, strings.Join(stmts, "\nGO\n"))
		}
	}
	// change describes a table, edits its definition, runs the generated
	// script and returns the table as described afterwards.
	change := func(t *testing.T, schema, name string, edit func(*driver.TableDef)) *driver.Table {
		t.Helper()
		from := describe(t, schema, name)
		to := defOf(from)
		edit(&to)
		stmts, err := c.AlterTableSQL(from, to)
		if err != nil {
			t.Fatal(err)
		}
		if len(stmts) == 0 {
			t.Fatal("no statements generated")
		}
		run(t, stmts)
		after := to.Ref.Name
		if after == "" {
			after = name
		}
		tb := describe(t, schema, after)
		roundTrip(t, tb)
		return tb
	}
	column := func(tb *driver.Table, name string) driver.Column {
		for _, c := range tb.Columns {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("%s has no column %s", tb.Ref.Name, name)
		return driver.Column{}
	}
	index := func(tb *driver.Table, name string) driver.Index {
		for _, ix := range tb.Indexes {
			if ix.Name == name {
				return ix
			}
		}
		t.Fatalf("%s has no index %s", tb.Ref.Name, name)
		return driver.Index{}
	}
	rows := func(t *testing.T, q string) int64 {
		t.Helper()
		var n int64
		if err := c.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("fixture round trips", func(t *testing.T) {
		roundTrip(t, describe(t, "dbo", "customers"))
		roundTrip(t, describe(t, "dbo", "orders"))
	})

	t.Run("create table", func(t *testing.T) {
		stmts, err := c.CreateTableSQL(driver.TableDef{
			Ref: driver.ObjectRef{Schema: "sales", Name: "notes"},
			Columns: []driver.ColumnDef{
				{Column: driver.Column{Name: "id", Type: "bigint", AutoIncrement: true}},
				{Column: driver.Column{Name: "body", Type: "nvarchar(max)", Nullable: true, Collation: "Latin1_General_CS_AS", Comment: "it's free text"}},
				{Column: driver.Column{Name: "qty", Type: "int", Default: strp("1")}},
				{Column: driver.Column{Name: "created", Type: "datetime2(3)", Default: strp("sysutcdatetime()")}},
				{Column: driver.Column{Name: "double_qty", Generated: "[qty]*2", GeneratedStored: true}},
				{Column: driver.Column{Name: "label", Generated: "N'#' + CAST([id] AS nvarchar(20))", Nullable: true}},
				{Column: driver.Column{Name: "customer_id", Type: "int", Nullable: true}},
				{Column: driver.Column{Name: "ver", Type: "rowversion"}},
			},
			PrimaryKey: []string{"id"},
			Indexes: []driver.Index{
				{Name: "IX_notes_created", Columns: []string{"created", "qty"}, Desc: []bool{true}, Where: "[qty] > 0"},
				{Name: "UX_notes_label", Columns: []string{"customer_id", "qty"}, Unique: true, Type: "nonclustered"},
			},
			ForeignKeys: []driver.ForeignKey{{Name: "FK_notes_customer", Columns: []string{"customer_id"}, RefTable: driver.ObjectRef{Schema: "dbo", Name: "customers"},
				RefColumns: []string{"id"}, OnDelete: "SET NULL"}},
			Checks:  []driver.Check{{Name: "CK_notes_qty", Expression: "[qty] >= 0"}},
			Comment: "Team notes",
		})
		if err != nil {
			t.Fatal(err)
		}
		run(t, stmts)
		tb := describe(t, "sales", "notes")
		roundTrip(t, tb)
		if tb.Comment != "Team notes" || column(tb, "body").Comment != "it's free text" || !column(tb, "id").AutoIncrement ||
			column(tb, "body").Collation != "Latin1_General_CS_AS" || !column(tb, "double_qty").GeneratedStored || column(tb, "label").GeneratedStored ||
			len(tb.ForeignKeys) != 1 || tb.ForeignKeys[0].OnDelete != "SET NULL" || index(tb, "IX_notes_created").Where == "" {
			t.Errorf("created table: %+v", tb)
		}
	})

	t.Run("type change with dependents", func(t *testing.T) {
		tb := change(t, "dbo", "orders", func(d *driver.TableDef) { col(d, "total").Type = "decimal(14,2)" })
		if column(tb, "total").Type != "decimal(14,2)" || deref(column(tb, "total").Default) != "0" || column(tb, "tax").Generated == "" ||
			!column(tb, "tax").GeneratedStored || len(tb.Checks) != 1 {
			t.Errorf("after: %+v", tb.Columns)
		}
		if ix := index(tb, "IX_orders_customer"); !strings.Contains(ix.Definition, "INCLUDE ([total])") || ix.Where != "[total]>(0)" {
			t.Errorf("index: %+v", ix)
		}
		if n := rows(t, "SELECT COUNT(*) FROM dbo.orders WHERE tax = total * 0.2"); n != 3 {
			t.Errorf("tax recomputed for %d rows", n)
		}
	})

	t.Run("rename columns with dependents", func(t *testing.T) {
		tb := change(t, "dbo", "orders", func(d *driver.TableDef) {
			col(d, "total").Name = "amount"
			col(d, "note").Name = "memo"
			col(d, "placed").Name = "placed_at"
		})
		if column(tb, "tax").Generated != "[amount]*(0.2)" || tb.Checks[0].Expression != "[amount]>=(0)" ||
			index(tb, "IX_orders_customer").Where != "[amount]>(0)" || column(tb, "memo").Comment != "Free text" ||
			!slices.Equal(index(tb, "IX_orders_placed").Columns, []string{"placed_at", "price"}) {
			t.Errorf("after: %+v %+v %+v", tb.Columns, tb.Checks, tb.Indexes)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		tb := change(t, "dbo", "orders", func(d *driver.TableDef) {
			col(d, "flag").Default = strp("1")
			col(d, "token").Default = nil
			col(d, "price").Default = strp("0.0")
		})
		if deref(column(tb, "flag").Default) != "1" || column(tb, "token").Default != nil || deref(column(tb, "price").Default) != "0.0" {
			t.Errorf("after: %+v", tb.Columns)
		}
	})

	t.Run("type, null and collation changes", func(t *testing.T) {
		tb := change(t, "dbo", "orders", func(d *driver.TableDef) {
			col(d, "flag").Nullable = true
			col(d, "memo").Collation = "Latin1_General_CS_AS"
			col(d, "price").Type = "decimal(19,4)" // in IX_orders_placed, with a default
			col(d, "token").Type = "nvarchar(36)"  // in UQ_orders_token
			col(d, "customer_id").Nullable = true  // foreign key column, in an index
		})
		if !column(tb, "flag").Nullable || column(tb, "memo").Collation != "Latin1_General_CS_AS" || column(tb, "price").Type != "decimal(19,4)" ||
			column(tb, "token").Type != "nvarchar(36)" || !strings.HasPrefix(index(tb, "UQ_orders_token").Definition, "ALTER TABLE") ||
			deref(column(tb, "price").Default) != "0.0" {
			t.Errorf("after: %+v %+v", tb.Columns, tb.Indexes)
		}
	})

	t.Run("indexes, keys and checks", func(t *testing.T) {
		tb := change(t, "dbo", "orders", func(d *driver.TableDef) {
			var ixs []driver.Index
			for _, ix := range d.Indexes {
				switch ix.Name {
				case "IX_orders_customer":
					ix.Columns = []string{"customer_id", "placed_at"}
					ixs = append(ixs, ix)
				case "UQ_orders_token":
				default:
					ixs = append(ixs, ix)
				}
			}
			d.Indexes = append(ixs, driver.Index{Name: "IX_orders_memo", Columns: []string{"price"}, Unique: true, Where: "[price] IS NOT NULL"})
			d.ForeignKeys[0].OnDelete = "NO ACTION"
			d.ForeignKeys[0].OnUpdate = "CASCADE"
			d.Checks = []driver.Check{{Name: "CK_orders_price", Expression: "[price] >= 0"}}
			d.PrimaryKey = []string{"id", "customer_id"}
			col(d, "customer_id").Nullable = false
		})
		if !slices.Equal(tb.PrimaryKey, []string{"id", "customer_id"}) || index(tb, "PK_orders").Type != "clustered" ||
			tb.ForeignKeys[0].OnDelete != "NO ACTION" || tb.ForeignKeys[0].OnUpdate != "CASCADE" ||
			len(tb.Checks) != 1 || tb.Checks[0].Name != "CK_orders_price" ||
			!strings.Contains(index(tb, "IX_orders_customer").Definition, "([customer_id] DESC, [placed_at] ASC) INCLUDE ([amount])") {
			t.Errorf("after: %+v %+v", tb.Indexes, tb.ForeignKeys)
		}
		for _, ix := range tb.Indexes {
			if ix.Name == "UQ_orders_token" {
				t.Error("unique constraint not dropped")
			}
		}
	})

	t.Run("computed columns and comments", func(t *testing.T) {
		tb := change(t, "dbo", "orders", func(d *driver.TableDef) {
			col(d, "tax").Generated = "[amount]*(0.15)"
			col(d, "tax").GeneratedStored = false
			col(d, "tax").Comment = "VAT"
			col(d, "memo").Comment = ""
			col(d, "price").Comment = "Unit price"
			d.Comment = "All orders"
		})
		if column(tb, "tax").GeneratedStored || column(tb, "tax").Generated != "[amount]*(0.15)" || column(tb, "tax").Comment != "VAT" ||
			column(tb, "memo").Comment != "" || column(tb, "price").Comment != "Unit price" || tb.Comment != "All orders" {
			t.Errorf("after: %+v %q", tb.Columns, tb.Comment)
		}
		tb = change(t, "dbo", "orders", func(d *driver.TableDef) { d.Comment = "" })
		if tb.Comment != "" {
			t.Errorf("comment %q", tb.Comment)
		}
	})

	t.Run("drop and add columns", func(t *testing.T) {
		tb := change(t, "dbo", "orders", func(d *driver.TableDef) {
			d.Columns = slices.DeleteFunc(d.Columns, func(c driver.ColumnDef) bool {
				return c.Name == "flag" || c.Name == "location" || c.Name == "price" // price is in two indexes
			})
			d.Columns = append(d.Columns,
				driver.ColumnDef{Column: driver.Column{Name: "flag", Type: "tinyint", Default: strp("7"), Comment: "new flag"}},
				driver.ColumnDef{Column: driver.Column{Name: "status", Type: "varchar(20)", Collation: "Latin1_General_CS_AS", Default: strp("'new'")}},
				driver.ColumnDef{Column: driver.Column{Name: "flag2", Generated: "[flag]*2"}},
			)
		})
		if column(tb, "flag").Type != "tinyint" || column(tb, "flag").Comment != "new flag" || column(tb, "flag2").Generated != "[flag]*(2)" ||
			deref(column(tb, "status").Default) != "'new'" {
			t.Errorf("after: %+v", tb.Columns)
		}
		if n := rows(t, "SELECT COUNT(*) FROM dbo.orders WHERE flag = 7 AND status = 'new' AND flag2 = 14"); n != 3 {
			t.Errorf("new columns filled for %d rows", n)
		}
		for _, ix := range tb.Indexes {
			if ix.Name == "IX_orders_placed" || ix.Name == "IX_orders_memo" {
				t.Errorf("index %s on a dropped column survived", ix.Name)
			}
		}
	})

	t.Run("swap names and rename table", func(t *testing.T) {
		tb := change(t, "dbo", "orders", func(d *driver.TableDef) {
			col(d, "memo").Name = "status"
			col(d, "status").Name = "memo"
			d.Ref.Name = "purchases"
		})
		if column(tb, "memo").Type != "varchar(20)" || column(tb, "status").Type != "nvarchar(max)" {
			t.Errorf("after: %+v", tb.Columns)
		}
	})

	t.Run("other shapes", func(t *testing.T) {
		for _, q := range []string{
			`CREATE TABLE [sales].[odd]] name] (
				[we]]ird] int NOT NULL,
				[it's] nvarchar(20) NULL DEFAULT (N'it''s'),
				[sp ace] int NOT NULL CHECK ([sp ace] > 0),
				code char(3) NOT NULL,
				CONSTRAINT [PK odd] PRIMARY KEY NONCLUSTERED ([we]]ird] DESC, code))`,
			`CREATE CLUSTERED INDEX CX_odd ON [sales].[odd]] name] (code, [sp ace] DESC)`,
			`INSERT [sales].[odd]] name] ([we]]ird], [sp ace], code) VALUES (1, 5, 'abc'), (2, 6, 'def')`,
			`CREATE TABLE dbo.xmldocs (id int NOT NULL PRIMARY KEY, doc xml NULL)`,
			`CREATE PRIMARY XML INDEX PXI_xmldocs ON dbo.xmldocs (doc)`,
			`CREATE TABLE dbo.shapes (id int NOT NULL PRIMARY KEY, g geometry NULL)`,
			`CREATE SPATIAL INDEX SX_shapes ON dbo.shapes (g) WITH (BOUNDING_BOX = (0, 0, 100, 100))`,
			`CREATE TABLE dbo.facts (a int NOT NULL, b int NULL, c nvarchar(10) NULL)`,
			`CREATE CLUSTERED COLUMNSTORE INDEX CCI_facts ON dbo.facts`,
			`CREATE TABLE dbo.facts2 (a int NOT NULL, b int NULL)`,
			`CREATE NONCLUSTERED COLUMNSTORE INDEX NCCI_facts2 ON dbo.facts2 (a, b)`,
			`CREATE TYPE dbo.email_address FROM nvarchar(320) NOT NULL`,
			`CREATE TABLE dbo.contacts (id int NOT NULL PRIMARY KEY, email dbo.email_address, note nvarchar(10) NULL, alt int NULL)`,
		} {
			if _, err := c.db.ExecContext(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		for _, n := range [][2]string{{"sales", "odd] name"}, {"dbo", "xmldocs"}, {"dbo", "facts"}, {"dbo", "facts2"}, {"dbo", "contacts"}, {"dbo", "shapes"}} {
			roundTrip(t, describe(t, n[0], n[1]))
		}
		tb := change(t, "sales", "odd] name", func(d *driver.TableDef) {
			col(d, "we]ird").Name = "we]ird2'"
			col(d, "it's").Default = strp("N'x'")
			col(d, "sp ace").Type = "bigint" // system-named check, clustered index
			col(d, "code").Type = "char(5)"  // nonclustered, descending primary key
		})
		pk := index(tb, "PK odd")
		if !slices.Equal(tb.PrimaryKey, []string{"we]ird2'", "code"}) || pk.Type != "nonclustered" || !slices.Equal(pk.Desc, []bool{true, false}) ||
			index(tb, "CX_odd").Type != "clustered" || len(tb.Checks) != 1 || deref(column(tb, "it's").Default) != "N'x'" {
			t.Errorf("after: %+v %+v %+v", tb.Columns, tb.Indexes, tb.Checks)
		}
		change(t, "dbo", "contacts", func(d *driver.TableDef) {
			col(d, "email").Nullable = true
			col(d, "note").Type = "nvarchar(20)"
		})
		tb = change(t, "dbo", "facts2", func(d *driver.TableDef) { col(d, "b").Type = "bigint" })
		if ix := index(tb, "NCCI_facts2"); ix.Type != "nonclustered columnstore" || !slices.Equal(ix.Columns, []string{"a", "b"}) {
			t.Errorf("after: %+v", tb.Indexes)
		}
		tb = change(t, "dbo", "facts", func(d *driver.TableDef) {
			col(d, "c").Type = "nvarchar(20)"
			d.Columns = append(d.Columns, driver.ColumnDef{Column: driver.Column{Name: "d", Type: "date", Nullable: true}})
		})
		if ix := index(tb, "CCI_facts"); ix.Type != "clustered columnstore" || len(ix.Columns) != 4 {
			t.Errorf("after: %+v", tb.Indexes)
		}
		// Indexes that cannot be scripted block changes that need them re-created.
		xd := describe(t, "dbo", "xmldocs")
		to := defOf(xd)
		col(&to, "doc").Nullable = false
		if _, err := c.AlterTableSQL(xd, to); err == nil || !strings.Contains(err.Error(), "xml indexes cannot be created here") {
			t.Errorf("err = %v", err)
		}
		if ix := index(xd, "PXI_xmldocs"); !slices.Equal(ix.Columns, []string{"doc"}) {
			t.Errorf("xml index: %+v", ix)
		}
		// Unrelated changes leave them alone.
		tb = change(t, "dbo", "shapes", func(d *driver.TableDef) {
			d.Columns = append(d.Columns, driver.ColumnDef{Column: driver.Column{Name: "label", Type: "nvarchar(20)", Nullable: true}})
		})
		if ix := index(tb, "SX_shapes"); ix.Type != "spatial" || !slices.Equal(ix.Columns, []string{"g"}) {
			t.Errorf("spatial index: %+v", ix)
		}
	})

	t.Run("refused changes", func(t *testing.T) {
		cust := describe(t, "dbo", "customers")
		to := defOf(cust)
		col(&to, "id").Type = "bigint"
		if _, err := c.AlterTableSQL(cust, to); err == nil || !strings.Contains(err.Error(), "FK_orders_customers") {
			t.Errorf("err = %v", err)
		}
		to = defOf(cust)
		col(&to, "name").AutoIncrement = true
		if _, err := c.AlterTableSQL(cust, to); err == nil || !strings.Contains(err.Error(), "IDENTITY") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("objects, schemas and databases", func(t *testing.T) {
		gen := func(stmts []string, err error) []string {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
			return stmts
		}
		run(t, gen(c.CreateSchemaSQL(dbName, "archive")))
		run(t, gen(c.RenameObjectSQL(driver.ObjectRef{Schema: "sales", Name: "notes", Kind: "table"}, "memos")))
		run(t, gen(c.TruncateSQL(driver.ObjectRef{Schema: "sales", Name: "memos", Kind: "table"})))
		run(t, gen(c.DropObjectSQL(driver.ObjectRef{Schema: "sales", Name: "memos", Kind: "table"}, false)))
		if _, err := c.db.ExecContext(ctx, "CREATE SEQUENCE archive.seq"); err != nil {
			t.Fatal(err)
		}
		run(t, gen(c.RenameObjectSQL(driver.ObjectRef{Schema: "archive", Name: "seq", Kind: "sequence"}, "seq2")))
		run(t, gen(c.DropObjectSQL(driver.ObjectRef{Schema: "archive", Name: "seq2", Kind: "sequence"}, false)))
		run(t, gen(c.DropSchemaSQL(dbName, "archive", false)))
		if n := rows(t, "SELECT COUNT(*) FROM sys.schemas WHERE name = 'archive'") + rows(t, "SELECT COUNT(*) FROM sys.objects WHERE name IN ('notes', 'memos')"); n != 0 {
			t.Errorf("%d objects left", n)
		}
		run(t, gen(c.CreateDatabaseSQL(extraDB, map[string]string{"collation": "Latin1_General_100_CI_AS_SC_UTF8"})))
		var coll string
		if err := c.db.QueryRowContext(ctx, "SELECT collation_name FROM sys.databases WHERE name = @p1", extraDB).Scan(&coll); err != nil ||
			coll != "Latin1_General_100_CI_AS_SC_UTF8" {
			t.Errorf("collation %q, %v", coll, err)
		}
		run(t, gen(c.DropDatabaseSQL(extraDB)))
		if n := rows(t, "SELECT COUNT(*) FROM sys.databases WHERE name = '"+extraDB+"'"); n != 0 {
			t.Error("database not dropped")
		}
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
