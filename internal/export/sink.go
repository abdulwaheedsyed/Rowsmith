package export

import (
	"sync/atomic"
	"time"

	"rowsmith/internal/driver"
)

// Sink feeds the first result set of a statement run into a Writer. Later
// result sets are ignored. It asks drivers for complete cell values.
type Sink struct {
	W Writer
	// Skip names result columns to leave out (e.g. hidden row keys).
	Skip map[string]bool
	// Progress, when set, is called at most every Interval with the number
	// of rows written so far.
	Progress func(rows int64)
	Interval time.Duration

	rows    atomic.Int64
	state   int // 0 waiting, 1 writing, 2 done
	keep    []int
	stmtErr error
	last    time.Time
	notices []string
}

var (
	_ driver.Sink          = (*Sink)(nil)
	_ driver.FullValueSink = (*Sink)(nil)
)

func (s *Sink) FullValues() bool { return true }

// Count reports how many rows were written.
func (s *Sink) Count() int64 { return s.rows.Load() }

// Err returns the first statement error the engine reported.
func (s *Sink) Err() error { return s.stmtErr }

// Wrote reports whether a result set was found.
func (s *Sink) Wrote() bool { return s.state > 0 }

// Notices returns warnings and messages the engine sent.
func (s *Sink) Notices() []string { return s.notices }

func (s *Sink) BeginStatement(driver.StatementInfo) error { return nil }

func (s *Sink) Columns(cols []driver.ResultColumn) error {
	if s.state != 0 {
		s.state = 2
		return nil
	}
	s.state = 1
	var out []driver.ResultColumn
	s.keep = s.keep[:0]
	for i, c := range cols {
		if s.Skip[c.Name] {
			continue
		}
		s.keep = append(s.keep, i)
		out = append(out, c)
	}
	return s.W.Begin(out)
}

func (s *Sink) Rows(rows [][]any) error {
	if s.state != 1 {
		return nil
	}
	cells := make([]any, len(s.keep))
	for _, r := range rows {
		for j, i := range s.keep {
			if i < len(r) {
				cells[j] = r[i]
			} else {
				cells[j] = nil
			}
		}
		if err := s.W.Row(cells); err != nil {
			return err
		}
		n := s.rows.Add(1)
		if s.Progress != nil && time.Since(s.last) >= s.Interval {
			s.last = time.Now()
			s.Progress(n)
		}
	}
	return nil
}

func (s *Sink) EndResult(driver.ResultSummary) error {
	if s.state == 1 {
		s.state = 2
	}
	return nil
}

func (s *Sink) Notice(level, text string) error {
	if len(s.notices) < 50 {
		s.notices = append(s.notices, level+": "+text)
	}
	return nil
}

func (s *Sink) EndStatement(err error) error {
	if err != nil && s.stmtErr == nil {
		s.stmtErr = err
	}
	return nil
}
