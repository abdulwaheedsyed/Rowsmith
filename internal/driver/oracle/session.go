package oracle

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	go_ora "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/network"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

// console wraps the shared console session with Oracle behavior: manual
// commit as in SQL*Plus, DBMS_OUTPUT as notices, PL/SQL compilation errors
// as statement errors, SQL type names in result headers, and a fresh
// connection after a driver failure.
type console struct {
	c      *conn
	scope  driver.Scope
	broken atomic.Bool // the connection was retired by safeConn

	mu   sync.Mutex // guards sess, which is replaced after a driver failure
	sess *sqlbase.Session

	// Statement state, only used while Execute runs.
	stmt       sqlsplit.Statement
	unit       *plsqlUnit // unit the statement creates, checked for compilation errors
	compileErr error
	inTx       bool
	txKnown    bool
}

func (c *conn) NewSession(ctx context.Context, s driver.Scope) (driver.Session, error) {
	k := &console{c: c, scope: s}
	sess, err := k.open(ctx)
	if err != nil {
		return nil, err
	}
	k.sess = sess
	return k, nil
}

// open pins a pooled connection (already set to ISO NLS formats) for the console.
func (k *console) open(ctx context.Context) (*sqlbase.Session, error) {
	sc, err := k.c.db.Conn(ctx)
	if err != nil {
		return nil, cleanErr(err)
	}
	fail := func(err error) (*sqlbase.Session, error) {
		_ = sc.Raw(func(any) error { return sqldriver.ErrBadConn })
		sc.Close()
		return nil, cleanErr(err)
	}
	// go-ora commits every statement unless a transaction is open. Opening one
	// at the driver level keeps changes pending until COMMIT or ROLLBACK,
	// which then run as plain SQL.
	if err := sc.Raw(func(dc any) error {
		b, ok := dc.(sqldriver.ConnBeginTx)
		if !ok {
			return errors.New("the driver connection cannot start transactions")
		}
		_, err := b.BeginTx(ctx, sqldriver.TxOptions{})
		return err
	}); err != nil {
		return fail(err)
	}
	setup := []string{"BEGIN DBMS_OUTPUT.ENABLE(NULL); END;"}
	if k.scope.Schema != "" && k.scope.Schema != k.c.user {
		setup = append(setup, "ALTER SESSION SET CURRENT_SCHEMA = "+quote(k.scope.Schema))
	}
	for _, q := range setup {
		if _, err := sc.ExecContext(ctx, q); err != nil {
			return fail(err)
		}
	}
	return sqlbase.NewSession(sc, dialect{c: k.c}, sqlbase.SessionHooks{
		Prepare:     k.prepare,
		After:       k.after,
		InTx:        k.inTransaction,
		MapError:    mapError,
		ReturnsRows: returnsRows,
		Close:       rollback,
	}), nil
}

func (k *console) Execute(ctx context.Context, script string, opts driver.ExecOptions, sink driver.Sink) error {
	k.mu.Lock()
	if k.broken.Load() {
		_ = k.sess.Close()
		sess, err := k.open(ctx)
		if err != nil {
			k.mu.Unlock()
			return err
		}
		k.sess, k.txKnown, k.inTx = sess, false, false
		k.broken.Store(false)
	}
	sess := k.sess
	k.mu.Unlock()
	return sess.Execute(ctx, script, opts, consoleSink{Sink: sink, k: k})
}

func (k *console) InTransaction() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return !k.broken.Load() && k.sess.InTransaction()
}

func (k *console) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.sess.Close()
}

// consoleSink renames wire types and reports compilation errors and driver
// failures of the statement that just ran.
type consoleSink struct {
	driver.Sink
	k *console
}

func (s consoleSink) Columns(cols []driver.ResultColumn) error {
	for i := range cols {
		cols[i].Type = sqlTypeName(cols[i].Type)
	}
	return s.Sink.Columns(cols)
}

func (s consoleSink) EndStatement(err error) error {
	if err == nil {
		err = s.k.compileErr
	}
	s.k.compileErr = nil
	if errors.Is(err, errDriverFailure) {
		s.k.broken.Store(true)
	}
	return s.Sink.EndStatement(err)
}

func (k *console) prepare(_ context.Context, _ *sql.Conn, st sqlsplit.Statement, _ driver.ExecOptions) (string, error) {
	k.stmt, k.unit, k.txKnown = st, parseUnit(st.SQL), false
	return consoleSQL(st.SQL), nil
}

// after collects compilation errors of a created unit, DBMS_OUTPUT lines and
// the transaction state, even when the statement failed.
func (k *console) after(ctx context.Context, sc *sql.Conn, sink driver.Sink) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if k.unit != nil {
		k.compileErr = k.unit.errors(ctx, sc, k.stmt, sink)
		k.unit = nil
	}
	k.txKnown = k.drainOutput(ctx, sc, sink) == nil
}

var errTxUnknown = errors.New("transaction state unknown")

// inTransaction reports the state after() read with DBMS_TRANSACTION.
func (k *console) inTransaction(context.Context, *sql.Conn) (bool, error) {
	if !k.txKnown {
		return false, errTxUnknown
	}
	return k.inTx, nil
}

func rollback(sc *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = sc.ExecContext(ctx, "ROLLBACK")
}

// outputBlock drains DBMS_OUTPUT into one newline-joined chunk of at most
// 32000 bytes. A line that does not fit is returned separately (:2) with
// status 0, meaning more lines may follow. It also reports whether a
// transaction is open, saving a round trip per statement.
const outputBlock = `DECLARE
	l VARCHAR2(32767);
	s INTEGER;
	b VARCHAR2(32767);
	o VARCHAR2(32767);
BEGIN
	LOOP
		DBMS_OUTPUT.GET_LINE(l, s);
		EXIT WHEN s <> 0;
		IF NVL(LENGTHB(b), 0) + NVL(LENGTHB(l), 0) >= 32000 THEN
			o := l;
			EXIT;
		END IF;
		b := b || l || CHR(10);
	END LOOP;
	:1 := b;
	:2 := o;
	:3 := s;
	:4 := CASE WHEN DBMS_TRANSACTION.LOCAL_TRANSACTION_ID IS NULL THEN 0 ELSE 1 END;
END;`

const maxOutputLines = 5000

func (k *console) drainOutput(ctx context.Context, sc *sql.Conn, sink driver.Sink) error {
	sent := 0
	for {
		var buf, over string
		var status, tx int64
		if _, err := sc.ExecContext(ctx, outputBlock, go_ora.Out{Dest: &buf, Size: 32767}, go_ora.Out{Dest: &over, Size: 32767},
			sql.Out{Dest: &status}, sql.Out{Dest: &tx}); err != nil {
			return err
		}
		k.inTx = tx != 0
		lines := outputLines(buf)
		if status == 0 {
			lines = append(lines, over)
		}
		for _, l := range lines {
			if sent == maxOutputLines {
				_ = sink.Notice("warning", fmt.Sprintf("DBMS_OUTPUT: only the first %d lines are shown", maxOutputLines))
				_, err := sc.ExecContext(ctx, "BEGIN DBMS_OUTPUT.DISABLE; DBMS_OUTPUT.ENABLE(NULL); END;")
				return err
			}
			_ = sink.Notice("output", l)
			sent++
		}
		if status != 0 {
			return nil
		}
	}
}

func outputLines(buf string) []string {
	if buf == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(buf, "\n"), "\n")
}

// ---- statement preparation -------------------------------------------------------

// consoleSQL readies a console statement for go-ora: SQL*Plus EXEC becomes an
// anonymous block, plain SQL loses trailing semicolons (the server rejects
// them) and PL/SQL keeps its final "END;".
func consoleSQL(s string) string {
	if w := leadingWords(s, 1); len(w) == 1 && (w[0] == "EXEC" || w[0] == "EXECUTE") {
		call := strings.TrimRightFunc(s[codeStart(s)+len(w[0]):], isTrailing)
		return "BEGIN " + strings.TrimSpace(call) + "; END;"
	}
	if isPLSQL(s) {
		return s
	}
	return strings.TrimRightFunc(s, isTrailing)
}

func isTrailing(r rune) bool { return r == ';' || unicode.IsSpace(r) }

// isPLSQL reports anonymous blocks and stored program units.
func isPLSQL(s string) bool {
	w := leadingWords(s, 6)
	if len(w) == 0 {
		return false
	}
	switch w[0] {
	case "DECLARE", "BEGIN":
		return true
	case "CREATE":
		for _, x := range w[1:] {
			switch x {
			case "OR", "REPLACE", "EDITIONABLE", "NONEDITIONABLE", "AND", "COMPILE", "RESOLVE", "NOFORCE":
				continue
			case "PROCEDURE", "FUNCTION", "PACKAGE", "TRIGGER", "TYPE", "LIBRARY", "JAVA":
				return true
			}
			return false
		}
	}
	return false
}

// returnsRows runs queries with Query and everything else, including PL/SQL
// blocks and DDL, with Exec.
func returnsRows(st sqlsplit.Statement) bool {
	w := leadingWords(st.SQL, 1)
	return len(w) == 1 && (w[0] == "SELECT" || w[0] == "WITH")
}

// codeStart skips leading whitespace and comments.
func codeStart(s string) int {
	i := 0
	for i < len(s) {
		switch {
		case s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\f':
			i++
		case strings.HasPrefix(s[i:], "--"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				return len(s)
			}
			i += j + 1
		case strings.HasPrefix(s[i:], "/*"):
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return len(s)
			}
			i += j + 4
		default:
			return i
		}
	}
	return i
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c == '#' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// leadingWords returns up to n upper-cased leading keywords, skipping
// comments and parentheses before the first one.
func leadingWords(s string, n int) []string {
	var out []string
	i := 0
	for len(out) < n {
		i += codeStart(s[i:])
		for len(out) == 0 && i < len(s) && s[i] == '(' {
			i++
			i += codeStart(s[i:])
		}
		j := i
		for j < len(s) && isWordByte(s[j]) {
			j++
		}
		if j == i {
			break
		}
		out = append(out, strings.ToUpper(s[i:j]))
		i = j
	}
	return out
}

// ---- compilation errors ------------------------------------------------------------

// plsqlUnit identifies a stored unit created by a console statement. Oracle
// reports its compilation errors only through ALL_ERRORS.
type plsqlUnit struct {
	typ, owner, name string
	line             int // 0-based line of the unit keyword within the statement
}

var unitRe = regexp.MustCompile(`(?is)^CREATE\s+(?:OR\s+REPLACE\s+)?(?:(?:NON)?EDITIONABLE\s+)?` +
	`(PACKAGE\s+BODY|TYPE\s+BODY|PROCEDURE|FUNCTION|PACKAGE|TRIGGER|TYPE)\s+(?:IF\s+NOT\s+EXISTS\s+)?` +
	`(?:("[^"]+"|[a-z][\w$#]*)\s*\.\s*)?("[^"]+"|[a-z][\w$#]*)`)

func parseUnit(s string) *plsqlUnit {
	start := codeStart(s)
	m := unitRe.FindStringSubmatchIndex(s[start:])
	if m == nil {
		return nil
	}
	sub := func(i int) string {
		if m[2*i] < 0 {
			return ""
		}
		return s[start+m[2*i] : start+m[2*i+1]]
	}
	return &plsqlUnit{typ: strings.ToUpper(strings.Join(strings.Fields(sub(1)), " ")), owner: unitName(sub(2)), name: unitName(sub(3)),
		line: strings.Count(s[:start+m[2]], "\n")}
}

func unitName(n string) string {
	if strings.HasPrefix(n, `"`) {
		return strings.Trim(n, `"`)
	}
	return strings.ToUpper(n)
}

var codeRe = regexp.MustCompile(`^([A-Z]{3}-\d{5}):`)

// errors returns the first compilation error as a statement error (all of
// them in Detail) and forwards compiler warnings as notices.
func (u *plsqlUnit) errors(ctx context.Context, sc *sql.Conn, st sqlsplit.Statement, sink driver.Sink) error {
	rows, err := sc.QueryContext(ctx, `SELECT LINE, POSITION, TEXT, ATTRIBUTE FROM ALL_ERRORS
		WHERE OWNER = NVL(:1, SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA')) AND NAME = :2 AND TYPE = :3 ORDER BY SEQUENCE`, u.owner, u.name, u.typ)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var first *driver.QueryError
	var all []string
	for rows.Next() {
		var line, pos int
		var text, attr sql.NullString
		if rows.Scan(&line, &pos, &text, &attr) != nil {
			continue
		}
		msg := strings.TrimSpace(text.String)
		where := fmt.Sprintf("line %d, column %d: %s", line, pos, msg)
		if attr.String == "WARNING" {
			_ = sink.Notice("warning", where)
			continue
		}
		all = append(all, where)
		if first == nil {
			first = &driver.QueryError{Message: msg, Line: st.Line + u.line + line - 1}
			if m := codeRe.FindStringSubmatch(msg); m != nil {
				first.Code = m[1]
			}
		}
	}
	if first == nil {
		return nil
	}
	first.Detail = fmt.Sprintf("%s %s was created with compilation errors:\n%s", u.typ, u.name, strings.Join(all, "\n"))
	return first
}

// ---- errors ------------------------------------------------------------------------

func mapError(err error, st sqlsplit.Statement) error {
	var oe *network.OracleError
	if !errors.As(err, &oe) {
		return err
	}
	if oe.ErrMsg == "" {
		_ = oe.Error() // fills ErrMsg for codes the server sent without text
	}
	return queryError(oe.ErrCode, oe.ErrMsg, oe.ErrPos(), st)
}

var (
	plsLineRe = regexp.MustCompile(`^ORA-06550: line (\d+), column \d+:`)
	atLineRe  = regexp.MustCompile(`ORA-06512: at line (\d+)`)
)

// queryError locates an Oracle error in the script: PL/SQL compilation
// errors name a line and column, runtime errors in anonymous blocks end with
// "at line N", and SQL parse errors carry a character offset.
func queryError(code int, msg string, pos int, st sqlsplit.Statement) *driver.QueryError {
	lines := strings.Split(strings.TrimSpace(msg), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	qe := &driver.QueryError{Code: oraCode(code), Message: lines[0]}
	if len(lines) > 1 {
		qe.Detail = strings.Join(lines[1:], "\n")
	}
	if m := plsLineRe.FindStringSubmatch(lines[0]); m != nil {
		n, _ := strconv.Atoi(m[1])
		qe.Line = st.Line + n - 1
		if len(lines) > 1 {
			qe.Message, qe.Detail = lines[1], strings.Join(lines, "\n")
		}
	} else if m := atLineRe.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		qe.Line = st.Line + n - 1
	}
	if pos > 0 {
		qe.Position = pos + 1
		if qe.Line == 0 {
			qe.Line = st.Line + strings.Count(prefixRunes(st.SQL, pos), "\n")
		}
	}
	return qe
}

func prefixRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}
