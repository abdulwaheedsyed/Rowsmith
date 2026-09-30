package sqlsplit

import (
	"regexp"
	"strings"
)

// plsqlBlock matches statements that SQL*Plus-style scripts terminate with a
// "/" line instead of a semicolon.
var plsqlBlock = regexp.MustCompile(`(?is)^\s*(BEGIN|DECLARE|CREATE\s+(OR\s+REPLACE\s+)?((NON)?EDITIONABLE\s+)?(PROCEDURE|FUNCTION|PACKAGE|TRIGGER|TYPE)\b)`)

// Join renders statements (without terminators) as one console script that
// Split turns back into the same statements. dialect is a driver dialect
// name; "mongodb" puts one command per line.
func Join(dialect string, stmts []string) string {
	var b strings.Builder
	for _, st := range stmts {
		st = strings.TrimSpace(st)
		if st == "" {
			continue
		}
		switch Dialect(dialect) {
		case MSSQL:
			b.WriteString(st + "\nGO\n")
		case Oracle:
			if plsqlBlock.MatchString(st) {
				b.WriteString(strings.TrimRight(st, ";") + ";\n/\n")
			} else {
				b.WriteString(st + ";\n")
			}
		default:
			if dialect == "mongodb" {
				b.WriteString(st + "\n")
			} else {
				b.WriteString(st + ";\n")
			}
		}
	}
	return b.String()
}
