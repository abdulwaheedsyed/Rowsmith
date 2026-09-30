// Package importer reads CSV, TSV, JSON, NDJSON and Excel files and loads
// their records into a table through a driver.BulkImporter.
package importer

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Options describe how to read a file.
type Options struct {
	Format    string `json:"format"`    // csv tsv json ndjson xlsx
	Delimiter string `json:"delimiter"` // csv: "" detects , ; tab or |
	Header    bool   `json:"header"`    // first row holds column names (csv, tsv, xlsx)
	NullText  string `json:"nullText"`  // text that means NULL, e.g. \N or NULL
	// EmptyAsNull turns empty fields into NULL for text columns too; other
	// columns always read an empty field as NULL.
	EmptyAsNull bool `json:"emptyAsNull"`
	// Documents keeps Extended JSON values as they are, for document stores.
	Documents bool `json:"-"`
}

// Source yields records as values aligned to Columns.
type Source interface {
	Columns() []string
	// Next returns the next record, or io.EOF.
	Next() ([]any, error)
	// Record is the 1-based number of the last record returned.
	Record() int
}

// DetectFormat guesses the format from a file name.
func DetectFormat(name string) string {
	n := strings.ToLower(strings.TrimSuffix(strings.ToLower(name), ".gz"))
	for _, ext := range []string{"csv", "tsv", "ndjson", "jsonl", "json", "xlsx", "sql", "js"} {
		if strings.HasSuffix(n, "."+ext) {
			switch ext {
			case "jsonl":
				return "ndjson"
			case "js":
				return "sql" // console scripts (MongoDB)
			}
			return ext
		}
	}
	if strings.HasSuffix(n, ".txt") {
		return "tsv"
	}
	return "csv"
}

// Open reads r as opts.Format. Excel needs random access, so xlsx data is
// read into memory up to maxXLSX bytes.
func Open(r io.Reader, opts Options) (Source, error) {
	switch opts.Format {
	case "csv", "":
		return newCSV(r, opts)
	case "tsv":
		return newTSV(r, opts), nil
	case "json":
		return newJSON(r, false)
	case "ndjson":
		return newJSON(r, true)
	case "xlsx":
		return newXLSX(r, opts)
	}
	return nil, fmt.Errorf("files of type %q cannot be imported into a table", opts.Format)
}

func headerNames(rec []string, n int) []string {
	out := make([]string, n)
	seen := map[string]int{}
	for i := range out {
		name := ""
		if i < len(rec) {
			name = strings.TrimSpace(rec[i])
		}
		if name == "" {
			name = fmt.Sprintf("Column %d", i+1)
		}
		if k := seen[name]; k > 0 {
			seen[name]++
			name = fmt.Sprintf("%s (%d)", name, k+1)
		} else {
			seen[name] = 1
		}
		out[i] = name
	}
	return out
}

func numbered(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("Column %d", i+1)
	}
	return out
}

// ---- CSV --------------------------------------------------------------------

type csvSource struct {
	r      *csv.Reader
	cols   []string
	first  []string
	n      int
	header bool
}

func stripBOM(br *bufio.Reader) {
	if b, err := br.Peek(3); err == nil && bytes.Equal(b, []byte{0xEF, 0xBB, 0xBF}) {
		br.Discard(3)
	}
}

// detectDelimiter picks the separator that occurs most, outside quotes, in
// the first line.
func detectDelimiter(br *bufio.Reader) rune {
	peek, _ := br.Peek(64 << 10)
	line := peek
	quoted := false
	counts := map[rune]int{}
	for _, c := range string(line) {
		if c == '"' {
			quoted = !quoted
			continue
		}
		if quoted {
			continue
		}
		if c == '\n' {
			break
		}
		switch c {
		case ',', ';', '\t', '|':
			counts[c]++
		}
	}
	best, n := ',', 0
	for _, c := range []rune{',', ';', '\t', '|'} {
		if counts[c] > n {
			best, n = c, counts[c]
		}
	}
	return best
}

func newCSV(r io.Reader, opts Options) (*csvSource, error) {
	br := bufio.NewReaderSize(r, 256<<10)
	stripBOM(br)
	c := csv.NewReader(br)
	c.FieldsPerRecord = -1
	c.LazyQuotes = true
	c.ReuseRecord = false
	if opts.Delimiter != "" {
		d := []rune(opts.Delimiter)
		if opts.Delimiter == `\t` {
			d = []rune{'\t'}
		}
		if len(d) != 1 {
			return nil, errors.New("the delimiter must be a single character")
		}
		c.Comma = d[0]
	} else {
		c.Comma = detectDelimiter(br)
	}
	s := &csvSource{r: c, header: opts.Header}
	first, err := c.Read()
	if err == io.EOF {
		s.cols = []string{}
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("line 1: %w", err)
	}
	if opts.Header {
		s.cols = headerNames(first, len(first))
	} else {
		s.cols = numbered(len(first))
		s.first = first
	}
	return s, nil
}

func (s *csvSource) Columns() []string { return s.cols }
func (s *csvSource) Record() int       { return s.n }

func (s *csvSource) Next() ([]any, error) {
	var rec []string
	if s.first != nil {
		rec, s.first = s.first, nil
	} else {
		var err error
		rec, err = s.r.Read()
		if err != nil {
			if err == io.EOF {
				return nil, io.EOF
			}
			line, _ := s.r.FieldPos(0)
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
	}
	s.n++
	out := make([]any, len(s.cols))
	for i := range out {
		if i < len(rec) {
			out[i] = rec[i]
		}
	}
	return out, nil
}

// ---- TSV (backslash escapes, as the exporter writes) -----------------------

type tsvSource struct {
	sc     *bufio.Scanner
	cols   []string
	first  []string
	n      int
	err    error
	primed bool
	opts   Options
}

var tsvUnescape = strings.NewReplacer(`\t`, "\t", `\n`, "\n", `\r`, "\r", `\\`, `\`)

func newTSV(r io.Reader, opts Options) *tsvSource {
	br := bufio.NewReaderSize(r, 256<<10)
	stripBOM(br)
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	s := &tsvSource{sc: sc, opts: opts}
	if sc.Scan() {
		first := s.split(sc.Text())
		if opts.Header {
			s.cols = headerNames(first, len(first))
		} else {
			s.cols = numbered(len(first))
			s.first = first
		}
	} else {
		s.cols = []string{}
		s.err = sc.Err()
	}
	return s
}

func (s *tsvSource) split(line string) []string {
	parts := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
	for i, p := range parts {
		parts[i] = tsvUnescape.Replace(p)
	}
	return parts
}

func (s *tsvSource) Columns() []string { return s.cols }
func (s *tsvSource) Record() int       { return s.n }

func (s *tsvSource) Next() ([]any, error) {
	var rec []string
	if s.first != nil {
		rec, s.first = s.first, nil
	} else {
		if s.err != nil {
			return nil, s.err
		}
		if !s.sc.Scan() {
			if err := s.sc.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		rec = s.split(s.sc.Text())
	}
	s.n++
	out := make([]any, len(s.cols))
	for i := range out {
		if i < len(rec) {
			out[i] = rec[i]
		}
	}
	return out, nil
}

// ---- JSON array / NDJSON ----------------------------------------------------

// jsonSource buffers the first records to learn the field names, then
// streams the rest. Fields first seen later in the file are ignored.
type jsonSource struct {
	dec   *json.Decoder
	lines bool
	cols  []string
	index map[string]int
	buf   []map[string]any
	n     int
	done  bool
}

const jsonSample = 500

func newJSON(r io.Reader, lines bool) (*jsonSource, error) {
	br := bufio.NewReaderSize(r, 256<<10)
	stripBOM(br)
	dec := json.NewDecoder(br)
	dec.UseNumber()
	s := &jsonSource{dec: dec, lines: lines, index: map[string]int{}}
	if !lines {
		tok, err := dec.Token()
		if err == io.EOF {
			s.done = true
			return s, nil
		}
		if err != nil {
			return nil, fmt.Errorf("not a JSON file: %w", err)
		}
		if d, ok := tok.(json.Delim); !ok || d != '[' {
			return nil, errors.New("a JSON import must be an array of objects: [ {…}, {…} ]")
		}
	}
	for len(s.buf) < jsonSample {
		m, err := s.read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		s.buf = append(s.buf, m)
		for _, k := range keysInOrder(m) {
			if _, ok := s.index[k]; !ok {
				s.index[k] = len(s.cols)
				s.cols = append(s.cols, k)
			}
		}
	}
	return s, nil
}

func (s *jsonSource) read() (map[string]any, error) {
	if s.done {
		return nil, io.EOF
	}
	if !s.lines && !s.dec.More() {
		s.done = true
		return nil, io.EOF
	}
	var raw json.RawMessage
	if err := s.dec.Decode(&raw); err != nil {
		if err == io.EOF {
			s.done = true
			return nil, io.EOF
		}
		return nil, fmt.Errorf("record %d: %w", s.n+len(s.buf)+1, err)
	}
	var m map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&m); err != nil || m == nil {
		return nil, fmt.Errorf("record %d is not a JSON object", s.n+len(s.buf)+1)
	}
	m["\x00order"] = rawKeyOrder(raw)
	return m, nil
}

// rawKeyOrder returns the top-level keys of an object in file order.
func rawKeyOrder(raw json.RawMessage) []string {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if _, err := d.Token(); err != nil {
		return nil
	}
	var keys []string
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return keys
		}
		k, _ := t.(string)
		keys = append(keys, k)
		var skip json.RawMessage
		if d.Decode(&skip) != nil {
			return keys
		}
	}
	return keys
}

func keysInOrder(m map[string]any) []string {
	if ks, ok := m["\x00order"].([]string); ok {
		return ks
	}
	return nil
}

func (s *jsonSource) Columns() []string { return s.cols }
func (s *jsonSource) Record() int       { return s.n }

func (s *jsonSource) Next() ([]any, error) {
	var m map[string]any
	if len(s.buf) > 0 {
		m, s.buf = s.buf[0], s.buf[1:]
	} else {
		var err error
		if m, err = s.read(); err != nil {
			return nil, err
		}
	}
	s.n++
	out := make([]any, len(s.cols))
	for k, v := range m {
		if i, ok := s.index[k]; ok {
			out[i] = v
		}
	}
	return out, nil
}
