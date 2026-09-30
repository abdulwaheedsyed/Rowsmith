package oracle

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sijms/go-ora/v2/network"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

func nf(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }
func ni(v int64) sql.NullInt64     { return sql.NullInt64{Int64: v, Valid: true} }

func TestPlanTree(t *testing.T) {
	root := planTree([]planRow{
		{id: 0, operation: "SELECT STATEMENT", cost: nf(3), rows: nf(2)},
		{id: 1, parent: ni(0), operation: "NESTED LOOPS"},
		{id: 2, parent: ni(1), operation: "TABLE ACCESS", options: "BY INDEX ROWID", owner: "SHOP", object: "ORDERS", alias: "O@SEL$1"},
		{id: 3, parent: ni(2), operation: "INDEX", options: "RANGE SCAN", owner: "SHOP", object: "ORDERS_IX", access: `"CUSTOMER_ID"=1`},
		{id: 4, parent: ni(1), operation: "TABLE ACCESS", options: "FULL", object: "CUSTOMERS", filter: `"ID">0`, bytes: nf(40),
			actualRows: nf(7), starts: nf(1), elapsed: nf(2500)},
	})
	if root == nil || root.Operation != "SELECT STATEMENT" || *root.Cost != 3 || *root.Rows != 2 || len(root.Children) != 1 {
		t.Fatalf("root %+v", root)
	}
	loops := root.Children[0]
	if loops.Operation != "NESTED LOOPS" || len(loops.Children) != 2 {
		t.Fatalf("nested loops %+v", loops)
	}
	byIndex, full := loops.Children[0], loops.Children[1]
	if byIndex.Operation != "TABLE ACCESS BY INDEX ROWID" || byIndex.Object != "SHOP.ORDERS" || byIndex.Props["Alias"] != "O@SEL$1" {
		t.Errorf("table access %+v", byIndex)
	}
	if ix := byIndex.Children[0]; ix.Operation != "INDEX RANGE SCAN" || ix.Props["Access predicates"] != `"CUSTOMER_ID"=1` {
		t.Errorf("index %+v", ix)
	}
	if full.Object != "CUSTOMERS" || full.Props["Filter predicates"] != `"ID">0` || full.Props["Bytes"] != "40" ||
		*full.ActualRows != 7 || *full.Loops != 1 || *full.TimeMS != 2.5 || full.Cost != nil {
		t.Errorf("full scan %+v", full)
	}
	if planTree(nil) != nil {
		t.Error("empty plan should have no root")
	}
	if two := planTree([]planRow{{id: 0, operation: "A"}, {id: 1, parent: ni(7), operation: "B"}}); two.Operation != "Plan" || len(two.Children) != 2 {
		t.Errorf("orphans %+v", two)
	}
}

func testEngine() *sqlbase.Engine {
	return &sqlbase.Engine{D: dialect{c: &conn{user: "SHOP"}}, RowIDExpr: "ROWID"}
}

func TestBrowseSQL(t *testing.T) {
	tbl := &driver.Table{Ref: driver.ObjectRef{Name: "LOG"}, Kind: "table", Columns: []driver.Column{
		{Name: "MSG", Kind: driver.KindString, BaseType: "varchar2", Nullable: true},
		{Name: "AT", Kind: driver.KindDateTime, BaseType: "date", Nullable: true},
		{Name: "N", Kind: driver.KindInt, BaseType: "number", Nullable: true},
		{Name: "SHAPE", Kind: driver.KindGeometry, BaseType: "sdo_geometry", Type: "SDO_GEOMETRY", Nullable: true},
		{Name: "PT", Kind: driver.KindOther, BaseType: "point_t", Type: "SHOP.POINT_T", Nullable: true},
		{Name: "DOC", Kind: driver.KindJSON, BaseType: "json", Type: "JSON", Nullable: true},
	}}
	sqlbase.ChooseRowKey(tbl, true)
	if tbl.RowKeyKind != "rowid" {
		t.Fatalf("row key %s", tbl.RowKeyKind)
	}
	q, args, err := testEngine().BuildSelect(tbl, driver.BrowseRequest{
		Filters: []driver.Filter{{Column: "MSG", Op: "contains", Value: "50%"}, {Column: "AT", Op: "regexp", Value: "^2024"}},
		Search:  "x", Offset: 20, Limit: 10,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT "MSG", "AT", "N", SDO_UTIL.TO_WKTGEOMETRY("SHAPE") AS "SHAPE", ` +
		`CASE WHEN "PT" IS NOT NULL THEN '(SHOP.POINT_T)' END AS "PT", JSON_SERIALIZE("DOC" RETURNING CLOB) AS "DOC", ` +
		`ROWID AS "__rowsmith_rowid" FROM "SHOP"."LOG" ` +
		`WHERE UPPER("MSG") LIKE UPPER(:1) ESCAPE '\' AND REGEXP_LIKE(TO_CHAR("AT", 'YYYY-MM-DD HH24:MI:SS'), :2, 'i') ` +
		`AND (UPPER("MSG") LIKE UPPER(:3) ESCAPE '\' OR UPPER(TO_CHAR("N")) LIKE UPPER(:4) ESCAPE '\' OR ` +
		`UPPER(JSON_SERIALIZE("DOC" RETURNING CLOB)) LIKE UPPER(:5) ESCAPE '\') OFFSET 20 ROWS FETCH NEXT 11 ROWS ONLY`
	if q != want {
		t.Errorf("browse SQL\n got: %s\nwant: %s", q, want)
	}
	if !reflect.DeepEqual(args, []any{`%50\%%`, "^2024", "%x%", "%x%", "%x%"}) {
		t.Errorf("args %q", args)
	}
	for _, tc := range []struct {
		col  driver.Column
		want string
	}{
		{driver.Column{Name: "L", BaseType: "long", Kind: driver.KindText}, "NULL"},
		{driver.Column{Name: "X", BaseType: "xmltype", Kind: driver.KindText}, `XMLSERIALIZE(CONTENT "X" AS CLOB)`},
		{driver.Column{Name: "T", BaseType: "timestamp with time zone", Kind: driver.KindTimestamp}, `TO_CHAR("T", 'YYYY-MM-DD HH24:MI:SS.FF TZH:TZM')`},
		{driver.Column{Name: "C", BaseType: "clob", Kind: driver.KindText}, `"C"`},
	} {
		if got := (dialect{}).TextExpr(tc.col); got != tc.want {
			t.Errorf("TextExpr(%s) = %s, want %s", tc.col.Name, got, tc.want)
		}
	}
	count, _, err := testEngine().BuildSelect(tbl, driver.BrowseRequest{Ref: driver.ObjectRef{Schema: "APP", Name: "LOG"}}, true)
	if err != nil || count != `SELECT COUNT(*) FROM "SHOP"."LOG"` {
		t.Errorf("count SQL %q %v", count, err)
	}
}

func TestInputExpr(t *testing.T) {
	d := dialect{c: &conn{user: "SHOP"}}
	date := &driver.Column{Name: "D", Kind: driver.KindDateTime, BaseType: "date"}
	ts := &driver.Column{Name: "T", Kind: driver.KindDateTime, BaseType: "timestamp"}
	tstz := &driver.Column{Name: "Z", Kind: driver.KindTimestamp, BaseType: "timestamp with time zone"}
	geom := &driver.Column{Name: "G", Kind: driver.KindGeometry, BaseType: "sdo_geometry", SRID: 4326}
	xml := &driver.Column{Name: "X", Kind: driver.KindText, BaseType: "xmltype"}
	num := &driver.Column{Name: "N", Kind: driver.KindInt, BaseType: "number"}
	cases := []struct {
		col  *driver.Column
		in   any
		expr string
		arg  any
	}{
		{date, "2024-05-06 07:08:09", "TO_DATE(?, 'YYYY-MM-DD HH24:MI:SS')", "2024-05-06 07:08:09"},
		{date, "2024-05-06T07:08:09.5Z", "TO_DATE(?, 'YYYY-MM-DD HH24:MI:SS')", "2024-05-06 07:08:09"},
		{date, "2024-05-06", "TO_DATE(?, 'YYYY-MM-DD HH24:MI:SS')", "2024-05-06 00:00:00"},
		{ts, "2024-05-06 07:08:09.123", "TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS.FF9')", "2024-05-06 07:08:09.123000000"},
		{ts, "2024-05-06 07:08", "TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS.FF9')", "2024-05-06 07:08:00.000000000"},
		{tstz, "2024-05-06 07:08:09.5+02:00", "TO_TIMESTAMP_TZ(?, 'YYYY-MM-DD HH24:MI:SS.FF9 TZH:TZM')", "2024-05-06 07:08:09.500000000 +02:00"},
		{tstz, "2024-05-06 07:08:09 -0530", "TO_TIMESTAMP_TZ(?, 'YYYY-MM-DD HH24:MI:SS.FF9 TZH:TZM')", "2024-05-06 07:08:09.000000000 -05:30"},
		{tstz, "2024-05-06 07:08:09", "TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS.FF9')", "2024-05-06 07:08:09.000000000"},
		{geom, "POINT (1 2)", "SDO_GEOMETRY(TO_CLOB(?), 4326)", "POINT (1 2)"},
		{geom, "SRID=3857;POINT(1 2)", "SDO_GEOMETRY(TO_CLOB(?), 3857)", "POINT(1 2)"},
		{geom, `{"type":"Point","coordinates":[1,2]}`, "SDO_GEOMETRY(TO_CLOB(?), 4326)", "POINT (1 2)"},
		{geom, map[string]any{"$geo": map[string]any{"type": "Point", "coordinates": []any{3, 4}}}, "SDO_GEOMETRY(TO_CLOB(?), 4326)", "POINT (3 4)"},
		{geom, map[string]any{"$null": true}, "NULL", nil},
		{xml, "<a/>", "XMLTYPE(?)", "<a/>"},
		{num, true, "?", 1},
		{nil, false, "?", 0},
		{num, 7.0, "?", int64(7)},
		{date, map[string]any{"$default": true}, "DEFAULT", nil},
	}
	for _, tc := range cases {
		expr, arg, err := d.InputExpr(tc.col, tc.in, "?")
		if err != nil || expr != tc.expr || !reflect.DeepEqual(arg, tc.arg) {
			t.Errorf("%v: got %q %#v %v, want %q %#v", tc.in, expr, arg, err, tc.expr, tc.arg)
		}
	}
	for _, bad := range []struct {
		col *driver.Column
		in  any
	}{{date, "yesterday"}, {geom, "SRID=x;POINT(1 2)"}, {geom, `{"type":"Nope"}`}} {
		if _, _, err := d.InputExpr(bad.col, bad.in, "?"); err == nil {
			t.Errorf("%v should be rejected", bad.in)
		}
	}
}

func TestEncode(t *testing.T) {
	d := dialect{}
	at := time.Date(2024, 5, 6, 7, 8, 9, 120000000, time.FixedZone("", 2*3600))
	cases := []struct {
		in   any
		kind driver.ValueKind
		want any
	}{
		{"42", driver.KindInt, int64(42)},
		{"-7", driver.KindInt, int64(-7)},
		{"12.5", driver.KindDecimal, "12.5"},
		{"123456789012345678901234567890", driver.KindDecimal, "123456789012345678901234567890"},
		{float32(0.1), driver.KindFloat, 0.1},
		{1.5, driver.KindFloat, 1.5},
		{at, driver.KindDateTime, "2024-05-06 07:08:09.12"},
		{at, driver.KindTimestamp, "2024-05-06 07:08:09.12+02:00"},
		{[]byte(`{"a":1}`), driver.KindJSON, `{"a":1}`},
		{"+03 00:00:00.000000", driver.KindInterval, "+03 00:00:00.000000"},
		{nil, driver.KindText, nil},
	}
	for _, tc := range cases {
		if got := d.Encode(tc.in, nil, tc.kind); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Encode(%#v, %s) = %#v, want %#v", tc.in, tc.kind, got, tc.want)
		}
	}
	if b, ok := d.Encode([]byte{0, 1, 2}, nil, driver.KindBinary).(map[string]any); !ok || b["size"] != 3 {
		t.Errorf("binary %#v", b)
	}
}

func TestTypeNames(t *testing.T) {
	for wire, want := range map[string]string{"NCHAR": "VARCHAR2", "LongVarChar": "CLOB", "LongRaw": "BLOB", "TimeStampTZ_DTY": "TIMESTAMP WITH TIME ZONE",
		"IBDouble": "BINARY_DOUBLE", "TNSType(252)": "BOOLEAN", "SOMETHING": "SOMETHING"} {
		if got := sqlTypeName(wire); got != want {
			t.Errorf("sqlTypeName(%s) = %s, want %s", wire, got, want)
		}
	}
	for _, tc := range []struct {
		prec, scale int64
		valid       bool
		want        driver.ValueKind
	}{{10, 0, true, driver.KindInt}, {18, 0, true, driver.KindInt}, {19, 0, true, driver.KindDecimal}, {12, 2, true, driver.KindDecimal},
		{38, 255, true, driver.KindDecimal}, {0, 0, true, driver.KindDecimal}, {0, 0, false, driver.KindDecimal}} {
		if got := numberKind(tc.valid, tc.prec, tc.valid, tc.scale); got != tc.want {
			t.Errorf("numberKind(%d, %d) = %s, want %s", tc.prec, tc.scale, got, tc.want)
		}
	}
}

func TestColumnTypes(t *testing.T) {
	cases := []struct {
		m    colMeta
		typ  string
		base string
		kind driver.ValueKind
	}{
		{colMeta{dataType: "NUMBER", precision: ni(10), scale: ni(0)}, "NUMBER(10)", "number", driver.KindInt},
		{colMeta{dataType: "NUMBER", precision: ni(12), scale: ni(2)}, "NUMBER(12,2)", "number", driver.KindDecimal},
		{colMeta{dataType: "NUMBER"}, "NUMBER", "number", driver.KindDecimal},
		{colMeta{dataType: "NUMBER", scale: ni(0)}, "NUMBER(*,0)", "number", driver.KindDecimal},
		{colMeta{dataType: "FLOAT", precision: ni(126)}, "FLOAT(126)", "float", driver.KindDecimal},
		{colMeta{dataType: "VARCHAR2", length: ni(400), charLength: ni(100), charUsed: "C"}, "VARCHAR2(100 CHAR)", "varchar2", driver.KindString},
		{colMeta{dataType: "VARCHAR2", length: ni(200), charLength: ni(200), charUsed: "B"}, "VARCHAR2(200 BYTE)", "varchar2", driver.KindString},
		{colMeta{dataType: "NVARCHAR2", length: ni(100), charLength: ni(50)}, "NVARCHAR2(50)", "nvarchar2", driver.KindString},
		{colMeta{dataType: "RAW", length: ni(16)}, "RAW(16)", "raw", driver.KindBinary},
		{colMeta{dataType: "DATE"}, "DATE", "date", driver.KindDateTime},
		{colMeta{dataType: "TIMESTAMP(6) WITH TIME ZONE"}, "TIMESTAMP(6) WITH TIME ZONE", "timestamp with time zone", driver.KindTimestamp},
		{colMeta{dataType: "TIMESTAMP(6) WITH LOCAL TIME ZONE"}, "TIMESTAMP(6) WITH LOCAL TIME ZONE", "timestamp with local time zone", driver.KindTimestamp},
		{colMeta{dataType: "INTERVAL DAY(2) TO SECOND(6)"}, "INTERVAL DAY(2) TO SECOND(6)", "interval day to second", driver.KindInterval},
		{colMeta{dataType: "CLOB"}, "CLOB", "clob", driver.KindText},
		{colMeta{dataType: "BINARY_DOUBLE"}, "BINARY_DOUBLE", "binary_double", driver.KindFloat},
		{colMeta{dataType: "XMLTYPE", typeOwner: "SYS"}, "XMLTYPE", "xmltype", driver.KindText},
		{colMeta{dataType: "SDO_GEOMETRY", typeOwner: "MDSYS"}, "SDO_GEOMETRY", "sdo_geometry", driver.KindGeometry},
		{colMeta{dataType: "POINT_T", typeOwner: "SHOP"}, "SHOP.POINT_T", "point_t", driver.KindOther},
		{colMeta{dataType: "ADDR_T", typeOwner: "SHOP", typeMod: "REF"}, "REF SHOP.ADDR_T", "addr_t", driver.KindOther},
		{colMeta{dataType: "BFILE"}, "BFILE", "bfile", driver.KindOther},
	}
	for _, tc := range cases {
		typ, base := columnType(tc.m), baseType(tc.m.dataType)
		if typ != tc.typ || base != tc.base || columnKind(base, tc.m) != tc.kind {
			t.Errorf("%+v: got %q %q %s, want %q %q %s", tc.m, typ, base, columnKind(base, tc.m), tc.typ, tc.base, tc.kind)
		}
	}
}

func TestStatementPreparation(t *testing.T) {
	script := `-- setup
CREATE TABLE t (id NUMBER);
INSERT INTO t VALUES (1);
BEGIN
  INSERT INTO t VALUES (2);
END;
/
CREATE OR REPLACE TYPE pt AS OBJECT (x NUMBER);
/
SELECT q'[a;b]' FROM dual;
EXEC p(1);`
	var got []string
	for _, st := range sqlsplit.Split(script, sqlsplit.Oracle) {
		got = append(got, consoleSQL(st.SQL))
	}
	want := []string{
		"-- setup\nCREATE TABLE t (id NUMBER)",
		"INSERT INTO t VALUES (1)",
		"BEGIN\n  INSERT INTO t VALUES (2);\nEND;",
		"CREATE OR REPLACE TYPE pt AS OBJECT (x NUMBER);",
		"SELECT q'[a;b]' FROM dual",
		"BEGIN p(1); END;",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("prepared statements\n got: %q\nwant: %q", got, want)
	}
	for in, out := range map[string]string{
		"SELECT 1 FROM dual ; \n":                              "SELECT 1 FROM dual",
		"/* c */ DECLARE x NUMBER; BEGIN NULL; END;":           "/* c */ DECLARE x NUMBER; BEGIN NULL; END;",
		"create or replace editionable package body p as end;": "create or replace editionable package body p as end;",
		"CREATE OR REPLACE VIEW v AS SELECT 1 x FROM dual;":    "CREATE OR REPLACE VIEW v AS SELECT 1 x FROM dual",
		"execute pkg.run":                                      "BEGIN pkg.run; END;",
	} {
		if got := consoleSQL(in); got != out {
			t.Errorf("consoleSQL(%q) = %q, want %q", in, got, out)
		}
	}
	for sql, want := range map[string]bool{"SELECT 1 FROM dual": true, "with x as (select 1 from dual) select * from x": true,
		"(SELECT 1 FROM dual) UNION (SELECT 2 FROM dual)": true, "-- c\n/* d */ select 1 from dual": true,
		"INSERT INTO t VALUES (1)": false, "BEGIN NULL; END;": false, "EXPLAIN PLAN FOR SELECT 1 FROM dual": false} {
		if got := returnsRows(sqlsplit.Statement{SQL: sql}); got != want {
			t.Errorf("returnsRows(%q) = %v", sql, got)
		}
	}
	if w := leadingWords("  -- x\n( /* y */ select a from b", 3); !reflect.DeepEqual(w, []string{"SELECT", "A", "FROM"}) {
		t.Errorf("leadingWords %q", w)
	}
}

func TestParseUnit(t *testing.T) {
	cases := map[string]*plsqlUnit{
		"CREATE OR REPLACE PROCEDURE shop.p_x IS BEGIN NULL; END;":        {typ: "PROCEDURE", owner: "SHOP", name: "P_X"},
		"create or replace\npackage  body \"MyPkg\" as end;":              {typ: "PACKAGE BODY", name: "MyPkg", line: 1},
		"-- note\nCREATE TRIGGER trg BEFORE INSERT ON t BEGIN NULL; END;": {typ: "TRIGGER", name: "TRG", line: 1},
		"CREATE NONEDITIONABLE FUNCTION IF NOT EXISTS f RETURN NUMBER":    {typ: "FUNCTION", name: "F"},
		"CREATE TYPE BODY t AS END;":                                      {typ: "TYPE BODY", name: "T"},
		"CREATE TABLE t (a NUMBER)":                                       nil,
		"BEGIN NULL; END;":                                                nil,
	}
	for sql, want := range cases {
		if got := parseUnit(sql); !reflect.DeepEqual(got, want) {
			t.Errorf("parseUnit(%q) = %+v, want %+v", sql, got, want)
		}
	}
}

func TestQueryError(t *testing.T) {
	st := sqlsplit.Statement{SQL: "BEGIN\n  NULL;\n  x := 1;\nEND;", Line: 10}
	qe := queryError(6550, "ORA-06550: line 3, column 3:\nPLS-00201: identifier 'X' must be declared\nORA-06550: line 3, column 3:\nPL/SQL: Statement ignored\n", 17, st)
	if qe.Code != "ORA-06550" || qe.Message != "PLS-00201: identifier 'X' must be declared" || qe.Line != 12 || qe.Position != 18 ||
		!strings.Contains(qe.Detail, "Statement ignored") {
		t.Errorf("compile error %+v", qe)
	}
	qe = queryError(20001, "ORA-20001: boom\nORA-06512: at \"SHOP.P\", line 5\nORA-06512: at line 2\n", 0, sqlsplit.Statement{SQL: "BEGIN\n  p;\nEND;", Line: 3})
	if qe.Code != "ORA-20001" || qe.Message != "ORA-20001: boom" || qe.Line != 4 || qe.Position != 0 || !strings.HasPrefix(qe.Detail, "ORA-06512") {
		t.Errorf("runtime error %+v", qe)
	}
	qe = queryError(942, "ORA-00942: table or view does not exist\n", 16, sqlsplit.Statement{SQL: "SELECT *\n  FROM nope", Line: 7})
	if qe.Line != 8 || qe.Position != 17 || qe.Detail != "" {
		t.Errorf("parse error %+v", qe)
	}
	if err := mapError(errors.New("plain"), st); err.Error() != "plain" {
		t.Errorf("non-Oracle error changed: %v", err)
	}
}

func TestCleanErr(t *testing.T) {
	oe := &network.OracleError{ErrCode: 1, ErrMsg: "ORA-00001: unique constraint (SHOP.PK) violated\n"}
	var qe *driver.QueryError
	if err := cleanErr(fmt.Errorf("change 2: %w", oe)); !errors.As(err, &qe) || qe.Code != "ORA-00001" ||
		qe.Message != "change 2: ORA-00001: unique constraint (SHOP.PK) violated" {
		t.Errorf("cleanErr = %#v", err)
	}
	if cleanErr(nil) != nil || cleanErr(sql.ErrNoRows) != sql.ErrNoRows {
		t.Error("cleanErr should pass other errors through")
	}
	if err := privErr(&network.OracleError{ErrCode: 942, ErrMsg: "ORA-00942: table or view does not exist"}, "Listing sessions", "V$SESSION"); !strings.Contains(err.Error(), "needs SELECT access to V$SESSION") {
		t.Errorf("privErr = %v", err)
	}
}

func ddlTable() (*driver.Table, constraints) {
	def := "'new'"
	ident := "GENERATED BY DEFAULT AS IDENTITY"
	t := &driver.Table{Ref: driver.ObjectRef{Schema: "SHOP", Name: "ORDERS"}, Kind: "table", Comment: "Orders", Options: map[string]string{},
		Columns: []driver.Column{
			{Name: "ID", Type: "NUMBER(10)", Default: &ident},
			{Name: "STATUS", Type: "VARCHAR2(20 BYTE)", Default: &def, Nullable: true, Comment: "it's the state"},
			{Name: "CUSTOMER_ID", Type: "NUMBER(10)"},
			{Name: "UPPER_STATUS", Type: "VARCHAR2(20 BYTE)", Generated: `UPPER("STATUS")`, Nullable: true},
		},
		PrimaryKey:  []string{"ID"},
		Checks:      []driver.Check{{Name: "ORDERS_STATUS_CK", Expression: "STATUS IN ('new', 'paid')"}},
		ForeignKeys: []driver.ForeignKey{{Name: "ORDERS_FK", Columns: []string{"CUSTOMER_ID"}, RefTable: driver.ObjectRef{Schema: "SHOP", Name: "CUSTOMERS"}, RefColumns: []string{"ID"}, OnDelete: "CASCADE"}},
		Indexes: []driver.Index{
			{Name: "ORDERS_PK", Primary: true, Unique: true, Columns: []string{"ID"}},
			{Name: "ORDERS_UK", Unique: true, Columns: []string{"CUSTOMER_ID", "STATUS"}},
			{Name: "ORDERS_IX", Columns: []string{`UPPER("STATUS")`, "CUSTOMER_ID"}, Desc: []bool{false, true}},
		},
		Triggers: []driver.Trigger{{Name: "ORDERS_BI", Statement: "CREATE OR REPLACE TRIGGER orders_bi BEFORE INSERT ON orders\nBEGIN NULL; END;"}},
	}
	cols := map[string]bool{"ID": true, "STATUS": true, "CUSTOMER_ID": true}
	for i := range t.Indexes {
		t.Indexes[i].Definition = indexDDL("SHOP", t.Indexes[i], t, cols)
	}
	return t, constraints{pk: "ORDERS_PK", pkIndex: "ORDERS_PK", unique: []namedColumns{{name: "ORDERS_UK", columns: []string{"CUSTOMER_ID", "STATUS"}}},
		indexes: map[string]bool{"ORDERS_PK": true, "ORDERS_UK": true}}
}

func TestTableDDL(t *testing.T) {
	tbl, cons := ddlTable()
	want := `CREATE TABLE "SHOP"."ORDERS" (
    "ID" NUMBER(10) GENERATED BY DEFAULT AS IDENTITY NOT NULL,
    "STATUS" VARCHAR2(20 BYTE) DEFAULT 'new',
    "CUSTOMER_ID" NUMBER(10) NOT NULL,
    "UPPER_STATUS" VARCHAR2(20 BYTE) GENERATED ALWAYS AS (UPPER("STATUS")) VIRTUAL,
    CONSTRAINT "ORDERS_PK" PRIMARY KEY ("ID"),
    CONSTRAINT "ORDERS_UK" UNIQUE ("CUSTOMER_ID", "STATUS"),
    CONSTRAINT "ORDERS_STATUS_CK" CHECK (STATUS IN ('new', 'paid')),
    CONSTRAINT "ORDERS_FK" FOREIGN KEY ("CUSTOMER_ID") REFERENCES "SHOP"."CUSTOMERS" ("ID") ON DELETE CASCADE
);`
	if got := buildTableDDL(tbl, cons); got != want {
		t.Errorf("table DDL\n got:\n%s\nwant:\n%s", got, want)
	}
	wantExtras := `

CREATE INDEX "SHOP"."ORDERS_IX" ON "SHOP"."ORDERS" (UPPER("STATUS"), "CUSTOMER_ID" DESC);

COMMENT ON TABLE "SHOP"."ORDERS" IS 'Orders';
COMMENT ON COLUMN "SHOP"."ORDERS"."STATUS" IS 'it''s the state';

CREATE OR REPLACE TRIGGER orders_bi BEFORE INSERT ON orders
BEGIN NULL; END;
/`
	if got := tableExtrasDDL(tbl, cons); got != wantExtras {
		t.Errorf("extras\n got: %q\nwant: %q", got, wantExtras)
	}
	tbl.Options["temporary"] = "yes"
	if got := buildTableDDL(tbl, cons); !strings.HasPrefix(got, "CREATE GLOBAL TEMPORARY TABLE") {
		t.Errorf("temporary table DDL %s", got)
	}
}

func TestRestoreColumns(t *testing.T) {
	tbl := &driver.Table{Columns: []driver.Column{
		{Name: "FLAG", Type: "BOOLEAN", Kind: driver.KindBool},
		{Name: "SHAPE", Type: "SDO_GEOMETRY", Kind: driver.KindGeometry, SRID: 4326},
		{Name: "NAME", Type: "VARCHAR2(10 BYTE)", Kind: driver.KindString},
	}}
	res := &driver.Result{
		Columns: []driver.ResultColumn{{Name: "FLAG", Type: "NUMBER", Kind: driver.KindDecimal}, {Name: "SHAPE", Type: "LongVarChar", Kind: driver.KindText},
			{Name: "NAME", Type: "NCHAR", Kind: driver.KindString}, {Name: sqlbase.HiddenRowKey, Type: "ROWID", Kind: driver.KindString}},
		Rows: [][]any{{"1", "POINT (1 2)", "a", "AAA"}, {"0", nil, "b", "AAB"}, {nil, "not wkt", "c", "AAC"}},
	}
	restoreColumns(tbl, res)
	types := []string{res.Columns[0].Type, res.Columns[1].Type, res.Columns[2].Type, res.Columns[3].Type}
	if !reflect.DeepEqual(types, []string{"BOOLEAN", "SDO_GEOMETRY", "VARCHAR2(10 BYTE)", "ROWID"}) || res.Columns[0].Kind != driver.KindBool ||
		res.Columns[1].Kind != driver.KindGeometry {
		t.Errorf("columns %+v", res.Columns)
	}
	if res.Rows[0][0] != true || res.Rows[1][0] != false || res.Rows[2][0] != nil {
		t.Errorf("booleans %v %v %v", res.Rows[0][0], res.Rows[1][0], res.Rows[2][0])
	}
	cell, ok := res.Rows[0][1].(map[string]any)
	if !ok || cell["srid"] != 4326 || cell["$geo"] == nil {
		t.Errorf("geometry %#v", res.Rows[0][1])
	}
	if res.Rows[1][1] != nil || res.Rows[2][1] != "not wkt" {
		t.Errorf("unconvertible geometry %#v %#v", res.Rows[1][1], res.Rows[2][1])
	}
}

func TestInfo(t *testing.T) {
	info := oracleDriver{}.Info()
	if info.ID != "oracle" || info.Name != "Oracle" || info.Order != 40 || info.Dialect != "plsql" || info.DefaultPort != 1521 ||
		info.QuoteChar != `"` || !reflect.DeepEqual(info.URLSchemes, []string{"oracle"}) || info.Caps.Databases || !info.Caps.Schemas || !info.SSH {
		t.Errorf("info %+v", info)
	}
	fields := map[string]driver.Field{}
	for _, f := range info.Fields {
		fields[f.Key] = f
	}
	for _, k := range []string{"host", "port", "service", "serviceType", "user", "password", "role", "tls", "tlsCA", "connectTimeout"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("missing field %s", k)
		}
	}
	if fields["service"].Placeholder != "FREEPDB1" || !fields["password"].Secret || fields["role"].Section != "advanced" {
		t.Errorf("fields %+v", fields)
	}
	for _, o := range fields["tls"].Options {
		if o.Value == "prefer" {
			t.Error("TLS mode prefer cannot be supported")
		}
	}
	if d, ok := driver.Get("oracle"); !ok || d.Info().ID != "oracle" {
		t.Error("driver not registered")
	}
}

func TestTLSConfig(t *testing.T) {
	params := func(mode string) driver.OpenParams {
		return driver.OpenParams{Params: map[string]any{"tls": mode, "tlsServerName": "db.internal"}}
	}
	if cfg, err := tlsConfig(params("disable"), "h"); cfg != nil || err != nil {
		t.Errorf("disable: %v %v", cfg, err)
	}
	if _, err := tlsConfig(params("prefer"), "h"); err == nil {
		t.Error("prefer should be rejected")
	}
	if cfg, err := tlsConfig(params("require"), "h"); err != nil || !cfg.InsecureSkipVerify || cfg.VerifyConnection != nil {
		t.Errorf("require: %+v %v", cfg, err)
	}
	cfg, err := tlsConfig(params("verify-full"), "h")
	if err != nil || !cfg.InsecureSkipVerify || cfg.VerifyConnection == nil || cfg.ServerName != "db.internal" {
		t.Errorf("verify-full: %+v %v", cfg, err)
	}
}

func TestOpenValidation(t *testing.T) {
	ctx := context.Background()
	for _, p := range []map[string]any{
		{"service": "X", "user": "u"},
		{"host": "h", "user": "u"},
		{"host": "h", "service": "X"},
		{"host": "h", "service": "X", "user": "u", "role": "sysadmin"},
		{"host": "h", "service": "X", "user": "u", "tls": "prefer"},
	} {
		if _, err := (oracleDriver{}).Open(ctx, driver.OpenParams{Params: p}); err == nil {
			t.Errorf("Open(%v) should fail before connecting", p)
		}
	}
}

func TestKillProcessValidation(t *testing.T) {
	for _, id := range []string{"", "12", "12,34,56", "12,34'; DROP TABLE t --", "a,b", " 1 , 2"} {
		if err := (&conn{}).KillProcess(context.Background(), id); err == nil || !strings.Contains(err.Error(), "invalid session id") {
			t.Errorf("KillProcess(%q) = %v", id, err)
		}
	}
}

func TestCheckNamedValue(t *testing.T) {
	c := &safeConn{}
	for in, want := range map[json.Number]any{"42": int64(42), "-7": int64(-7), "12.50": "12.50", "1e400": "1e400"} {
		nv := &sqldriver.NamedValue{Value: in}
		if err := c.CheckNamedValue(nv); err != nil || nv.Value != want {
			t.Errorf("CheckNamedValue(%s) = %#v %v, want %#v", in, nv.Value, err, want)
		}
	}
	nv := &sqldriver.NamedValue{Value: "text"}
	if err := c.CheckNamedValue(nv); err != nil || nv.Value != "text" {
		t.Errorf("strings must pass unchanged: %#v %v", nv.Value, err)
	}
}

func TestOutputLines(t *testing.T) {
	if outputLines("") != nil {
		t.Error("no output expected")
	}
	if got := outputLines("a\n\nb\n"); !reflect.DeepEqual(got, []string{"a", "", "b"}) {
		t.Errorf("lines %q", got)
	}
}
