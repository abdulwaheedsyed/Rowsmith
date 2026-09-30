package mongodb

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"rowsmith/internal/driver"
)

// DDL generation (driver.DDLGenerator). The "statements" are console
// commands in the mongosh syntax shell.go parses, qualified with
// db.getSiblingDB(...) so they run against the right database from any
// console. Documents have no fixed schema, so columns are ignored: the
// designer manages indexes and collection options.

var design = driver.TableDesign{
	Indexes: true, PartialIndexes: true,
	// "" is a regular ascending/descending key. For text every field is a
	// text field; for the others the first field gets the type.
	IndexTypes: []string{"", "text", "2dsphere", "2d", "hashed"},
	Options: []driver.Field{
		{Key: "capped", Label: "Capped collection", Type: driver.FieldBool, Span: 2,
			Help: "Fixed size, insertion order; the oldest documents make room for new ones."},
		{Key: "size", Label: "Maximum size (bytes)", Type: driver.FieldNumber, Span: 2, ShowIf: map[string][]string{"capped": {"true"}}},
		{Key: "max", Label: "Maximum documents", Type: driver.FieldNumber, Span: 2, ShowIf: map[string][]string{"capped": {"true"}}},
		{Key: "validator", Label: "Validator", Type: driver.FieldTextarea,
			Placeholder: `{"$jsonSchema": {"bsonType": "object", "required": ["name"]}}`,
			Help:        "A query document or $jsonSchema that inserted and updated documents must match."},
		{Key: "validationLevel", Label: "Validation level", Type: driver.FieldSelect, Span: 3, Options: []driver.Option{
			{Value: "strict", Label: "Strict: all inserts and updates"}, {Value: "moderate", Label: "Moderate: valid documents only"},
			{Value: "off", Label: "Off"}}},
		{Key: "validationAction", Label: "Invalid documents", Type: driver.FieldSelect, Span: 3, Options: []driver.Option{
			{Value: "error", Label: "Reject"}, {Value: "warn", Label: "Accept and log a warning"}}},
	},
	Note: "Documents have no fixed schema; the designer manages indexes and collection options. " +
		"Changed indexes are dropped and rebuilt. An index comment such as \"TTL 3600s\" expires documents after that many seconds.",
}

// ---- shell literals -------------------------------------------------------------

// shellLiteral renders a BSON value in the console's literal syntax;
// parseLiteral reads it back to the same value (whole numbers that fit
// become int32, as in mongosh).
func shellLiteral(v any) string {
	var b strings.Builder
	writeLiteral(&b, v, "", "")
	return b.String()
}

// shellLiteralIndent is shellLiteral spread over lines, for text areas.
func shellLiteralIndent(v any) string {
	var b strings.Builder
	writeLiteral(&b, v, "\n", "  ")
	return b.String()
}

func quoteStr(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

func writeLiteral(b *strings.Builder, v any, nl, step string) {
	sep := ", "
	if step != "" {
		sep = ","
	}
	list := func(open, close string, n int, item func(i int, nl string)) {
		if n == 0 {
			b.WriteString(open + close)
			return
		}
		inner := nl + step
		b.WriteString(open)
		if step != "" {
			b.WriteString(inner)
		}
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(sep)
				if step != "" {
					b.WriteString(inner)
				}
			}
			item(i, inner)
		}
		if step != "" {
			b.WriteString(nl)
		}
		b.WriteString(close)
	}
	switch x := v.(type) {
	case nil, bson.Null:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		b.WriteString(quoteStr(x))
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int:
		b.WriteString(strconv.Itoa(x))
	case float64:
		b.WriteString(floatLiteral(x))
	case bson.Decimal128:
		b.WriteString("NumberDecimal(" + quoteStr(x.String()) + ")")
	case bson.ObjectID:
		b.WriteString(`ObjectId("` + x.Hex() + `")`)
	case bson.DateTime:
		b.WriteString(`ISODate("` + x.Time().UTC().Format("2006-01-02T15:04:05.000Z") + `")`)
	case time.Time:
		b.WriteString(`ISODate("` + x.UTC().Format("2006-01-02T15:04:05.000Z") + `")`)
	case bson.Regex:
		if !strings.ContainsAny(x.Pattern, "/\n") && x.Pattern != "" {
			b.WriteString("/" + x.Pattern + "/" + x.Options)
		} else {
			b.WriteString("RegExp(" + quoteStr(x.Pattern) + ", " + quoteStr(x.Options) + ")")
		}
	case bson.Binary:
		if x.Subtype == 4 && len(x.Data) == 16 {
			b.WriteString(`UUID("` + hex.EncodeToString(x.Data) + `")`)
		} else {
			fmt.Fprintf(b, "BinData(%d, %s)", x.Subtype, quoteStr(base64.StdEncoding.EncodeToString(x.Data)))
		}
	case bson.Timestamp:
		fmt.Fprintf(b, "Timestamp(%d, %d)", x.T, x.I)
	case bson.MinKey:
		b.WriteString("MinKey")
	case bson.MaxKey:
		b.WriteString("MaxKey")
	case bson.Undefined:
		b.WriteString("undefined")
	case bson.D:
		list("{", "}", len(x), func(i int, nl string) {
			b.WriteString(quoteStr(x[i].Key) + ": ")
			writeLiteral(b, x[i].Value, nl, step)
		})
	case bson.M:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		d := make(bson.D, len(keys))
		for i, k := range keys {
			d[i] = bson.E{Key: k, Value: x[k]}
		}
		writeLiteral(b, d, nl, step)
	case map[string]any:
		writeLiteral(b, bson.M(x), nl, step)
	case bson.A:
		list("[", "]", len(x), func(i int, nl string) { writeLiteral(b, x[i], nl, step) })
	case []any:
		writeLiteral(b, bson.A(x), nl, step)
	default:
		// Types the console cannot write (JavaScript, symbols, DB pointers):
		// fall back to Extended JSON so they are at least readable.
		raw, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: x}}, false, false)
		if err != nil {
			b.WriteString(quoteStr(fmt.Sprint(x)))
			return
		}
		b.WriteString(strings.TrimSuffix(strings.TrimPrefix(string(raw), `{"v":`), "}"))
	}
}

// floatLiteral keeps doubles doubles: 5.0 stays "5.0", not the int32 "5".
func floatLiteral(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return `Double("-Infinity")`
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return s
}

// numText renders a numeric option for display: 1048576, not 1.048576e+06.
func numText(v any) string {
	switch x := v.(type) {
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bson.Decimal128:
		return x.String()
	}
	return fmt.Sprint(v)
}

// parseDoc reads a document written in console syntax; "" is no document.
func parseDoc(s, what string) (bson.D, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	v, err := parseLiteral(s)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid: %s", what, strings.TrimPrefix(err.Error(), "line 1: "))
	}
	d, ok := v.(bson.D)
	if !ok {
		return nil, fmt.Errorf("%s must be a document such as {\"status\": \"active\"}", what)
	}
	return d, nil
}

// ---- indexes --------------------------------------------------------------------

// creatableKeys turns a listed index key into the key createIndex takes:
// text indexes are stored as {_fts: "text", _ftsx: 1} with the fields in
// the weights document.
func creatableKeys(key, weights bson.D) bson.D {
	out := bson.D{}
	for _, e := range key {
		switch e.Key {
		case "_fts":
			for _, w := range weights {
				out = append(out, bson.E{Key: w.Key, Value: "text"})
			}
		case "_ftsx":
		default:
			out = append(out, e)
		}
	}
	return out
}

// keyColumns maps a key document to index columns: the field paths, which
// ones descend, and the special index type if any field has one.
func keyColumns(key bson.D) (cols []string, desc []bool, typ string) {
	for _, e := range key {
		cols = append(cols, e.Key)
		if s, ok := e.Value.(string); ok {
			if typ == "" {
				typ = s
			}
			desc = append(desc, false)
			continue
		}
		desc = append(desc, toFloat(e.Value) < 0)
	}
	return cols, desc, typ
}

// Index options the server fills in or that belong to the key itself.
var impliedIndexOptions = map[string]bool{"v": true, "ns": true, "key": true, "textIndexVersion": true, "2dsphereIndexVersion": true}

// describeIndex converts one listIndexes entry. Definition holds the exact
// createIndex command, which keeps options the editor does not show
// (sparse, collation, weights…) and the per-field types of mixed indexes.
func describeIndex(coll string, spec bson.D) driver.Index {
	var ix driver.Index
	var key, weights bson.D
	for _, e := range spec {
		switch e.Key {
		case "name":
			ix.Name = fmt.Sprint(e.Value)
		case "key":
			key, _ = e.Value.(bson.D)
		case "weights":
			weights, _ = e.Value.(bson.D)
		case "unique":
			ix.Unique, _ = e.Value.(bool)
		case "partialFilterExpression":
			ix.Where = shellLiteral(e.Value)
		case "expireAfterSeconds":
			ix.Comment = "TTL " + numText(e.Value) + "s"
		}
	}
	key = creatableKeys(key, weights)
	ix.Columns, ix.Desc, ix.Type = keyColumns(key)
	if ix.Name == "_id_" {
		ix.Primary, ix.Unique = true, true
	}
	opts := bson.D{{Key: "name", Value: ix.Name}}
	for _, e := range spec {
		if impliedIndexOptions[e.Key] || e.Key == "name" || textDefault(e, weights) {
			continue
		}
		opts = append(opts, e)
	}
	ix.Definition = "db.getCollection(" + quoteStr(coll) + ").createIndex(" + shellLiteral(key) + ", " + shellLiteral(opts) + ")"
	return ix
}

// textDefault reports text index options that only restate the defaults.
func textDefault(e bson.E, weights bson.D) bool {
	switch e.Key {
	case "weights":
		for _, w := range weights {
			if toFloat(w.Value) != 1 {
				return false
			}
		}
		return true
	case "default_language":
		return e.Value == "english"
	case "language_override":
		return e.Value == "language"
	}
	return false
}

// indexSpec is an index as createIndex takes it.
type indexSpec struct {
	name string
	key  bson.D
	opts bson.D // name first
}

func (s indexSpec) String() string { return shellLiteral(s.key) + ", " + shellLiteral(s.opts) }

var ttlRe = regexp.MustCompile(`(?i)^\s*TTL\s+(\d+)\s*(?:s|sec|seconds?)?\s*$`)

// definitionParts reads the key and options back from Index.Definition.
func definitionParts(def string) (key, opts bson.D) {
	if def == "" {
		return nil, nil
	}
	cmds, err := parseScript(def)
	if err != nil || len(cmds) != 1 || len(cmds[0].calls) != 1 || cmds[0].calls[0].name != "createIndex" {
		return nil, nil
	}
	args := cmds[0].calls[0].args
	key, _ = argDoc(args, 0)
	opts, _ = argDoc(args, 1)
	return key, opts
}

// specOf resolves an edited index to what createIndex will receive. The
// key comes from Definition while the fields still match it, so indexes
// that mix special and regular fields survive other edits; editable
// options (unique, filter, TTL) always come from the index itself.
func specOf(ix driver.Index) (indexSpec, error) {
	cols := make([]string, len(ix.Columns))
	for i, c := range ix.Columns {
		cols[i] = strings.TrimSpace(c)
		if cols[i] == "" {
			return indexSpec{}, fmt.Errorf("index %s has an empty field name", orDefault(ix.Name, "(new)"))
		}
	}
	if len(cols) == 0 {
		return indexSpec{}, fmt.Errorf("index %s needs at least one field", orDefault(ix.Name, "(new)"))
	}
	desc := make([]bool, len(cols))
	copy(desc, ix.Desc)
	typ := strings.TrimSpace(ix.Type)

	seen := map[string]bool{}
	for _, c := range cols {
		if seen[c] {
			return indexSpec{}, fmt.Errorf("index %s lists %s twice", orDefault(ix.Name, "(new)"), c)
		}
		seen[c] = true
	}
	defKey, defOpts := definitionParts(ix.Definition)
	key := defKey
	if dc, dd, dt := keyColumns(defKey); defKey == nil || !slicesEqual(dc, cols) || dt != typ || !sameDirections(defKey, dd, desc) {
		key = buildKey(cols, desc, typ, defKey)
	}

	name := strings.TrimSpace(ix.Name)
	if name == "" {
		parts := make([]string, len(key))
		for i, e := range key {
			parts[i] = e.Key + "_" + fmt.Sprint(e.Value)
		}
		name = strings.Join(parts, "_")
	}
	opts := bson.D{{Key: "name", Value: name}}
	if ix.Unique {
		opts = append(opts, bson.E{Key: "unique", Value: true})
	}
	filter, err := parseDoc(ix.Where, "the filter of index "+name)
	if err != nil {
		return indexSpec{}, err
	}
	if filter != nil {
		opts = append(opts, bson.E{Key: "partialFilterExpression", Value: filter})
	}
	if m := ttlRe.FindStringSubmatch(ix.Comment); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		opts = append(opts, bson.E{Key: "expireAfterSeconds", Value: n})
	}
	textFields := map[string]bool{}
	for _, e := range key {
		if e.Value == "text" {
			textFields[e.Key] = true
		}
	}
	for _, e := range defOpts {
		switch e.Key {
		case "name", "unique", "partialFilterExpression", "expireAfterSeconds":
			continue
		case "weights":
			// Keep the weights of fields that are still text fields.
			w, _ := e.Value.(bson.D)
			var kept bson.D
			for _, f := range w {
				if textFields[f.Key] {
					kept = append(kept, f)
				}
			}
			if len(kept) == 0 {
				continue
			}
			e.Value = kept
		case "default_language", "language_override":
			if len(textFields) == 0 {
				continue
			}
		}
		opts = append(opts, e)
	}
	return indexSpec{name: name, key: key, opts: opts}, nil
}

// buildKey writes the key of an edited index. Fields keep the role (special
// or regular) they had in the previous key; a new field of a text index is
// a text field, and in other special indexes the first field takes the type
// when no field has it.
func buildKey(cols []string, desc []bool, typ string, prev bson.D) bson.D {
	was := map[string]bool{}
	hadSpecial := false
	for _, e := range prev {
		_, s := e.Value.(string)
		was[e.Key] = s
		hadSpecial = hadSpecial || s
	}
	key := make(bson.D, len(cols))
	anySpecial := false
	for i, c := range cols {
		special, known := was[c]
		switch {
		case typ == "":
			special = false
		case typ == "text":
			special = !(hadSpecial && known && !special)
		default:
			special = hadSpecial && known && special
		}
		var v any = int32(1)
		switch {
		case special:
			v, anySpecial = typ, true
		case desc[i]:
			v = int32(-1)
		}
		key[i] = bson.E{Key: c, Value: v}
	}
	if typ != "" && !anySpecial {
		key[0].Value = typ
	}
	return key
}

// sameDirections compares the regular fields' directions; special fields
// have none.
func sameDirections(key bson.D, a, b []bool) bool {
	for i, e := range key {
		if _, special := e.Value.(string); !special && a[i] != b[i] {
			return false
		}
	}
	return true
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// indexSpecs resolves the non-_id indexes of a definition by name.
func indexSpecs(list []driver.Index) ([]indexSpec, map[string]indexSpec, error) {
	var order []indexSpec
	byName := map[string]indexSpec{}
	for _, ix := range list {
		if ix.Primary || ix.Name == "_id_" {
			continue
		}
		s, err := specOf(ix)
		if err != nil {
			return nil, nil, err
		}
		if _, dup := byName[s.name]; dup {
			return nil, nil, fmt.Errorf("two indexes are named %s", s.name)
		}
		byName[s.name] = s
		order = append(order, s)
	}
	return order, byName, nil
}

// ---- statements -----------------------------------------------------------------

func dbExpr(database string) string {
	if database == "" {
		return "db"
	}
	return "db.getSiblingDB(" + quoteStr(database) + ")"
}

func collExpr(ref driver.ObjectRef) string {
	return dbExpr(ref.Database) + ".getCollection(" + quoteStr(ref.Name) + ")"
}

func createIndexStmt(ref driver.ObjectRef, s indexSpec) string {
	return collExpr(ref) + ".createIndex(" + s.String() + ")"
}

func boolOpt(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// numOpt reads a numeric option; "" and 0 mean unset.
func numOpt(opts map[string]string, key, what string) (int64, error) {
	s := strings.TrimSpace(opts[key])
	if s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || f != math.Trunc(f) || f > math.MaxInt64/2 {
		return 0, fmt.Errorf("%s must be a whole number", what)
	}
	return int64(f), nil
}

// collOptions is the designable part of a collection's options.
type collOptions struct {
	capped    bool
	size, max int64
	validator bson.D
	level     string
	action    string
}

func readCollOptions(opts map[string]string) (collOptions, error) {
	var o collOptions
	var err error
	o.capped = boolOpt(opts["capped"])
	if o.capped {
		if o.size, err = numOpt(opts, "size", "the maximum size"); err != nil {
			return o, err
		}
		if o.max, err = numOpt(opts, "max", "the maximum number of documents"); err != nil {
			return o, err
		}
	}
	if o.validator, err = parseDoc(opts["validator"], "the validator"); err != nil {
		return o, err
	}
	o.level = strings.TrimSpace(opts["validationLevel"])
	o.action = strings.TrimSpace(opts["validationAction"])
	return o, nil
}

func (c *conn) CreateTableSQL(def driver.TableDef) ([]string, error) {
	if strings.TrimSpace(def.Ref.Name) == "" {
		return nil, errors.New("give the collection a name")
	}
	o, err := readCollOptions(def.Options)
	if err != nil {
		return nil, err
	}
	opts := bson.D{}
	if o.capped {
		if o.size <= 0 {
			return nil, errors.New("a capped collection needs a maximum size in bytes")
		}
		opts = append(opts, bson.E{Key: "capped", Value: true}, bson.E{Key: "size", Value: o.size})
		if o.max > 0 {
			opts = append(opts, bson.E{Key: "max", Value: o.max})
		}
	}
	if len(o.validator) > 0 {
		opts = append(opts, bson.E{Key: "validator", Value: o.validator})
	}
	if o.level != "" {
		opts = append(opts, bson.E{Key: "validationLevel", Value: o.level})
	}
	if o.action != "" {
		opts = append(opts, bson.E{Key: "validationAction", Value: o.action})
	}
	create := dbExpr(def.Ref.Database) + ".createCollection(" + quoteStr(def.Ref.Name)
	if len(opts) > 0 {
		create += ", " + shellLiteral(opts)
	}
	out := []string{create + ")"}
	specs, _, err := indexSpecs(def.Indexes)
	if err != nil {
		return nil, err
	}
	for _, s := range specs {
		out = append(out, createIndexStmt(def.Ref, s))
	}
	return out, nil
}

// AlterTableSQL diffs indexes (changed ones are dropped and re-created) and
// collection options, then renames. Column differences are ignored.
func (c *conn) AlterTableSQL(from *driver.Table, to driver.TableDef) ([]string, error) {
	if from == nil {
		return nil, errors.New("current collection definition is missing")
	}
	ref := from.Ref
	renamed := to.Ref.Name != "" && to.Ref.Name != from.Ref.Name

	oldOpts, err := readCollOptions(from.Options)
	if err != nil {
		return nil, err
	}
	newOpts, err := readCollOptions(to.Options)
	if err != nil {
		return nil, err
	}
	fromSpecs, fromIx, err := indexSpecs(from.Indexes)
	if err != nil {
		return nil, err
	}
	toSpecs, toIx, err := indexSpecs(to.Indexes)
	if err != nil {
		return nil, err
	}

	var out []string
	converted := false
	switch {
	case !oldOpts.capped && newOpts.capped:
		if newOpts.size <= 0 {
			return nil, errors.New("a capped collection needs a maximum size in bytes")
		}
		// convertToCapped rebuilds the collection and keeps only the _id index.
		out = append(out, dbExpr(ref.Database)+".runCommand("+shellLiteral(bson.D{{Key: "convertToCapped", Value: ref.Name}, {Key: "size", Value: newOpts.size}})+")")
		converted = true
		oldOpts.capped, oldOpts.size, oldOpts.max = true, newOpts.size, 0
	case oldOpts.capped && !newOpts.capped:
		return nil, errors.New("MongoDB cannot turn a capped collection back into a regular one; copy its documents into a new collection instead")
	}

	mod := bson.D{{Key: "collMod", Value: ref.Name}}
	if newOpts.capped {
		if newOpts.size <= 0 {
			return nil, errors.New("a capped collection needs a maximum size in bytes")
		}
		if newOpts.size != oldOpts.size {
			mod = append(mod, bson.E{Key: "cappedSize", Value: newOpts.size})
		}
		if newOpts.max != oldOpts.max {
			mod = append(mod, bson.E{Key: "cappedMax", Value: newOpts.max})
		}
	}
	if shellLiteral(oldOpts.validator) != shellLiteral(newOpts.validator) {
		v := newOpts.validator
		if v == nil {
			v = bson.D{}
		}
		mod = append(mod, bson.E{Key: "validator", Value: v})
	}
	if orDefault(newOpts.level, "strict") != orDefault(oldOpts.level, "strict") {
		mod = append(mod, bson.E{Key: "validationLevel", Value: orDefault(newOpts.level, "strict")})
	}
	if orDefault(newOpts.action, "error") != orDefault(oldOpts.action, "error") {
		mod = append(mod, bson.E{Key: "validationAction", Value: orDefault(newOpts.action, "error")})
	}
	if len(mod) > 1 {
		out = append(out, dbExpr(ref.Database)+".runCommand("+shellLiteral(mod)+")")
	}

	for _, s := range fromSpecs {
		if converted {
			break // the conversion already dropped every index but _id
		}
		if n, ok := toIx[s.name]; !ok || n.String() != s.String() {
			out = append(out, collExpr(ref)+".dropIndex("+quoteStr(s.name)+")")
		}
	}
	for _, s := range toSpecs {
		if old, ok := fromIx[s.name]; converted || !ok || old.String() != s.String() {
			out = append(out, createIndexStmt(ref, s))
		}
	}

	if from.Kind == "view" && len(out) > 0 {
		return nil, errors.New("views have no indexes or collection options; change the view's pipeline instead")
	}
	if renamed {
		if from.Kind == "view" {
			return nil, errors.New("MongoDB cannot rename views; recreate the view under the new name")
		}
		out = append(out, collExpr(ref)+".renameCollection("+quoteStr(strings.TrimSpace(to.Ref.Name))+")")
	}
	return out, nil
}

func collectionKind(kind string) bool {
	return kind == "" || kind == "collection" || kind == "timeseries"
}

func (c *conn) DropObjectSQL(ref driver.ObjectRef, _ bool) ([]string, error) {
	if strings.TrimSpace(ref.Name) == "" {
		return nil, errors.New("choose a collection")
	}
	if !collectionKind(ref.Kind) && ref.Kind != "view" {
		return nil, fmt.Errorf("cannot drop objects of kind %q", ref.Kind)
	}
	return []string{collExpr(ref) + ".drop()"}, nil
}

func (c *conn) TruncateSQL(ref driver.ObjectRef) ([]string, error) {
	if strings.TrimSpace(ref.Name) == "" {
		return nil, errors.New("choose a collection")
	}
	if !collectionKind(ref.Kind) {
		return nil, fmt.Errorf("only collections can be emptied, not a %s", ref.Kind)
	}
	return []string{collExpr(ref) + ".deleteMany({})"}, nil
}

func (c *conn) RenameObjectSQL(ref driver.ObjectRef, newName string) ([]string, error) {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return nil, errors.New("enter a new name")
	}
	if !collectionKind(ref.Kind) {
		return nil, fmt.Errorf("MongoDB cannot rename a %s; recreate it under the new name", ref.Kind)
	}
	return []string{collExpr(ref) + ".renameCollection(" + quoteStr(newName) + ")"}, nil
}

// CreateDatabaseSQL: MongoDB creates a database together with its first
// collection, named in opts["collection"].
func (c *conn) CreateDatabaseSQL(name string, opts map[string]string) ([]string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("enter a database name")
	}
	if strings.ContainsAny(name, `/\. "$`) {
		return nil, errors.New(`database names cannot contain spaces or any of / \ . " $`)
	}
	coll := strings.TrimSpace(opts["collection"])
	if coll == "" {
		return nil, errors.New("MongoDB creates a database together with its first collection; name that collection")
	}
	return []string{dbExpr(name) + ".createCollection(" + quoteStr(coll) + ")"}, nil
}

func (c *conn) DropDatabaseSQL(name string) ([]string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("choose a database")
	}
	if systemDBs[name] {
		return nil, fmt.Errorf("%s is a system database and cannot be dropped", name)
	}
	return []string{dbExpr(name) + ".dropDatabase()"}, nil
}
