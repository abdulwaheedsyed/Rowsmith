package sqlsplit

import (
	"strings"

	"rowsmith/internal/driver"
)

type Statement struct {
	SQL    string               `json:"sql"`
	Start  int                  `json:"start"` // byte offset of the first character in the script
	End    int                  `json:"end"`   // byte offset just past the last character (delimiter excluded)
	Line   int                  `json:"line"`  // 1-based
	Kind   driver.StatementKind `json:"kind"`
	Danger driver.Danger        `json:"danger"`
}

// Split breaks a script into executable statements.
func Split(script string, d Dialect) []Statement {
	sp := splitter{s: script, d: d, delim: ";"}
	return sp.run()
}

// StatementAt returns the statement containing (or immediately preceding) the
// byte offset, as used by "run statement at cursor".
func StatementAt(script string, d Dialect, offset int) (Statement, bool) {
	stmts := Split(script, d)
	if len(stmts) == 0 {
		return Statement{}, false
	}
	best := -1
	for i, st := range stmts {
		if offset >= st.Start && offset <= st.End {
			return st, true
		}
		if st.Start <= offset {
			best = i
		}
	}
	if best < 0 {
		best = 0
	}
	// Cursor sits in the gap after a statement (e.g. after its semicolon):
	// pick the preceding one unless the next one starts on the same line.
	return stmts[best], true
}

type splitter struct {
	s     string
	d     Dialect
	delim string

	out []Statement
}

func (sp *splitter) run() []Statement {
	lx := &lexer{s: sp.s, d: sp.d}
	stmtStart := -1 // offset of first non-space token of the current statement
	lastEnd := 0    // offset just past the last significant token
	var words []string
	parenDepth := 0
	blockDepth := 0
	mode := modeNormal
	pendingEndSuffix := false // saw END, waiting to see whether it is END IF/LOOP/...

	flush := func(end int) {
		if stmtStart >= 0 && hasCode(sp.s[stmtStart:end], sp.d) {
			text := strings.TrimRightFunc(sp.s[stmtStart:end], isSpaceRune)
			sp.out = append(sp.out, Statement{
				SQL:   text,
				Start: stmtStart,
				End:   stmtStart + len(text),
				Line:  1 + strings.Count(sp.s[:stmtStart], "\n"),
			})
		}
		stmtStart, words, parenDepth, blockDepth, mode, pendingEndSuffix = -1, nil, 0, 0, modeNormal, false
	}

	for {
		// Line-oriented client commands are recognized at the beginning of a line.
		if lx.i == 0 || (lx.i <= len(sp.s) && lx.i > 0 && sp.s[lx.i-1] == '\n') {
			onlyComments := stmtStart < 0 || !hasCode(sp.s[stmtStart:lx.i], sp.d)
			if n, ok := sp.lineCommand(lx.i, onlyComments); ok {
				switch n.kind {
				case cmdDelimiter:
					sp.delim = n.arg
					if onlyComments {
						stmtStart = -1 // comments before a client command belong to neither statement
					}
				case cmdBatch, cmdSlash:
					flush(lastEndOr(lastEnd, lx.i))
				}
				lx.i = n.next
				lastEnd = lx.i
				continue
			}
		}

		// A custom DELIMITER (MySQL client syntax) ends the statement wherever
		// it appears outside literals, regardless of BEGIN...END nesting.
		if sp.delim != ";" && strings.HasPrefix(sp.s[lx.i:], sp.delim) {
			if stmtStart >= 0 {
				flush(lx.i)
			}
			lx.i += len(sp.delim)
			lastEnd = lx.i
			continue
		}

		tok := lx.next()
		if sp.delim != ";" && (tok.typ == tWord || tok.typ == tPunct || tok.typ == tNumber) {
			if k := strings.Index(sp.s[tok.start:tok.end], sp.delim); k > 0 {
				tok.end = tok.start + k // e.g. "END$$": stop the word before the delimiter
				lx.i = tok.end
			}
		}
		if tok.typ == tEOF {
			flush(lastEndOr(lastEnd, len(sp.s)))
			break
		}
		if tok.typ == tSpace {
			continue
		}
		if stmtStart < 0 {
			stmtStart = tok.start
		}
		lastEnd = tok.end
		if tok.typ == tComment {
			continue
		}

		text := sp.s[tok.start:tok.end]
		if tok.typ == tWord {
			w := strings.ToUpper(text)
			skipOpener := false
			if pendingEndSuffix {
				pendingEndSuffix = false
				switch w {
				case "IF", "LOOP", "WHILE", "REPEAT", "FOR":
					// END IF etc. close constructs we do not count.
					blockDepth++ // undo the decrement applied at END
					skipOpener = true
				case "CASE":
					skipOpener = true // END CASE closes the CASE already counted
				}
			}
			words = append(words, w)
			if len(words) <= 12 && mode == modeNormal && sp.delim == ";" {
				if mode = sp.detectMode(words); mode == modeBlock {
					// Keywords seen before the switch (e.g. BEGIN of BEGIN ATOMIC) still count.
					blockDepth = 0
					for _, pw := range words[:len(words)-1] {
						switch pw {
						case "BEGIN", "CASE":
							blockDepth++
						case "END":
							blockDepth--
						}
					}
				}
			}
			if mode == modeBlock && !skipOpener {
				switch w {
				case "BEGIN":
					blockDepth++
				case "CASE":
					blockDepth++
				case "END":
					if blockDepth > 0 {
						blockDepth--
						pendingEndSuffix = true
					}
				}
			}
			continue
		}
		pendingEndSuffix = false

		if tok.typ == tPunct {
			switch text {
			case "(":
				parenDepth++
			case ")":
				if parenDepth > 0 {
					parenDepth--
				}
			case ";":
				if sp.d == MSSQL || mode == modeSlash || sp.delim != ";" {
					continue
				}
				if parenDepth == 0 && blockDepth == 0 {
					lastEnd = tok.start
					flush(tok.start)
				}
			}
		}
	}
	for i := range sp.out {
		sp.out[i].Kind, sp.out[i].Danger = Classify(sp.out[i].SQL, sp.d)
	}
	return sp.out
}

func lastEndOr(lastEnd, fallback int) int {
	if lastEnd > 0 {
		return lastEnd
	}
	return fallback
}

type splitMode uint8

const (
	modeNormal splitMode = iota
	modeBlock            // track BEGIN/END depth; ';' at depth 0 ends the statement
	modeSlash            // Oracle PL/SQL: only a "/" line (or end of script) ends the statement
)

// detectMode inspects the leading keywords of a statement to decide whether
// its body contains nested semicolons.
func (sp *splitter) detectMode(words []string) splitMode {
	first := words[0]
	switch sp.d {
	case Oracle:
		if first == "DECLARE" || first == "BEGIN" {
			return modeSlash
		}
		if first == "CREATE" && definesRoutine(words, true) {
			return modeSlash
		}
	case MySQL, SQLite:
		if first == "CREATE" && definesRoutine(words, false) {
			return modeBlock
		}
		if sp.d == MySQL && first == "BEGIN" && len(words) >= 2 && words[1] == "NOT" {
			return modeBlock // MariaDB BEGIN NOT ATOMIC
		}
	case BigQuery:
		if first == "CREATE" && definesRoutine(words, false) {
			return modeBlock
		}
		if first == "BEGIN" && len(words) >= 2 && words[1] != "TRANSACTION" {
			return modeBlock
		}
	case Postgres:
		if first == "CREATE" && definesRoutine(words, false) {
			for i := 1; i < len(words); i++ {
				if words[i] == "ATOMIC" && words[i-1] == "BEGIN" {
					return modeBlock
				}
			}
		}
	}
	return modeNormal
}

func definesRoutine(words []string, oracle bool) bool {
	for _, w := range words[1:] {
		switch w {
		case "PROCEDURE", "FUNCTION", "TRIGGER", "EVENT":
			return true
		case "PACKAGE", "TYPE", "LIBRARY", "JAVA":
			return oracle
		case "TABLE", "VIEW", "INDEX", "SEQUENCE", "SCHEMA", "DATABASE", "USER", "ROLE", "AS", "ON":
			return false
		}
	}
	return false
}

type cmdKind uint8

const (
	cmdDelimiter cmdKind = iota + 1
	cmdBatch             // T-SQL GO
	cmdSlash             // Oracle "/"
)

type lineCmd struct {
	kind cmdKind
	arg  string
	next int // offset after the command line
}

func (sp *splitter) lineCommand(i int, atStatementStart bool) (lineCmd, bool) {
	end := lineEnd(sp.s, i)
	line := strings.TrimSpace(sp.s[i:end])
	next := end
	if next < len(sp.s) {
		next++ // consume newline
	}
	switch sp.d {
	case MySQL:
		if atStatementStart && len(line) > 10 && strings.EqualFold(line[:9], "DELIMITER") && isSpace(line[9]) {
			arg := strings.TrimSpace(line[10:])
			if f := strings.Fields(arg); len(f) > 0 {
				return lineCmd{kind: cmdDelimiter, arg: f[0], next: next}, true
			}
		}
	case MSSQL:
		l := line
		if k := strings.Index(l, "--"); k >= 0 {
			l = strings.TrimSpace(l[:k])
		}
		if f := strings.Fields(l); len(f) >= 1 && len(f) <= 2 && strings.EqualFold(f[0], "GO") {
			if len(f) == 1 || isDigits(f[1]) {
				return lineCmd{kind: cmdBatch, next: next}, true
			}
		}
	case Oracle:
		if line == "/" {
			return lineCmd{kind: cmdSlash, next: next}, true
		}
	}
	return lineCmd{}, false
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func isSpaceRune(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v'
}

// hasCode reports whether text contains anything besides whitespace and comments.
func hasCode(text string, d Dialect) bool {
	lx := &lexer{s: text, d: d}
	for {
		t := lx.next()
		switch t.typ {
		case tEOF:
			return false
		case tSpace, tComment:
			continue
		default:
			return true
		}
	}
}
