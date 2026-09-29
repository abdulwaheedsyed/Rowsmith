package bigquery

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
)

// The optional capabilities the API discovers by type assertion.
var (
	_ driver.Explainer      = (*conn)(nil)
	_ driver.ProcessManager = (*conn)(nil)
	_ driver.Definer        = (*conn)(nil)
	_ driver.Catalog        = (*conn)(nil)
	_ driver.Classifier     = (*conn)(nil)
)

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return r
}

func TestInfo(t *testing.T) {
	d, ok := driver.Get("bigquery")
	if !ok {
		t.Fatal("bigquery driver not registered")
	}
	info := d.Info()
	if info.Name != "BigQuery" || info.Order != 70 || info.Dialect != "bigquery" || info.QuoteChar != "`" || info.SSH {
		t.Fatalf("info = %+v", info)
	}
	want := driver.Caps{Schemas: true, SQL: true, Explain: true, Processes: true, Geometry: true, CostEstimate: true}
	if info.Caps != want {
		t.Fatalf("caps = %+v", info.Caps)
	}
	secret := map[string]bool{}
	for _, f := range info.Fields {
		secret[f.Key] = f.Secret
	}
	if !secret["credentials"] {
		t.Error("the service account key must be a secret field")
	}
	for _, k := range []string{"project", "location", "dataset", "maxBilledGB", "endpoint"} {
		if _, ok := secret[k]; !ok {
			t.Errorf("field %s missing", k)
		}
	}
}

func TestEncodeValue(t *testing.T) {
	ts := time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.UTC)
	cases := []struct {
		name string
		typ  bigquery.FieldType
		v    bigquery.Value
		want any
	}{
		{"nil", bigquery.StringFieldType, nil, nil},
		{"int", bigquery.IntegerFieldType, int64(42), int64(42)},
		{"int beyond 2^53", bigquery.IntegerFieldType, int64(9007199254740993), "9007199254740993"},
		{"negative int beyond 2^53", bigquery.IntegerFieldType, int64(math.MinInt64), "-9223372036854775808"},
		{"float", bigquery.FloatFieldType, 1.5, 1.5},
		{"NaN", bigquery.FloatFieldType, math.NaN(), "NaN"},
		{"infinity", bigquery.FloatFieldType, math.Inf(-1), "-Inf"},
		{"bool", bigquery.BooleanFieldType, true, true},
		{"string", bigquery.StringFieldType, "héllo", "héllo"},
		{"numeric", bigquery.NumericFieldType, rat("123.456"), "123.456"},
		{"numeric integer", bigquery.NumericFieldType, rat("100"), "100"},
		{"numeric negative", bigquery.NumericFieldType, rat("-0.5"), "-0.5"},
		{"numeric max", bigquery.NumericFieldType, rat("99999999999999999999999999999.999999999"), "99999999999999999999999999999.999999999"},
		{"bignumeric", bigquery.BigNumericFieldType, rat("1.12345678901234567890123456789012345678"), "1.12345678901234567890123456789012345678"},
		{"bignumeric huge", bigquery.BigNumericFieldType, rat("578960446186580977117854925043439539266"), "578960446186580977117854925043439539266"},
		{"date", bigquery.DateFieldType, civil.Date{Year: 2024, Month: 1, Day: 2}, "2024-01-02"},
		{"time", bigquery.TimeFieldType, civil.Time{Hour: 3, Minute: 4, Second: 5, Nanosecond: 500000000}, "03:04:05.5"},
		{"datetime", bigquery.DateTimeFieldType, civil.DateTimeOf(ts), "2024-01-02 03:04:05.123456"},
		{"timestamp", bigquery.TimestampFieldType, ts, "2024-01-02 03:04:05.123456+00:00"},
		{"json", bigquery.JSONFieldType, `{"a":1}`, `{"a":1}`},
		{"interval", bigquery.IntervalFieldType, &bigquery.IntervalValue{Years: 1, Months: 2, Days: 3, Hours: 4, Minutes: 5, Seconds: 6}, "1-2 3 4:5:6"},
		{"range", bigquery.RangeFieldType, &bigquery.RangeValue{Start: civil.Date{Year: 2024, Month: 1, Day: 1}}, "[2024-01-01, UNBOUNDED)"},
		{"geography garbage", bigquery.GeographyFieldType, "not wkt", "not wkt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := encodeValue(c.v, &bigquery.FieldSchema{Name: "x", Type: c.typ})
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestEncodeBinaryAndGeography(t *testing.T) {
	b, ok := encodeValue([]byte{0, 1}, &bigquery.FieldSchema{Type: bigquery.BytesFieldType}).(map[string]any)
	if !ok || b["$bin"] != "AAE=" || b["size"] != 2 {
		t.Errorf("bytes = %#v", b)
	}
	g, ok := encodeValue("POINT(-0.1276 51.5072)", &bigquery.FieldSchema{Type: bigquery.GeographyFieldType}).(map[string]any)
	if !ok || g["srid"] != 4326 || g["wkt"] == nil {
		t.Fatalf("geography = %#v", g)
	}
	var gj struct {
		Type        string    `json:"type"`
		Coordinates []float64 `json:"coordinates"`
	}
	if err := json.Unmarshal(g["$geo"].(json.RawMessage), &gj); err != nil || gj.Type != "Point" || gj.Coordinates[1] != 51.5072 {
		t.Errorf("GeoJSON = %s (%v)", g["$geo"], err)
	}
	poly, ok := encodeValue("POLYGON((0 0, 1 0, 1 1, 0 0))", &bigquery.FieldSchema{Type: bigquery.GeographyFieldType}).(map[string]any)
	if !ok || !strings.Contains(string(poly["$geo"].(json.RawMessage)), "Polygon") {
		t.Errorf("polygon = %#v", poly)
	}
}

func TestEncodeNested(t *testing.T) {
	inner := bigquery.Schema{
		{Name: "c", Type: bigquery.StringFieldType},
		{Name: "d", Type: bigquery.NumericFieldType},
		{Name: "when", Type: bigquery.DateFieldType},
	}
	fs := &bigquery.FieldSchema{Name: "s", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{
		{Name: "a", Type: bigquery.IntegerFieldType},
		{Name: "b", Type: bigquery.RecordFieldType, Repeated: true, Schema: inner},
		{Name: "tags", Type: bigquery.StringFieldType, Repeated: true},
		{Name: "big", Type: bigquery.IntegerFieldType, Repeated: true},
	}}
	v := []bigquery.Value{
		int64(1),
		[]bigquery.Value{
			[]bigquery.Value{"x", rat("1.5"), civil.Date{Year: 2020, Month: 2, Day: 29}},
			[]bigquery.Value{"y", nil, nil},
		},
		[]bigquery.Value{},
		[]bigquery.Value{int64(1), int64(1 << 60)},
	}
	want := map[string]any{
		"a": int64(1),
		"b": []any{
			map[string]any{"c": "x", "d": "1.5", "when": "2020-02-29"},
			map[string]any{"c": "y", "d": nil, "when": nil},
		},
		"tags": []any{},
		"big":  []any{int64(1), "1152921504606846976"},
	}
	if got := encodeValue(v, fs); !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v\nwant %#v", got, want)
	}
	row := encodeRow([]bigquery.Value{v, int64(7)}, bigquery.Schema{fs, {Name: "n", Type: bigquery.IntegerFieldType}})
	if len(row) != 2 || row[1] != int64(7) {
		t.Errorf("row = %#v", row)
	}
	if b, err := json.Marshal(row); err != nil || !strings.Contains(string(b), `"d":"1.5"`) {
		t.Errorf("row JSON = %s, %v", b, err)
	}
}

func TestTypeNames(t *testing.T) {
	cases := []struct {
		fs   *bigquery.FieldSchema
		typ  string
		base string
		kind driver.ValueKind
	}{
		{&bigquery.FieldSchema{Type: bigquery.IntegerFieldType}, "INT64", "int64", driver.KindInt},
		{&bigquery.FieldSchema{Type: bigquery.FloatFieldType}, "FLOAT64", "float64", driver.KindFloat},
		{&bigquery.FieldSchema{Type: bigquery.BooleanFieldType}, "BOOL", "bool", driver.KindBool},
		{&bigquery.FieldSchema{Type: bigquery.StringFieldType, MaxLength: 100}, "STRING(100)", "string", driver.KindString},
		{&bigquery.FieldSchema{Type: bigquery.NumericFieldType, Precision: 10, Scale: 2}, "NUMERIC(10, 2)", "numeric", driver.KindDecimal},
		{&bigquery.FieldSchema{Type: bigquery.BigNumericFieldType, Precision: 40}, "BIGNUMERIC(40)", "bignumeric", driver.KindDecimal},
		{&bigquery.FieldSchema{Type: bigquery.StringFieldType, Repeated: true}, "ARRAY<STRING>", "array", driver.KindArray},
		{&bigquery.FieldSchema{Type: bigquery.GeographyFieldType}, "GEOGRAPHY", "geography", driver.KindGeometry},
		{&bigquery.FieldSchema{Type: bigquery.TimestampFieldType}, "TIMESTAMP", "timestamp", driver.KindTimestamp},
		{&bigquery.FieldSchema{Type: bigquery.DateTimeFieldType}, "DATETIME", "datetime", driver.KindDateTime},
		{&bigquery.FieldSchema{Type: bigquery.JSONFieldType}, "JSON", "json", driver.KindJSON},
		{&bigquery.FieldSchema{Type: bigquery.RangeFieldType, RangeElementType: &bigquery.RangeElementType{Type: bigquery.DateFieldType}},
			"RANGE<DATE>", "range", driver.KindString},
		{&bigquery.FieldSchema{Type: bigquery.RecordFieldType, Schema: bigquery.Schema{
			{Name: "a", Type: bigquery.IntegerFieldType},
			{Name: "odd-name", Type: bigquery.RecordFieldType, Repeated: true, Schema: bigquery.Schema{{Name: "z", Type: bigquery.BooleanFieldType}}},
		}}, "STRUCT<a INT64, `odd-name` ARRAY<STRUCT<z BOOL>>>", "struct", driver.KindObject},
		{&bigquery.FieldSchema{Type: bigquery.RecordFieldType, Repeated: true, Schema: bigquery.Schema{{Name: "a", Type: bigquery.StringFieldType}}},
			"ARRAY<STRUCT<a STRING>>", "array", driver.KindArray},
	}
	for _, c := range cases {
		if got := typeName(c.fs); got != c.typ {
			t.Errorf("typeName = %q, want %q", got, c.typ)
		}
		if got := baseType(c.fs); got != c.base {
			t.Errorf("%s: baseType = %q, want %q", c.typ, got, c.base)
		}
		if got := kindOf(c.fs); got != c.kind {
			t.Errorf("%s: kind = %q, want %q", c.typ, got, c.kind)
		}
	}
	cols := resultColumns(bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType, Required: true}, {Name: "tags", Type: bigquery.StringFieldType, Repeated: true}})
	if *cols[0].Nullable || *cols[1].Nullable || cols[1].Kind != driver.KindArray {
		t.Errorf("result columns = %+v %+v", cols[0], cols[1])
	}
}

func testTable() *driver.Table {
	md := &bigquery.TableMetadata{Type: bigquery.RegularTable, Schema: bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType, Required: true},
		{Name: "name", Type: bigquery.StringFieldType},
		{Name: "score", Type: bigquery.NumericFieldType},
		{Name: "ratio", Type: bigquery.FloatFieldType},
		{Name: "active", Type: bigquery.BooleanFieldType},
		{Name: "created", Type: bigquery.TimestampFieldType},
		{Name: "address", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "city", Type: bigquery.StringFieldType}}},
		{Name: "tags", Type: bigquery.StringFieldType, Repeated: true},
		{Name: "loc", Type: bigquery.GeographyFieldType},
		{Name: "doc", Type: bigquery.JSONFieldType},
	}}
	return describeMetadata(driver.ObjectRef{Schema: "ds", Name: "t"}, &bigquery.Table{ProjectID: "p", DatasetID: "ds", TableID: "t"}, md)
}

func paramMap(ps []bigquery.QueryParameter) map[string]any {
	m := map[string]any{}
	for _, p := range ps {
		m[p.Name] = p.Value
	}
	return m
}

func TestBuildSelect(t *testing.T) {
	tb := testTable()
	path := tablePath("p", "ds", "t")
	cases := []struct {
		name   string
		req    driver.BrowseRequest
		count  bool
		sql    string
		params map[string]any
	}{
		{
			name: "filters",
			req: driver.BrowseRequest{Limit: 50, Offset: 100, Filters: []driver.Filter{
				{Column: "name", Op: "=", Value: "Bob"},
				{Column: "id", Op: ">", Value: float64(10)},
				{Column: "score", Op: "between", Values: []any{"1.5", float64(2)}},
				{Column: "created", Op: ">=", Value: "2024-01-01 00:00:00+00"},
				{Column: "active", Op: "=", Value: "true"},
				{Column: "ratio", Op: "!=", Value: 0.5},
			}},
			sql: "SELECT `id`, `name`, `score`, `ratio`, `active`, `created`, `address`, `tags`, `loc`, `doc` FROM `p.ds.t`" +
				" WHERE `name` = @p1 AND `id` > @p2 AND `score` BETWEEN CAST(@p3 AS NUMERIC) AND CAST(@p4 AS NUMERIC)" +
				" AND `created` >= CAST(@p5 AS TIMESTAMP) AND `active` = @p6 AND `ratio` != @p7 LIMIT 51 OFFSET 100",
			params: map[string]any{"p1": "Bob", "p2": int64(10), "p3": "1.5", "p4": "2", "p5": "2024-01-01 00:00:00+00", "p6": true, "p7": 0.5},
		},
		{
			name: "patterns",
			req: driver.BrowseRequest{Columns: []string{"name", "nope", "id"}, Filters: []driver.Filter{
				{Column: "name", Op: "contains", Value: "50%_off"},
				{Column: "id", Op: "startswith", Value: float64(12)},
				{Column: "address", Op: "notnull"},
				{Column: "tags", Op: "like", Value: "%x%"},
				{Column: "loc", Op: "regexp", Value: "^POINT"},
				{Column: "name", Op: "in", Values: []any{"a", "b"}},
				{Column: "id", Op: "notin", Values: []any{}},
				{Column: "doc", Op: "empty"},
			}},
			sql: "SELECT `name`, `id` FROM `p.ds.t` WHERE LOWER(`name`) LIKE LOWER(@p1) AND LOWER(CAST(`id` AS STRING)) LIKE LOWER(@p2)" +
				" AND `address` IS NOT NULL AND LOWER(TO_JSON_STRING(`tags`)) LIKE LOWER(@p3) AND REGEXP_CONTAINS(ST_ASTEXT(`loc`), @p4)" +
				" AND `name` IN (@p5, @p6) AND TRUE AND (`doc` IS NULL OR TO_JSON_STRING(`doc`) = '') LIMIT 201",
			params: map[string]any{"p1": `%50\%\_off%`, "p2": "12%", "p3": "%x%", "p4": "(?i)^POINT", "p5": "a", "p6": "b"},
		},
		{
			name: "search sort where",
			req:  driver.BrowseRequest{Search: "ada", Where: "score > 1", Sort: []driver.Sort{{Column: "created", Desc: true}, {Column: "id"}}, Columns: []string{"id"}},
			sql: "SELECT `id` FROM `p.ds.t` WHERE (LOWER(`id`) LIKE LOWER(@p1)" +
				" OR LOWER(`name`) LIKE LOWER(@p2) OR LOWER(CAST(`score` AS STRING)) LIKE LOWER(@p3) OR LOWER(TO_JSON_STRING(`doc`)) LIKE LOWER(@p4))" +
				" AND (score > 1) ORDER BY `created` DESC, `id` LIMIT 201",
			params: map[string]any{"p1": "%ada%", "p2": "%ada%", "p3": "%ada%", "p4": "%ada%"},
		},
		{
			name:   "count",
			req:    driver.BrowseRequest{Filters: []driver.Filter{{Column: "id", Op: "null"}}, Sort: []driver.Sort{{Column: "id"}}},
			count:  true,
			sql:    "SELECT COUNT(*) FROM `p.ds.t` WHERE `id` IS NULL",
			params: map[string]any{},
		},
	}
	// Integer columns are searched as text too.
	cases[2].sql = strings.Replace(cases[2].sql, "LOWER(`id`)", "LOWER(CAST(`id` AS STRING))", 1)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, params, err := buildSelect(path, tb, c.req, c.count)
			if err != nil {
				t.Fatal(err)
			}
			if sql != c.sql {
				t.Errorf("sql =\n%s\nwant\n%s", sql, c.sql)
			}
			if got := paramMap(params); !reflect.DeepEqual(got, c.params) {
				t.Errorf("params = %#v, want %#v", got, c.params)
			}
		})
	}

	pk := testTable()
	pk.PrimaryKey = []string{"id"}
	if sql, _, _ := buildSelect(path, pk, driver.BrowseRequest{Filters: []driver.Filter{{Column: "id", Op: "notnull"}}}, false); !strings.Contains(sql, "ORDER BY `id` LIMIT") {
		t.Errorf("primary key order missing: %s", sql)
	}

	bad := []driver.BrowseRequest{
		{Filters: []driver.Filter{{Column: "nope", Op: "="}}},
		{Filters: []driver.Filter{{Column: "id", Op: "~~"}}},
		{Filters: []driver.Filter{{Column: "address", Op: "=", Value: "x"}}},
		{Filters: []driver.Filter{{Column: "id", Op: "between", Values: []any{float64(1)}}}},
		{Sort: []driver.Sort{{Column: "tags"}}},
		{Sort: []driver.Sort{{Column: "nope"}}},
	}
	for _, req := range bad {
		if sql, _, err := buildSelect(path, tb, req, false); err == nil {
			t.Errorf("%+v: expected an error, got %s", req, sql)
		}
	}
}

func TestMapError(t *testing.T) {
	ge := &googleapi.Error{Code: 400, Message: `Syntax error: Unexpected identifier "x" at [3:15]`, Errors: []googleapi.ErrorItem{{Reason: "invalidQuery"}}}
	var qe *driver.QueryError
	if err := mapError(fmt.Errorf("run: %w", ge)); !errors.As(err, &qe) || qe.Line != 3 || qe.Code != "invalidQuery" || qe.Message != ge.Message {
		t.Errorf("googleapi error = %#v", err)
	}
	if err := mapError(&bigquery.Error{Message: "Not found: Table p:d.t was not found in location US", Reason: "notFound"}); !errors.As(err, &qe) || qe.Hint == "" || qe.Line != 0 {
		t.Errorf("job error = %#v", err)
	}
	if err := mapError(&bigquery.Error{Message: `Syntax error: Unexpected identifier "SELEC" [at 2:1]`, Reason: "jobInternalError"}); !errors.As(err, &qe) || qe.Line != 2 {
		t.Errorf("emulator error = %#v", err)
	}
	if err := mapError(&googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "accessDenied", Message: "Access Denied: Project p"}}}); !errors.As(err, &qe) ||
		qe.Message != "Access Denied: Project p" || qe.Hint == "" {
		t.Errorf("403 = %#v", err)
	}
	if err := mapError(&googleapi.Error{Code: 502}); !errors.As(err, &qe) || qe.Message != "Bad Gateway" {
		t.Errorf("502 = %#v", err)
	}
	plain := errors.New("boom")
	if err := mapError(plain); err != plain {
		t.Errorf("plain error changed: %#v", err)
	}
	if mapError(nil) != nil {
		t.Error("nil error mapped to non-nil")
	}
	if !dryRunUnavailable(&googleapi.Error{Code: 501}) || dryRunUnavailable(ge) || dryRunUnavailable(plain) {
		t.Error("dryRunUnavailable misclassifies errors")
	}
	if err := refusal(12<<30, 10<<30); !errors.As(err, &qe) || qe.Code != "bytesBilledLimitExceeded" || !strings.Contains(qe.Message, "12 GB") {
		t.Errorf("refusal = %#v", err)
	}
}

func TestAnalyzePlan(t *testing.T) {
	stats := &bq.JobStatistics{StartTime: 1000, EndTime: 3500, Query: &bq.JobStatistics2{
		TotalBytesProcessed: 2048, TotalBytesBilled: 10 << 20, TotalSlotMs: 900, StatementType: "SELECT",
		QueryPlan: []*bq.ExplainQueryStage{
			{Id: 0, Name: "S00: Input", StartMs: 1100, EndMs: 1400, RecordsRead: 1000, RecordsWritten: 10, SlotMs: 400,
				ParallelInputs: 4, CompletedParallelInputs: 4, ComputeRatioAvg: 0.5, ComputeRatioMax: 1, ShuffleOutputBytes: 1536,
				Steps: []*bq.ExplainQueryStep{{Kind: "READ", Substeps: []string{"$1:name", "FROM p.ds.events"}}, {Kind: "WRITE", Substeps: []string{"$1", "TO __stage00_output"}}}},
			{Id: 1, Name: "S01: Input", StartMs: 1100, EndMs: 1200, RecordsRead: 5, RecordsWritten: 5,
				Steps: []*bq.ExplainQueryStep{{Kind: "READ", Substeps: []string{"FROM p.ds.users"}}}},
			{Id: 2, Name: "S02: Join+", InputStages: googleapi.Int64s{0, 1}, StartMs: 1400, EndMs: 2000, RecordsRead: 15, RecordsWritten: 3,
				Steps: []*bq.ExplainQueryStep{{Kind: "JOIN"}, {Kind: "AGGREGATE"}}},
			{Id: 3, Name: "S03: Output", InputStages: googleapi.Int64s{2}, StartMs: 2000, EndMs: 2100, RecordsRead: 3, RecordsWritten: 3,
				ShuffleOutputBytesSpilled: 10},
		},
	}}
	plan := analyzePlan(stats)
	root := plan.Root
	if root.TimeMS == nil || *root.TimeMS != 2500 || plan.Format != "json" {
		t.Fatalf("root = %+v", root)
	}
	if len(root.Children) != 1 || root.Children[0].Operation != "S03: Output" {
		t.Fatalf("root children = %+v", root.Children)
	}
	join := root.Children[0].Children[0]
	if join.Operation != "S02: Join+" || len(join.Children) != 2 || join.Detail != "JOIN → AGGREGATE" {
		t.Fatalf("join = %+v", join)
	}
	in := join.Children[0]
	if in.Object != "p.ds.events" || *in.TimeMS != 300 || *in.ActualRows != 10 || *in.Cost != 400 {
		t.Errorf("input stage = %+v", in)
	}
	// Times include the inputs: join 600 + 300 + 100, output 100 + 1000.
	if *join.TimeMS != 1000 || *root.Children[0].TimeMS != 1100 || join.Props["Stage time (ms)"] != "600" {
		t.Errorf("inclusive times: join %v, output %v", *join.TimeMS, *root.Children[0].TimeMS)
	}
	if in.Props["Records read"] != "1000" || in.Props["Shuffle output"] != "1.5 KB" || in.Props["Compute ratio (avg / max)"] != "0.50 / 1.00" ||
		in.Props["Parallel inputs"] != "4 of 4 completed" || !strings.Contains(in.Props["Steps"], "READ: $1:name; FROM p.ds.events") {
		t.Errorf("input props = %v", in.Props)
	}
	if join.Children[1].Object != "p.ds.users" {
		t.Errorf("second input = %+v", join.Children[1])
	}
	if root.Children[0].Props["Shuffle spilled to disk"] != "10 B" {
		t.Errorf("output props = %v", root.Children[0].Props)
	}
	if plan.Totals["Stages"] != "4" || plan.Totals["Bytes billed"] != "10 MB" || plan.Totals["Slot time"] != float64(900) || !strings.Contains(plan.Raw, `"S00: Input"`) {
		t.Errorf("totals = %v raw = %.80s", plan.Totals, plan.Raw)
	}

	empty := analyzePlan(&bq.JobStatistics{Query: &bq.JobStatistics2{}})
	if empty.Root == nil || len(empty.Root.Children) != 0 || !strings.Contains(empty.Root.Detail, "no query plan") {
		t.Errorf("empty plan = %+v", empty.Root)
	}
	if analyzePlan(nil).Root == nil {
		t.Error("nil statistics must still produce a root")
	}
}

func TestDryRunPlan(t *testing.T) {
	plan := dryRunPlan(&bigquery.QueryStatistics{StatementType: "SELECT", TotalBytesProcessed: 1288490189,
		ReferencedTables: []*bigquery.Table{{ProjectID: "p", DatasetID: "d", TableID: "a"}, {ProjectID: "p", DatasetID: "d", TableID: "b"}}})
	if plan.Root.Operation != "SELECT" || plan.Root.Detail != "will process 1.2 GB" || len(plan.Root.Children) != 2 || plan.Root.Children[1].Object != "p.d.b" {
		t.Errorf("plan = %+v", plan.Root)
	}
	if plan.Totals["Bytes processed"] != "1.2 GB" || plan.Totals["Tables"] != "p.d.a, p.d.b" || !strings.Contains(plan.Raw, `"totalBytesProcessed": 1288490189`) {
		t.Errorf("totals = %v", plan.Totals)
	}
}

func TestStatementKinds(t *testing.T) {
	for typ, want := range map[string]driver.StatementKind{
		"SELECT": driver.StmtRead, "INSERT": driver.StmtWrite, "MERGE": driver.StmtWrite, "CREATE_TABLE_AS_SELECT": driver.StmtDDL,
		"DROP_TABLE": driver.StmtDDL, "ALTER_TABLE": driver.StmtDDL, "TRUNCATE_TABLE": driver.StmtDDL, "GRANT": driver.StmtDCL,
		"COMMIT_TRANSACTION": driver.StmtTCL, "SCRIPT": driver.StmtUnknown, "": driver.StmtUnknown,
	} {
		if got := statementKind(typ); got != want {
			t.Errorf("statementKind(%q) = %s, want %s", typ, got, want)
		}
	}
	if !isDML("DELETE") || isDML("SELECT") {
		t.Error("isDML")
	}
	stmts := sqlsplit.Split("DECLARE x INT64; SELECT 1; CREATE TABLE t (a INT64)", sqlsplit.BigQuery)
	if k := scriptKind(stmts); k != driver.StmtDDL {
		t.Errorf("scriptKind = %s", k)
	}
	if k := scriptKind(sqlsplit.Split("SELECT 1; SELECT 2", sqlsplit.BigQuery)); k != driver.StmtRead {
		t.Errorf("read-only scriptKind = %s", k)
	}
	if k := (&conn{}).Classify("DELETE FROM t WHERE TRUE"); k != driver.StmtWrite {
		t.Errorf("Classify = %s", k)
	}
}

func TestFormatting(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1 KB", 1536: "1.5 KB", 10 << 30: "10 GB", 1288490189: "1.2 GB", 3 << 50: "3 PB"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
	est := &bigquery.QueryStatistics{TotalBytesProcessed: 5 << 20, TotalBytesProcessedAccuracy: "UPPER_BOUND"}
	if got := estimateText("script", est); got != "This script will process at most 5 MB when run." {
		t.Errorf("estimate = %q", got)
	}
	if got := quote("a`b\\c"); got != "`a\\`b\\\\c`" {
		t.Errorf("quote = %s", got)
	}
	if got := labelValue("Rowsmith Studio!"); got != "rowsmithstudio" {
		t.Errorf("labelValue = %q", got)
	}
}

func TestParseJobID(t *testing.T) {
	cases := map[string][3]string{
		"job_1":                          {"def", "EU", "job_1"},
		"US.job_2":                       {"def", "US", "job_2"},
		"other:me-central2.job_3":        {"other", "me-central2", "job_3"},
		"example.com:proj:asia-east1.j4": {"example.com:proj", "asia-east1", "j4"},
	}
	for id, want := range cases {
		p, l, j, err := parseJobID(id, "def", "EU")
		if err != nil || [3]string{p, l, j} != want {
			t.Errorf("parseJobID(%q) = %s %s %s %v", id, p, l, j, err)
		}
	}
	if _, _, _, err := parseJobID("US.", "def", ""); err == nil {
		t.Error("empty job id accepted")
	}
}

func TestOpenParams(t *testing.T) {
	if email, err := serviceAccountEmail(`{"type":"service_account","client_email":"sa@p.iam.gserviceaccount.com"}`); err != nil || email != "sa@p.iam.gserviceaccount.com" {
		t.Errorf("service account = %q, %v", email, err)
	}
	for _, bad := range []string{"not json", `{"type":"external_account","credential_source":{"url":"http://169.254.169.254"}}`, `{"type":"authorized_user"}`} {
		if _, err := serviceAccountEmail(bad); err == nil {
			t.Errorf("key %q accepted", bad)
		}
	}
	gb := func(v any) (float64, error) {
		p := driver.OpenParams{Params: map[string]any{}}
		if v != nil {
			p.Params["maxBilledGB"] = v
		}
		return gigabytes(p, "maxBilledGB")
	}
	for in, want := range map[any]float64{nil: 10, "": 10, " 2.5 ": 2.5, float64(0): 0, 3: 3} {
		if got, err := gb(in); err != nil || got != want {
			t.Errorf("gigabytes(%#v) = %v, %v", in, got, err)
		}
	}
	for _, in := range []any{float64(-1), "abc", "NaN", float64(1e12)} {
		if _, err := gb(in); err == nil {
			t.Errorf("gigabytes(%#v) accepted", in)
		}
	}
	c := &conn{project: "p", dataset: "def"}
	for schema, want := range map[string][2]string{"": {"p", "def"}, "ds": {"p", "ds"}, "bigquery-public-data.samples": {"bigquery-public-data", "samples"}} {
		p, d, err := c.datasetOf(schema)
		if err != nil || [2]string{p, d} != want {
			t.Errorf("datasetOf(%q) = %s %s %v", schema, p, d, err)
		}
	}
	if _, _, err := (&conn{project: "p"}).datasetOf(""); err == nil {
		t.Error("missing dataset accepted")
	}
}

func TestDescribeMetadata(t *testing.T) {
	exp := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	md := &bigquery.TableMetadata{
		Type: bigquery.RegularTable, Description: "Events", Location: "EU", NumRows: 42, NumBytes: 4096, ExpirationTime: exp,
		Labels: map[string]string{"team": "data", "env": "prod"},
		Schema: bigquery.Schema{
			{Name: "id", Type: bigquery.IntegerFieldType, Required: true, Description: "event id"},
			{Name: "day", Type: bigquery.DateFieldType, DefaultValueExpression: "CURRENT_DATE()"},
			{Name: "user", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "name", Type: bigquery.StringFieldType}}},
			{Name: "tags", Type: bigquery.StringFieldType, Repeated: true},
			{Name: "loc", Type: bigquery.GeographyFieldType},
		},
		TimePartitioning:       &bigquery.TimePartitioning{Type: bigquery.MonthPartitioningType, Field: "day", Expiration: 30 * 24 * time.Hour},
		RequirePartitionFilter: true,
		Clustering:             &bigquery.Clustering{Fields: []string{"id"}},
		TableConstraints:       &bigquery.TableConstraints{PrimaryKey: &bigquery.PrimaryKey{Columns: []string{"id"}}},
		StreamingBuffer:        &bigquery.StreamingBuffer{EstimatedRows: 5},
	}
	tb := describeMetadata(driver.ObjectRef{Name: "events"}, &bigquery.Table{ProjectID: "p", DatasetID: "ds", TableID: "events"}, md)
	if tb.Kind != "table" || tb.Ref.Schema != "ds" || *tb.RowEstimate != 42 || *tb.Size != 4096 || tb.Editable || tb.Comment != "Events" {
		t.Fatalf("table = %+v", tb)
	}
	id := tb.Columns[0]
	if id.Nullable || !id.PrimaryKey || id.Comment != "event id" || tb.PrimaryKey[0] != "id" {
		t.Errorf("id = %+v", id)
	}
	if tags := tb.Columns[3]; tags.Nullable || tags.Kind != driver.KindArray {
		t.Errorf("tags = %+v", tags)
	}
	if loc := tb.Columns[4]; loc.SRID != 4326 || loc.Kind != driver.KindGeometry {
		t.Errorf("loc = %+v", loc)
	}
	if d := tb.Columns[1].Default; d == nil || *d != "CURRENT_DATE()" {
		t.Errorf("default = %v", d)
	}
	want := map[string]string{"partition_by": "DATE_TRUNC(`day`, MONTH)", "partition_expiration_days": "30", "require_partition_filter": "true",
		"cluster_by": "`id`", "labels": "env=prod, team=data", "location": "EU", "streaming_buffer_rows": "5", "expiration": "2030-01-01 00:00:00 UTC"}
	for k, v := range want {
		if tb.Options[k] != v {
			t.Errorf("option %s = %q, want %q", k, tb.Options[k], v)
		}
	}
	ddl := buildDDL(tablePath("p", "ds", "events"), tb, md)
	for _, part := range []string{
		"CREATE TABLE `p.ds.events`\n(\n  `id` INT64 NOT NULL OPTIONS(description=\"event id\"),\n  `day` DATE DEFAULT CURRENT_DATE(),",
		"  `user` STRUCT<name STRING>,\n  `tags` ARRAY<STRING>,",
		"  PRIMARY KEY (`id`) NOT ENFORCED\n)",
		"PARTITION BY DATE_TRUNC(`day`, MONTH)\nCLUSTER BY `id`\nOPTIONS(",
		`description="Events"`, "partition_expiration_days=30", "require_partition_filter=true",
		`expiration_timestamp=TIMESTAMP "2030-01-01 00:00:00 UTC"`, `labels=[("env", "prod"), ("team", "data")]`,
	} {
		if !strings.Contains(ddl, part) {
			t.Errorf("DDL lacks %q:\n%s", part, ddl)
		}
	}

	view := describeMetadata(driver.ObjectRef{Schema: "ds", Name: "v"}, &bigquery.Table{ProjectID: "p", DatasetID: "ds", TableID: "v"},
		&bigquery.TableMetadata{Type: bigquery.ViewTable, ViewQuery: "SELECT 1 AS x", Schema: bigquery.Schema{{Name: "x", Type: bigquery.IntegerFieldType}}})
	if view.Kind != "view" || view.Definition != "SELECT 1 AS x" || view.RowEstimate != nil {
		t.Errorf("view = %+v", view)
	}
	if got := buildDDL("`p.ds.v`", view, &bigquery.TableMetadata{ViewQuery: "SELECT 1 AS x"}); got != "CREATE VIEW `p.ds.v`\nAS SELECT 1 AS x;" {
		t.Errorf("view DDL = %q", got)
	}
}

func TestPartitionExpr(t *testing.T) {
	ts := bigquery.Schema{{Name: "ts", Type: bigquery.TimestampFieldType}, {Name: "d", Type: bigquery.DateFieldType}, {Name: "dt", Type: bigquery.DateTimeFieldType}}
	cases := []struct {
		md   *bigquery.TableMetadata
		want string
	}{
		{&bigquery.TableMetadata{}, ""},
		{&bigquery.TableMetadata{TimePartitioning: &bigquery.TimePartitioning{}}, "_PARTITIONDATE"},
		{&bigquery.TableMetadata{TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.HourPartitioningType}}, "TIMESTAMP_TRUNC(_PARTITIONTIME, HOUR)"},
		{&bigquery.TableMetadata{Schema: ts, TimePartitioning: &bigquery.TimePartitioning{Field: "d"}}, "`d`"},
		{&bigquery.TableMetadata{Schema: ts, TimePartitioning: &bigquery.TimePartitioning{Field: "ts", Type: bigquery.DayPartitioningType}}, "TIMESTAMP_TRUNC(`ts`, DAY)"},
		{&bigquery.TableMetadata{Schema: ts, TimePartitioning: &bigquery.TimePartitioning{Field: "dt", Type: bigquery.YearPartitioningType}}, "DATETIME_TRUNC(`dt`, YEAR)"},
		{&bigquery.TableMetadata{RangePartitioning: &bigquery.RangePartitioning{Field: "n", Range: &bigquery.RangePartitioningRange{Start: 0, End: 100, Interval: 10}}},
			"RANGE_BUCKET(`n`, GENERATE_ARRAY(0, 100, 10))"},
	}
	for _, c := range cases {
		if got := partitionExpr(c.md); got != c.want {
			t.Errorf("partitionExpr = %q, want %q", got, c.want)
		}
	}
}

func TestRoutineDDL(t *testing.T) {
	i64 := &bigquery.StandardSQLDataType{TypeKind: "INT64"}
	md := &bigquery.RoutineMetadata{Type: "SCALAR_FUNCTION", Language: "SQL", Body: "x + 1", ReturnType: i64,
		Arguments: []*bigquery.RoutineArgument{{Name: "x", DataType: i64}}}
	if got := routineDDL("p", "ds", "inc", md); got != "CREATE FUNCTION `p.ds.inc`(x INT64)\nRETURNS INT64\nAS (x + 1);" {
		t.Errorf("function DDL = %q", got)
	}
	arr := &bigquery.StandardSQLDataType{TypeKind: "ARRAY", ArrayElementType: &bigquery.StandardSQLDataType{TypeKind: "STRUCT",
		StructType: &bigquery.StandardSQLStructType{Fields: []*bigquery.StandardSQLField{{Name: "a", Type: i64}}}}}
	if got := sqlTypeName(arr); got != "ARRAY<STRUCT<a INT64>>" {
		t.Errorf("sqlTypeName = %q", got)
	}
	if standardKind(arr) != driver.KindArray || standardKind(i64) != driver.KindInt || standardKind(&bigquery.StandardSQLDataType{TypeKind: "DATE"}) != driver.KindDate {
		t.Error("standardKind")
	}
	proc := &bigquery.RoutineMetadata{Type: "PROCEDURE", Body: "BEGIN SELECT 1; END",
		Arguments: []*bigquery.RoutineArgument{{Name: "n", Mode: "INOUT", DataType: i64}}}
	if got := routineDDL("p", "ds", "pr", proc); got != "CREATE PROCEDURE `p.ds.pr`(INOUT n INT64)\nBEGIN SELECT 1; END;" {
		t.Errorf("procedure DDL = %q", got)
	}
}
