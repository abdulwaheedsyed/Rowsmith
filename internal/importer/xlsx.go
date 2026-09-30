package importer

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"strconv"
	"strings"
	"time"
)

// maxXLSX bounds the workbook size read into memory (zip needs random access).
const maxXLSX = 200 << 20

type xlsxSource struct {
	dec     *xml.Decoder
	shared  []string
	dateFmt map[int]bool // style index -> formats a date
	cols    []string
	first   []any
	n       int
	width   int
}

func newXLSX(r io.Reader, opts Options) (*xlsxSource, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxXLSX+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxXLSX {
		return nil, errors.New("Excel files larger than 200 MB cannot be imported; save the sheet as CSV")
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("not an Excel workbook (.xlsx)")
	}
	files := map[string]*zip.File{}
	for _, f := range z.File {
		files[f.Name] = f
	}
	s := &xlsxSource{dateFmt: map[int]bool{}}
	if f := files["xl/sharedStrings.xml"]; f != nil {
		if s.shared, err = readShared(f); err != nil {
			return nil, err
		}
	}
	if f := files["xl/styles.xml"]; f != nil {
		s.dateFmt = readDateStyles(f)
	}
	sheet := firstSheet(files)
	if sheet == nil {
		return nil, errors.New("the workbook has no sheets")
	}
	rc, err := sheet.Open()
	if err != nil {
		return nil, err
	}
	// The decompressed sheet streams; the reader stays open until EOF.
	s.dec = xml.NewDecoder(rc)
	first, err := s.row()
	if err == io.EOF {
		s.cols = []string{}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if opts.Header {
		names := make([]string, len(first))
		for i, v := range first {
			names[i], _ = v.(string)
			if names[i] == "" && v != nil {
				names[i] = fmt.Sprint(v)
			}
		}
		s.cols = headerNames(names, len(first))
	} else {
		s.cols = numbered(len(first))
		s.first = first
	}
	s.width = len(s.cols)
	return s, nil
}

// firstSheet follows workbook.xml to the first sheet's part.
func firstSheet(files map[string]*zip.File) *zip.File {
	var wb struct {
		Sheets []struct {
			RID string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	var rels struct {
		Rel []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if decodeZip(files["xl/workbook.xml"], &wb) == nil && decodeZip(files["xl/_rels/workbook.xml.rels"], &rels) == nil && len(wb.Sheets) > 0 {
		for _, r := range rels.Rel {
			if r.ID == wb.Sheets[0].RID {
				t := strings.TrimPrefix(r.Target, "/")
				if !strings.HasPrefix(t, "xl/") {
					t = path.Join("xl", t)
				}
				if f := files[t]; f != nil {
					return f
				}
			}
		}
	}
	return files["xl/worksheets/sheet1.xml"]
}

func decodeZip(f *zip.File, v any) error {
	if f == nil {
		return errors.New("missing part")
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	return xml.NewDecoder(rc).Decode(v)
}

func readShared(f *zip.File) ([]string, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	d := xml.NewDecoder(rc)
	var out []string
	var cur strings.Builder
	inSI, inT, inRPh := false, false, false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				inT = inSI
			case "rPh": // phonetic hints are not part of the text
				inRPh = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				out = append(out, cur.String())
				inSI = false
			case "t":
				inT = false
			case "rPh":
				inRPh = false
			}
		case xml.CharData:
			if inT && !inRPh {
				cur.Write(t)
			}
		}
	}
}

// readDateStyles finds cell styles whose number format shows a date or time.
func readDateStyles(f *zip.File) map[int]bool {
	var st struct {
		NumFmts []struct {
			ID   int    `xml:"numFmtId,attr"`
			Code string `xml:"formatCode,attr"`
		} `xml:"numFmts>numFmt"`
		Xfs []struct {
			NumFmt int `xml:"numFmtId,attr"`
		} `xml:"cellXfs>xf"`
	}
	out := map[int]bool{}
	if decodeZip(f, &st) != nil {
		return out
	}
	custom := map[int]bool{}
	for _, nf := range st.NumFmts {
		custom[nf.ID] = isDateCode(nf.Code)
	}
	for i, xf := range st.Xfs {
		id := xf.NumFmt
		if (id >= 14 && id <= 22) || (id >= 45 && id <= 47) || custom[id] {
			out[i] = true
		}
	}
	return out
}

func isDateCode(code string) bool {
	c := strings.ToLower(code)
	// Drop quoted text and bracketed parts like [Red] or [$-409].
	var b strings.Builder
	quoted, bracket := false, false
	for _, r := range c {
		switch {
		case r == '"':
			quoted = !quoted
		case quoted:
		case r == '[':
			bracket = true
		case r == ']':
			bracket = false
		case bracket:
		default:
			b.WriteRune(r)
		}
	}
	c = b.String()
	return strings.ContainsAny(c, "dy") || (strings.Contains(c, "h") && strings.Contains(c, "m")) || strings.Contains(c, "ss")
}

// excelTime converts a serial date (1900 system) to text.
func excelTime(v float64) string {
	base := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	days := math.Floor(v)
	secs := math.Round((v - days) * 86400)
	t := base.AddDate(0, 0, int(days)).Add(time.Duration(secs) * time.Second)
	switch {
	case days == 0 && secs > 0:
		return t.Format("15:04:05")
	case secs == 0:
		return t.Format("2006-01-02")
	}
	return t.Format("2006-01-02 15:04:05")
}

// colIndex turns "C12" into 2.
func colIndex(ref string) int {
	n := 0
	for _, r := range ref {
		if r < 'A' || r > 'Z' {
			break
		}
		n = n*26 + int(r-'A'+1)
	}
	return n - 1
}

// row reads the next <row> as values; missing cells are nil.
func (s *xlsxSource) row() ([]any, error) {
	for {
		tok, err := s.dec.Token()
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "row" {
			continue
		}
		var vals []any
		for {
			tok, err := s.dec.Token()
			if err != nil {
				return nil, err
			}
			if ee, ok := tok.(xml.EndElement); ok && ee.Name.Local == "row" {
				return vals, nil
			}
			ce, ok := tok.(xml.StartElement)
			if !ok || ce.Name.Local != "c" {
				continue
			}
			var ref, typ string
			style := -1
			for _, a := range ce.Attr {
				switch a.Name.Local {
				case "r":
					ref = a.Value
				case "t":
					typ = a.Value
				case "s":
					style, _ = strconv.Atoi(a.Value)
				}
			}
			text, err := s.cellText(ce.Name.Local)
			if err != nil {
				return nil, err
			}
			idx := len(vals)
			if ref != "" {
				idx = colIndex(ref)
			}
			if idx < 0 || idx > 16383 {
				continue
			}
			for len(vals) <= idx {
				vals = append(vals, nil)
			}
			vals[idx] = s.value(typ, text, style)
		}
	}
}

// cellText collects <v> or inline <is><t> text of the current cell.
func (s *xlsxSource) cellText(end string) (string, error) {
	var b strings.Builder
	depth, capture := 0, false
	for {
		tok, err := s.dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			capture = t.Name.Local == "v" || t.Name.Local == "t"
			if t.Name.Local == "rPh" {
				if err := s.dec.Skip(); err != nil {
					return "", err
				}
				depth--
			}
		case xml.EndElement:
			if depth == 0 && t.Name.Local == end {
				return b.String(), nil
			}
			depth--
			capture = false
		case xml.CharData:
			if capture {
				b.Write(t)
			}
		}
	}
}

func (s *xlsxSource) value(typ, text string, style int) any {
	switch typ {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(text))
		if err == nil && i >= 0 && i < len(s.shared) {
			return s.shared[i]
		}
		return nil
	case "inlineStr", "str":
		return text
	case "b":
		return strings.TrimSpace(text) == "1"
	case "e":
		return nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if s.dateFmt[style] {
		if f, err := strconv.ParseFloat(text, 64); err == nil {
			return excelTime(f)
		}
	}
	return text // numbers keep their written precision
}

func (s *xlsxSource) Columns() []string { return s.cols }
func (s *xlsxSource) Record() int       { return s.n }

func (s *xlsxSource) Next() ([]any, error) {
	var rec []any
	if s.first != nil {
		rec, s.first = s.first, nil
	} else {
		var err error
		if rec, err = s.row(); err != nil {
			return nil, err
		}
	}
	s.n++
	out := make([]any, s.width)
	copy(out, rec)
	return out, nil
}
