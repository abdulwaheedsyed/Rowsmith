package sqlsplit

import (
	"strings"

	"rowsmith/internal/driver"
)

type sigTok struct {
	typ   tokType
	text  string // upper-cased for words
	depth int    // parenthesis depth
}

func significant(sql string, d Dialect) []sigTok {
	lx := &lexer{s: sql, d: d}
	var out []sigTok
	depth := 0
	for {
		t := lx.next()
		if t.typ == tEOF {
			return out
		}
		if t.typ == tSpace || t.typ == tComment {
			// MySQL executable comments /*! ... */ run as code; treat as unknown code.
			if t.typ == tComment && d == MySQL && strings.HasPrefix(sql[t.start:], "/*!") {
				out = append(out, sigTok{typ: tWord, text: "/*!", depth: depth})
			}
			continue
		}
		txt := sql[t.start:t.end]
		if t.typ == tWord {
			txt = strings.ToUpper(txt)
		}
		if t.typ == tPunct && txt == ")" && depth > 0 {
			depth--
		}
		out = append(out, sigTok{typ: t.typ, text: txt, depth: depth})
		if t.typ == tPunct && txt == "(" {
			depth++
		}
	}
}

var writeWords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "UPSERT": true, "REPLACE": true,
	"TRUNCATE": true, "DROP": true, "CREATE": true, "ALTER": true, "GRANT": true, "REVOKE": true, "DENY": true,
	"EXEC": true, "EXECUTE": true, "CALL": true, "RENAME": true, "COMMIT": true, "ROLLBACK": true,
	"SAVEPOINT": true, "LOCK": true, "COPY": true, "LOAD": true, "KILL": true, "SHUTDOWN": true,
	"/*!": true,
}

// Classify determines the statement kind and whether it is destructive.
func Classify(sql string, d Dialect) (driver.StatementKind, driver.Danger) {
	toks := significant(sql, d)
	if len(toks) == 0 {
		return driver.StmtSession, driver.Danger{}
	}
	// Skip leading parentheses: "(SELECT ...) UNION ...".
	i := 0
	for i < len(toks) && toks[i].typ == tPunct && toks[i].text == "(" {
		i++
	}
	if i >= len(toks) || toks[i].typ != tWord {
		return driver.StmtUnknown, driver.Danger{}
	}
	first := toks[i].text
	word := func(k int) string {
		if k < len(toks) && toks[k].typ == tWord {
			return toks[k].text
		}
		return ""
	}

	var kind driver.StatementKind
	switch first {
	case "SELECT", "WITH", "VALUES", "TABLE":
		kind = driver.StmtRead
		if embeddedWrite(toks[i+1:], d) {
			kind = driver.StmtWrite
		}
	case "SHOW", "DESCRIBE", "DESC", "HELP", "LIST":
		kind = driver.StmtRead
	case "EXPLAIN":
		kind = driver.StmtRead
		// EXPLAIN ANALYZE executes the statement it explains.
		for k := i + 1; k < len(toks) && k < i+6; k++ {
			if w := word(k); w == "ANALYZE" || w == "ANALYSE" {
				rest := strings.Join(texts(toks[k+1:]), " ")
				inner, _ := Classify(rest, d)
				if inner != driver.StmtRead {
					kind = driver.StmtWrite
				}
				break
			}
		}
	case "PRAGMA":
		kind = driver.StmtRead
		for _, t := range toks[i:] {
			if t.typ == tPunct && t.text == "=" {
				kind = driver.StmtSession
			}
		}
	case "INSERT", "UPDATE", "DELETE", "MERGE", "REPLACE", "UPSERT", "COPY", "LOAD", "CALL", "EXEC", "EXECUTE",
		"DO", "HANDLER", "IMPORT", "LOCK", "UNLOCK", "NOTIFY", "KILL", "FLUSH", "PURGE", "RESET", "CHECKPOINT",
		"INSTALL", "UNINSTALL", "CACHE", "BACKUP", "RESTORE", "DBCC", "BULK":
		kind = driver.StmtWrite
		if first == "RESET" && d == Postgres {
			kind = driver.StmtSession // RESET <parameter>
		}
	case "CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME", "COMMENT", "ANALYZE", "VACUUM", "REINDEX", "CLUSTER",
		"OPTIMIZE", "REPAIR", "REFRESH", "SECURITY", "FLASHBACK", "AUDIT", "NOAUDIT", "ASSOCIATE", "DISASSOCIATE", "UNDROP":
		kind = driver.StmtDDL
	case "GRANT", "REVOKE", "DENY":
		kind = driver.StmtDCL
	case "BEGIN", "START", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE", "END", "ABORT", "XA":
		kind = driver.StmtTCL
		switch {
		case first == "START" && word(i+1) != "TRANSACTION":
			kind = driver.StmtWrite // START SLAVE / REPLICA / GROUP_REPLICATION
		case first == "BEGIN" && (d == Oracle || d == BigQuery) && word(i+1) != "TRANSACTION":
			kind = driver.StmtUnknown // anonymous block
		case first == "BEGIN" && d == MSSQL && word(i+1) != "TRAN" && word(i+1) != "TRANSACTION" && word(i+1) != "DISTRIBUTED":
			kind = driver.StmtUnknown // BEGIN ... END block
		case first == "END" && d != Postgres:
			kind = driver.StmtUnknown
		}
	case "SET":
		kind = driver.StmtSession
		joined := " " + strings.Join(texts(toks[i+1:]), " ") + " "
		switch {
		case strings.Contains(joined, " GLOBAL ") || strings.Contains(joined, " PERSIST ") || strings.Contains(joined, " PERSIST_ONLY "),
			strings.Contains(joined, "@@GLOBAL"), strings.Contains(joined, " PASSWORD "):
			kind = driver.StmtWrite
		case strings.Contains(joined, "READ_ONLY") || strings.Contains(joined, "READ WRITE") || strings.Contains(joined, "READ_WRITE") ||
			strings.Contains(joined, "TX_READ_ONLY") || strings.Contains(joined, "READONLY"):
			// Attempts to leave read-only mode.
			kind = driver.StmtWrite
		case word(i+1) == "ROLE" || strings.Contains(joined, "SESSION AUTHORIZATION"):
			kind = driver.StmtSession
		}
		if embeddedWrite(toks[i+1:], d) {
			kind = driver.StmtWrite // SET @x = (DELETE ...) style tricks
		}
	case "USE", "DECLARE", "PREPARE", "DEALLOCATE", "FETCH", "CLOSE", "MOVE", "LISTEN", "UNLISTEN", "DISCARD", "OPEN", "PRINT", "RAISERROR", "WAITFOR":
		kind = driver.StmtSession
		if first == "DECLARE" && (d == Oracle || embeddedWrite(toks[i+1:], d)) {
			kind = driver.StmtUnknown
		}
	case "IF", "WHILE", "LOOP", "REPEAT", "FOR":
		kind = driver.StmtUnknown // procedural control flow
	default:
		kind = driver.StmtUnknown
	}

	// T-SQL batches can hold many statements without separators: any write
	// keyword anywhere in the batch makes the whole batch a write.
	if d == MSSQL && kind.Safe() && embeddedWrite(toks[i+1:], d) {
		kind = driver.StmtWrite
	}
	return kind, danger(first, toks[i:], kind)
}

// embeddedWrite looks for data-modifying keywords inside a read-looking
// statement: data-modifying CTEs, SELECT ... INTO, FOR UPDATE, and T-SQL batches.
func embeddedWrite(toks []sigTok, d Dialect) bool {
	for k, t := range toks {
		if t.typ != tWord {
			continue
		}
		prevDot := k > 0 && toks[k-1].typ == tPunct && toks[k-1].text == "."
		nextParen := k+1 < len(toks) && toks[k+1].typ == tPunct && toks[k+1].text == "("
		nextDot := k+1 < len(toks) && toks[k+1].typ == tPunct && toks[k+1].text == "."
		if prevDot || nextDot {
			continue // qualified identifiers such as t.update
		}
		switch t.text {
		case "INTO":
			// SELECT ... INTO @var / :var assigns variables; anything else creates a table or writes a file.
			if k+1 < len(toks) && toks[k+1].typ == tParam {
				continue
			}
			if d == MySQL && k+1 < len(toks) && toks[k+1].typ == tWord && (toks[k+1].text == "OUTFILE" || toks[k+1].text == "DUMPFILE") {
				return true
			}
			if d == Oracle {
				continue // SELECT INTO only exists inside PL/SQL
			}
			return true
		case "REPLACE", "INSERT", "LEFT", "RIGHT":
			if nextParen {
				continue // string functions
			}
		}
		// Includes FOR UPDATE: row locks are not allowed in read-only sessions.
		if writeWords[t.text] {
			return true
		}
	}
	return false
}

func danger(first string, toks []sigTok, kind driver.StatementKind) driver.Danger {
	switch first {
	case "DROP", "TRUNCATE", "UNDROP":
		return driver.Danger{Level: "destructive", Reason: strings.ToLower(first) + " removes objects or data permanently"}
	case "DELETE", "UPDATE":
		if !hasTopLevel(toks, "WHERE") {
			return driver.Danger{Level: "destructive", Reason: strings.ToLower(first) + " without WHERE affects every row"}
		}
		return driver.Danger{Level: "caution", Reason: "modifies data"}
	case "ALTER":
		if hasTopLevel(toks, "DROP") {
			return driver.Danger{Level: "destructive", Reason: "alter drops a column, constraint or partition"}
		}
		return driver.Danger{Level: "caution", Reason: "changes structure"}
	}
	switch kind {
	case driver.StmtRead, driver.StmtSession, driver.StmtTCL:
		return driver.Danger{}
	case driver.StmtDDL:
		return driver.Danger{Level: "caution", Reason: "changes structure"}
	case driver.StmtDCL:
		return driver.Danger{Level: "caution", Reason: "changes permissions"}
	case driver.StmtWrite:
		return driver.Danger{Level: "caution", Reason: "modifies data"}
	}
	return driver.Danger{Level: "caution", Reason: "statement type could not be verified as read-only"}
}

func hasTopLevel(toks []sigTok, w string) bool {
	for _, t := range toks {
		if t.typ == tWord && t.depth == 0 && t.text == w {
			return true
		}
	}
	return false
}

func texts(toks []sigTok) []string {
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = t.text
	}
	return out
}
