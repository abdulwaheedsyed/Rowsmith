package mongodb

import (
	"math"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"rowsmith/internal/driver"
)

func TestParseScript(t *testing.T) {
	script := `// recent paid orders
db.orders.find({ status: 'paid', total: { $gt: 100.5 }, _id: ObjectId("66f1a2b3c4d5e6f708192a3b") },
  { total: 1 }).sort({ placed: -1 }).limit(20);
db.getCollection("audit log").aggregate([{ $group: { _id: "$actor", n: { $sum: 1 } } },])
show collections
use analytics
db["weird-name"].countDocuments({ tags: /^beta/i, big: NumberLong("9007199254740993"), when: ISODate("2025-01-02T03:04:05Z") })`
	cmds, err := parseScript(script)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 5 {
		t.Fatalf("got %d commands", len(cmds))
	}
	find := cmds[0]
	if find.target != "orders" || len(find.calls) != 3 || find.calls[0].name != "find" || find.line != 2 {
		t.Fatalf("find parsed wrong: %+v", find)
	}
	f := find.calls[0].args[0].(bson.D)
	if f[0].Key != "status" || f[0].Value != "paid" {
		t.Fatalf("filter: %#v", f)
	}
	if gt := f[1].Value.(bson.D)[0]; gt.Key != "$gt" || gt.Value != 100.5 {
		t.Fatalf("$gt: %#v", gt)
	}
	if _, ok := f[2].Value.(bson.ObjectID); !ok {
		t.Fatalf("ObjectId not parsed: %#v", f[2].Value)
	}
	if find.calls[1].args[0].(bson.D)[0].Value != int32(-1) {
		t.Fatalf("sort value should be int32 -1")
	}
	if cmds[1].target != "audit log" || cmds[1].calls[0].name != "aggregate" {
		t.Fatalf("getCollection: %+v", cmds[1])
	}
	if cmds[2].show != "collections" || cmds[3].use != "analytics" {
		t.Fatalf("show/use: %+v %+v", cmds[2], cmds[3])
	}
	cd := cmds[4].calls[0].args[0].(bson.D)
	if re, ok := cd[0].Value.(bson.Regex); !ok || re.Pattern != "^beta" || re.Options != "i" {
		t.Fatalf("regex: %#v", cd[0].Value)
	}
	if cd[1].Value != int64(9007199254740993) {
		t.Fatalf("NumberLong: %#v", cd[1].Value)
	}
	if _, ok := cd[2].Value.(bson.DateTime); !ok {
		t.Fatalf("ISODate: %#v", cd[2].Value)
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{"db.orders.find({ a: })", "select * from t", "db.orders", "db.x.find(foo)", "db.x.find({a: 1}) extra"} {
		if _, err := parseScript(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]driver.StatementKind{
		"db.t.find({})":                                   driver.StmtRead,
		"db.t.aggregate([{ $match: {} }])":                driver.StmtRead,
		"db.t.aggregate([{ $match: {} }, { $out: 'x' }])": driver.StmtWrite,
		"db.t.updateOne({a: 1}, {$set: {b: 2}})":          driver.StmtWrite,
		"db.t.createIndex({a: 1})":                        driver.StmtDDL,
		"db.runCommand({ ping: 1 })":                      driver.StmtRead,
		"db.runCommand({ dropDatabase: 1 })":              driver.StmtWrite,
		"show dbs":                                        driver.StmtRead,
		"use x":                                           driver.StmtSession,
	}
	for src, want := range cases {
		cmds, err := parseScript(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if got := cmds[0].kind(); got != want {
			t.Errorf("%s: got %s want %s", src, got, want)
		}
	}
	cmds, _ := parseScript("db.t.deleteMany({})")
	if d := cmds[0].danger(cmds[0].kind()); d.Level != "destructive" {
		t.Errorf("deleteMany({}) should be destructive, got %+v", d)
	}
}

func TestLiteralsAndCells(t *testing.T) {
	v, err := parseLiteral(`{a: [1, 2.5, 'x', true, null], n: NumberDecimal("1.10"), u: UUID("0123456789abcdef0123456789abcdef"), inf: Infinity}`)
	if err != nil {
		t.Fatal(err)
	}
	d := v.(bson.D)
	if _, ok := d[1].Value.(bson.Decimal128); !ok {
		t.Fatalf("decimal: %#v", d[1].Value)
	}
	if b, ok := d[2].Value.(bson.Binary); !ok || b.Subtype != 4 {
		t.Fatalf("uuid: %#v", d[2].Value)
	}
	if f, ok := d[3].Value.(float64); !ok || !math.IsInf(f, 1) {
		t.Fatalf("Infinity: %#v", d[3].Value)
	}
	id := bson.NewObjectID()
	cell := encode(id).(map[string]any)
	back, err := decodeCell(map[string]any{"$oid": cell["$oid"]}, "objectId")
	if err != nil || back != id {
		t.Fatalf("objectId round trip: %v %v", back, err)
	}
	if v, _ := decodeCell("42", "int"); v != int32(42) {
		t.Fatalf("int hint: %#v", v)
	}
	if v, _ := decodeCell("42", "string"); v != "42" {
		t.Fatalf("string hint: %#v", v)
	}
	if v, _ := decodeCell("{ tags: ['a'] }", "object"); v == nil {
		t.Fatalf("object literal")
	}
	geo := encode(bson.D{{Key: "type", Value: "Point"}, {Key: "coordinates", Value: bson.A{4.9, 52.3}}}).(map[string]any)
	if _, ok := geo["$geo"]; !ok {
		t.Fatalf("GeoJSON not detected: %#v", geo)
	}
}
