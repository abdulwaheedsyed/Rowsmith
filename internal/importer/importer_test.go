package importer

import (
	"archive/zip"
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"

	"rowsmith/internal/driver"
	"rowsmith/internal/export"
)

func all(t *testing.T, s Source) [][]any {
	t.Helper()
	var out [][]any
	for {
		r, err := s.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
}

func TestCSVDetectsDelimiterAndDedupesHeaders(t *testing.T) {
	src, err := Open(strings.NewReader("\xEF\xBB\xBFid;name;name;\n1;\"Ada; Lovelace\";x;\n2;Bob;;\n"), Options{Format: "csv", Header: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := src.Columns(); !reflect.DeepEqual(got, []string{"id", "name", "name (2)", "Column 4"}) {
		t.Errorf("columns %v", got)
	}
	rows := all(t, src)
	if len(rows) != 2 || rows[0][1] != "Ada; Lovelace" || rows[1][2] != "" || src.Record() != 2 {
		t.Errorf("rows %#v", rows)
	}
	noHeader, _ := Open(strings.NewReader("1,a\n2,b\n"), Options{Format: "csv"})
	if r := all(t, noHeader); len(r) != 2 || noHeader.Columns()[0] != "Column 1" {
		t.Errorf("headerless: %v %v", noHeader.Columns(), r)
	}
}

func TestTSVUnescapes(t *testing.T) {
	src, _ := Open(strings.NewReader("a\tb\nline\\none\tx\\ty\n"), Options{Format: "tsv", Header: true})
	rows := all(t, src)
	if rows[0][0] != "line\none" || rows[0][1] != "x\ty" {
		t.Errorf("%#v", rows)
	}
}

func TestJSONKeepsFieldOrder(t *testing.T) {
	src, err := Open(strings.NewReader(`[{"b":1,"a":{"x":true}},{"a":null,"c":"new"}]`), Options{Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	if got := src.Columns(); !reflect.DeepEqual(got, []string{"b", "a", "c"}) {
		t.Errorf("columns %v", got)
	}
	rows := all(t, src)
	if rows[0][0].(interface{ String() string }).String() != "1" || rows[1][2] != "new" || rows[1][1] != nil {
		t.Errorf("%#v", rows)
	}
	nd, _ := Open(strings.NewReader("{\"id\":1}\n\n{\"id\":2}\n"), Options{Format: "ndjson"})
	if r := all(t, nd); len(r) != 2 {
		t.Errorf("ndjson %v", r)
	}
	if _, err := Open(strings.NewReader(`{"not":"an array"}`), Options{Format: "json"}); err == nil {
		t.Error("object accepted as JSON import")
	}
}

func TestXLSXRoundTripWithExporter(t *testing.T) {
	var buf bytes.Buffer
	w, _ := export.New(&buf, export.Options{Format: export.XLSX, Header: true})
	w.Begin([]driver.ResultColumn{{Name: "id", Kind: driver.KindInt}, {Name: "name"}, {Name: "ok", Kind: driver.KindBool}, {Name: "big", Kind: driver.KindDecimal}})
	w.Row([]any{int64(1), "Ada & <co>", true, "12345678901234567890.5"})
	w.Row([]any{int64(2), nil, false, "1.5"})
	w.Close()
	src, err := Open(&buf, Options{Format: "xlsx", Header: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src.Columns(), []string{"id", "name", "ok", "big"}) {
		t.Errorf("columns %v", src.Columns())
	}
	want := [][]any{{"1", "Ada & <co>", true, "12345678901234567890.5"}, {"2", nil, false, "1.5"}}
	if got := all(t, src); !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v", got)
	}
}

func TestXLSXSharedStringsSparseCellsAndDates(t *testing.T) {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	add := func(name, body string) { f, _ := z.Create(name); f.Write([]byte(body)) }
	add("xl/workbook.xml", `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Data" sheetId="1" r:id="rId7"/></sheets></workbook>`)
	add("xl/_rels/workbook.xml.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId7" Target="worksheets/data.xml"/></Relationships>`)
	add("xl/sharedStrings.xml", `<sst><si><t>when</t></si><si><t>who</t></si><si><r><t>Ada </t></r><r><t>L.</t></r><rPh><t>x</t></rPh></si></sst>`)
	add("xl/styles.xml", `<styleSheet><numFmts><numFmt numFmtId="164" formatCode="yyyy\-mm\-dd\ hh:mm"/><numFmt numFmtId="165" formatCode="&quot;d&quot;0.00"/></numFmts><cellXfs><xf numFmtId="0"/><xf numFmtId="14"/><xf numFmtId="164"/><xf numFmtId="165"/></cellXfs></styleSheet>`)
	add("xl/worksheets/data.xml", `<worksheet><sheetData>
		<row r="1"><c r="A1" t="s"><v>0</v></c><c r="C1" t="s"><v>1</v></c></row>
		<row r="2"><c r="A2" s="1"><v>45351</v></c><c r="C2" t="s"><v>2</v></c><c r="D2" s="3"><v>7.25</v></c></row>
		<row r="3"><c r="A3" s="2"><v>45351.5</v></c><c r="B3" t="e"><v>#N/A</v></c><c r="C3"/></row>
	</sheetData></worksheet>`)
	z.Close()
	src, err := Open(&buf, Options{Format: "xlsx", Header: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src.Columns(), []string{"when", "Column 2", "who"}) {
		t.Errorf("columns %v", src.Columns())
	}
	rows := all(t, src)
	want := [][]any{{"2024-02-29", nil, "Ada L."}, {"2024-02-29 12:00:00", nil, nil}}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("got %#v", rows)
	}
}

func TestPrepare(t *testing.T) {
	col := func(k driver.ValueKind) *driver.Column { return &driver.Column{Name: "c", Kind: k} }
	cases := []struct {
		v    any
		c    *driver.Column
		opts Options
		want any
	}{
		{"", col(driver.KindString), Options{}, ""},
		{"", col(driver.KindString), Options{EmptyAsNull: true}, nil},
		{"", col(driver.KindInt), Options{}, nil},
		{`\N`, col(driver.KindString), Options{NullText: `\N`}, nil},
		{" 42 ", col(driver.KindInt), Options{}, "42"},
		{"Yes", col(driver.KindBool), Options{}, true},
		{"0x00FF", col(driver.KindBinary), Options{}, map[string]any{"$bin": "AP8="}},
		{map[string]any{"$oid": "65f0"}, col(driver.KindString), Options{}, "65f0"},
		{map[string]any{"a": []any{1.0}}, col(driver.KindJSON), Options{}, `{"a":[1]}`},
	}
	for _, tc := range cases {
		got, err := Prepare(tc.v, tc.c, tc.opts)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Prepare(%#v, %s) = %#v, %v; want %#v", tc.v, tc.c.Kind, got, err, tc.want)
		}
	}
	if _, err := Prepare("maybe", col(driver.KindBool), Options{}); err == nil {
		t.Error("accepted a non-boolean")
	}
}
