package export

import (
	"archive/zip"
	"bufio"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"rowsmith/internal/driver"
)

// A minimal streaming Office Open XML workbook: one sheet, inline strings,
// a bold frozen header row with a filter. Rows go straight to the zip
// stream, so memory use does not grow with the export.

const (
	xlsxMaxRows = 1048576
	xlsxMaxText = 32767
)

type xlsxWriter struct {
	z     *zip.Writer
	sheet *bufio.Writer
	opts  Options
	cols  []driver.ResultColumn
	refs  []string
	row   int
	err   error
	// Truncated counts cells cut to Excel's 32,767-character limit.
	Truncated int
}

func newXLSX(w io.Writer, opts Options) *xlsxWriter {
	x := &xlsxWriter{z: zip.NewWriter(w), opts: opts}
	name := opts.SheetName
	if name == "" {
		name = "Export"
	}
	name = sheetName(name)
	for _, f := range []struct{ path, body string }{
		{"[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/></Types>`},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="` + xmlEscape(name) + `" sheetId="1" r:id="rId1"/></sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>`},
		{"xl/styles.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts><fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills><borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="2"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/></cellXfs></styleSheet>`},
	} {
		if x.err != nil {
			break
		}
		var fw io.Writer
		if fw, x.err = x.z.Create(f.path); x.err == nil {
			_, x.err = io.WriteString(fw, f.body)
		}
	}
	return x
}

// sheetName applies Excel's sheet name rules.
func sheetName(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`[]:*?/\`, r) {
			return '_'
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > 31 {
		s = string([]rune(s)[:31])
	}
	return s
}

func colRef(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}

func (x *xlsxWriter) Begin(cols []driver.ResultColumn) error {
	if x.err != nil {
		return x.err
	}
	fw, err := x.z.Create("xl/worksheets/sheet1.xml")
	if err != nil {
		return err
	}
	x.sheet = bufio.NewWriterSize(fw, 64<<10)
	x.cols = cols
	x.refs = make([]string, len(cols))
	for i := range cols {
		x.refs[i] = colRef(i)
	}
	w := x.sheet
	w.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" +
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	if x.opts.Header {
		w.WriteString(`<sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>`)
	}
	if len(cols) > 0 {
		w.WriteString(`<cols><col min="1" max="` + strconv.Itoa(len(cols)) + `" width="18" customWidth="1"/></cols>`)
	}
	w.WriteString(`<sheetData>`)
	if x.opts.Header {
		x.row++
		w.WriteString(`<row r="1">`)
		for i, c := range cols {
			x.inline(x.refs[i]+"1", c.Name, true)
		}
		w.WriteString(`</row>`)
	}
	return nil
}

func (x *xlsxWriter) inline(ref, s string, bold bool) {
	if len(s) > xlsxMaxText {
		s = s[:xlsxMaxText]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		x.Truncated++
	}
	x.sheet.WriteString(`<c r="` + ref + `" t="inlineStr"`)
	if bold {
		x.sheet.WriteString(` s="1"`)
	}
	x.sheet.WriteString(`><is><t xml:space="preserve">` + xmlEscape(s) + `</t></is></c>`)
}

func (x *xlsxWriter) Row(cells []any) error {
	if x.row >= xlsxMaxRows {
		return ErrTooManyRows
	}
	x.row++
	r := strconv.Itoa(x.row)
	w := x.sheet
	w.WriteString(`<row r="` + r + `">`)
	for i, c := range x.cols {
		var v any
		if i < len(cells) {
			v = cells[i]
		}
		ref := x.refs[i] + r
		switch t := v.(type) {
		case nil:
			continue
		case bool:
			b := "0"
			if t {
				b = "1"
			}
			w.WriteString(`<c r="` + ref + `" t="b"><v>` + b + `</v></c>`)
			continue
		case int64, int, int32, float64, float32:
			s, _ := Text(t)
			if excelNumber(s) {
				w.WriteString(`<c r="` + ref + `"><v>` + s + `</v></c>`)
				continue
			}
		case string:
			if (c.Kind == driver.KindInt || c.Kind == driver.KindFloat || c.Kind == driver.KindDecimal) && excelNumber(t) {
				w.WriteString(`<c r="` + ref + `"><v>` + t + `</v></c>`)
				continue
			}
		}
		s, _ := Text(v)
		x.inline(ref, s, false)
	}
	_, err := w.WriteString(`</row>`)
	return err
}

// excelNumber reports whether s survives as an Excel number (15 significant
// digits); longer values stay text so no digits are lost.
func excelNumber(s string) bool {
	if !numeric.MatchString(s) {
		return false
	}
	digits := 0
	for _, r := range strings.TrimLeft(strings.SplitN(strings.ToLower(s), "e", 2)[0], "+-0.") {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return digits <= 15
}

func (x *xlsxWriter) Close() error {
	if x.err != nil {
		return x.err
	}
	if x.sheet == nil {
		if err := x.Begin(nil); err != nil {
			return err
		}
	}
	x.sheet.WriteString(`</sheetData>`)
	if x.opts.Header && len(x.cols) > 0 {
		x.sheet.WriteString(`<autoFilter ref="A1:` + x.refs[len(x.refs)-1] + `1"/>`)
	}
	x.sheet.WriteString(`</worksheet>`)
	if err := x.sheet.Flush(); err != nil {
		return err
	}
	return x.z.Close()
}

// xmlEscape escapes markup and drops characters XML 1.0 cannot carry.
func xmlEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '"':
			b.WriteString("&quot;")
		case r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || r >= 0x10000:
			b.WriteRune(r)
		}
	}
	return b.String()
}
