package schedule

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	c := Config{Delivery: Delivery{Emails: []string{" Ops@Example.com", "ops@example.com", "Ana <ana@example.org>", ""}}}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Output.Format != "csv" || c.Output.MaxRows != DefaultMaxRows {
		t.Fatalf("defaults: %+v", c.Output)
	}
	if strings.Join(c.Delivery.Emails, ",") != "ops@example.com,ana@example.org" {
		t.Fatalf("emails: %v", c.Delivery.Emails)
	}
	bad := []Config{
		{Output: Output{Format: "pdf"}},
		{Output: Output{MaxRows: MaxRowsLimit + 1}},
		{Alert: Alert{Kind: "rows", Op: ">", Value: "many"}},
		{Alert: Alert{Kind: "rows", Op: "~", Value: "1"}},
		{Alert: Alert{Kind: "value", Op: ">", Value: "abc"}},
		{Alert: Alert{Kind: "sometimes"}},
		{Delivery: Delivery{Emails: []string{"not an address"}}},
		{Delivery: Delivery{Emails: []string{"a@b.c\r\nBcc: x@y.z"}}},
	}
	for i, b := range bad {
		if err := b.Normalize(); err == nil {
			t.Errorf("case %d: expected an error", i)
		}
	}
}

func TestAlertCheck(t *testing.T) {
	obs := Observation{Rows: 3, Columns: []string{"sku", "stock"}, First: []any{"A-1", int64(4)}, Hash: "h2"}
	cases := []struct {
		a     Alert
		fired bool
		seen  string
	}{
		{Alert{Kind: "rows", Op: ">", Value: "0"}, true, "3 rows"},
		{Alert{Kind: "rows", Op: "=", Value: "0"}, false, "3 rows"},
		{Alert{Kind: "value", Op: "<", Value: "5", Column: "Stock"}, true, "4"},
		{Alert{Kind: "value", Op: ">=", Value: "5", Column: "stock"}, false, "4"},
		{Alert{Kind: "value", Op: "=", Value: "A-1"}, true, "A-1"},
		{Alert{Kind: "value", Op: "!=", Value: "A-1"}, false, "A-1"},
		{Alert{Kind: "changed"}, true, "changed"},
	}
	for i, c := range cases {
		fired, seen, err := c.a.Check(obs, "h1")
		if err != nil || fired != c.fired || seen != c.seen {
			t.Errorf("case %d (%s): fired=%v seen=%q err=%v", i, c.a.Describe(), fired, seen, err)
		}
	}
	if fired, seen, _ := (Alert{Kind: "changed"}).Check(obs, ""); fired || seen != "first result" {
		t.Errorf("first run should set the baseline: %v %q", fired, seen)
	}
	if _, _, err := (Alert{Kind: "value", Op: ">", Value: "1", Column: "missing"}).Check(obs, ""); err == nil {
		t.Error("a missing column should be an error")
	}
	if _, _, err := (Alert{Kind: "value", Op: ">", Value: "1"}).Check(obs, ""); err == nil {
		t.Error("comparing text with a number should be an error")
	}
	if fired, seen, _ := (Alert{Kind: "value", Op: ">", Value: "1"}).Check(Observation{Columns: []string{"n"}}, ""); fired || seen != "no rows" {
		t.Errorf("no rows: %v %q", fired, seen)
	}
}
