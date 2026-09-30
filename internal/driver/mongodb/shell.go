package mongodb

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"rowsmith/internal/driver"
)

// A small parser for the familiar mongosh syntax:
//
//	db.orders.find({ status: "paid", total: { $gt: 100 } }).sort({ placed: -1 }).limit(20)
//	db.getCollection("audit log").aggregate([{ $group: { _id: "$actor", n: { $sum: 1 } } }])
//	show collections · use analytics · db.runCommand({ ping: 1 })
//	db.getSiblingDB("shop").getCollection("orders").createIndex({ placed: -1 })
//
// Arguments are JSON5-like literals (unquoted keys, single quotes, trailing
// commas, comments) plus ObjectId(), ISODate(), NumberLong() and friends.
// Nothing is evaluated as JavaScript.

type call struct {
	name string
	args []any
}

// command is one parsed statement.
type command struct {
	text  string
	line  int
	start int
	end   int

	show string // "dbs", "collections", …
	use  string

	database string // from db.getSiblingDB(name); "" is the console's database
	target   string // "" for db-level calls, else the collection name
	calls    []call // method chain after db[.collection]
}

type lexer struct {
	s    string
	i    int
	line int
}

func (l *lexer) err(format string, a ...any) error {
	return fmt.Errorf("line %d: %s", l.line, fmt.Sprintf(format, a...))
}

func (l *lexer) skipSpace(newlines bool) {
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case c == '\n':
			if !newlines {
				return
			}
			l.line++
			l.i++
		case c == ' ' || c == '\t' || c == '\r':
			l.i++
		case strings.HasPrefix(l.s[l.i:], "//"):
			for l.i < len(l.s) && l.s[l.i] != '\n' {
				l.i++
			}
		case strings.HasPrefix(l.s[l.i:], "/*"):
			end := strings.Index(l.s[l.i+2:], "*/")
			if end < 0 {
				l.i = len(l.s)
				return
			}
			l.line += strings.Count(l.s[l.i:l.i+2+end], "\n")
			l.i += end + 4
		default:
			return
		}
	}
}

func (l *lexer) peek() byte {
	if l.i < len(l.s) {
		return l.s[l.i]
	}
	return 0
}

func (l *lexer) expect(c byte) error {
	l.skipSpace(true)
	if l.peek() != c {
		if l.i >= len(l.s) {
			return l.err("expected %q but the statement ended", c)
		}
		return l.err("expected %q near %q", c, snippet(l.s[l.i:]))
	}
	l.i++
	return nil
}

func snippet(s string) string {
	if len(s) > 24 {
		return s[:24] + "…"
	}
	return s
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isIdent(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

func (l *lexer) ident() string {
	start := l.i
	for l.i < len(l.s) && isIdent(l.s[l.i]) {
		l.i++
	}
	return l.s[start:l.i]
}

func (l *lexer) str() (string, error) {
	q := l.s[l.i]
	l.i++
	var b strings.Builder
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case c == q:
			l.i++
			return b.String(), nil
		case c == '\\' && l.i+1 < len(l.s):
			l.i++
			e := l.s[l.i]
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'u':
				if l.i+4 < len(l.s) {
					if r, err := strconv.ParseUint(l.s[l.i+1:l.i+5], 16, 32); err == nil {
						b.WriteRune(rune(r))
						l.i += 4
						break
					}
				}
				b.WriteByte('u')
			default:
				b.WriteByte(e)
			}
			l.i++
		default:
			if c == '\n' {
				l.line++
			}
			b.WriteByte(c)
			l.i++
		}
	}
	return "", l.err("unterminated string")
}

// value parses one literal.
func (l *lexer) value() (any, error) {
	l.skipSpace(true)
	if l.i >= len(l.s) {
		return nil, l.err("expected a value but the statement ended")
	}
	c := l.s[l.i]
	switch {
	case c == '{':
		return l.object()
	case c == '[':
		return l.array()
	case c == '"' || c == '\'' || c == '`':
		return l.str()
	case c == '/':
		return l.regex()
	case c == '-' || c == '+' || c == '.' || c >= '0' && c <= '9':
		return l.number()
	case isIdentStart(c):
		id := l.ident()
		switch id {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		case "undefined":
			return bson.Undefined{}, nil
		case "Infinity":
			return math.Inf(1), nil
		case "NaN":
			return math.NaN(), nil
		case "MinKey":
			l.optionalCall()
			return bson.MinKey{}, nil
		case "MaxKey":
			l.optionalCall()
			return bson.MaxKey{}, nil
		case "new":
			l.skipSpace(true)
			name := l.ident()
			return l.constructor(name)
		}
		return l.constructor(id)
	}
	return nil, l.err("unexpected %q", snippet(l.s[l.i:]))
}

func (l *lexer) optionalCall() {
	l.skipSpace(true)
	if l.peek() == '(' {
		l.i++
		l.skipSpace(true)
		if l.peek() == ')' {
			l.i++
		}
	}
}

func (l *lexer) constructor(name string) (any, error) {
	l.skipSpace(true)
	if l.peek() != '(' {
		return nil, l.err("unknown identifier %q (variables are not supported)", name)
	}
	args, err := l.args()
	if err != nil {
		return nil, err
	}
	arg := func(i int) (any, bool) {
		if i < len(args) {
			return args[i], true
		}
		return nil, false
	}
	switch name {
	case "ObjectId", "ObjectID":
		v, ok := arg(0)
		if !ok {
			return bson.NewObjectID(), nil
		}
		s, _ := v.(string)
		id, err := bson.ObjectIDFromHex(s)
		if err != nil {
			return nil, l.err("ObjectId needs 24 hex characters")
		}
		return id, nil
	case "ISODate", "Date":
		v, ok := arg(0)
		if !ok {
			return bson.NewDateTimeFromTime(time.Now()), nil
		}
		switch x := v.(type) {
		case string:
			t, ok := parseTime(x)
			if !ok {
				return nil, l.err("cannot parse date %q", x)
			}
			return bson.NewDateTimeFromTime(t), nil
		case int32, int64, float64:
			return bson.DateTime(toInt64(x)), nil
		}
		return nil, l.err("%s expects a string or milliseconds", name)
	case "NumberLong":
		v, _ := arg(0)
		switch x := v.(type) {
		case string:
			n, err := strconv.ParseInt(x, 10, 64)
			if err != nil {
				return nil, l.err("NumberLong: %v", err)
			}
			return n, nil
		default:
			return toInt64(x), nil
		}
	case "NumberInt", "Int32":
		v, _ := arg(0)
		if s, ok := v.(string); ok {
			n, err := strconv.ParseInt(s, 10, 32)
			if err != nil {
				return nil, l.err("NumberInt: %v", err)
			}
			return int32(n), nil
		}
		return int32(toInt64(v)), nil
	case "NumberDecimal", "Decimal128":
		v, _ := arg(0)
		d, err := bson.ParseDecimal128(fmt.Sprint(v))
		if err != nil {
			return nil, l.err("NumberDecimal: %v", err)
		}
		return d, nil
	case "Double":
		v, _ := arg(0)
		return toFloat(v), nil
	case "UUID":
		v, _ := arg(0)
		s, _ := v.(string)
		raw, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
		if err != nil || len(raw) != 16 {
			return nil, l.err("UUID needs 32 hex digits")
		}
		return bson.Binary{Subtype: 4, Data: raw}, nil
	case "BinData":
		sub, _ := arg(0)
		v, _ := arg(1)
		s, _ := v.(string)
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, l.err("BinData needs base64")
		}
		return bson.Binary{Subtype: byte(toInt64(sub)), Data: raw}, nil
	case "Timestamp":
		t, _ := arg(0)
		i, _ := arg(1)
		return bson.Timestamp{T: uint32(toInt64(t)), I: uint32(toInt64(i))}, nil
	case "RegExp":
		p, _ := arg(0)
		o, _ := arg(1)
		return bson.Regex{Pattern: fmt.Sprint(p), Options: optStr(o)}, nil
	}
	return nil, l.err("%s() is not supported", name)
}

func optStr(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int32:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

func (l *lexer) number() (any, error) {
	start := l.i
	if l.s[l.i] == '-' || l.s[l.i] == '+' {
		l.i++
	}
	for l.i < len(l.s) && (l.s[l.i] >= '0' && l.s[l.i] <= '9' || l.s[l.i] == '.' || l.s[l.i] == 'e' || l.s[l.i] == 'E' ||
		(l.s[l.i] == '-' || l.s[l.i] == '+') && (l.s[l.i-1] == 'e' || l.s[l.i-1] == 'E')) {
		l.i++
	}
	txt := strings.TrimPrefix(l.s[start:l.i], "+")
	if txt == "Infinity" {
		return math.Inf(1), nil
	}
	if n, err := strconv.ParseInt(txt, 10, 64); err == nil {
		// Like mongosh: whole numbers that fit are int32, larger ones int64.
		if n >= math.MinInt32 && n <= math.MaxInt32 {
			return int32(n), nil
		}
		return n, nil
	}
	f, err := strconv.ParseFloat(txt, 64)
	if err != nil {
		return nil, l.err("invalid number %q", txt)
	}
	return f, nil
}

func (l *lexer) regex() (any, error) {
	l.i++ // opening slash
	var b strings.Builder
	inClass := false
	for l.i < len(l.s) {
		c := l.s[l.i]
		if c == '\\' && l.i+1 < len(l.s) {
			b.WriteByte(c)
			b.WriteByte(l.s[l.i+1])
			l.i += 2
			continue
		}
		if c == '[' {
			inClass = true
		} else if c == ']' {
			inClass = false
		} else if c == '/' && !inClass {
			l.i++
			flags := l.ident()
			return bson.Regex{Pattern: b.String(), Options: flags}, nil
		} else if c == '\n' {
			break
		}
		b.WriteByte(c)
		l.i++
	}
	return nil, l.err("unterminated regular expression")
}

func (l *lexer) object() (any, error) {
	l.i++ // {
	d := bson.D{}
	for {
		l.skipSpace(true)
		if l.peek() == '}' {
			l.i++
			return d, nil
		}
		var key string
		switch c := l.peek(); {
		case c == '"' || c == '\'' || c == '`':
			k, err := l.str()
			if err != nil {
				return nil, err
			}
			key = k
		case isIdentStart(c) || c >= '0' && c <= '9':
			start := l.i
			for l.i < len(l.s) && (isIdent(l.s[l.i]) || l.s[l.i] == '.') {
				l.i++
			}
			key = l.s[start:l.i]
		default:
			return nil, l.err("expected a field name near %q", snippet(l.s[l.i:]))
		}
		if err := l.expect(':'); err != nil {
			return nil, err
		}
		v, err := l.value()
		if err != nil {
			return nil, err
		}
		d = append(d, bson.E{Key: key, Value: v})
		l.skipSpace(true)
		switch l.peek() {
		case ',':
			l.i++
		case '}':
		default:
			return nil, l.err("expected ',' or '}' near %q", snippet(l.s[l.i:]))
		}
	}
}

func (l *lexer) array() (any, error) {
	l.i++ // [
	a := bson.A{}
	for {
		l.skipSpace(true)
		if l.peek() == ']' {
			l.i++
			return a, nil
		}
		v, err := l.value()
		if err != nil {
			return nil, err
		}
		a = append(a, v)
		l.skipSpace(true)
		switch l.peek() {
		case ',':
			l.i++
		case ']':
		default:
			return nil, l.err("expected ',' or ']' near %q", snippet(l.s[l.i:]))
		}
	}
}

func (l *lexer) args() ([]any, error) {
	if err := l.expect('('); err != nil {
		return nil, err
	}
	var out []any
	for {
		l.skipSpace(true)
		if l.peek() == ')' {
			l.i++
			return out, nil
		}
		v, err := l.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		l.skipSpace(true)
		switch l.peek() {
		case ',':
			l.i++
		case ')':
		default:
			return nil, l.err("expected ',' or ')' near %q", snippet(l.s[l.i:]))
		}
	}
}

// parseLiteral parses a single standalone literal (used for grid edits and filters).
func parseLiteral(s string) (any, error) {
	l := &lexer{s: s, line: 1}
	v, err := l.value()
	if err != nil {
		return nil, err
	}
	l.skipSpace(true)
	if l.i < len(l.s) {
		return nil, l.err("unexpected %q after the value", snippet(l.s[l.i:]))
	}
	return v, nil
}

// parseScript splits a script into commands. Statements are separated by
// semicolons or by line breaks between complete expressions.
func parseScript(s string) ([]command, error) {
	l := &lexer{s: s, line: 1}
	var out []command
	for {
		l.skipSpace(true)
		for l.peek() == ';' {
			l.i++
			l.skipSpace(true)
		}
		if l.i >= len(l.s) {
			return out, nil
		}
		cmd := command{start: l.i, line: l.line}
		if err := l.statement(&cmd); err != nil {
			return nil, err
		}
		cmd.end = l.i
		cmd.text = strings.TrimSpace(s[cmd.start:cmd.end])
		out = append(out, cmd)
		l.skipSpace(false)
		switch c := l.peek(); c {
		case ';', '\n', 0:
		default:
			return nil, l.err("unexpected %q after the statement", snippet(l.s[l.i:]))
		}
	}
}

func (l *lexer) statement(cmd *command) error {
	if !isIdentStart(l.peek()) {
		return l.err("statements start with db., show or use")
	}
	word := l.ident()
	switch word {
	case "show":
		l.skipSpace(false)
		cmd.show = strings.ToLower(l.ident())
		if cmd.show == "" {
			return l.err("show what? Try show dbs or show collections")
		}
		return nil
	case "use":
		l.skipSpace(false)
		start := l.i
		for l.i < len(l.s) && l.s[l.i] != ';' && l.s[l.i] != '\n' && l.s[l.i] != ' ' {
			l.i++
		}
		cmd.use = strings.Trim(l.s[start:l.i], `"'`)
		if cmd.use == "" {
			return l.err("use needs a database name")
		}
		return nil
	case "db":
	default:
		return l.err("unknown statement %q; statements start with db., show or use", word)
	}
	for {
		// Method chains may continue on the next line (".sort(...)"), but a
		// line break before anything else ends the statement.
		save, saveLine := l.i, l.line
		l.skipSpace(true)
		if c := l.peek(); c != '.' && !(c == '[' && cmd.target == "" && len(cmd.calls) == 0) && len(cmd.calls) > 0 {
			l.i, l.line = save, saveLine
			return nil
		}
		switch l.peek() {
		case '.':
			l.i++
			l.skipSpace(true)
			name := l.ident()
			if name == "" {
				return l.err("expected a name after '.'")
			}
			l.skipSpace(true)
			if l.peek() == '(' {
				args, err := l.args()
				if err != nil {
					return err
				}
				if name == "getSiblingDB" && cmd.database == "" && cmd.target == "" && len(cmd.calls) == 0 {
					n, _ := firstString(args)
					if n == "" {
						return l.err("getSiblingDB needs a database name")
					}
					cmd.database = n
					continue
				}
				if name == "getCollection" && cmd.target == "" && len(cmd.calls) == 0 {
					n, _ := firstString(args)
					if n == "" {
						return l.err("getCollection needs a collection name")
					}
					cmd.target = n
					continue
				}
				cmd.calls = append(cmd.calls, call{name: name, args: args})
			} else if cmd.target == "" && len(cmd.calls) == 0 {
				cmd.target = name
			} else {
				return l.err("expected %s(...)", name)
			}
		case '[':
			if cmd.target != "" || len(cmd.calls) > 0 {
				return l.err("unexpected '['")
			}
			l.i++
			l.skipSpace(true)
			v, err := l.value()
			if err != nil {
				return err
			}
			cmd.target = fmt.Sprint(v)
			if err := l.expect(']'); err != nil {
				return err
			}
		default:
			if len(cmd.calls) == 0 {
				return l.err("call a method, for example db.%s.find()", orDefault(cmd.target, "collection"))
			}
			return nil
		}
	}
}

func firstString(args []any) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	s, ok := args[0].(string)
	return s, ok
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ---- classification ------------------------------------------------------------

var readMethods = map[string]bool{
	"find": true, "findOne": true, "aggregate": true, "countDocuments": true, "estimatedDocumentCount": true, "count": true,
	"distinct": true, "getIndexes": true, "stats": true, "explain": true, "getCollectionNames": true, "getCollectionInfos": true,
	"getName": true, "version": true, "serverStatus": true, "currentOp": true, "listCollections": true, "hostInfo": true,
	"sort": true, "limit": true, "skip": true, "project": true, "hint": true, "toArray": true, "pretty": true, "maxTimeMS": true,
	"collation": true, "comment": true, "batchSize": true, "itcount": true, "size": true, "isCapped": true, "dataSize": true,
	"storageSize": true, "totalIndexSize": true,
}

var readCommands = map[string]bool{
	"ping": true, "buildinfo": true, "serverstatus": true, "dbstats": true, "collstats": true, "listcollections": true,
	"listindexes": true, "listdatabases": true, "hello": true, "ismaster": true, "connectionstatus": true, "hostinfo": true,
	"getparameter": true, "getcmdlineopts": true, "count": true, "distinct": true, "find": true, "explain": true, "usersinfo": true,
	"rolesinfo": true, "currentop": true, "top": true, "validate": false, "getlog": true, "replsetgetstatus": true,
	"datasize": true, "features": true,
}

// ddlCommands change collections, views or indexes.
var ddlCommands = map[string]bool{
	"create": true, "createindexes": true, "collmod": true, "converttocapped": true, "renamecollection": true,
	"drop": true, "dropindexes": true, "dropdatabase": true,
}

// destructiveCommands remove data when run through runCommand.
var destructiveCommands = map[string]string{
	"drop": "drop removes the collection permanently", "dropdatabase": "dropDatabase removes the database permanently",
	"dropindexes": "dropIndexes removes indexes", "converttocapped": "convertToCapped discards documents beyond the size and drops secondary indexes",
}

func (c *command) kind() driver.StatementKind {
	if c.show != "" {
		return driver.StmtRead
	}
	if c.use != "" {
		return driver.StmtSession
	}
	for _, cl := range c.calls {
		switch cl.name {
		case "runCommand", "adminCommand":
			if len(cl.args) == 0 {
				return driver.StmtUnknown
			}
			d, ok := cl.args[0].(bson.D)
			if ok && len(d) > 0 && ddlCommands[strings.ToLower(d[0].Key)] {
				return driver.StmtDDL
			}
			if !ok || len(d) == 0 || !readCommands[strings.ToLower(d[0].Key)] {
				return driver.StmtWrite
			}
			if strings.EqualFold(d[0].Key, "aggregate") && pipelineWrites(d) {
				return driver.StmtWrite
			}
			continue
		case "aggregate":
			if len(cl.args) > 0 {
				if p, ok := cl.args[0].(bson.A); ok && stagesWrite(p) {
					return driver.StmtWrite
				}
			}
			continue
		case "createIndex", "createIndexes", "dropIndex", "dropIndexes", "drop", "dropDatabase", "createCollection", "createView",
			"renameCollection":
			return driver.StmtDDL
		}
		if !readMethods[cl.name] {
			return driver.StmtWrite
		}
	}
	return driver.StmtRead
}

func stagesWrite(p bson.A) bool {
	for _, st := range p {
		if d, ok := st.(bson.D); ok && len(d) > 0 && (d[0].Key == "$out" || d[0].Key == "$merge") {
			return true
		}
	}
	return false
}

func pipelineWrites(cmd bson.D) bool {
	for _, e := range cmd {
		if e.Key == "pipeline" {
			if p, ok := e.Value.(bson.A); ok {
				return stagesWrite(p)
			}
		}
	}
	return false
}

func (c *command) danger(k driver.StatementKind) driver.Danger {
	for _, cl := range c.calls {
		switch cl.name {
		case "drop", "dropDatabase", "dropIndexes":
			return driver.Danger{Level: "destructive", Reason: cl.name + " removes data permanently"}
		case "runCommand", "adminCommand":
			if len(cl.args) > 0 {
				if d, ok := cl.args[0].(bson.D); ok && len(d) > 0 && destructiveCommands[strings.ToLower(d[0].Key)] != "" {
					return driver.Danger{Level: "destructive", Reason: destructiveCommands[strings.ToLower(d[0].Key)]}
				}
			}
		case "deleteMany", "updateMany":
			if len(cl.args) == 0 {
				return driver.Danger{Level: "destructive", Reason: cl.name + " without a filter affects every document"}
			}
			if d, ok := cl.args[0].(bson.D); ok && len(d) == 0 {
				return driver.Danger{Level: "destructive", Reason: cl.name + "({}) affects every document"}
			}
		}
	}
	switch k {
	case driver.StmtRead, driver.StmtSession:
		return driver.Danger{}
	case driver.StmtDDL:
		return driver.Danger{Level: "caution", Reason: "changes collections or indexes"}
	}
	return driver.Danger{Level: "caution", Reason: "modifies data"}
}
