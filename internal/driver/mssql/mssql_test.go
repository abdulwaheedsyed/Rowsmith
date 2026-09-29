package mssql

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	ms "github.com/microsoft/go-mssqldb"
	"github.com/twpayne/go-geom/encoding/wkt"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

func TestInfo(t *testing.T) {
	d, ok := driver.Get("mssql")
	if !ok {
		t.Fatal("mssql is not registered")
	}
	info := d.Info()
	if info.Name != "SQL Server" || info.Order != 30 || info.Dialect != "mssql" || info.DefaultPort != 1433 || info.QuoteChar != "[" ||
		strings.Join(info.URLSchemes, ",") != "sqlserver,mssql" || !info.Caps.Databases || !info.Caps.Schemas {
		t.Errorf("info: %+v", info)
	}
	keys := map[string]driver.Field{}
	for _, f := range info.Fields {
		keys[f.Key] = f
	}
	for _, k := range []string{"host", "port", "user", "password", "database", "encrypt", "trustServerCertificate", "connectTimeout", "readOnlyIntent"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("missing field %s", k)
		}
	}
	if !keys["password"].Secret || keys["readOnlyIntent"].Section != "advanced" {
		t.Error("field attributes")
	}
}

func TestConfig(t *testing.T) {
	p := driver.OpenParams{Params: map[string]any{"user": "app", "encrypt": "strict", "connectTimeout": 7.0},
		Secrets: map[string]string{"password": "p@ss:w/rd?&=;x"}, ReadOnly: true, AppName: "Rowsmith test"}
	cfg, err := config(p, "db.example.com", 1444)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "db.example.com" || cfg.Port != 1444 || cfg.User != "app" || cfg.Password != "p@ss:w/rd?&=;x" ||
		cfg.AppName != "Rowsmith test" || !cfg.ReadOnlyIntent || cfg.DialTimeout != 7*time.Second || cfg.TLSConfig == nil ||
		cfg.ConnTimeout != 0 { // a connection timeout would cut off every long-running query
		t.Errorf("config: %+v", cfg)
	}
	p.Params["encrypt"] = "sometimes"
	if _, err := config(p, "h", 1433); err == nil {
		t.Error("unknown encryption mode accepted")
	}
	p.Params["encrypt"] = "disable"
	p.ReadOnly = false
	cfg, err = config(p, "h", 1433)
	if err != nil || cfg.TLSConfig != nil || cfg.ReadOnlyIntent {
		t.Errorf("disabled encryption: %+v %v", cfg, err)
	}
}

func TestTypeString(t *testing.T) {
	cases := []struct {
		name             string
		max, prec, scale int
		want             string
	}{
		{"nvarchar", 200, 0, 0, "nvarchar(100)"},
		{"nvarchar", -1, 0, 0, "nvarchar(max)"},
		{"varbinary", 16, 0, 0, "varbinary(16)"},
		{"decimal", 9, 12, 2, "decimal(12,2)"},
		{"datetime2", 8, 27, 7, "datetime2(7)"},
		{"timestamp", 8, 0, 0, "rowversion"},
		{"int", 4, 10, 0, "int"},
	}
	for _, c := range cases {
		if got := typeString(c.name, c.max, c.prec, c.scale); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	var col driver.Column
	finishColumn(&col, "email_address", "nvarchar", true, false, 640, 0, 0)
	if col.Type != "email_address" || col.BaseType != "nvarchar" || col.Kind != driver.KindString || *col.Length != 320 {
		t.Errorf("alias type: %+v", col)
	}
	col = driver.Column{}
	finishColumn(&col, "varchar", "varchar", false, false, -1, 0, 0)
	if col.Type != "varchar(max)" || col.Kind != driver.KindText || col.Length != nil {
		t.Errorf("varchar(max): %+v", col)
	}
}

func TestUnwrapParens(t *testing.T) {
	cases := map[string]string{
		"((0))":           "0",
		"(getdate())":     "getdate()",
		"([total]*(0.2))": "[total]*(0.2)",
		"([a])+([b])":     "([a])+([b])",
		"(N'(x')":         "N'(x'",
		"(N'it''s (')":    "N'it''s ('",
		"([we)ird]>(1))":  "[we)ird]>(1)",
		"plain":           "plain",
	}
	for in, want := range cases {
		if got := unwrapParens(in); got != want {
			t.Errorf("unwrapParens(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDialectSQL(t *testing.T) {
	d := dialect{}
	if got := d.Qualify(driver.ObjectRef{Database: "sh]op", Schema: "", Name: "t"}); got != "[sh]]op].[dbo].[t]" {
		t.Errorf("qualify: %s", got)
	}
	if got := d.Paginate("SELECT 1", 10, 20, false); got != "SELECT 1 ORDER BY (SELECT NULL) OFFSET 20 ROWS FETCH NEXT 10 ROWS ONLY" {
		t.Errorf("paginate: %s", got)
	}
	if got := d.Paginate("SELECT 1 ORDER BY [a]", 5, 0, true); got != "SELECT 1 ORDER BY [a] OFFSET 0 ROWS FETCH NEXT 5 ROWS ONLY" {
		t.Errorf("paginate ordered: %s", got)
	}
	geomCol := driver.Column{Name: "g", Kind: driver.KindGeometry, BaseType: "geometry"}
	if got := d.SelectExpr(geomCol); got != "'SRID=' + CAST([g].STSrid AS varchar(12)) + ';' + [g].STAsText()" {
		t.Errorf("select geometry: %s", got)
	}
	if got := d.TextExpr(driver.Column{Name: "n", BaseType: "int"}); got != "CAST([n] AS NVARCHAR(MAX))" {
		t.Errorf("text expr: %s", got)
	}

	tb := &driver.Table{Ref: driver.ObjectRef{Database: "shop", Schema: "dbo", Name: "t"}, Kind: "table", PrimaryKey: []string{"id"},
		Columns: []driver.Column{{Name: "id", Kind: driver.KindInt, BaseType: "int"}, {Name: "name", Kind: driver.KindString, BaseType: "nvarchar"}, geomCol}}
	eng := &sqlbase.Engine{D: d}
	q, args, err := eng.BuildSelect(tb, driver.BrowseRequest{Filters: []driver.Filter{{Column: "name", Op: "contains", Value: "a_b"}}, Limit: 50}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT [id], [name], 'SRID=' + CAST([g].STSrid AS varchar(12)) + ';' + [g].STAsText() AS [g] FROM [shop].[dbo].[t] ` +
		`WHERE [name] LIKE REPLACE(@p1, '[', '\[') ESCAPE '\' ORDER BY [id] OFFSET 0 ROWS FETCH NEXT 51 ROWS ONLY`
	if q != want || len(args) != 1 || args[0] != `%a\_b%` {
		t.Errorf("browse SQL:\n%s\n%v", q, args)
	}
	if pre, suf := d.LimitOne(); pre != "TOP (1) " || suf != "" {
		t.Errorf("limit one: %q %q", pre, suf)
	}
}

func TestEncode(t *testing.T) {
	// 6F9619FF-8B86-D011-B42D-00C04FC964FF as sent on the wire.
	wire := []byte{0xFF, 0x19, 0x96, 0x6F, 0x86, 0x8B, 0x11, 0xD0, 0xB4, 0x2D, 0x00, 0xC0, 0x4F, 0xC9, 0x64, 0xFF}
	if got := encode(wire, "UNIQUEIDENTIFIER", driver.KindUUID); got != "6f9619ff-8b86-d011-b42d-00c04fc964ff" {
		t.Errorf("uuid: %v", got)
	}
	tm := time.Date(2024, 1, 2, 3, 4, 5, 123333333, time.UTC)
	if got := encode(tm, "DATETIME", driver.KindDateTime); got != "2024-01-02 03:04:05.123" {
		t.Errorf("datetime: %v", got)
	}
	if got := encode(tm, "DATETIME2", driver.KindDateTime); got != "2024-01-02 03:04:05.123333" {
		t.Errorf("datetime2: %v", got)
	}
	off := time.Date(2024, 3, 1, 10, 0, 0, 0, time.FixedZone("", 2*3600))
	if got := encode(off, "DATETIMEOFFSET", driver.KindTimestamp); got != "2024-03-01 10:00:00+02:00" {
		t.Errorf("datetimeoffset: %v", got)
	}
	if got := encode([]byte("12.3400"), "MONEY", driver.KindDecimal); got != "12.3400" {
		t.Errorf("money: %v", got)
	}
	if got := encode(true, "BIT", driver.KindBool); got != true {
		t.Errorf("bit: %v", got)
	}
	if got, ok := encode([]byte{0, 0, 0, 0, 1, 12}, "GEOMETRY", driver.KindGeometry).(map[string]any); !ok || got["size"] != 6 {
		t.Errorf("raw geometry: %v", got)
	}
	for name, want := range map[string]driver.ValueKind{"timestamp": driver.KindBinary, "hierarchyid": driver.KindString,
		"datetimeoffset": driver.KindTimestamp, "uniqueidentifier": driver.KindUUID, "money": driver.KindDecimal, "geography": driver.KindGeometry} {
		if got := kindOf(name, false); got != want {
			t.Errorf("kindOf(%s) = %s, want %s", name, got, want)
		}
	}
}

func TestInputExpr(t *testing.T) {
	d := dialect{}
	geomCol := &driver.Column{Name: "g", Kind: driver.KindGeometry, BaseType: "geometry"}
	expr, arg, err := d.InputExpr(geomCol, map[string]any{"$geo": map[string]any{"type": "Point", "coordinates": []any{5.0, 6.0}}, "srid": 3857.0}, "@p1")
	if err != nil || expr != "geometry::STGeomFromText(@p1, 3857)" || arg != "POINT (5 6)" {
		t.Errorf("geojson: %s %v %v", expr, arg, err)
	}
	geogCol := &driver.Column{Name: "g", Kind: driver.KindGeometry, BaseType: "geography", SRID: 4326}
	expr, arg, err = d.InputExpr(geogCol, "SRID=4269;POINT(1 2)", "@p2")
	if err != nil || expr != "geography::STGeomFromText(@p2, 4269)" || arg != "POINT(1 2)" {
		t.Errorf("ewkt: %s %v %v", expr, arg, err)
	}
	if expr, _, _ := d.InputExpr(geogCol, map[string]any{"$null": true}, "@p3"); expr != "NULL" {
		t.Errorf("geometry null: %s", expr)
	}
	if _, _, err := d.InputExpr(geomCol, "{not json", "@p1"); err == nil {
		t.Error("invalid GeoJSON accepted")
	}
	dt := &driver.Column{Name: "d", Kind: driver.KindDateTime, BaseType: "datetime"}
	_, arg, err = d.InputExpr(dt, "2024-01-13T10:11:12.5", "@p1")
	if v, ok := arg.(ms.DateTime1); err != nil || !ok || !time.Time(v).Equal(time.Date(2024, 1, 13, 10, 11, 12, 500000000, time.UTC)) {
		t.Errorf("datetime: %#v %v", arg, err)
	}
	if _, arg, _ := d.InputExpr(dt, "13/01/2024", "@p1"); arg != "13/01/2024" {
		t.Errorf("unparsed datetime should pass through: %#v", arg)
	}
	if _, arg, _ := d.InputExpr(&driver.Column{Name: "b", BaseType: "bit", Kind: driver.KindBool}, true, "@p1"); arg != 1 {
		t.Errorf("bit: %#v", arg)
	}
}

func TestErrors(t *testing.T) {
	e := ms.Error{Number: 208, Class: 16, State: 1, Message: "Invalid object name 'dbo.nope'.", LineNo: 3}
	err := batchError(e, sqlsplit.Statement{Line: 10})
	var qe *driver.QueryError
	if !errors.As(err, &qe) || qe.Line != 12 || qe.Code != "208" || qe.Detail != "Level 16, state 1" {
		t.Errorf("batch error: %#v", err)
	}
	e.ProcName = "dbo.p"
	if err := batchError(e, sqlsplit.Statement{Line: 10}); err.(*driver.QueryError).Line != 0 ||
		!strings.Contains(err.(*driver.QueryError).Detail, "procedure dbo.p, line 3") {
		t.Errorf("procedure error: %#v", err)
	}
	if got := noticeText(batchError(ms.Error{Number: 50000, Message: "boom", LineNo: 2}, sqlsplit.Statement{Line: 1})); got != "Msg 50000, line 2: boom" {
		t.Errorf("notice: %s", got)
	}
	first := ms.Error{Number: 1505, Message: "duplicate key"}
	last := ms.Error{Number: 1750, Message: "See previous errors.", All: []ms.Error{first, {Number: 1750, Message: "See previous errors."}}}
	err = mapError(fmt.Errorf("change 2: %w", last))
	if !errors.As(err, &qe) || qe.Message != "change 2: duplicate key\nSee previous errors." || qe.Code != "1505" {
		t.Errorf("mapError: %#v", err)
	}
	if plain := errors.New("x"); mapError(plain) != plain {
		t.Error("non-server errors must pass through")
	}
}

func TestGrantSQL(t *testing.T) {
	cases := []struct{ state, perms, sec, col, want string }{
		{"GRANT", "CONNECT", "", "", "GRANT CONNECT TO [u];"},
		{"GRANT", "INSERT, SELECT", "SCHEMA::[sales]", "", "GRANT INSERT, SELECT ON SCHEMA::[sales] TO [u];"},
		{"DENY", "SELECT", "[dbo].[t]", "secret", "DENY SELECT ON [dbo].[t] ([secret]) TO [u];"},
		{"GRANT_WITH_GRANT_OPTION", "EXECUTE", "[dbo].[p]", "", "GRANT EXECUTE ON [dbo].[p] TO [u] WITH GRANT OPTION;"},
		{"GRANT", "CONTROL", "-- symmetric keys #256", "", "-- GRANT CONTROL ON symmetric keys #256 TO [u]"},
	}
	for _, c := range cases {
		if got := grantSQL(c.state, c.perms, c.sec, c.col, "u"); got != c.want {
			t.Errorf("%q, want %q", got, c.want)
		}
	}
	if timing, ev := triggerTiming(true, true, false, true); timing != "INSTEAD OF" || ev != "INSERT, DELETE" {
		t.Errorf("trigger timing: %s %s", timing, ev)
	}
}

const estimatedXML = `<ShowPlanXML xmlns="http://schemas.microsoft.com/sqlserver/2004/07/showplan" Version="1.564">
<BatchSequence><Batch><Statements>
<StmtSimple StatementText="SELECT o.id FROM dbo.orders o JOIN dbo.customers c ON c.id = o.customer_id WHERE c.id = 1" StatementType="SELECT"
  StatementSubTreeCost="0.0065" StatementEstRows="2" StatementOptmLevel="FULL">
 <QueryPlan DegreeOfParallelism="1" CompileTime="3">
  <RelOp NodeId="0" PhysicalOp="Nested Loops" LogicalOp="Inner Join" EstimateRows="2" EstimatedTotalSubtreeCost="0.0065" EstimateIO="0" EstimateCPU="0.00001">
   <OutputList/>
   <NestedLoops Optimized="0">
    <RelOp NodeId="1" PhysicalOp="Clustered Index Seek" LogicalOp="Clustered Index Seek" EstimateRows="1" EstimatedTotalSubtreeCost="0.0032">
     <IndexScan Ordered="1">
      <Object Database="[shop]" Schema="[dbo]" Table="[customers]" Index="[PK_customers]" Alias="[c]"/>
      <SeekPredicates><SeekPredicateNew><SeekKeys><Prefix ScanType="EQ">
       <RangeColumns><ColumnReference Column="id"/></RangeColumns>
       <RangeExpressions><ScalarOperator ScalarString="(1)"/></RangeExpressions>
      </Prefix></SeekKeys></SeekPredicateNew></SeekPredicates>
     </IndexScan>
    </RelOp>
    <RelOp NodeId="2" PhysicalOp="Clustered Index Scan" LogicalOp="Clustered Index Scan" EstimateRows="2" EstimatedTotalSubtreeCost="0.0033"
      EstimateRebinds="0" EstimateRewinds="0">
     <Warnings NoJoinPredicate="true"/>
     <IndexScan Ordered="0">
      <Object Database="[shop]" Schema="[dbo]" Table="[orders]" Index="[PK_orders]" Alias="[o]"/>
      <Predicate><ScalarOperator ScalarString="[shop].[dbo].[orders].[customer_id] as [o].[customer_id]=(1)"/></Predicate>
     </IndexScan>
    </RelOp>
   </NestedLoops>
  </RelOp>
 </QueryPlan>
</StmtSimple>
<StmtSimple StatementText="SELECT 1" StatementType="SELECT WITHOUT QUERY"/>
</Statements></Batch></BatchSequence></ShowPlanXML>`

const actualXML = `<?xml version="1.0" encoding="utf-16"?><ShowPlanXML xmlns="http://schemas.microsoft.com/sqlserver/2004/07/showplan">
<BatchSequence><Batch><Statements><StmtSimple StatementText="SELECT COUNT(*) FROM t" StatementType="SELECT">
<QueryPlan CompileTime="1"><QueryTimeStats ElapsedTime="12" CpuTime="4"/>
<RelOp PhysicalOp="Stream Aggregate" LogicalOp="Aggregate" EstimateRows="1" EstimatedTotalSubtreeCost="0.5">
 <RunTimeInformation><RunTimeCountersPerThread Thread="0" ActualRows="1" ActualExecutions="1" ActualElapsedms="12"/></RunTimeInformation>
 <StreamAggregate>
  <RelOp PhysicalOp="Table Scan" LogicalOp="Table Scan" EstimateRows="100" EstimatedTotalSubtreeCost="0.4" Parallel="1">
   <RunTimeInformation>
    <RunTimeCountersPerThread Thread="1" ActualRows="60" ActualExecutions="1" ActualElapsedms="9" ActualLogicalReads="5"/>
    <RunTimeCountersPerThread Thread="2" ActualRows="40" ActualExecutions="1" ActualElapsedms="10" ActualLogicalReads="3"/>
   </RunTimeInformation>
   <TableScan><Object Schema="[dbo]" Table="[t]"/></TableScan>
  </RelOp>
 </StreamAggregate>
</RelOp></QueryPlan></StmtSimple></Statements></Batch></BatchSequence></ShowPlanXML>`

func TestParsePlan(t *testing.T) {
	p, err := parsePlan([]string{estimatedXML})
	if err != nil {
		t.Fatal(err)
	}
	if p.Format != "xml" || p.Raw != estimatedXML || p.Root.Operation != "Batch" || len(p.Root.Children) != 2 {
		t.Fatalf("root: %+v", p.Root)
	}
	st := p.Root.Children[0]
	if st.Operation != "SELECT" || *st.Cost != 0.0065 || *st.Rows != 2 || st.Props["Degree of parallelism"] != "1" || len(st.Children) != 1 {
		t.Errorf("statement: %+v", st)
	}
	nl := st.Children[0]
	if nl.Operation != "Nested Loops" || nl.Detail != "Inner Join" || len(nl.Children) != 2 {
		t.Fatalf("nested loops: %+v", nl)
	}
	seek, scan := nl.Children[0], nl.Children[1]
	if seek.Object != "dbo.customers c" || seek.Detail != "using PK_customers" || seek.Props["Seek predicate"] != "id = (1)" {
		t.Errorf("seek: %+v", seek)
	}
	if scan.Object != "dbo.orders o" || !strings.Contains(scan.Props["Predicate"], "=(1)") || scan.Props["Warnings"] != "NoJoinPredicate" {
		t.Errorf("scan: %+v", scan)
	}
	if p.Totals["Compile Time"] != 3.0 || p.Totals["Execution Time"] != nil {
		t.Errorf("totals: %v", p.Totals)
	}

	p, err = parsePlan([]string{actualXML})
	if err != nil {
		t.Fatal(err)
	}
	agg := p.Root.Children[0]
	scanNode := agg.Children[0]
	if *agg.ActualRows != 1 || *agg.TimeMS != 12 || *scanNode.ActualRows != 50 || *scanNode.Loops != 2 || *scanNode.TimeMS != 5 ||
		scanNode.Props["Logical reads"] != "8" || scanNode.Props["Parallel"] != "yes" || scanNode.Object != "dbo.t" {
		t.Errorf("actual: %+v / %+v", agg, scanNode)
	}
	if p.Totals["Execution Time"] != 12.0 || p.Totals["CPU Time"] != 4.0 {
		t.Errorf("totals: %v", p.Totals)
	}
	if _, err := parsePlan([]string{"<oops"}); err == nil {
		t.Error("invalid XML accepted")
	}
	if _, err := parsePlan(nil); err == nil {
		t.Error("empty plan accepted")
	}
}

func TestDecodeSpatial(t *testing.T) {
	// Values as SQL Server serializes them (CAST(... AS varbinary(max))).
	cases := []struct {
		hex       string
		geography bool
		srid      int
		want      string
	}{
		{"00000000010C000000000000F03F0000000000000040", false, 0, "POINT (1 2)"},
		{"00000000010D000000000000F03F00000000000000400000000000000840", false, 0, "POINT Z (1 2 3)"},
		{"0000000001140000000000000000000000000000000000000000000008400000000000001040", false, 0, "LINESTRING (0 0, 3 4)"},
		{"0000000001040300000000000000000000000000000000000000000000000000F03F000000000000F03F000000000000004000000000000000000100" +
			"0000010000000001000000FFFFFFFF0000000002", false, 0, "LINESTRING (0 0, 1 1, 2 0)"},
		{"110F0000010409000000000000000000000000000000000000000000000000002440000000000000000000000000000024400000000000002440" +
			"00000000000000000000000000002440000000000000000000000000000000000000000000000040000000000000004000000000000000400000" +
			"0000000008400000000000000840000000000000084000000000000000400000000000000040020000000200000000000500000001000000FFFF" +
			"FFFF0000000003", false, 3857, "POLYGON ((0 0, 10 0, 10 10, 0 10, 0 0), (2 2, 2 3, 3 3, 2 2))"},
		{"00000000010402000000000000000000F03F000000000000F03F0000000000000040000000000000004002000000010000000001010000000300" +
			"0000FFFFFFFF0000000004000000000000000001000000000100000001", false, 0, "MULTIPOINT (1 1, 2 2)"},
		{"0000000001040800000000000000000000000000000000000000000000000000F03F0000000000000000000000000000F03F000000000000F03F" +
			"000000000000000000000000000000000000000000001440000000000000144000000000000018400000000000001440000000000000184000" +
			"0000000000184000000000000014400000000000001440020000000200000000020400000003000000FFFFFFFF000000000600000000000000" +
			"0003000000000100000003", false, 0, "MULTIPOLYGON (((0 0, 1 0, 1 1, 0 0)), ((5 5, 6 5, 6 6, 5 5)))"},
		{"00000000010404000000000000000000F03F000000000000F03F00000000000000000000000000000000000000000000F03F000000000000F03F" +
			"00000000000000400000000000000040020000000100000000010100000003000000FFFFFFFF000000000700000000000000000100000000" +
			"0100000002", false, 0, "GEOMETRYCOLLECTION (POINT (1 1), LINESTRING (0 0, 1 1, 2 2))"},
		{"000000000104000000000000000001000000FFFFFFFFFFFFFFFF01", false, 0, "POINT EMPTY"},
		{"E6100000010C0000000000404A40CDCCCCCCCCCC2A40", true, 4326, "POINT (13.4 52.5)"},
		{"E61000000104030000000000000000404A40CDCCCCCCCCCC2A40CDCCCCCCCC6C4840CDCCCCCCCCCC02400000000000C0494000000000000000000" +
			"1000000010000000001000000FFFFFFFF0000000002", true, 4326, "LINESTRING (13.4 52.5, 2.35 48.85, 0 51.5)"},
	}
	for _, c := range cases {
		b, err := hex.DecodeString(c.hex)
		if err != nil {
			t.Fatal(err)
		}
		g, srid, err := decodeSpatial(b, c.geography)
		if err != nil {
			t.Errorf("%s: %v", c.want, err)
			continue
		}
		if got, _ := wkt.Marshal(g); got != c.want || srid != c.srid {
			t.Errorf("got %s (srid %d), want %s (srid %d)", got, srid, c.want, c.srid)
		}
	}
	curve, _ := hex.DecodeString("0000000002040300000000000000000000000000000000000000000000000000F03F000000000000F03F0000000000000040000000000000" +
		"000001000000020000000001000000FFFFFFFF0000000008")
	if _, _, err := decodeSpatial(curve, false); err == nil {
		t.Error("circular strings are not supported and must fail")
	}
	for _, bad := range [][]byte{nil, {1, 2, 3}, {0, 0, 0, 0, 1, 4, 0xFF, 0xFF, 0xFF, 0x7F}} {
		if _, _, err := decodeSpatial(bad, false); err == nil {
			t.Errorf("corrupt value %x accepted", bad)
		}
	}
	if cell, ok := encode(curve, "GEOMETRY", driver.KindGeometry).(map[string]any); !ok || cell["$bin"] == nil {
		t.Errorf("undecodable geometry should stay binary: %v", cell)
	}
}
