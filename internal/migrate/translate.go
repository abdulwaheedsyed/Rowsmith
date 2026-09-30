package migrate

import (
	"regexp"
	"slices"
	"strings"

	"rowsmith/internal/driver"
)

var (
	numberRe   = regexp.MustCompile(`^[-+]?\d+(\.\d+)?([eE][-+]?\d+)?$`)
	stringRe   = regexp.MustCompile(`^[Nn]?'((?:[^']|'')*)'$`)
	pgCastRe   = regexp.MustCompile(`^(.*?)::[a-z_ ]+(\[\])?(\(\d+(,\d+)?\))?$`)
	nowRe      = regexp.MustCompile(`^(now\(\)|current_timestamp(\(\d*\))?|getdate\(\)|sysdatetime\(\)|getutcdate\(\)|sysutcdatetime\(\)|sysdatetimeoffset\(\)|sysdate|systimestamp|localtimestamp(\(\d*\))?|statement_timestamp\(\)|transaction_timestamp\(\)|clock_timestamp\(\)|datetime\('now'\)|utc_timestamp(\(\d*\))?)$`)
	todayRe    = regexp.MustCompile(`^(current_date|curdate\(\)|cast\(getdate\(\) as date\)|trunc\(sysdate\)|date\('now'\))$`)
	uuidGenRe  = regexp.MustCompile(`^(gen_random_uuid\(\)|uuid_generate_v4\(\)|newid\(\)|uuid\(\)|newsequentialid\(\))$`)
	identityRe = regexp.MustCompile(`(?i)^(nextval\(|generated\s+(by\s+default|always)\s+(on\s+null\s+)?as\s+identity)`)
)

func unwrapParens(s string) string {
	for len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		depth, ok := 0, true
		for i, r := range s {
			switch r {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 && i < len(s)-1 {
					ok = false
				}
			}
		}
		if !ok {
			break
		}
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// translateDefault rewrites a column default for another engine. It
// returns nil with a note when the expression has no safe equivalent.
func translateDefault(srcEngine, dstEngine string, def string, k canon, dstType string) (*string, string) {
	d := unwrapParens(strings.TrimSpace(def))
	if d == "" || strings.EqualFold(d, "null") || identityRe.MatchString(d) {
		return nil, ""
	}
	dst := family(dstEngine)
	if family(srcEngine) == Postgres {
		if m := pgCastRe.FindStringSubmatch(d); m != nil && m[2] == "" {
			d = unwrapParens(strings.TrimSpace(m[1]))
		}
	}
	low := strings.ToLower(d)
	lt := strings.ToLower(dstType)
	out := func(s string) (*string, string) { return &s, "" }
	skip := func() (*string, string) { return nil, "the default " + def + " was not carried over" }
	if dst == MySQL && (strings.Contains(lt, "text") || strings.Contains(lt, "blob") || lt == "json" || k.T == "geometry") {
		return nil, "MySQL cannot give this type a default; the default " + def + " was dropped"
	}
	if dst == MongoDB {
		return nil, ""
	}
	switch {
	case k.T == "bool":
		var on bool
		switch low {
		case "true", "1", "b'1'", "'1'", "'t'", "'true'", "'y'":
			on = true
		case "false", "0", "b'0'", "'0'", "'f'", "'false'", "'n'":
		default:
			return skip()
		}
		switch {
		case dst == Postgres || (dst == Oracle && strings.EqualFold(dstType, "BOOLEAN")):
			return out(map[bool]string{true: "TRUE", false: "FALSE"}[on])
		default:
			return out(map[bool]string{true: "1", false: "0"}[on])
		}
	case numberRe.MatchString(d):
		return out(d)
	case stringRe.MatchString(d):
		lit := stringRe.FindStringSubmatch(d)[1]
		if dst == MSSQL {
			return out("N'" + lit + "'")
		}
		return out("'" + lit + "'")
	case nowRe.MatchString(low):
		if k.T != "datetime" && k.T != "timestamptz" {
			return skip()
		}
		switch dst {
		case MySQL:
			// The precision must match the column's: DATETIME(6) takes CURRENT_TIMESTAMP(6).
			if p, _ := typeParams(lt); p > 0 {
				return out("CURRENT_TIMESTAMP(" + itoa(p) + ")")
			}
			return out("CURRENT_TIMESTAMP")
		case MSSQL:
			if k.T == "timestamptz" {
				return out("SYSDATETIMEOFFSET()")
			}
			return out("SYSDATETIME()")
		case Oracle:
			if strings.HasPrefix(lt, "date") {
				return out("SYSDATE")
			}
			return out("SYSTIMESTAMP")
		}
		return out("CURRENT_TIMESTAMP")
	case todayRe.MatchString(low):
		switch dst {
		case MySQL:
			return out("(CURRENT_DATE)")
		case MSSQL:
			return out("CAST(GETDATE() AS date)")
		case Oracle:
			return out("TRUNC(SYSDATE)")
		}
		return out("CURRENT_DATE")
	case uuidGenRe.MatchString(low):
		switch dst {
		case MySQL:
			return out("(UUID())")
		case Postgres:
			return out("gen_random_uuid()")
		case MSSQL:
			if k.T == "uuid" {
				return out("NEWID()")
			}
		}
		return skip()
	}
	return skip()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// NameCase converts identifiers: keep, lower or upper.
func applyCase(name, mode string) string {
	switch mode {
	case "lower":
		return strings.ToLower(name)
	case "upper":
		return strings.ToUpper(name)
	}
	return name
}

// DefaultCase suggests how to spell names on the target: Oracle folds
// unquoted names to upper case, PostgreSQL to lower case, so names that
// match that folding stay easy to type without quotes.
func DefaultCase(srcEngine, dstEngine string) string {
	switch {
	case family(dstEngine) == Oracle && family(srcEngine) != Oracle:
		return "upper"
	case family(srcEngine) == Oracle && family(dstEngine) != Oracle && dstEngine != MongoDB:
		return "lower"
	case family(dstEngine) == Postgres && (srcEngine == MSSQL):
		return "lower"
	}
	return "keep"
}

func maxIdent(engine string) int {
	switch family(engine) {
	case Postgres:
		return 63
	case MySQL:
		return 64
	case Oracle:
		return 128
	}
	return 128
}

func clipIdent(engine, name string) string {
	if n := maxIdent(engine); len(name) > n {
		return name[:n]
	}
	return name
}

// indexType maps an index method to one the target offers, or reports that
// the index should be left out.
func indexType(srcEngine, dstEngine string, ix driver.Index, geo bool, offered []string) (string, string, bool) {
	t := strings.ToLower(ix.Type)
	same := family(srcEngine) == family(dstEngine)
	has := func(x string) bool {
		for _, o := range offered {
			if strings.EqualFold(o, x) {
				return true
			}
		}
		return false
	}
	switch {
	case same:
		return ix.Type, "", true
	case geo || t == "spatial" || t == "gist" || t == "spgist":
		if !geo {
			return "", "index " + ix.Name + " (" + ix.Type + ") has no equivalent and was left out", false
		}
		switch family(dstEngine) {
		case Postgres:
			return "gist", "", true
		case MySQL:
			return "spatial", "", true
		case MongoDB:
			return "2dsphere", "", true
		}
		return "", "spatial index " + ix.Name + " was left out: create it on the target by hand", false
	case t == "fulltext" || t == "gin" || t == "text":
		if family(dstEngine) == MySQL && has("fulltext") {
			return "fulltext", "", true
		}
		return "", "full-text index " + ix.Name + " was left out", false
	case t == "hash":
		if has("hash") {
			return "hash", "", true
		}
	}
	return "", "", true
}

// fkAction keeps a foreign key rule the target understands.
func fkAction(action string, offered []string) string {
	a := strings.ToUpper(strings.TrimSpace(action))
	if a == "" || a == "NO ACTION" {
		return ""
	}
	if slices.ContainsFunc(offered, func(o string) bool { return strings.EqualFold(o, a) }) {
		return a
	}
	if a == "RESTRICT" {
		return ""
	}
	return ""
}

// uniqueNames renames indexes and constraints whose names clash across the
// tables of a plan: PostgreSQL, Oracle and SQLite keep these names per
// schema, while MySQL and SQL Server keep them per table.
func uniqueNames(names map[string]int, table, name, engine string) string {
	if name == "" {
		return name
	}
	perSchema := family(engine) == Postgres || family(engine) == Oracle || family(engine) == SQLite
	key := strings.ToLower(name)
	if !perSchema {
		key = strings.ToLower(table) + "\x00" + key
	}
	names[key]++
	if names[key] == 1 {
		return name
	}
	return clipIdent(engine, table+"_"+name)
}
