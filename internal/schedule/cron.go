// Package schedule runs saved queries on a timetable: it parses cron
// expressions, evaluates alert conditions, stores result files encrypted,
// and delivers results by email and webhook.
package schedule

import (
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed five-field cron expression: minute, hour, day of month,
// month and day of week. Day of month may be L for the last day.
type Cron struct {
	minute, hour, dom, month, dow uint64
	lastDay                       bool
	domAny, dowAny                bool
}

var macros = map[string]string{
	"@hourly": "0 * * * *", "@daily": "0 0 * * *", "@midnight": "0 0 * * *",
	"@weekly": "0 0 * * 0", "@monthly": "0 0 1 * *", "@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *",
}

var monthNames = map[string]int{"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6, "JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12}
var dayNames = map[string]int{"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6}

// ParseCron reads an expression such as "30 8 * * 1-5" or "@daily".
func ParseCron(expr string) (*Cron, error) {
	expr = strings.TrimSpace(expr)
	if m, ok := macros[strings.ToLower(expr)]; ok {
		expr = m
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, errors.New("a schedule needs five fields: minute, hour, day of month, month and day of week")
	}
	c := &Cron{}
	var err error
	if c.minute, err = field(f[0], 0, 59, nil, "minute"); err != nil {
		return nil, err
	}
	if c.hour, err = field(f[1], 0, 23, nil, "hour"); err != nil {
		return nil, err
	}
	dom := f[2]
	if strings.EqualFold(dom, "L") {
		c.lastDay, dom = true, ""
	} else if parts := strings.Split(dom, ","); len(parts) > 1 {
		kept := parts[:0]
		for _, p := range parts {
			if strings.EqualFold(p, "L") {
				c.lastDay = true
			} else {
				kept = append(kept, p)
			}
		}
		dom = strings.Join(kept, ",")
	}
	if dom != "" {
		if c.dom, err = field(dom, 1, 31, nil, "day of month"); err != nil {
			return nil, err
		}
	}
	if c.month, err = field(f[3], 1, 12, monthNames, "month"); err != nil {
		return nil, err
	}
	if c.dow, err = field(f[4], 0, 7, dayNames, "day of week"); err != nil {
		return nil, err
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow = c.dow&^(1<<7) | 1
	}
	c.domAny = f[2] == "*" || f[2] == "?"
	c.dowAny = f[4] == "*" || f[4] == "?"
	return c, nil
}

func field(s string, min, max int, names map[string]int, label string) (uint64, error) {
	var set uint64
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			return 0, fmt.Errorf("the %s field has an empty item", label)
		}
		rng, step := part, 1
		if i := strings.IndexByte(part, '/'); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n < 1 {
				return 0, fmt.Errorf("%q is not a valid step in the %s field", part[i+1:], label)
			}
			rng, step = part[:i], n
		}
		lo, hi := min, max
		switch {
		case rng == "*" || rng == "?":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = value(a, names); err != nil {
				return 0, fmt.Errorf("%q is not valid in the %s field", a, label)
			}
			if hi, err = value(b, names); err != nil {
				return 0, fmt.Errorf("%q is not valid in the %s field", b, label)
			}
		default:
			v, err := value(rng, names)
			if err != nil {
				return 0, fmt.Errorf("%q is not valid in the %s field", rng, label)
			}
			lo, hi = v, v
			if strings.Contains(part, "/") {
				hi = max // "5/15" means from 5, every 15
			}
		}
		if lo < min || hi > max || lo > hi {
			return 0, fmt.Errorf("the %s field allows %d to %d", label, min, max)
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << uint(v)
		}
	}
	return set, nil
}

func value(s string, names map[string]int) (int, error) {
	if v, ok := names[strings.ToUpper(s)]; ok {
		return v, nil
	}
	return strconv.Atoi(s)
}

func (c *Cron) dayMatches(t time.Time) bool {
	if c.month&(1<<uint(t.Month())) == 0 {
		return false
	}
	dom := c.dom&(1<<uint(t.Day())) != 0
	if c.lastDay && t.AddDate(0, 0, 1).Day() == 1 {
		dom = true
	}
	dow := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return dow
	case c.dowAny:
		return dom
	}
	return dom || dow // both restricted: either matches, as in classic cron
}

// Next returns the first matching minute strictly after t, in loc. It
// returns the zero time when nothing matches within five years.
func (c *Cron) Next(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	y, m, d := t.Date()
	for i := 0; i < 5*366+1; i++ {
		day := time.Date(y, m, d+i, 12, 0, 0, 0, loc) // noon: never skipped by a DST change
		if !c.dayMatches(day) {
			continue
		}
		for h := 0; h < 24; h++ {
			if c.hour&(1<<uint(h)) == 0 {
				continue
			}
			for min := 0; min < 60; min++ {
				if c.minute&(1<<uint(min)) == 0 {
					continue
				}
				at := time.Date(day.Year(), day.Month(), day.Day(), h, min, 0, 0, loc)
				if at.Hour() != h || at.Minute() != min {
					// Skipped by a DST change: run as far after the gap as the
					// time was into it (02:30 becomes 03:30).
					_, before := at.Add(-6 * time.Hour).Zone()
					at = time.Date(day.Year(), day.Month(), day.Day(), h, min, 0, 0, time.UTC).Add(-time.Duration(before) * time.Second).In(loc)
				}
				if at.After(t) {
					return at
				}
			}
		}
	}
	return time.Time{}
}

// Runs lists the next n run times after t.
func (c *Cron) Runs(t time.Time, loc *time.Location, n int) []time.Time {
	var out []time.Time
	for len(out) < n {
		t = c.Next(t, loc)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out
}

// MinGap is the shortest time between two runs within the coming year,
// measured on a sample: minute and hour lists make it exact enough.
func (c *Cron) MinGap(from time.Time, loc *time.Location) time.Duration {
	// Within a day the smallest gap comes from the minute and hour sets;
	// check the first matching day and the wrap to the next one.
	runs := c.Runs(from, loc, 2+bits.OnesCount64(c.minute)*bits.OnesCount64(c.hour))
	gap := time.Duration(0)
	for i := 1; i < len(runs); i++ {
		if g := runs[i].Sub(runs[i-1]); gap == 0 || g < gap {
			gap = g
		}
	}
	return gap
}
