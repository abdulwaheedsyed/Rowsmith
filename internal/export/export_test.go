package export

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"strings"
	"testing"

	"rowsmith/internal/driver"
)

var cols = []driver.ResultColumn{
	{Name: "id", Kind: driver.KindInt}, {Name: "name", Kind: driver.KindString}, {Name: "price", Kind: driver.KindDecimal},
	{Name: "ok", Kind: driver.KindBool}, {Name: "blob", Kind: driver.KindBinary}, {Name: "note", Kind: driver.KindText},
}

func rows() [][]any {
	return [][]any{
		{int64(1), "Ada, \"the first\"", "12.50", true, map[string]any{"$bin": "AP8=", "size": 2}, nil},
		{"9007199254740993", "Bob\nNewline", "0.10", false, driver.LargeBinary{Data: []byte{1, 2, 3}}, driver.LongText{Text: strings.Repeat("x", 10)}},
	}
}

func write(t *testing.T, opts Options) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := New(&buf, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Begin(cols); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows() {
		if err := w.Row(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestCSV(t *testing.T) {
	got := write(t, Options{Format: CSV, Header: true, NullText: `\N`})
	want := "id,name,price,ok,blob,note\n1,\"Ada, \"\"the first\"\"\",12.50,true,0x00FF,\\N\n9007199254740993,\"Bob\nNewline\",0.10,false,0x010203,xxxxxxxxxx\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := write(t, Options{Format: CSV, Delimiter: ";", BOM: true}); !strings.HasPrefix(got, "\xEF\xBB\xBF1;") {
		t.Errorf("bom/delimiter: %q", got[:12])
	}
	if got := write(t, Options{Format: TSV}); !strings.Contains(got, "Bob\\nNewline") {
		t.Errorf("tsv escaping: %q", got)
	}
}

func TestJSONKeepsOrderAndTypes(t *testing.T) {
	got := write(t, Options{Format: JSON})
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, got)
	}
	if !strings.HasPrefix(strings.TrimSpace(strings.SplitN(got, "\n", 3)[1]), `{"id":1,"name":`) {
		t.Errorf("column order lost: %s", got)
	}
	if parsed[0]["blob"] != "0x00FF" || parsed[0]["ok"] != true || parsed[1]["note"] != "xxxxxxxxxx" || parsed[0]["note"] != nil {
		t.Errorf("values: %#v", parsed)
	}
	nd := write(t, Options{Format: NDJSON})
	if n := strings.Count(nd, "\n"); n != 2 {
		t.Errorf("ndjson lines = %d", n)
	}
	var empty bytes.Buffer
	w, _ := New(&empty, Options{Format: JSON})
	w.Begin(cols)
	w.Close()
	if strings.TrimSpace(empty.String()) != "[]" {
		t.Errorf("empty = %q", empty.String())
	}
}

func TestDocumentsKeepExtendedJSON(t *testing.T) {
	var buf bytes.Buffer
	w, _ := New(&buf, Options{Format: NDJSON, Documents: true})
	w.Begin([]driver.ResultColumn{{Name: "_id"}, {Name: "n"}, {Name: "…"}})
	w.Row([]any{map[string]any{"$oid": "65f0c0ffee00000000000001"}, nil, map[string]any{"extra": map[string]any{"$date": "2024-01-01T00:00:00Z"}}})
	w.Close()
	want := `{"_id":{"$oid":"65f0c0ffee00000000000001"},"extra":{"$date":"2024-01-01T00:00:00Z"}}` + "\n"
	if buf.String() != want {
		t.Errorf("got %s", buf.String())
	}
}

func TestSQLInsertsPerDialect(t *testing.T) {
	q := func(s string) string { return `"` + s + `"` }
	for dialect, want := range map[string][]string{
		MySQL:    {"INSERT INTO `t` (", "(1, 'Ada, \"the first\"', 12.50, TRUE, X'00FF', NULL)", "'Bob\\nNewline'", "X'010203'"},
		Postgres: {`INSERT INTO "t" (`, `'\x00FF'::bytea`, "TRUE", "'Bob\nNewline'"},
		MSSQL:    {"N'Ada, \"the first\"'", "0x00FF", ", 1, "},
		Oracle:   {"HEXTORAW('00FF')", "INSERT INTO \"t\""},
		SQLite:   {"X'00FF'", ", 1, "},
		BigQuery: {"FROM_BASE64('AP8=')", "TRUE"},
	} {
		qi := q
		if dialect == MySQL {
			qi = func(s string) string { return "`" + s + "`" }
		}
		got := write(t, Options{Format: SQL, Dialect: dialect, Table: driver.ObjectRef{Name: "t"}, QuoteIdent: qi})
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q in\n%s", dialect, w, got)
			}
		}
		if dialect == Oracle && strings.Count(got, "INSERT INTO") != 2 {
			t.Errorf("oracle should write one row per INSERT:\n%s", got)
		}
		if dialect != Oracle && strings.Count(got, "INSERT INTO") != 1 {
			t.Errorf("%s should batch rows:\n%s", dialect, got)
		}
	}
}

func TestLiteralEdgeCases(t *testing.T) {
	var st LiteralStats
	c := func(k driver.ValueKind) driver.ResultColumn { return driver.ResultColumn{Kind: k} }
	cases := []struct {
		dialect string
		col     driver.ResultColumn
		v       any
		want    string
	}{
		{MySQL, c(driver.KindString), `it's a \ test`, `'it\'s a \\ test'`},
		{Postgres, c(driver.KindString), "it's", `'it''s'`},
		{Postgres, c(driver.KindInt), "12abc", `'12abc'`},
		{Oracle, c(driver.KindDate), "2024-02-29", "TO_DATE('2024-02-29', 'YYYY-MM-DD')"},
		{Oracle, c(driver.KindTimestamp), "2024-02-29 10:00:00.5+03:00", "TO_TIMESTAMP_TZ('2024-02-29 10:00:00.5+03:00', 'YYYY-MM-DD HH24:MI:SS.FFTZH:TZM')"},
		{MySQL, driver.ResultColumn{Kind: driver.KindGeometry}, map[string]any{"$geo": map[string]any{"type": "Point", "coordinates": []any{4.9, 52.4}}, "srid": float64(4326)},
			`ST_GeomFromGeoJSON('{"coordinates":[4.9,52.4],"type":"Point"}', 1, 4326)`},
		{MSSQL, driver.ResultColumn{Kind: driver.KindGeometry, Type: "geography"}, map[string]any{"$geo": map[string]any{"type": "Point", "coordinates": []any{4.9, 52.4}}, "srid": float64(4326)},
			`geography::STGeomFromText(N'POINT (4.9 52.4)', 4326)`},
		{Postgres, c(driver.KindJSON), map[string]any{"a": []any{1.0, "x"}}, `'{"a":[1,"x"]}'`},
	}
	for _, tc := range cases {
		if got := Literal(tc.dialect, tc.col, tc.v, &st); got != tc.want {
			t.Errorf("%s %v: got %s, want %s", tc.dialect, tc.v, got, tc.want)
		}
	}
	long := strings.Repeat("é", 3000) // 6000 bytes
	got := Literal(Oracle, c(driver.KindText), long, &st)
	if !strings.HasPrefix(got, "TO_CLOB('") || !strings.Contains(got, "') || TO_CLOB('") {
		t.Errorf("long Oracle text not chunked: %.60s", got)
	}
	if Literal(Oracle, c(driver.KindBinary), driver.LargeBinary{Data: make([]byte, 3000)}, &st) != "NULL" || st.TooLarge != 1 {
		t.Error("oversized Oracle RAW should be NULL and counted")
	}
}

func TestXLSXIsWellFormed(t *testing.T) {
	got := write(t, Options{Format: XLSX, Header: true, SheetName: "orders/2024"})
	z, err := zip.NewReader(strings.NewReader(got), int64(len(got)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, f := range z.File {
		r, _ := f.Open()
		b, _ := io.ReadAll(r)
		r.Close()
		names[f.Name] = string(b)
		d := xml.NewDecoder(bytes.NewReader(b))
		for {
			if _, err := d.Token(); err == io.EOF {
				break
			} else if err != nil {
				t.Fatalf("%s is not well-formed XML: %v", f.Name, err)
			}
		}
	}
	sheet := names["xl/worksheets/sheet1.xml"]
	for _, want := range []string{`<c r="A2"><v>1</v></c>`, `<c r="A3" t="inlineStr">`, `t="b"><v>1</v>`, `Ada, &quot;the first&quot;`, `<autoFilter ref="A1:F1"/>`, `state="frozen"`} {
		if !strings.Contains(sheet, want) {
			t.Errorf("sheet missing %q:\n%s", want, sheet)
		}
	}
	if !strings.Contains(names["xl/workbook.xml"], `name="orders_2024"`) {
		t.Error("sheet name not sanitized")
	}
	if colRef(0) != "A" || colRef(25) != "Z" || colRef(26) != "AA" || colRef(701) != "ZZ" || colRef(702) != "AAA" {
		t.Error("column references")
	}
}
