package mongodb

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"rowsmith/internal/driver"
)

var _ driver.DDLGenerator = (*conn)(nil)

// defFromTable builds the definition the structure editor submits for an
// existing collection: everything Describe reported, columns linked by name.
func defFromTable(t *driver.Table) driver.TableDef {
	def := driver.TableDef{Ref: t.Ref, PrimaryKey: append([]string(nil), t.PrimaryKey...), Comment: t.Comment,
		Indexes: append([]driver.Index(nil), t.Indexes...), Options: map[string]string{}}
	for _, c := range t.Columns {
		def.Columns = append(def.Columns, driver.ColumnDef{Column: c, OriginalName: c.Name})
	}
	for k, v := range t.Options {
		def.Options[k] = v
	}
	return def
}

func TestShellLiteralRoundTrip(t *testing.T) {
	id := bson.NewObjectID()
	when := bson.NewDateTimeFromTime(time.Date(2025, 1, 2, 3, 4, 5, 123e6, time.UTC))
	dec, _ := bson.ParseDecimal128("12.50")
	doc := bson.D{
		{Key: "s", Value: `quote " back \ tag <b> & ünï`}, {Key: "i", Value: int32(-7)}, {Key: "big", Value: int64(9007199254740993)},
		{Key: "f", Value: 5.0}, {Key: "g", Value: 1.5e-9}, {Key: "inf", Value: math.Inf(1)}, {Key: "ninf", Value: math.Inf(-1)},
		{Key: "b", Value: true}, {Key: "n", Value: nil}, {Key: "id", Value: id}, {Key: "at", Value: when}, {Key: "dec", Value: dec},
		{Key: "re", Value: bson.Regex{Pattern: "^ab+c", Options: "i"}}, {Key: "slash", Value: bson.Regex{Pattern: "a/b", Options: ""}},
		{Key: "uuid", Value: bson.Binary{Subtype: 4, Data: []byte("0123456789abcdef")}}, {Key: "bin", Value: bson.Binary{Subtype: 0, Data: []byte{0, 1, 2}}},
		{Key: "ts", Value: bson.Timestamp{T: 10, I: 2}}, {Key: "min", Value: bson.MinKey{}}, {Key: "max", Value: bson.MaxKey{}},
		{Key: "arr", Value: bson.A{int32(1), "x", bson.D{{Key: "$gt", Value: int32(2)}}, bson.A{}}}, {Key: "empty", Value: bson.D{}},
		{Key: "odd key.with $", Value: "v"},
	}
	for _, render := range []func(any) string{shellLiteral, shellLiteralIndent} {
		text := render(doc)
		back, err := parseLiteral(text)
		if err != nil {
			t.Fatalf("parse %s: %v", text, err)
		}
		if !reflect.DeepEqual(back, doc) {
			t.Fatalf("round trip changed the document:\n%s\n%#v", text, back)
		}
	}
	if got := shellLiteral(bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: bson.A{"x", 2.5}}}); got != `{"a": 1, "b": ["x", 2.5]}` {
		t.Errorf("compact = %s", got)
	}
	if got := shellLiteralIndent(bson.D{{Key: "a", Value: bson.D{{Key: "b", Value: int32(1)}}}}); got != "{\n  \"a\": {\n    \"b\": 1\n  }\n}" {
		t.Errorf("indented = %q", got)
	}
	if got := shellLiteral(int64(5)); got != "5" {
		t.Errorf("int64 = %s", got)
	}
}

// listed is what listIndexes returns for the test collection.
func listed() []bson.D {
	return []bson.D{
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "_id", Value: int32(1)}}}, {Key: "name", Value: "_id_"}},
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "customerId", Value: int32(1)}, {Key: "placedAt", Value: int32(-1)}}},
			{Key: "name", Value: "cust_date"}, {Key: "unique", Value: true}, {Key: "sparse", Value: true}},
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "status", Value: int32(1)}}}, {Key: "name", Value: "status_1"},
			{Key: "partialFilterExpression", Value: bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"new", "paid"}}}}}}},
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "_fts", Value: "text"}, {Key: "_ftsx", Value: int32(1)}}}, {Key: "name", Value: "search"},
			{Key: "weights", Value: bson.D{{Key: "body", Value: int32(1)}, {Key: "title", Value: int32(5)}}},
			{Key: "default_language", Value: "english"}, {Key: "language_override", Value: "language"}, {Key: "textIndexVersion", Value: int32(3)}},
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "category", Value: int32(1)}, {Key: "loc", Value: "2dsphere"}}},
			{Key: "name", Value: "geo"}, {Key: "2dsphereIndexVersion", Value: int32(3)}},
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "placedAt", Value: int32(1)}}}, {Key: "name", Value: "expire"},
			{Key: "expireAfterSeconds", Value: int32(86400)}},
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "sku", Value: "hashed"}}}, {Key: "name", Value: "sku_hashed"}},
	}
}

func describedOrders() *driver.Table {
	t := &driver.Table{Ref: driver.ObjectRef{Database: "shop", Name: "orders", Kind: "collection"}, Kind: "collection",
		Columns:    []driver.Column{{Name: "_id", Type: "objectId", PrimaryKey: true}, {Name: "total", Type: "double", Nullable: true}},
		PrimaryKey: []string{"_id"},
		Options: map[string]string{"capped": "true", "size": numText(1048576.0), "max": numText(int32(1000)),
			"validator": shellLiteralIndent(bson.D{{Key: "$jsonSchema", Value: bson.D{{Key: "bsonType", Value: "object"},
				{Key: "required", Value: bson.A{"customerId"}}}}}),
			"validationLevel": "strict", "validationAction": "error"}}
	for _, spec := range listed() {
		t.Indexes = append(t.Indexes, describeIndex("orders", spec))
	}
	return t
}

func TestDescribeIndex(t *testing.T) {
	ix := describedOrders().Indexes
	want := []driver.Index{
		{Name: "_id_", Columns: []string{"_id"}, Desc: []bool{false}, Primary: true, Unique: true,
			Definition: `db.getCollection("orders").createIndex({"_id": 1}, {"name": "_id_"})`},
		{Name: "cust_date", Columns: []string{"customerId", "placedAt"}, Desc: []bool{false, true}, Unique: true,
			Definition: `db.getCollection("orders").createIndex({"customerId": 1, "placedAt": -1}, {"name": "cust_date", "unique": true, "sparse": true})`},
		{Name: "status_1", Columns: []string{"status"}, Desc: []bool{false}, Where: `{"status": {"$in": ["new", "paid"]}}`,
			Definition: `db.getCollection("orders").createIndex({"status": 1}, {"name": "status_1", "partialFilterExpression": {"status": {"$in": ["new", "paid"]}}})`},
		{Name: "search", Columns: []string{"body", "title"}, Desc: []bool{false, false}, Type: "text",
			Definition: `db.getCollection("orders").createIndex({"body": "text", "title": "text"}, {"name": "search", "weights": {"body": 1, "title": 5}})`},
		{Name: "geo", Columns: []string{"category", "loc"}, Desc: []bool{false, false}, Type: "2dsphere",
			Definition: `db.getCollection("orders").createIndex({"category": 1, "loc": "2dsphere"}, {"name": "geo"})`},
		{Name: "expire", Columns: []string{"placedAt"}, Desc: []bool{false}, Comment: "TTL 86400s",
			Definition: `db.getCollection("orders").createIndex({"placedAt": 1}, {"name": "expire", "expireAfterSeconds": 86400})`},
		{Name: "sku_hashed", Columns: []string{"sku"}, Desc: []bool{false}, Type: "hashed",
			Definition: `db.getCollection("orders").createIndex({"sku": "hashed"}, {"name": "sku_hashed"})`},
	}
	if !reflect.DeepEqual(ix, want) {
		for i := range want {
			if i < len(ix) && !reflect.DeepEqual(ix[i], want[i]) {
				t.Errorf("index %d:\n got %+v\nwant %+v", i, ix[i], want[i])
			}
		}
		t.FailNow()
	}
	// Every definition is a console command.
	for _, x := range ix {
		if cmds, err := parseScript(x.Definition); err != nil || len(cmds) != 1 || cmds[0].kind() != driver.StmtDDL {
			t.Errorf("definition %q does not parse: %v", x.Definition, err)
		}
	}
}

// checkStatements asserts every generated statement parses as one console
// command and, joined like the server joins them, as a script.
func checkStatements(t *testing.T, stmts []string) {
	t.Helper()
	for _, s := range stmts {
		cmds, err := parseScript(s)
		if err != nil || len(cmds) != 1 {
			t.Errorf("statement %q does not parse: %v", s, err)
			continue
		}
		if k := cmds[0].kind(); k != driver.StmtDDL && k != driver.StmtWrite {
			t.Errorf("statement %q classified as %s", s, k)
		}
	}
	if cmds, err := parseScript(strings.Join(stmts, ";\n")); err != nil || len(cmds) != len(stmts) {
		t.Errorf("script of %d statements parsed into %d: %v", len(stmts), len(cmds), err)
	}
}

func TestMongoCreateTableSQL(t *testing.T) {
	c := &conn{}
	stmts, err := c.CreateTableSQL(driver.TableDef{
		Ref:     driver.ObjectRef{Database: "shop", Name: "events"},
		Columns: []driver.ColumnDef{{Column: driver.Column{Name: "ignored", Type: "string"}}},
		Options: map[string]string{"capped": "true", "size": "1048576", "max": "1000",
			"validator":       `{ $jsonSchema: { bsonType: "object", required: ['kind'] } }`,
			"validationLevel": "moderate", "validationAction": "warn"},
		Indexes: []driver.Index{
			{Name: "_id_", Primary: true, Columns: []string{"_id"}},
			{Name: "kind_at", Columns: []string{"kind", "at"}, Desc: []bool{false, true}, Unique: true, Where: `{kind: {$exists: true}}`},
			{Name: "search", Columns: []string{"title", "body"}, Type: "text"},
			{Name: "near", Columns: []string{"loc", "kind"}, Type: "2dsphere", Desc: []bool{false, true}},
			{Columns: []string{"at"}, Comment: "TTL 3600s"},
			{Name: "h", Columns: []string{"user.id"}, Type: "hashed"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	coll := `db.getSiblingDB("shop").getCollection("events")`
	want := []string{
		`db.getSiblingDB("shop").createCollection("events", {"capped": true, "size": 1048576, "max": 1000, ` +
			`"validator": {"$jsonSchema": {"bsonType": "object", "required": ["kind"]}}, "validationLevel": "moderate", "validationAction": "warn"})`,
		coll + `.createIndex({"kind": 1, "at": -1}, {"name": "kind_at", "unique": true, "partialFilterExpression": {"kind": {"$exists": true}}})`,
		coll + `.createIndex({"title": "text", "body": "text"}, {"name": "search"})`,
		coll + `.createIndex({"loc": "2dsphere", "kind": -1}, {"name": "near"})`,
		coll + `.createIndex({"at": 1}, {"name": "at_1", "expireAfterSeconds": 3600})`,
		coll + `.createIndex({"user.id": "hashed"}, {"name": "h"})`,
	}
	if !reflect.DeepEqual(stmts, want) {
		t.Fatalf("got:\n%s\n\nwant:\n%s", strings.Join(stmts, "\n"), strings.Join(want, "\n"))
	}
	checkStatements(t, stmts)

	plain, err := c.CreateTableSQL(driver.TableDef{Ref: driver.ObjectRef{Name: "notes"}, Options: map[string]string{"capped": "false", "size": "10"}})
	if err != nil || !reflect.DeepEqual(plain, []string{`db.createCollection("notes")`}) {
		t.Fatalf("plain = %q, %v", plain, err)
	}

	for name, def := range map[string]driver.TableDef{
		"no name":     {},
		"no size":     {Ref: driver.ObjectRef{Name: "x"}, Options: map[string]string{"capped": "true"}},
		"bad size":    {Ref: driver.ObjectRef{Name: "x"}, Options: map[string]string{"capped": "true", "size": "big"}},
		"validator":   {Ref: driver.ObjectRef{Name: "x"}, Options: map[string]string{"validator": "{a: }"}},
		"not a doc":   {Ref: driver.ObjectRef{Name: "x"}, Options: map[string]string{"validator": "[1]"}},
		"filter":      {Ref: driver.ObjectRef{Name: "x"}, Indexes: []driver.Index{{Name: "i", Columns: []string{"a"}, Where: "status = 1"}}},
		"no fields":   {Ref: driver.ObjectRef{Name: "x"}, Indexes: []driver.Index{{Name: "i"}}},
		"empty field": {Ref: driver.ObjectRef{Name: "x"}, Indexes: []driver.Index{{Name: "i", Columns: []string{" "}}}},
		"twice":       {Ref: driver.ObjectRef{Name: "x"}, Indexes: []driver.Index{{Name: "i", Columns: []string{"a", "a"}}}},
		"same name":   {Ref: driver.ObjectRef{Name: "x"}, Indexes: []driver.Index{{Name: "i", Columns: []string{"a"}}, {Name: "i", Columns: []string{"b"}}}},
	} {
		if _, err := c.CreateTableSQL(def); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestMongoAlterUnchanged(t *testing.T) {
	c := &conn{}
	from := describedOrders()
	if stmts, err := c.AlterTableSQL(from, defFromTable(from)); err != nil || len(stmts) != 0 {
		t.Fatalf("unchanged collection produced %q, %v", stmts, err)
	}
	// Reformatted documents, dropped columns and cosmetic flags are not changes.
	def := defFromTable(from)
	def.Columns = nil
	def.Options["validator"] = `{$jsonSchema: {bsonType: 'object', required: ['customerId']}}`
	def.Options["validationLevel"] = ""
	def.Options["size"] = "1048576.0"
	def.Indexes[2].Where = `{ status: { $in: ['new', 'paid'] } }`
	def.Indexes[3].Desc = []bool{true, false} // text fields have no direction
	def.Indexes[6].Desc = nil
	def.Indexes = def.Indexes[1:] // the _id index is never touched
	if stmts, err := c.AlterTableSQL(from, def); err != nil || len(stmts) != 0 {
		t.Fatalf("cosmetic edits produced %q, %v", stmts, err)
	}
	view := &driver.Table{Ref: driver.ObjectRef{Name: "v", Kind: "view"}, Kind: "view", Options: map[string]string{"viewOn": "orders"}}
	if stmts, err := c.AlterTableSQL(view, defFromTable(view)); err != nil || len(stmts) != 0 {
		t.Fatalf("unchanged view produced %q, %v", stmts, err)
	}
}

func TestMongoAlterTableSQL(t *testing.T) {
	c := &conn{}
	from := describedOrders()
	def := defFromTable(from)
	ix := def.Indexes
	ix[1].Unique = false                                                                        // cust_date: rebuilt, keeps sparse
	ix[3].Columns, ix[3].Desc = []string{"title"}, []bool{false}                                // search: body removed, title keeps its weight
	ix[4].Columns, ix[4].Desc = []string{"category", "loc", "name"}, []bool{false, false, true} // geo: loc stays 2dsphere
	ix[5].Comment = "TTL 60s"
	def.Indexes = []driver.Index{ix[0], ix[1], ix[3], ix[4], ix[5], ix[6], {Name: "by_total", Columns: []string{"total"}, Desc: []bool{true}}}
	def.Options["size"] = "2097152"
	def.Options["validator"] = `{"total": {"$gte": 0}}`
	def.Options["validationAction"] = "warn"
	def.Ref.Name = "orders_2025"

	stmts, err := c.AlterTableSQL(from, def)
	if err != nil {
		t.Fatal(err)
	}
	coll := `db.getSiblingDB("shop").getCollection("orders")`
	want := []string{
		`db.getSiblingDB("shop").runCommand({"collMod": "orders", "cappedSize": 2097152, "validator": {"total": {"$gte": 0}}, "validationAction": "warn"})`,
		coll + `.dropIndex("cust_date")`,
		coll + `.dropIndex("status_1")`,
		coll + `.dropIndex("search")`,
		coll + `.dropIndex("geo")`,
		coll + `.dropIndex("expire")`,
		coll + `.createIndex({"customerId": 1, "placedAt": -1}, {"name": "cust_date", "sparse": true})`,
		coll + `.createIndex({"title": "text"}, {"name": "search", "weights": {"title": 5}})`,
		coll + `.createIndex({"category": 1, "loc": "2dsphere", "name": -1}, {"name": "geo"})`,
		coll + `.createIndex({"placedAt": 1}, {"name": "expire", "expireAfterSeconds": 60})`,
		coll + `.createIndex({"total": -1}, {"name": "by_total"})`,
		coll + `.renameCollection("orders_2025")`,
	}
	if !reflect.DeepEqual(stmts, want) {
		t.Fatalf("got:\n%s\n\nwant:\n%s", strings.Join(stmts, "\n"), strings.Join(want, "\n"))
	}
	checkStatements(t, stmts)

	// Converting to capped rebuilds the collection with only _id, so every
	// index is created again and none dropped.
	plain := describedOrders()
	plain.Options = map[string]string{}
	plain.Indexes = plain.Indexes[:2]
	pd := defFromTable(plain)
	pd.Options = map[string]string{"capped": "true", "size": "4096", "max": "10", "validationLevel": "strict"}
	stmts, err = c.AlterTableSQL(plain, pd)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		`db.getSiblingDB("shop").runCommand({"convertToCapped": "orders", "size": 4096})`,
		`db.getSiblingDB("shop").runCommand({"collMod": "orders", "cappedMax": 10})`,
		coll + `.createIndex({"customerId": 1, "placedAt": -1}, {"name": "cust_date", "unique": true, "sparse": true})`,
	}
	if !reflect.DeepEqual(stmts, want) {
		t.Fatalf("got:\n%s\n\nwant:\n%s", strings.Join(stmts, "\n"), strings.Join(want, "\n"))
	}
	checkStatements(t, stmts)

	// Clearing the validator and settings.
	cleared := defFromTable(describedOrders())
	cleared.Options["validator"], cleared.Options["max"] = " ", ""
	stmts, err = c.AlterTableSQL(describedOrders(), cleared)
	if err != nil || !reflect.DeepEqual(stmts, []string{`db.getSiblingDB("shop").runCommand({"collMod": "orders", "cappedMax": 0, "validator": {}})`}) {
		t.Fatalf("cleared = %q, %v", stmts, err)
	}

	errs := map[string]func(d *driver.TableDef){
		"uncap":     func(d *driver.TableDef) { d.Options["capped"] = "false" },
		"no size":   func(d *driver.TableDef) { d.Options["size"] = "" },
		"filter":    func(d *driver.TableDef) { d.Indexes[2].Where = "{status: }" },
		"same name": func(d *driver.TableDef) { d.Indexes[2].Name = "cust_date" },
		"validator": func(d *driver.TableDef) { d.Options["validator"] = "x" },
	}
	for name, edit := range errs {
		d := defFromTable(describedOrders())
		edit(&d)
		if stmts, err := c.AlterTableSQL(describedOrders(), d); err == nil {
			t.Errorf("%s: expected an error, got %q", name, stmts)
		}
	}
	view := &driver.Table{Ref: driver.ObjectRef{Name: "v", Kind: "view"}, Kind: "view"}
	vd := defFromTable(view)
	vd.Ref.Name = "v2"
	if _, err := c.AlterTableSQL(view, vd); err == nil {
		t.Error("renaming a view accepted")
	}
	vd = defFromTable(view)
	vd.Indexes = []driver.Index{{Name: "a", Columns: []string{"a"}}}
	if _, err := c.AlterTableSQL(view, vd); err == nil {
		t.Error("indexing a view accepted")
	}
	if _, err := c.AlterTableSQL(nil, driver.TableDef{}); err == nil {
		t.Error("nil collection accepted")
	}
}

func TestMongoObjectDDL(t *testing.T) {
	c := &conn{}
	one := func(stmts []string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if len(stmts) != 1 {
			t.Fatalf("statements = %q", stmts)
		}
		checkStatements(t, stmts)
		return stmts[0]
	}
	ref := driver.ObjectRef{Database: "shop", Name: `odd "name"`, Kind: "collection"}
	if got := one(c.DropObjectSQL(ref, false)); got != `db.getSiblingDB("shop").getCollection("odd \"name\"").drop()` {
		t.Errorf("drop = %s", got)
	}
	if got := one(c.DropObjectSQL(driver.ObjectRef{Name: "v", Kind: "view"}, true)); got != `db.getCollection("v").drop()` {
		t.Errorf("drop view = %s", got)
	}
	if got := one(c.TruncateSQL(driver.ObjectRef{Database: "shop", Name: "orders", Kind: "timeseries"})); got != `db.getSiblingDB("shop").getCollection("orders").deleteMany({})` {
		t.Errorf("truncate = %s", got)
	}
	if got := one(c.RenameObjectSQL(driver.ObjectRef{Database: "shop", Name: "orders"}, " orders_old ")); got != `db.getSiblingDB("shop").getCollection("orders").renameCollection("orders_old")` {
		t.Errorf("rename = %s", got)
	}
	if got := one(c.CreateDatabaseSQL("crm", map[string]string{"collection": "people"})); got != `db.getSiblingDB("crm").createCollection("people")` {
		t.Errorf("create database = %s", got)
	}
	if got := one(c.DropDatabaseSQL("crm")); got != `db.getSiblingDB("crm").dropDatabase()` {
		t.Errorf("drop database = %s", got)
	}
	for name, fn := range map[string]func() ([]string, error){
		"drop kind":       func() ([]string, error) { return c.DropObjectSQL(driver.ObjectRef{Name: "x", Kind: "index"}, false) },
		"drop no name":    func() ([]string, error) { return c.DropObjectSQL(driver.ObjectRef{}, false) },
		"truncate view":   func() ([]string, error) { return c.TruncateSQL(driver.ObjectRef{Name: "v", Kind: "view"}) },
		"rename view":     func() ([]string, error) { return c.RenameObjectSQL(driver.ObjectRef{Name: "v", Kind: "view"}, "w") },
		"rename empty":    func() ([]string, error) { return c.RenameObjectSQL(driver.ObjectRef{Name: "v"}, "") },
		"create no coll":  func() ([]string, error) { return c.CreateDatabaseSQL("crm", nil) },
		"create bad name": func() ([]string, error) { return c.CreateDatabaseSQL("a.b", map[string]string{"collection": "c"}) },
		"drop admin":      func() ([]string, error) { return c.DropDatabaseSQL("admin") },
		"drop empty":      func() ([]string, error) { return c.DropDatabaseSQL(" ") },
	} {
		if stmts, err := fn(); err == nil {
			t.Errorf("%s: expected an error, got %q", name, stmts)
		}
	}
}

func TestParseSiblingDB(t *testing.T) {
	cmds, err := parseScript(`db.getSiblingDB("crm").getCollection("people").find({})
db.getSiblingDB('crm').people.createIndex({a: 1})
db.getSiblingDB("crm").runCommand({collMod: "people", validator: {}})
db.getSiblingDB("crm")["x y"].drop()
db.getSiblingDB("crm").dropDatabase()`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 5 {
		t.Fatalf("got %d commands", len(cmds))
	}
	for i, want := range []struct{ target, call string }{{"people", "find"}, {"people", "createIndex"}, {"", "runCommand"}, {"x y", "drop"}, {"", "dropDatabase"}} {
		if c := cmds[i]; c.database != "crm" || c.target != want.target || c.calls[0].name != want.call {
			t.Errorf("command %d = %+v", i, c)
		}
	}
	if k := cmds[2].kind(); k != driver.StmtDDL {
		t.Errorf("collMod kind = %s", k)
	}
	if d := cmds[4].danger(cmds[4].kind()); d.Level != "destructive" {
		t.Errorf("dropDatabase danger = %+v", d)
	}
	for _, bad := range []string{`db.getSiblingDB().x.find()`, `db.getSiblingDB("a").getSiblingDB("b").x.find()`} {
		if _, err := parseScript(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if cmds, _ := parseScript(`db.runCommand({drop: "x"})`); cmds[0].danger(cmds[0].kind()).Level != "destructive" {
		t.Error("runCommand drop should be destructive")
	}
}

func TestMongoDesign(t *testing.T) {
	d, _ := driver.Get("mongodb")
	info := d.Info()
	ds := info.Design
	if !info.Caps.DDL || ds == nil || ds.Columns || !ds.Indexes || !ds.PartialIndexes || ds.PrimaryKey || ds.ForeignKeys ||
		len(ds.IndexTypes) == 0 || ds.IndexTypes[0] != "" || ds.Note == "" {
		t.Fatalf("design = %+v caps = %+v", ds, info.Caps)
	}
	for _, f := range ds.Options {
		switch f.Key {
		case "capped", "size", "max", "validator", "validationLevel", "validationAction":
		default:
			t.Errorf("option %s is not read by Describe", f.Key)
		}
	}
}
