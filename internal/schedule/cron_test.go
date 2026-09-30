package schedule

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestCronNext(t *testing.T) {
	utc := time.UTC
	base := time.Date(2026, 9, 30, 10, 17, 30, 0, utc) // a Wednesday
	cases := []struct {
		expr string
		want []string
	}{
		{"*/15 * * * *", []string{"2026-09-30 10:30", "2026-09-30 10:45", "2026-09-30 11:00"}},
		{"0 8 * * 1-5", []string{"2026-10-01 08:00", "2026-10-02 08:00", "2026-10-05 08:00"}},
		{"30 9 * * MON", []string{"2026-10-05 09:30", "2026-10-12 09:30"}},
		{"0 6 1 * *", []string{"2026-10-01 06:00", "2026-11-01 06:00"}},
		{"0 18 L * *", []string{"2026-09-30 18:00", "2026-10-31 18:00", "2026-11-30 18:00"}},
		{"0 0 29 2 *", []string{"2028-02-29 00:00"}},
		{"@hourly", []string{"2026-09-30 11:00", "2026-09-30 12:00"}},
		{"0 12 * * 7", []string{"2026-10-04 12:00"}},
		{"5/20 * * * *", []string{"2026-09-30 10:25", "2026-09-30 10:45", "2026-09-30 11:05"}},
		{"0 9 13 * FRI", []string{"2026-10-02 09:00", "2026-10-09 09:00", "2026-10-13 09:00"}}, // either matches
		{"0 0 * JAN,JUL *", []string{"2027-01-01 00:00"}},
	}
	for _, c := range cases {
		cr, err := ParseCron(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		got := cr.Runs(base, utc, len(c.want))
		for i, w := range c.want {
			if i >= len(got) || got[i].Format("2006-01-02 15:04") != w {
				t.Fatalf("%s: run %d = %v, want %s", c.expr, i, got, w)
			}
		}
	}
}

func TestCronRejects(t *testing.T) {
	for _, e := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "*/0 * * * *", "5-1 * * * *", "a * * * *", "1,,2 * * * *"} {
		if _, err := ParseCron(e); err == nil {
			t.Errorf("%q: expected an error", e)
		}
	}
}

func TestCronTimezonesAndDST(t *testing.T) {
	riyadh := mustLoc(t, "Asia/Riyadh")
	cr, _ := ParseCron("0 8 * * *")
	got := cr.Next(time.Date(2026, 9, 30, 6, 0, 0, 0, time.UTC), riyadh) // 09:00 in Riyadh
	if got.UTC().Format(time.RFC3339) != "2026-10-01T05:00:00Z" {
		t.Fatalf("Riyadh 08:00 = %v", got.UTC())
	}
	// 02:30 does not exist on 2026-03-08 in New York: it runs at 03:30.
	ny := mustLoc(t, "America/New_York")
	cr, _ = ParseCron("30 2 * * *")
	got = cr.Next(time.Date(2026, 3, 7, 12, 0, 0, 0, ny), ny)
	if got.Format("2006-01-02 15:04 MST") != "2026-03-08 03:30 EDT" {
		t.Fatalf("spring forward: %v", got)
	}
	got = cr.Next(got, ny)
	if got.Format("2006-01-02 15:04") != "2026-03-09 02:30" {
		t.Fatalf("day after: %v", got)
	}
}

func TestCronMinGap(t *testing.T) {
	from := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	for expr, want := range map[string]time.Duration{
		"*/5 * * * *": 5 * time.Minute, "* * * * *": time.Minute, "0,2 * * * *": 2 * time.Minute,
		"0 8 * * *": 24 * time.Hour, "0 8,9 * * 1": time.Hour, "58 23 * * *": 24 * time.Hour,
	} {
		cr, _ := ParseCron(expr)
		if got := cr.MinGap(from, time.UTC); got != want {
			t.Errorf("%s: gap %v, want %v", expr, got, want)
		}
	}
}
