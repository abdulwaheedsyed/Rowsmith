package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"

	"rowsmith/internal/export"
)

// Config is what a schedule produces and who gets it.
type Config struct {
	Output   Output   `json:"output"`
	Alert    Alert    `json:"alert"`
	Delivery Delivery `json:"delivery"`
}

type Output struct {
	Format  string `json:"format"`  // csv, tsv, xlsx, json or ndjson
	Gzip    bool   `json:"gzip"`    //
	MaxRows int    `json:"maxRows"` // rows written to the file
	Attach  bool   `json:"attach"`  // attach the file to emails
	Preview int    `json:"preview"` // rows shown in the email body
}

// Alert makes a schedule notify only when a condition holds.
type Alert struct {
	Kind   string `json:"kind"`   // "" (every run), rows, value or changed
	Op     string `json:"op"`     // >, >=, <, <=, =, !=
	Value  string `json:"value"`  // number (or text for = and !=)
	Column string `json:"column"` // for value: a column of the first row; "" for the first column
	Edge   bool   `json:"edge"`   // notify only when the condition starts to hold
}

type Delivery struct {
	Emails    []string `json:"emails"`
	OnFailure bool     `json:"onFailure"` // email the owner when a run fails
}

const (
	DefaultMaxRows = 100_000
	MaxRowsLimit   = 1_000_000
	MaxRecipients  = 50
)

var formats = map[string]bool{export.CSV: true, export.TSV: true, export.XLSX: true, export.JSON: true, export.NDJSON: true}
var ops = map[string]bool{">": true, ">=": true, "<": true, "<=": true, "=": true, "!=": true}

// ParseConfig reads a stored configuration, filling in defaults.
func ParseConfig(raw string) Config {
	var c Config
	_ = json.Unmarshal([]byte(raw), &c)
	c.defaults()
	return c
}

func (c *Config) defaults() {
	if c.Output.Format == "" {
		c.Output.Format = export.CSV
	}
	if c.Output.MaxRows <= 0 {
		c.Output.MaxRows = DefaultMaxRows
	}
	if c.Delivery.Emails == nil {
		c.Delivery.Emails = []string{}
	}
}

// Normalize validates a configuration from the API and cleans it up.
func (c *Config) Normalize() error {
	c.defaults()
	o := &c.Output
	if !formats[o.Format] {
		return fmt.Errorf("unknown file format %q", o.Format)
	}
	if o.MaxRows > MaxRowsLimit {
		return fmt.Errorf("a scheduled file holds at most %d rows", MaxRowsLimit)
	}
	if o.Format == export.XLSX && o.MaxRows > 1_048_575 {
		o.MaxRows = 1_048_575
	}
	if o.Preview < 0 || o.Preview > 50 {
		return errors.New("show between 0 and 50 rows in the email")
	}
	a := &c.Alert
	switch a.Kind {
	case "":
		*a = Alert{}
	case "changed":
		a.Op, a.Value, a.Column = "", "", ""
	case "rows", "value":
		if !ops[a.Op] {
			return fmt.Errorf("unknown comparison %q", a.Op)
		}
		a.Value = strings.TrimSpace(a.Value)
		_, numErr := strconv.ParseFloat(a.Value, 64)
		if a.Kind == "rows" && numErr != nil {
			return errors.New("compare the row count with a number")
		}
		if a.Kind == "value" && numErr != nil && a.Op != "=" && a.Op != "!=" {
			return errors.New("only = and ≠ compare text; use a number for the other comparisons")
		}
		if a.Kind == "rows" {
			a.Column = ""
		}
		a.Column = strings.TrimSpace(a.Column)
	default:
		return fmt.Errorf("unknown alert condition %q", a.Kind)
	}
	seen := map[string]bool{}
	emails := []string{}
	for _, e := range c.Delivery.Emails {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		addr, err := mail.ParseAddress(e)
		if err != nil || strings.ContainsAny(addr.Address, "\r\n") {
			return fmt.Errorf("%q is not an email address", e)
		}
		k := strings.ToLower(addr.Address)
		if !seen[k] {
			seen[k] = true
			emails = append(emails, k)
		}
	}
	if len(emails) > MaxRecipients {
		return fmt.Errorf("send to at most %d addresses", MaxRecipients)
	}
	c.Delivery.Emails = emails
	return nil
}

// JSON encodes the configuration for storage.
func (c Config) JSON() string {
	b, _ := json.Marshal(c)
	return string(b)
}

// Describe says in words when the alert fires.
func (a Alert) Describe() string {
	op := map[string]string{">": "is more than", ">=": "is at least", "<": "is less than", "<=": "is at most", "=": "is", "!=": "is not"}[a.Op]
	switch a.Kind {
	case "rows":
		return fmt.Sprintf("the row count %s %s", op, a.Value)
	case "value":
		col := a.Column
		if col == "" {
			col = "the first column"
		}
		return fmt.Sprintf("%s %s %s", col, op, a.Value)
	case "changed":
		return "the result changes"
	}
	return ""
}

// Observation is what a run saw, for the alert to judge.
type Observation struct {
	Rows    int64
	Columns []string
	First   []any // first row, nil without rows
	Hash    string
}

// Check reports whether the condition holds and the value it looked at.
// An error means the condition no longer fits the result.
func (a Alert) Check(o Observation, prevHash string) (bool, string, error) {
	switch a.Kind {
	case "":
		return false, "", nil
	case "changed":
		switch {
		case prevHash == "":
			return false, "first result", nil
		case prevHash != o.Hash:
			return true, "changed", nil
		}
		return false, "unchanged", nil
	case "rows":
		n := float64(o.Rows)
		limit, _ := strconv.ParseFloat(a.Value, 64)
		return compareNum(n, a.Op, limit), strconv.FormatInt(o.Rows, 10) + " rows", nil
	case "value":
		idx := 0
		if a.Column != "" {
			idx = -1
			for i, c := range o.Columns {
				if strings.EqualFold(c, a.Column) {
					idx = i
					break
				}
			}
			if idx < 0 {
				return false, "", fmt.Errorf("the alert looks at column %q, which the query does not return", a.Column)
			}
		}
		if len(o.Columns) == 0 {
			return false, "", errors.New("the query returned no columns for the alert to look at")
		}
		if o.First == nil {
			return false, "no rows", nil
		}
		s, null := export.Text(o.First[idx])
		if null {
			return false, "NULL", nil
		}
		shown := s
		if len(shown) > 200 {
			shown = shown[:200] + "…"
		}
		want, wantErr := strconv.ParseFloat(a.Value, 64)
		got, gotErr := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if wantErr == nil && gotErr == nil {
			return compareNum(got, a.Op, want), shown, nil
		}
		switch a.Op {
		case "=":
			return s == a.Value, shown, nil
		case "!=":
			return s != a.Value, shown, nil
		}
		return false, shown, fmt.Errorf("the alert compares %s with a number, but the value is %q", strOr(a.Column, "the first column"), shown)
	}
	return false, "", fmt.Errorf("unknown alert condition %q", a.Kind)
}

func compareNum(a float64, op string, b float64) bool {
	switch op {
	case ">":
		return a > b
	case ">=":
		return a >= b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case "=":
		return a == b
	case "!=":
		return a != b
	}
	return false
}

func strOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
