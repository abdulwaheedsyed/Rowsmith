package oracle

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
)

// TestIntegrationDDL runs generated DDL through a console session on a real
// server (see oracle_integration_test.go for the environment), reads the
// result back with Describe and checks that the described table, submitted
// unchanged, needs no statements.

var ddlDrops = []string{
	"DROP TABLE RSD_ORDERS CASCADE CONSTRAINTS PURGE", "DROP TABLE RSD_CUSTOMERS CASCADE CONSTRAINTS PURGE",
	"DROP TABLE RSD_CLIENTS CASCADE CONSTRAINTS PURGE", "DROP TABLE RSD_LEGACY CASCADE CONSTRAINTS PURGE",
	"DROP TABLE RSD_LEGACY2 CASCADE CONSTRAINTS PURGE", "DROP VIEW RSD_V", "DROP VIEW RSD_V2", "DROP MATERIALIZED VIEW RSD_MV",
	"DROP SEQUENCE RSD_SEQ", "DROP SEQUENCE RSD_SEQ2", "DROP SYNONYM RSD_SYN", "DROP SYNONYM RSD_SYN2", "DROP PROCEDURE RSD_PROC",
	"DROP FUNCTION RSD_FN", "DROP PACKAGE RSD_PKG", "DROP TYPE RSD_T FORCE",
}

// ddlScript joins statements the way the server hands them to the console:
// plain SQL ends with ";", PL/SQL with a "/" line.
func ddlScript(stmts []string) string {
	var b strings.Builder
	for _, s := range stmts {
		if isPLSQL(s) {
			b.WriteString(s + "\n/\n")
		} else {
			b.WriteString(s + ";\n")
		}
	}
	return b.String()
}

func execDDL(t *testing.T, sess driver.Session, stmts []string) {
	t.Helper()
	if len(stmts) == 0 {
		t.Fatal("no statements generated")
	}
	out := run(t, sess, ddlScript(stmts), driver.ExecOptions{StopOnError: true})
	for _, st := range out.stmts {
		if st.err != nil {
			t.Fatalf("statement %q: %v\nscript:\n%s", st.info.SQL, st.err, ddlScript(stmts))
		}
	}
	if len(out.stmts) != len(stmts) {
		t.Fatalf("ran %d statements, generated %d:\n%s", len(out.stmts), len(stmts), ddlScript(stmts))
	}
}

func describeTable(t *testing.T, c *conn, name string) *driver.Table {
	t.Helper()
	tbl, err := c.Describe(context.Background(), driver.ObjectRef{Name: name, Kind: "table"})
	if err != nil {
		t.Fatalf("describe %s: %v", name, err)
	}
	return tbl
}

// assertUnchanged describes a table and submits it as is.
func assertUnchanged(t *testing.T, c *conn, name string) *driver.Table {
	t.Helper()
	tbl := describeTable(t, c, name)
	stmts, err := c.AlterTableSQL(tbl, tableDef(tbl))
	if err != nil {
		t.Fatalf("%s round trip: %v", name, err)
	}
	if len(stmts) > 0 {
		t.Errorf("%s: unchanged definition produced:\n%s", name, strings.Join(stmts, "\n"))
	}
	return tbl
}

func indexNamed(tbl *driver.Table, name string) *driver.Index {
	for i := range tbl.Indexes {
		if tbl.Indexes[i].Name == name {
			return &tbl.Indexes[i]
		}
	}
	return nil
}

func objectExists(t *testing.T, c *conn, typ, name string) bool {
	t.Helper()
	var n int
	if err := c.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM USER_OBJECTS WHERE OBJECT_TYPE = :1 AND OBJECT_NAME = :2`, typ, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestIntegrationDDL(t *testing.T) {
	c := mustOpen(t, testParams(t, appUser(), os.Getenv("ROWSMITH_TEST_ORACLE_PASSWORD")))
	ctx := context.Background()
	cleanup := func() {
		for _, q := range ddlDrops {
			_, _ = c.db.ExecContext(ctx, q)
		}
		// Generated DROP TABLE keeps tables in the recycle bin.
		bin, _ := sqlbase.Strings(ctx, c.db, `SELECT OBJECT_NAME FROM USER_RECYCLEBIN WHERE TYPE = 'TABLE' AND ORIGINAL_NAME LIKE 'RSD\_%' ESCAPE '\'`)
		for _, name := range bin {
			_, _ = c.db.ExecContext(ctx, "PURGE TABLE "+quote(name))
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	sess, err := c.NewSession(ctx, driver.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	user := strings.ToUpper(appUser())

	t.Run("Create", func(t *testing.T) {
		customers := driver.TableDef{
			Ref: driver.ObjectRef{Name: "RSD_CUSTOMERS"},
			Columns: []driver.ColumnDef{
				{Column: driver.Column{Name: "ID", Type: "NUMBER(10)", AutoIncrement: true}},
				{Column: driver.Column{Name: "NAME", Type: "VARCHAR2(100 CHAR)", Comment: "Full name"}},
				{Column: driver.Column{Name: "EMAIL", Type: "VARCHAR2(200 BYTE)", Nullable: true}},
				{Column: driver.Column{Name: "CREATED", Type: "DATE", Default: strp("SYSDATE")}},
				{Column: driver.Column{Name: "BALANCE", Type: "NUMBER(12,2)", Nullable: true, Default: strp("0")}},
				{Column: driver.Column{Name: "STATUS", Type: "VARCHAR2(10 BYTE)", Default: strp("ON NULL 'new'")}},
				{Column: driver.Column{Name: "NOTES", Type: "CLOB", Nullable: true}},
				{Column: driver.Column{Name: "NAME_UPPER", Generated: `UPPER("NAME")`, Nullable: true}},
			},
			PrimaryKey: []string{"ID"},
			Checks:     []driver.Check{{Name: "RSD_CUSTOMERS_BALANCE_CK", Expression: "BALANCE >= 0"}, {Expression: "LENGTH(NAME) > 1"}},
			Indexes: []driver.Index{
				{Name: "RSD_CUSTOMERS_EMAIL_UX", Columns: []string{"EMAIL"}, Unique: true},
				{Name: "RSD_CUSTOMERS_LOWER_IX", Columns: []string{`(LOWER("EMAIL"))`}},
				{Name: "RSD_CUSTOMERS_CREATED_IX", Columns: []string{"CREATED", "BALANCE"}, Desc: []bool{true, false}},
				{Name: "RSD_CUSTOMERS_REV_IX", Columns: []string{"BALANCE"}, Type: "btree (reverse)"},
				{Name: "RSD_CUSTOMERS_STATUS_BX", Columns: []string{"STATUS"}, Type: "bitmap"},
				{Name: "RSD_CUSTOMERS_NU_IX", Columns: []string{"NAME_UPPER"}}, // rebuilt when its expression changes
			},
			Comment: "Customers",
		}
		stmts, err := c.CreateTableSQL(customers)
		if err != nil {
			t.Fatal(err)
		}
		execDDL(t, sess, stmts)
		orders := driver.TableDef{
			Ref: driver.ObjectRef{Schema: user, Name: "RSD_ORDERS"},
			Columns: []driver.ColumnDef{
				{Column: driver.Column{Name: "ID", Type: "NUMBER(10)"}},
				{Column: driver.Column{Name: "CUSTOMER_ID", Type: "NUMBER(10)"}},
				{Column: driver.Column{Name: "PLACED_AT", Type: "TIMESTAMP", Default: strp("SYSTIMESTAMP")}},
			},
			PrimaryKey: []string{"ID"},
			ForeignKeys: []driver.ForeignKey{{Name: "RSD_ORDERS_CUSTOMER_FK", Columns: []string{"CUSTOMER_ID"},
				RefTable: driver.ObjectRef{Name: "RSD_CUSTOMERS"}, RefColumns: []string{"ID"}, OnDelete: "CASCADE"}},
		}
		if stmts, err = c.CreateTableSQL(orders); err != nil {
			t.Fatal(err)
		}
		execDDL(t, sess, stmts)

		tbl := assertUnchanged(t, c, "RSD_CUSTOMERS")
		assertUnchanged(t, c, "RSD_ORDERS")
		id, status, virt := column(t, tbl, "ID"), column(t, tbl, "STATUS"), column(t, tbl, "NAME_UPPER")
		if !id.AutoIncrement || id.Default == nil || *id.Default != autoIdentity || id.Nullable {
			t.Errorf("identity %+v", id)
		}
		if status.Default == nil || *status.Default != "ON NULL 'new'" || status.Nullable {
			t.Errorf("DEFAULT ON NULL %+v", status)
		}
		if virt.Generated != `UPPER("NAME")` || column(t, tbl, "NAME").Comment != "Full name" || tbl.Comment != "Customers" {
			t.Errorf("virtual %+v, comments %q", virt, tbl.Comment)
		}
		if len(tbl.PrimaryKey) != 1 || tbl.Indexes[0].Name != "RSD_CUSTOMERS_PK" || !tbl.Indexes[0].Primary || len(tbl.Checks) != 2 {
			t.Errorf("keys %v %+v checks %+v", tbl.PrimaryKey, tbl.Indexes[0], tbl.Checks)
		}
		for name, typ := range map[string]string{"RSD_CUSTOMERS_REV_IX": "btree (reverse)", "RSD_CUSTOMERS_STATUS_BX": "bitmap", "RSD_CUSTOMERS_LOWER_IX": "btree"} {
			if ix := indexNamed(tbl, name); ix == nil || ix.Type != typ {
				t.Errorf("index %s: %+v", name, ix)
			}
		}
		if ix := indexNamed(tbl, "RSD_CUSTOMERS_CREATED_IX"); ix == nil || !slices.Equal(ix.Columns, []string{"CREATED", "BALANCE"}) || !ix.Desc[0] {
			t.Errorf("descending index %+v", ix)
		}
	})

	// A table written by hand, with what Oracle adds on its own: system
	// names, NOT NULL constraints, a removed default and padded defaults.
	t.Run("HandWritten", func(t *testing.T) {
		for _, q := range []string{
			"CREATE TABLE RSD_LEGACY (\n  ID NUMBER GENERATED ALWAYS AS IDENTITY,\n  CODE VARCHAR2(20) CONSTRAINT RSD_LEGACY_CODE_NN NOT NULL,\n" +
				"  QTY NUMBER DEFAULT 1   \n  ,\n  NOTE VARCHAR2(30) DEFAULT 'x'\n\n  ,\n  AMOUNT NUMBER(10,2) CHECK (AMOUNT > 0),\n" +
				"  LABEL VARCHAR2(10) CONSTRAINT RSD_LEGACY_LABEL_NN NOT NULL,\n  PRIMARY KEY (ID),\n  UNIQUE (CODE, QTY),\n" +
				"  CONSTRAINT RSD_LEGACY_NOTE_UK UNIQUE (NOTE))",
			"ALTER TABLE RSD_LEGACY MODIFY (NOTE DEFAULT NULL)",
			"CREATE INDEX RSD_LEGACY_IX ON RSD_LEGACY (UPPER(NOTE) DESC, QTY)",
			"COMMENT ON COLUMN RSD_LEGACY.QTY IS 'Quantity'",
		} {
			if _, err := c.db.ExecContext(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		tbl := assertUnchanged(t, c, "RSD_LEGACY")
		if qty, note := column(t, tbl, "QTY"), column(t, tbl, "NOTE"); qty.Default == nil || *qty.Default != "1" || note.Default != nil {
			t.Errorf("defaults %v %v", qty.Default, note.Default)
		}
		if len(tbl.Checks) != 1 || !strings.HasPrefix(tbl.Checks[0].Name, "SYS_C") || tbl.Checks[0].Expression != "AMOUNT > 0" {
			t.Errorf("checks %+v", tbl.Checks)
		}
		var unique *driver.Index
		for i, ix := range tbl.Indexes {
			if ix.Unique && !ix.Primary && ix.Name != "RSD_LEGACY_NOTE_UK" {
				unique = &tbl.Indexes[i]
			}
		}
		if unique == nil || !strings.HasPrefix(unique.Name, "SYS_C") {
			t.Fatalf("unique constraint index %+v", tbl.Indexes)
		}
		if _, ok := constraintOf(*unique); !ok {
			t.Errorf("unique constraint not recorded: %q", unique.Definition)
		}

		// Change the system-named keys and drop the identity.
		def := tableDef(tbl)
		for i := range def.Indexes {
			switch def.Indexes[i].Name {
			case unique.Name:
				def.Indexes[i].Columns = []string{"CODE"}
			case "RSD_LEGACY_NOTE_UK":
				def.Indexes[i].Columns = []string{"NOTE", "AMOUNT"}
			}
		}
		def.PrimaryKey = []string{"ID", "CODE"}
		defColumn(&def, "ID").AutoIncrement = false
		defColumn(&def, "LABEL").Nullable = true // drops the named NOT NULL constraint
		defColumn(&def, "NOTE").Default = strp("'y'")
		def.Checks = nil
		stmts, err := c.AlterTableSQL(tbl, def)
		if err != nil {
			t.Fatal(err)
		}
		execDDL(t, sess, stmts)
		tbl = assertUnchanged(t, c, "RSD_LEGACY")
		if !slices.Equal(tbl.PrimaryKey, []string{"ID", "CODE"}) || indexNamed(tbl, "RSD_LEGACY_PK") == nil || len(tbl.Checks) != 0 {
			t.Errorf("keys %v %+v %+v", tbl.PrimaryKey, tbl.Indexes, tbl.Checks)
		}
		if id := column(t, tbl, "ID"); id.AutoIncrement || id.Default != nil {
			t.Errorf("identity kept: %+v", id)
		}
		if l, n := column(t, tbl, "LABEL"), column(t, tbl, "NOTE"); !l.Nullable || n.Default == nil || *n.Default != "'y'" {
			t.Errorf("LABEL %+v NOTE %+v", l, n)
		}
		found := false
		for _, ix := range tbl.Indexes {
			if cn, ok := constraintOf(ix); ok && !ix.Primary && slices.Equal(ix.Columns, []string{"CODE"}) && strings.HasPrefix(cn, "SYS_C") {
				found = true
			}
		}
		if !found {
			t.Errorf("unique constraint not re-created: %+v", tbl.Indexes)
		}
		if ix := indexNamed(tbl, "RSD_LEGACY_NOTE_UK"); ix == nil || !slices.Equal(ix.Columns, []string{"NOTE", "AMOUNT"}) {
			t.Errorf("named unique constraint %+v", ix)
		} else if cn, ok := constraintOf(*ix); !ok || cn != "RSD_LEGACY_NOTE_UK" {
			t.Errorf("named unique constraint became %q", ix.Definition)
		}
	})

	t.Run("Alter", func(t *testing.T) {
		tbl := describeTable(t, c, "RSD_CUSTOMERS")
		def := tableDef(tbl)
		defColumn(&def, "NAME").Name = "FULL_NAME"
		email := defColumn(&def, "EMAIL")
		email.Type, email.Comment = "VARCHAR2(320 BYTE)", "Login"
		created := defColumn(&def, "CREATED")
		created.Default, created.Nullable = nil, true
		defColumn(&def, "BALANCE").Default = strp("100")
		defColumn(&def, "STATUS").Default = strp("'open'")
		defColumn(&def, "NAME_UPPER").Generated = `LOWER("FULL_NAME")`
		defColumn(&def, "ID").Default = strp("GENERATED ALWAYS AS IDENTITY")
		var cols []driver.ColumnDef
		for _, col := range def.Columns {
			if col.OriginalName != "NOTES" {
				cols = append(cols, col)
			}
		}
		def.Columns = append(cols, driver.ColumnDef{Column: driver.Column{Name: "TAGS", Type: "VARCHAR2(200 CHAR)", Nullable: true,
			Default: strp("'none'"), Comment: "Labels"}})
		var checks []driver.Check
		for _, ch := range def.Checks {
			if ch.Name == "RSD_CUSTOMERS_BALANCE_CK" {
				checks = append(checks, driver.Check{Name: ch.Name, Expression: "BALANCE >= -100"})
			}
		}
		def.Checks = append(checks, driver.Check{Name: "RSD_CUSTOMERS_TAGS_CK", Expression: "TAGS <> 'bad'"})
		var ixs []driver.Index
		for _, ix := range def.Indexes {
			switch ix.Name {
			case "RSD_CUSTOMERS_REV_IX":
				continue
			case "RSD_CUSTOMERS_EMAIL_UX":
				ix.Columns = []string{"EMAIL", "FULL_NAME"}
			case "RSD_CUSTOMERS_CREATED_IX":
				ix.Desc = []bool{false, true}
			}
			ixs = append(ixs, ix)
		}
		def.Indexes = append(ixs, driver.Index{Name: "RSD_CUSTOMERS_TAGS_IX", Columns: []string{"TAGS"}})
		def.Comment = "Clients"
		def.Ref.Name = "RSD_CLIENTS"
		stmts, err := c.AlterTableSQL(tbl, def)
		if err != nil {
			t.Fatal(err)
		}
		execDDL(t, sess, stmts)

		tbl = assertUnchanged(t, c, "RSD_CLIENTS")
		if tbl.Comment != "Clients" || len(tbl.Columns) != 8 || tbl.Columns[7].Name != "TAGS" || column(t, tbl, "TAGS").Comment != "Labels" {
			t.Errorf("table %q columns %+v", tbl.Comment, tbl.Columns)
		}
		if e := column(t, tbl, "EMAIL"); e.Type != "VARCHAR2(320 BYTE)" || e.Comment != "Login" {
			t.Errorf("EMAIL %+v", e)
		}
		if cr := column(t, tbl, "CREATED"); cr.Default != nil || !cr.Nullable {
			t.Errorf("CREATED %+v", cr)
		}
		if st := column(t, tbl, "STATUS"); st.Default == nil || *st.Default != "'open'" || st.Nullable {
			t.Errorf("STATUS %+v", st)
		}
		if id := column(t, tbl, "ID"); id.Default == nil || *id.Default != "GENERATED ALWAYS AS IDENTITY" {
			t.Errorf("ID %+v", id)
		}
		if v := column(t, tbl, "NAME_UPPER"); v.Generated != `LOWER("FULL_NAME")` {
			t.Errorf("NAME_UPPER %+v", v)
		}
		if ix := indexNamed(tbl, "RSD_CUSTOMERS_EMAIL_UX"); ix == nil || !slices.Equal(ix.Columns, []string{"EMAIL", "FULL_NAME"}) || !ix.Unique {
			t.Errorf("unique index %+v", ix)
		}
		if indexNamed(tbl, "RSD_CUSTOMERS_REV_IX") != nil || indexNamed(tbl, "RSD_CUSTOMERS_TAGS_IX") == nil || indexNamed(tbl, "RSD_CUSTOMERS_NU_IX") == nil {
			t.Errorf("indexes %+v", tbl.Indexes)
		}
		if len(tbl.Checks) != 2 || len(tbl.Referenced) != 1 {
			t.Errorf("checks %+v referenced %+v", tbl.Checks, tbl.Referenced)
		}

		// The referencing table: a nullable column for ON DELETE SET NULL.
		orders := describeTable(t, c, "RSD_ORDERS")
		def = tableDef(orders)
		defColumn(&def, "CUSTOMER_ID").Nullable = true
		def.ForeignKeys[0].OnDelete = "SET NULL"
		if stmts, err = c.AlterTableSQL(orders, def); err != nil {
			t.Fatal(err)
		}
		execDDL(t, sess, stmts)
		orders = assertUnchanged(t, c, "RSD_ORDERS")
		if fk := orders.ForeignKeys[0]; fk.OnDelete != "SET NULL" || fk.RefTable.Name != "RSD_CLIENTS" {
			t.Errorf("foreign key %+v", fk)
		}
	})

	t.Run("Refused", func(t *testing.T) {
		tbl := describeTable(t, c, "RSD_CLIENTS")
		def := tableDef(tbl)
		def.PrimaryKey = []string{"ID", "EMAIL"}
		if _, err := c.AlterTableSQL(tbl, def); err == nil || !strings.Contains(err.Error(), "RSD_ORDERS.RSD_ORDERS_CUSTOMER_FK") {
			t.Errorf("referenced primary key: %v", err)
		}
		def = tableDef(tbl)
		defColumn(&def, "BALANCE").AutoIncrement = true
		if _, err := c.AlterTableSQL(tbl, def); err == nil || !strings.Contains(err.Error(), "identity") {
			t.Errorf("identity on existing column: %v", err)
		}
		orders := describeTable(t, c, "RSD_ORDERS")
		def = tableDef(orders)
		def.ForeignKeys[0].OnUpdate = "CASCADE"
		if _, err := c.AlterTableSQL(orders, def); err == nil || !strings.Contains(err.Error(), "ON UPDATE") {
			t.Errorf("ON UPDATE: %v", err)
		}
	})

	t.Run("Objects", func(t *testing.T) {
		for _, q := range []string{
			"CREATE VIEW RSD_V AS SELECT ID, FULL_NAME FROM RSD_CLIENTS",
			"CREATE SEQUENCE RSD_SEQ",
			"CREATE SYNONYM RSD_SYN FOR RSD_CLIENTS",
			"CREATE MATERIALIZED VIEW RSD_MV AS SELECT COUNT(*) AS N FROM RSD_ORDERS",
			"CREATE OR REPLACE PROCEDURE RSD_PROC IS BEGIN NULL; END;",
			"CREATE OR REPLACE FUNCTION RSD_FN RETURN NUMBER IS BEGIN RETURN 1; END;",
			"CREATE OR REPLACE PACKAGE RSD_PKG AS FUNCTION one RETURN NUMBER; END RSD_PKG;",
			"CREATE OR REPLACE PACKAGE BODY RSD_PKG AS FUNCTION one RETURN NUMBER IS BEGIN RETURN 1; END; END RSD_PKG;",
			"CREATE OR REPLACE TYPE RSD_T AS OBJECT (X NUMBER)",
			"CREATE OR REPLACE TRIGGER RSD_TRG BEFORE INSERT ON RSD_LEGACY FOR EACH ROW BEGIN NULL; END;",
			"INSERT INTO RSD_LEGACY (ID, CODE, QTY) VALUES (1, 'a', 1)",
		} {
			if _, err := c.db.ExecContext(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		ref := func(kind, name string) driver.ObjectRef {
			return driver.ObjectRef{Schema: user, Name: name, Kind: kind}
		}
		var stmts []string
		add := func(s []string, err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
			stmts = append(stmts, s...)
		}
		add(c.TruncateSQL(ref("table", "RSD_LEGACY")))
		add(c.RenameObjectSQL(ref("table", "RSD_LEGACY"), "RSD_LEGACY2"))
		add(c.RenameObjectSQL(ref("view", "RSD_V"), "RSD_V2"))
		add(c.RenameObjectSQL(ref("sequence", "RSD_SEQ"), "RSD_SEQ2"))
		add(c.RenameObjectSQL(ref("synonym", "RSD_SYN"), "RSD_SYN2"))
		add(c.RenameObjectSQL(ref("index", "RSD_CUSTOMERS_TAGS_IX"), "RSD_CLIENTS_TAGS_IX"))
		add(c.RenameObjectSQL(ref("trigger", "RSD_TRG"), "RSD_TRG2"))
		execDDL(t, sess, stmts)
		if n, err := c.Count(ctx, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "RSD_LEGACY2"}}); err != nil || n.Rows != 0 {
			t.Errorf("truncate left %+v %v", n, err)
		}
		for typ, name := range map[string]string{"TABLE": "RSD_LEGACY2", "VIEW": "RSD_V2", "SEQUENCE": "RSD_SEQ2", "SYNONYM": "RSD_SYN2",
			"INDEX": "RSD_CLIENTS_TAGS_IX", "TRIGGER": "RSD_TRG2"} {
			if !objectExists(t, c, typ, name) {
				t.Errorf("%s %s missing after rename", typ, name)
			}
		}

		stmts = nil
		add(c.DropObjectSQL(ref("trigger", "RSD_TRG2"), false))
		add(c.DropObjectSQL(ref("index", "RSD_CLIENTS_TAGS_IX"), false))
		add(c.DropObjectSQL(ref("view", "RSD_V2"), true))
		add(c.DropObjectSQL(ref("materialized_view", "RSD_MV"), false))
		add(c.DropObjectSQL(ref("sequence", "RSD_SEQ2"), false))
		add(c.DropObjectSQL(ref("synonym", "RSD_SYN2"), false))
		add(c.DropObjectSQL(ref("procedure", "RSD_PROC"), false))
		add(c.DropObjectSQL(ref("function", "RSD_FN"), false))
		add(c.DropObjectSQL(ref("package_body", "RSD_PKG"), false))
		add(c.DropObjectSQL(ref("package", "RSD_PKG"), false))
		add(c.DropObjectSQL(ref("type", "RSD_T"), true))
		add(c.DropObjectSQL(ref("table", "RSD_CLIENTS"), true)) // referenced by RSD_ORDERS
		add(c.DropObjectSQL(ref("table", "RSD_ORDERS"), false))
		add(c.DropObjectSQL(ref("table", "RSD_LEGACY2"), false))
		execDDL(t, sess, stmts)
		var left []string
		rows, err := c.db.QueryContext(ctx, `SELECT OBJECT_TYPE || ' ' || OBJECT_NAME FROM USER_OBJECTS WHERE OBJECT_NAME LIKE 'RSD\_%' ESCAPE '\'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if rows.Scan(&s) == nil {
				left = append(left, s)
			}
		}
		if len(left) > 0 {
			t.Errorf("objects left after dropping: %v", left)
		}
	})
}

// ddlFixtureRoundTrip runs on the fixture of TestIntegration: every table
// submitted unchanged needs no statements, and CREATE TABLE generated from
// its description builds a table that describes the same.
func ddlFixtureRoundTrip(t *testing.T, c *conn) {
	ctx := context.Background()
	sess, err := c.NewSession(ctx, driver.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	drop := func() { _, _ = c.db.ExecContext(ctx, "DROP TABLE RS_COPY CASCADE CONSTRAINTS PURGE") }
	drop()
	defer drop()
	names := []string{"RS_CUSTOMERS", "RS_ORDERS", "RS_LOG", "RS_SHAPES"}
	if objectExists(t, c, "TABLE", "RS_MODERN") {
		names = append(names, "RS_MODERN")
	}
	for _, name := range names {
		tbl := assertUnchanged(t, c, name)

		def := tableDef(tbl)
		def.Ref.Name = "RS_COPY"
		var ixs []driver.Index
		for i, ix := range def.Indexes {
			if !ix.Primary {
				ix.Name = "RS_COPY_IX" + strconv.Itoa(i)
				ixs = append(ixs, ix)
			}
		}
		def.Indexes = ixs
		for i := range def.Checks {
			def.Checks[i].Name = "RS_COPY_CK" + strconv.Itoa(i)
		}
		for i := range def.ForeignKeys {
			def.ForeignKeys[i].Name = "RS_COPY_FK" + strconv.Itoa(i)
		}
		stmts, err := c.CreateTableSQL(def)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		execDDL(t, sess, stmts)
		cp := assertUnchanged(t, c, "RS_COPY")
		if len(cp.Columns) != len(tbl.Columns) || !slices.Equal(cp.PrimaryKey, tbl.PrimaryKey) || len(cp.Indexes) != len(tbl.Indexes) ||
			len(cp.Checks) != len(tbl.Checks) || len(cp.ForeignKeys) != len(tbl.ForeignKeys) || cp.Comment != tbl.Comment {
			t.Errorf("%s copy differs:\n%+v\n%+v", name, cp, tbl)
		}
		for i, col := range tbl.Columns {
			if i >= len(cp.Columns) {
				break
			}
			got := cp.Columns[i]
			if got.Name != col.Name || got.Type != col.Type || got.Nullable != col.Nullable || deref(got.Default) != deref(col.Default) ||
				got.Generated != col.Generated || got.AutoIncrement != col.AutoIncrement || got.Comment != col.Comment {
				t.Errorf("%s.%s copied as %+v, want %+v", name, col.Name, got, col)
			}
		}
		drop()
	}
}

// TestIntegrationSpatial creates a table with geometry columns: the
// generator registers their spatial metadata, so a spatial index can be
// created, and describing the table reads both back.
func TestIntegrationSpatial(t *testing.T) {
	c := mustOpen(t, testParams(t, appUser(), os.Getenv("ROWSMITH_TEST_ORACLE_PASSWORD")))
	ctx := context.Background()
	srv, err := c.Server(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if srv.Extras["Spatial"] == "" {
		t.Skip("Oracle Spatial is not installed (slim images leave it out)")
	}
	drop := func() {
		_, _ = c.db.ExecContext(ctx, `DROP TABLE "RSD_PLACES" PURGE`)
		_, _ = c.db.ExecContext(ctx, `DELETE FROM USER_SDO_GEOM_METADATA WHERE TABLE_NAME = 'RSD_PLACES'`)
	}
	drop()
	t.Cleanup(drop)
	sess, err := c.NewSession(ctx, driver.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	def := driver.TableDef{
		Ref: driver.ObjectRef{Name: "RSD_PLACES"},
		Columns: []driver.ColumnDef{
			{Column: driver.Column{Name: "ID", Type: "NUMBER(10)"}},
			{Column: driver.Column{Name: "LOC", Type: "SDO_GEOMETRY", Nullable: true, SRID: 4326}},
			{Column: driver.Column{Name: "AREA", Type: "SDO_GEOMETRY", Nullable: true, SRID: 3857}},
		},
		PrimaryKey: []string{"ID"},
		Indexes:    []driver.Index{{Name: "RSD_PLACES_LOC_SX", Columns: []string{"LOC"}, Type: "spatial"}},
	}
	stmts, err := c.CreateTableSQL(def)
	if err != nil {
		t.Fatal(err)
	}
	execDDL(t, sess, stmts)
	tbl := describeTable(t, c, "RSD_PLACES")
	srids := map[string]int{}
	for _, col := range tbl.Columns {
		srids[col.Name] = col.SRID
	}
	if srids["LOC"] != 4326 || srids["AREA"] != 3857 {
		t.Fatalf("SRIDs from the metadata: %v", srids)
	}
	ix := indexNamed(tbl, "RSD_PLACES_LOC_SX")
	if ix == nil || ix.Type != "spatial" {
		t.Fatalf("spatial index: %+v", ix)
	}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO RSD_PLACES (ID, LOC) VALUES (1, SDO_GEOMETRY('POINT (46.6753 24.7136)', 4326))`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM RSD_PLACES WHERE SDO_WITHIN_DISTANCE(LOC,
		SDO_GEOMETRY('POINT (46.68 24.71)', 4326), 'distance=10 unit=KM') = 'TRUE'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("spatial query through the index: %d, %v", n, err)
	}
	if !strings.Contains(tbl.DDL, "INDEXTYPE IS MDSYS.SPATIAL_INDEX_V2") {
		t.Fatalf("DDL should recreate the spatial index:\n%s", tbl.DDL)
	}
}
