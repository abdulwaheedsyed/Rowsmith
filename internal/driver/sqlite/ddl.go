package sqlite

import (
	"fmt"
	"strings"

	"rowsmith/internal/driver"
)

// SQLite keeps CHECK constraints, generated-column expressions, index
// expressions and trigger timing only in the text of sqlite_schema.sql. The
// helpers below read them from a light tokenization of that text.

// token is a word, a quoted name or literal, or one punctuation character.
type token struct {
	text       string
	start, end int
	quoted     bool
}

func (t token) is(word string) bool { return !t.quoted && strings.EqualFold(t.text, word) }

// lex tokenizes SQL text, skipping whitespace and comments.
func lex(s string) []token {
	var out []token
	for i := 0; i < len(s); {
		c, start := s[i], i
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
			continue
		case strings.HasPrefix(s[i:], "--"):
			if k := strings.IndexByte(s[i:], '\n'); k >= 0 {
				i += k
			} else {
				i = len(s)
			}
			continue
		case strings.HasPrefix(s[i:], "/*"):
			if k := strings.Index(s[i+2:], "*/"); k >= 0 {
				i += k + 4
			} else {
				i = len(s)
			}
			continue
		case c == '\'' || c == '"' || c == '`' || c == '[':
			i = closeQuote(s, i)
		case isWord(c):
			for i < len(s) && isWord(s[i]) {
				i++
			}
		default:
			i++
		}
		out = append(out, token{text: s[start:i], start: start, end: i, quoted: c == '\'' || c == '"' || c == '`' || c == '['})
	}
	return out
}

func isWord(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// closeQuote returns the offset just past the quoted section starting at i.
func closeQuote(s string, i int) int {
	if s[i] == '[' {
		if k := strings.IndexByte(s[i:], ']'); k >= 0 {
			return i + k + 1
		}
		return len(s)
	}
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] == q {
			if j+1 < len(s) && s[j+1] == q {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(s)
}

// unquote strips identifier quoting.
func unquote(s string) string {
	if len(s) < 2 {
		return s
	}
	switch s[0] {
	case '[':
		return s[1 : len(s)-1]
	case '"', '`', '\'':
		return strings.ReplaceAll(s[1:len(s)-1], s[:1]+s[:1], s[:1])
	}
	return s
}

// closing returns the index of the parenthesis matching toks[i], or
// len(toks) when the text is unbalanced.
func closing(toks []token, i int) int {
	depth := 0
	for j := i; j < len(toks); j++ {
		switch toks[j].text {
		case "(":
			depth++
		case ")":
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return len(toks)
}

// parenList splits the first parenthesized list in toks at its top-level
// commas and returns the index just past the closing parenthesis.
func parenList(toks []token) (items [][]token, next int) {
	for i, t := range toks {
		if t.text != "(" {
			continue
		}
		end := closing(toks, i)
		start, depth := i+1, 0
		for j := i + 1; j < end; j++ {
			switch toks[j].text {
			case "(":
				depth++
			case ")":
				depth--
			case ",":
				if depth == 0 {
					items, start = append(items, toks[start:j]), j+1
				}
			}
		}
		return append(items, toks[start:end]), end + 1
	}
	return nil, len(toks)
}

// span returns the source text covered by toks.
func span(s string, toks []token) string {
	if len(toks) == 0 {
		return ""
	}
	return s[toks[0].start:toks[len(toks)-1].end]
}

type tableDef struct {
	checks       []driver.Check
	generated    map[string]string // column -> expression
	collations   map[string]string // column -> collation
	withoutRowid bool
	strict       bool
	module       string // virtual table module

	// What a rebuild must carry over, as the CREATE TABLE text states it.
	autoincrement string          // column declared INTEGER PRIMARY KEY AUTOINCREMENT
	unnamed       map[string]bool // check names made up for unnamed CHECK constraints
	deferred      []bool          // per foreign key in declaration order: DEFERRABLE INITIALLY DEFERRED
	onConflict    bool            // some constraint has an ON CONFLICT clause
}

// parseTable reads what PRAGMA table_xinfo does not report from CREATE TABLE text.
func parseTable(ddl string) tableDef {
	toks := lex(ddl)
	def := tableDef{generated: map[string]string{}, collations: map[string]string{}, unnamed: map[string]bool{}}
	if len(toks) > 1 && toks[1].is("VIRTUAL") {
		for i := 2; i+1 < len(toks); i++ {
			if toks[i].is("USING") {
				def.module = toks[i+1].text
				break
			}
		}
		return def
	}
	items, next := parenList(toks)
	for _, t := range toks[min(next, len(toks)):] {
		switch {
		case t.is("ROWID"):
			def.withoutRowid = true
		case t.is("STRICT"):
			def.strict = true
		}
	}
	for _, it := range items {
		if len(it) == 0 {
			continue
		}
		column := !(it[0].is("CONSTRAINT") || it[0].is("PRIMARY") || it[0].is("UNIQUE") || it[0].is("CHECK") || it[0].is("FOREIGN"))
		name := unquote(it[0].text)
		for k := 0; k < len(it); k++ {
			switch t := it[k]; {
			case t.text == "(":
				k = closing(it, k) // e.g. DEFAULT (expr)
				continue
			case k == 0: // the column name or the constraint keyword
			case column && t.is("COLLATE") && k+1 < len(it):
				def.collations[name] = unquote(it[k+1].text)
			case column && t.is("AUTOINCREMENT"):
				def.autoincrement = name
			case t.is("REFERENCES"):
				def.deferred = append(def.deferred, false)
			case t.is("DEFERRABLE") && !it[k-1].is("NOT") && len(def.deferred) > 0:
				if k+2 < len(it) && it[k+1].is("INITIALLY") && it[k+2].is("DEFERRED") {
					def.deferred[len(def.deferred)-1] = true
				}
			case t.is("CONFLICT") && it[k-1].is("ON"):
				def.onConflict = true
			}
			if k+1 >= len(it) || it[k+1].text != "(" {
				continue
			}
			end := closing(it, k+1)
			expr := span(ddl, it[k+2:min(end, len(it))])
			switch {
			case it[k].is("CHECK"):
				name := ""
				if k >= 2 && it[k-2].is("CONSTRAINT") {
					name = unquote(it[k-1].text)
				}
				def.checks = append(def.checks, driver.Check{Name: name, Expression: expr})
			case it[k].is("AS") && column:
				def.generated[name] = expr
			}
			k = end
		}
	}
	for i := range def.checks {
		if def.checks[i].Name == "" {
			def.checks[i].Name = fmt.Sprintf("check_%d", i+1)
			def.unnamed[def.checks[i].Name] = true
		}
	}
	return def
}

// parseIndex returns the key expressions and the WHERE clause of a CREATE
// INDEX statement. Sort order is left out: index_xinfo reports it.
func parseIndex(ddl string) (keys []string, where string) {
	toks := lex(ddl)
	items, next := parenList(toks)
	for _, it := range items {
		if n := len(it); n > 1 && (it[n-1].is("ASC") || it[n-1].is("DESC")) {
			it = it[:n-1]
		}
		keys = append(keys, span(ddl, it))
	}
	if next+1 < len(toks) && toks[next].is("WHERE") {
		where = strings.TrimSpace(ddl[toks[next+1].start:])
	}
	return keys, where
}

// parseTrigger reads the timing and event of a CREATE TRIGGER statement.
func parseTrigger(ddl string) driver.Trigger {
	toks := lex(ddl)
	tr := driver.Trigger{Timing: "BEFORE", Statement: ddl} // SQLite's default timing
	i := 0
	for i < len(toks) && !toks[i].is("TRIGGER") {
		i++
	}
	i++
	if i < len(toks) && toks[i].is("IF") {
		i += 3 // IF NOT EXISTS
	}
	i++ // trigger name
	if i < len(toks) && toks[i].text == "." {
		i += 2 // schema-qualified name
	}
	for ; i < len(toks); i++ {
		switch t := toks[i]; {
		case t.is("BEFORE"), t.is("AFTER"):
			tr.Timing = strings.ToUpper(t.text)
		case t.is("INSTEAD"):
			tr.Timing = "INSTEAD OF"
		case t.is("DELETE"), t.is("INSERT"), t.is("UPDATE"):
			tr.Event = strings.ToUpper(t.text)
			return tr
		}
	}
	return tr
}
