// Package sqlsplit splits SQL scripts into statements and classifies them.
//
// The lexer understands each dialect's comments and quoting (backslash
// escapes, dollar quotes, q'[...]', [brackets], backticks, BigQuery triple
// quotes), so delimiters inside literals never split a statement. The
// splitter additionally handles MySQL DELIMITER, T-SQL GO batches, Oracle "/"
// terminators and BEGIN...END bodies of stored routines and triggers.
package sqlsplit

import "strings"

type Dialect string

const (
	MySQL    Dialect = "mysql"
	Postgres Dialect = "postgresql"
	MSSQL    Dialect = "mssql"
	Oracle   Dialect = "plsql"
	SQLite   Dialect = "sqlite"
	BigQuery Dialect = "bigquery"
	Generic  Dialect = "generic"
)

type tokType uint8

const (
	tEOF tokType = iota
	tSpace
	tComment
	tString
	tQuoted // quoted identifier
	tWord
	tNumber
	tParam
	tPunct
)

type token struct {
	typ        tokType
	start, end int
}

type lexer struct {
	s string
	i int
	d Dialect
}

func (l *lexer) peekByte(off int) byte {
	if l.i+off < len(l.s) {
		return l.s[l.i+off]
	}
	return 0
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v' }

func isWordStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isWordChar(c byte) bool { return isWordStart(c) || c >= '0' && c <= '9' || c == '$' }

func (l *lexer) next() token {
	s, i := l.s, l.i
	if i >= len(s) {
		return token{typ: tEOF, start: i, end: i}
	}
	c := s[i]
	start := i
	emit := func(t tokType, end int) token {
		if end > len(s) {
			end = len(s)
		}
		l.i = end
		return token{typ: t, start: start, end: end}
	}

	switch {
	case isSpace(c):
		j := i + 1
		for j < len(s) && isSpace(s[j]) {
			j++
		}
		return emit(tSpace, j)

	case c == '-' && l.peekByte(1) == '-':
		// MySQL requires whitespace (or end) after "--" for it to be a comment.
		if l.d == MySQL {
			n := l.peekByte(2)
			if n != 0 && !isSpace(n) {
				return emit(tPunct, i+1)
			}
		}
		return emit(tComment, lineEnd(s, i))

	case c == '#' && (l.d == MySQL || l.d == BigQuery):
		return emit(tComment, lineEnd(s, i))

	case c == '/' && l.peekByte(1) == '*':
		return emit(tComment, l.blockComment(i))

	case c == '\'':
		if l.d == BigQuery && strings.HasPrefix(s[i:], "'''") {
			return emit(tString, closeTriple(s, i, "'''", true))
		}
		return emit(tString, closeQuote(s, i+1, '\'', l.backslashIn('\'')))

	case c == '"':
		if l.d == BigQuery && strings.HasPrefix(s[i:], `"""`) {
			return emit(tString, closeTriple(s, i, `"""`, true))
		}
		t := tQuoted
		if l.d == MySQL || l.d == BigQuery {
			t = tString
		}
		return emit(t, closeQuote(s, i+1, '"', l.backslashIn('"')))

	case c == '`' && (l.d == MySQL || l.d == BigQuery || l.d == SQLite || l.d == Generic):
		return emit(tQuoted, closeQuote(s, i+1, '`', l.d == BigQuery))

	case c == '[' && (l.d == MSSQL || l.d == SQLite):
		j := i + 1
		for j < len(s) {
			if s[j] == ']' {
				if j+1 < len(s) && s[j+1] == ']' {
					j += 2
					continue
				}
				return emit(tQuoted, j+1)
			}
			j++
		}
		return emit(tQuoted, len(s))

	case c == '$' && l.d == Postgres:
		if tag, ok := dollarTag(s, i); ok {
			if k := strings.Index(s[i+len(tag):], tag); k >= 0 {
				return emit(tString, i+len(tag)+k+len(tag))
			}
			return emit(tString, len(s))
		}
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j > i+1 {
			return emit(tParam, j)
		}
		return emit(tPunct, i+1)

	case isWordStart(c):
		// String prefixes: E'..' (Postgres), N'..' (MSSQL/Oracle/MySQL), X'..', B'..', q'[..]' (Oracle), r'..' / b'..' (BigQuery).
		if n := l.peekByte(1); n == '\'' || (n == '"' && l.d == BigQuery) {
			switch c | 0x20 {
			case 'e':
				if l.d == Postgres {
					return emit(tString, closeQuote(s, i+2, '\'', true))
				}
			case 'n', 'x', 'b', 'u':
				if l.d == BigQuery && (c|0x20) == 'b' {
					return emit(tString, l.bqString(i+1, false))
				}
				return emit(tString, closeQuote(s, i+2, '\'', l.backslashIn('\'')))
			case 'q':
				if l.d == Oracle && i+2 < len(s) {
					return emit(tString, oracleQ(s, i+2))
				}
			case 'r':
				if l.d == BigQuery {
					return emit(tString, l.bqString(i+1, true))
				}
			}
		}
		if l.d == BigQuery && (c|0x20 == 'r' || c|0x20 == 'b') && i+2 < len(s) {
			n1, n2 := s[i+1]|0x20, s[i+2]
			if (n1 == 'b' || n1 == 'r') && (n2 == '\'' || n2 == '"') {
				return emit(tString, l.bqString(i+2, c|0x20 == 'r' || n1 == 'r'))
			}
		}
		if (c == 'N' || c == 'n') && l.peekByte(1) == '\'' {
			return emit(tString, closeQuote(s, i+2, '\'', false))
		}
		j := i + 1
		for j < len(s) && isWordChar(s[j]) {
			j++
		}
		return emit(tWord, j)

	case c >= '0' && c <= '9' || c == '.' && l.peekByte(1) >= '0' && l.peekByte(1) <= '9':
		j := i + 1
		for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.' || s[j] == 'e' || s[j] == 'E' || s[j] == 'x' || s[j] == 'X' ||
			s[j] >= 'a' && s[j] <= 'f' || s[j] >= 'A' && s[j] <= 'F' || s[j] == '_') {
			j++
		}
		return emit(tNumber, j)

	case (c == '?' || c == ':' || c == '@') && l.d != Postgres:
		j := i + 1
		for j < len(s) && (isWordChar(s[j]) || s[j] == '@') {
			j++
		}
		if j > i+1 || c == '?' {
			return emit(tParam, j)
		}
		return emit(tPunct, i+1)
	}
	return emit(tPunct, i+1)
}

func (l *lexer) backslashIn(q byte) bool {
	switch l.d {
	case MySQL, BigQuery:
		return true
	}
	return false
}

func (l *lexer) blockComment(i int) int {
	s := l.s
	depth := 0
	j := i
	for j < len(s)-1 {
		if s[j] == '/' && s[j+1] == '*' {
			depth++
			j += 2
			if l.d != Postgres && depth > 1 {
				depth = 1 // only Postgres nests block comments
			}
			continue
		}
		if s[j] == '*' && s[j+1] == '/' {
			depth--
			j += 2
			if depth == 0 {
				return j
			}
			continue
		}
		j++
	}
	return len(s)
}

// bqString lexes a BigQuery string starting at the quote at position q.
func (l *lexer) bqString(q int, raw bool) int {
	s := l.s
	if q >= len(s) {
		return len(s)
	}
	quote := s[q]
	triple := strings.Repeat(string(quote), 3)
	if strings.HasPrefix(s[q:], triple) {
		return closeTriple(s, q, triple, !raw)
	}
	return closeQuote(s, q+1, quote, !raw)
}

func lineEnd(s string, i int) int {
	if k := strings.IndexByte(s[i:], '\n'); k >= 0 {
		return i + k
	}
	return len(s)
}

// closeQuote returns the offset just past the closing quote, treating a
// doubled quote as an escaped quote.
func closeQuote(s string, j int, q byte, backslash bool) int {
	for j < len(s) {
		c := s[j]
		if backslash && c == '\\' {
			j += 2
			continue
		}
		if c == q {
			if j+1 < len(s) && s[j+1] == q {
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return len(s)
}

func closeTriple(s string, i int, triple string, backslash bool) int {
	j := i + 3
	for j < len(s) {
		if backslash && s[j] == '\\' {
			j += 2
			continue
		}
		if strings.HasPrefix(s[j:], triple) {
			return j + 3
		}
		j++
	}
	return len(s)
}

// dollarTag recognizes $$ or $tag$ at position i.
func dollarTag(s string, i int) (string, bool) {
	j := i + 1
	if j < len(s) && s[j] == '$' {
		return "$$", true
	}
	if j >= len(s) || !(isWordStart(s[j])) {
		return "", false
	}
	for j < len(s) && (isWordStart(s[j]) || s[j] >= '0' && s[j] <= '9') {
		j++
	}
	if j < len(s) && s[j] == '$' {
		return s[i : j+1], true
	}
	return "", false
}

// oracleQ lexes q'<open>...<close>' starting at the opening delimiter.
func oracleQ(s string, i int) int {
	if i >= len(s) {
		return len(s)
	}
	open := s[i]
	closeCh := open
	switch open {
	case '[':
		closeCh = ']'
	case '{':
		closeCh = '}'
	case '(':
		closeCh = ')'
	case '<':
		closeCh = '>'
	}
	end := string([]byte{closeCh, '\''})
	if k := strings.Index(s[i+1:], end); k >= 0 {
		return i + 1 + k + 2
	}
	return len(s)
}
