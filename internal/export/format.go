package export

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"rowsmith/internal/driver"
)

// Format names accepted by New.
const (
	CSV    = "csv"
	TSV    = "tsv"
	JSON   = "json"
	NDJSON = "ndjson"
	XLSX   = "xlsx"
	SQL    = "sql"
)

// Options configure a row writer.
type Options struct {
	Format    string `json:"format"`
	Delimiter string `json:"delimiter,omitempty"` // CSV only; default ","
	Header    bool   `json:"header"`              // CSV/TSV/XLSX column names in the first row
	NullText  string `json:"nullText,omitempty"`  // CSV/TSV text for NULL (default empty)
	BOM       bool   `json:"bom,omitempty"`       // CSV/TSV: UTF-8 byte order mark, for Excel
	// SQL INSERT output.
	Dialect   string           `json:"-"`
	Table     driver.ObjectRef `json:"table"`     // target of the INSERT statements
	BatchRows int              `json:"batchRows"` // rows per INSERT statement
	// Documents keeps Extended JSON types (document stores).
	Documents bool `json:"-"`
	// SheetName for XLSX.
	SheetName string `json:"sheetName,omitempty"`
	// Hints are the table's columns for table exports: geometry that the
	// browse query returns as GeoJSON text is written back as geometry.
	Hints map[string]driver.Column `json:"-"`
	// Values replaces the VALUES keyword (e.g. "OVERRIDING SYSTEM VALUE VALUES").
	Values string `json:"-"`
	// QuoteIdent quotes identifiers for SQL output.
	QuoteIdent func(string) string `json:"-"`
	// Qualify renders the INSERT target; defaults to QuoteIdent(Table.Name).
	Qualify func(driver.ObjectRef) string `json:"-"`
}

// Extension is the file name extension for a format.
func Extension(format string) string {
	switch format {
	case NDJSON:
		return "ndjson"
	case "":
		return "csv"
	}
	return format
}

// ContentType is the media type served for a format.
func ContentType(format string) string {
	switch format {
	case CSV:
		return "text/csv; charset=utf-8"
	case TSV:
		return "text/tab-separated-values; charset=utf-8"
	case JSON:
		return "application/json"
	case NDJSON:
		return "application/x-ndjson"
	case XLSX:
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case SQL:
		return "application/sql; charset=utf-8"
	}
	return "application/octet-stream"
}

// Writer receives one result set.
type Writer interface {
	Begin(cols []driver.ResultColumn) error
	Row(cells []any) error
	// Close finishes the file (it does not close the underlying io.Writer).
	Close() error
}

// ErrTooManyRows stops an Excel export at the sheet limit.
var ErrTooManyRows = errors.New("an Excel sheet holds at most 1,048,575 data rows; export as CSV instead")

// New returns a writer for opts.Format writing to w.
func New(w io.Writer, opts Options) (Writer, error) {
	switch opts.Format {
	case CSV, TSV, "":
		return newDelimited(w, opts)
	case JSON, NDJSON:
		return &jsonWriter{w: bufio.NewWriterSize(w, 64<<10), lines: opts.Format == NDJSON, docs: opts.Documents}, nil
	case XLSX:
		return newXLSX(w, opts), nil
	case SQL:
		if opts.QuoteIdent == nil {
			return nil, errors.New("SQL export needs an identifier quoting rule")
		}
		if opts.Table.Name == "" {
			return nil, errors.New("enter the table name for the INSERT statements")
		}
		return newInsertWriter(w, opts), nil
	}
	return nil, fmt.Errorf("unknown export format %q", opts.Format)
}

// ---- CSV / TSV --------------------------------------------------------------

type delimited struct {
	w    *bufio.Writer
	c    *csv.Writer
	opts Options
	rec  []string
	tsv  bool
}

func newDelimited(w io.Writer, opts Options) (*delimited, error) {
	bw := bufio.NewWriterSize(w, 64<<10)
	d := &delimited{w: bw, opts: opts, tsv: opts.Format == TSV}
	if opts.BOM {
		bw.WriteString("\xEF\xBB\xBF")
	}
	if !d.tsv {
		d.c = csv.NewWriter(bw)
		if opts.Delimiter != "" {
			r := []rune(opts.Delimiter)
			if len(r) != 1 || r[0] == '"' || r[0] == '\r' || r[0] == '\n' {
				return nil, errors.New("the delimiter must be a single character other than a quote or line break")
			}
			d.c.Comma = r[0]
		}
	}
	return d, nil
}

func (d *delimited) write(rec []string) error {
	if d.tsv {
		for i, f := range rec {
			if i > 0 {
				d.w.WriteByte('\t')
			}
			d.w.WriteString(tsvEscape.Replace(f))
		}
		return d.w.WriteByte('\n')
	}
	return d.c.Write(rec)
}

var tsvEscape = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

func (d *delimited) Begin(cols []driver.ResultColumn) error {
	d.rec = make([]string, len(cols))
	if !d.opts.Header {
		return nil
	}
	for i, c := range cols {
		d.rec[i] = c.Name
	}
	return d.write(d.rec)
}

func (d *delimited) Row(cells []any) error {
	for i := range d.rec {
		var v any
		if i < len(cells) {
			v = cells[i]
		}
		s, null := Text(v)
		if null {
			s = d.opts.NullText
		}
		d.rec[i] = s
	}
	return d.write(d.rec)
}

func (d *delimited) Close() error {
	if d.c != nil {
		d.c.Flush()
		if err := d.c.Error(); err != nil {
			return err
		}
	}
	return d.w.Flush()
}

// ---- JSON / NDJSON ----------------------------------------------------------

type jsonWriter struct {
	w     *bufio.Writer
	lines bool
	docs  bool
	cols  []driver.ResultColumn
	n     int64
}

func (j *jsonWriter) Begin(cols []driver.ResultColumn) error {
	j.cols = cols
	if !j.lines {
		_, err := j.w.WriteString("[")
		return err
	}
	return nil
}

func (j *jsonWriter) Row(cells []any) error {
	obj := orderedObject{}
	for i, c := range j.cols {
		var v any
		if i < len(cells) {
			v = cells[i]
		}
		if j.docs {
			if v == nil {
				continue // absent field
			}
			if c.Name == "…" {
				// Fields outside the inferred columns travel in one object.
				switch m := v.(type) {
				case driver.Doc:
					for _, k := range m.Keys {
						obj = append(obj, kv{k, docJSON(m.Values[k])})
					}
				case map[string]any:
					for k, e := range m {
						obj = append(obj, kv{k, docJSON(e)})
					}
				}
				continue
			}
			obj = append(obj, kv{c.Name, docJSON(v)})
		} else {
			obj = append(obj, kv{c.Name, plainJSON(v)})
		}
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	if j.lines {
		j.w.Write(b)
		return j.w.WriteByte('\n')
	}
	if j.n > 0 {
		j.w.WriteByte(',')
	}
	j.n++
	j.w.WriteString("\n  ")
	_, err = j.w.Write(b)
	return err
}

func (j *jsonWriter) Close() error {
	if !j.lines {
		if j.cols == nil {
			j.w.WriteString("[")
		}
		if j.n > 0 {
			j.w.WriteString("\n")
		}
		j.w.WriteString("]\n")
	}
	return j.w.Flush()
}

type kv struct {
	k string
	v any
}

// orderedObject marshals as a JSON object keeping column order.
type orderedObject []kv

func (o orderedObject) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(e.k)
		b.Write(k)
		b.WriteByte(':')
		v, err := json.Marshal(e.v)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// ---- SQL INSERT -------------------------------------------------------------

type insertWriter struct {
	w      *bufio.Writer
	opts   Options
	cols   []driver.ResultColumn
	prefix string
	batch  int
	n      int
	Stats  LiteralStats
}

func newInsertWriter(w io.Writer, opts Options) *insertWriter {
	b := opts.BatchRows
	switch {
	case opts.Dialect == Oracle:
		b = 1 // no multi-row VALUES before 23ai
	case b <= 0:
		b = 100
	case opts.Dialect == MSSQL && b > 1000:
		b = 1000 // T-SQL row constructor limit
	}
	return &insertWriter{w: bufio.NewWriterSize(w, 64<<10), opts: opts, batch: b}
}

func (s *insertWriter) Begin(cols []driver.ResultColumn) error {
	s.cols = cols
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = s.opts.QuoteIdent(c.Name)
	}
	target := s.opts.QuoteIdent(s.opts.Table.Name)
	if s.opts.Qualify != nil {
		target = s.opts.Qualify(s.opts.Table)
	}
	values := s.opts.Values
	if values == "" {
		values = "VALUES"
	}
	s.prefix = "INSERT INTO " + target + " (" + strings.Join(names, ", ") + ") " + values
	return nil
}

func (s *insertWriter) Row(cells []any) error {
	if s.n == 0 {
		s.w.WriteString(s.prefix)
		s.w.WriteString("\n  (")
	} else {
		s.w.WriteString(",\n  (")
	}
	for i, c := range s.cols {
		if i > 0 {
			s.w.WriteString(", ")
		}
		var v any
		if i < len(cells) {
			v = cells[i]
		}
		if h, ok := s.opts.Hints[c.Name]; ok && h.Kind == driver.KindGeometry {
			if str, ok := v.(string); ok && strings.HasPrefix(strings.TrimSpace(str), "{") {
				s.w.WriteString(geoLit(s.opts.Dialect, strings.ToLower(h.Type), json.RawMessage(str), "", h.SRID, &s.Stats))
				continue
			}
		}
		s.w.WriteString(Literal(s.opts.Dialect, c, v, &s.Stats))
	}
	s.w.WriteByte(')')
	s.n++
	if s.n >= s.batch {
		return s.flush()
	}
	return nil
}

func (s *insertWriter) flush() error {
	if s.n == 0 {
		return nil
	}
	s.n = 0
	_, err := s.w.WriteString(";\n")
	return err
}

func (s *insertWriter) Close() error {
	if err := s.flush(); err != nil {
		return err
	}
	return s.w.Flush()
}
