package mongodb

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"rowsmith/internal/driver"
)

// Integration tests run against a real server when ROWSMITH_TEST_MONGO_HOST
// is set (see dev/compose.yaml). They create and drop their own databases.
//
//	ROWSMITH_TEST_MONGO_HOST, _PORT (27017), _USER (root), _PASSWORD

func openTestConn(t *testing.T) *conn {
	t.Helper()
	host := os.Getenv("ROWSMITH_TEST_MONGO_HOST")
	if host == "" {
		t.Skip("ROWSMITH_TEST_MONGO_HOST not set")
	}
	user := os.Getenv("ROWSMITH_TEST_MONGO_USER")
	if user == "" {
		user = "root"
	}
	port := os.Getenv("ROWSMITH_TEST_MONGO_PORT")
	if port == "" {
		port = "27017"
	}
	d, _ := driver.Get("mongodb")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cn, err := d.Open(ctx, driver.OpenParams{AppName: "rowsmith-test",
		Params:  map[string]any{"mode": "fields", "host": host, "port": port, "user": user, "authSource": "admin", "tls": "disable"},
		Secrets: map[string]string{"password": os.Getenv("ROWSMITH_TEST_MONGO_PASSWORD")}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cn.Close() })
	return cn.(*conn)
}

// recSink records the outcome of each statement.
type recSink struct {
	stmts []driver.StatementInfo
	errs  []error
	rows  [][]any
}

func (s *recSink) BeginStatement(i driver.StatementInfo) error {
	s.stmts = append(s.stmts, i)
	return nil
}
func (s *recSink) Columns([]driver.ResultColumn) error  { return nil }
func (s *recSink) Rows(r [][]any) error                 { s.rows = append(s.rows, r...); return nil }
func (s *recSink) EndResult(driver.ResultSummary) error { return nil }
func (s *recSink) Notice(string, string) error          { return nil }
func (s *recSink) EndStatement(err error) error         { s.errs = append(s.errs, err); return nil }

// runScript executes generated statements the way the UI does: joined into
// one console script.
func runScript(t *testing.T, sess driver.Session, stmts []string) {
	t.Helper()
	if len(stmts) == 0 {
		return
	}
	script := strings.Join(stmts, ";\n")
	t.Log(script)
	sink := &recSink{}
	if err := sess.Execute(context.Background(), script, driver.ExecOptions{StopOnError: true}, sink); err != nil {
		t.Fatalf("execute: %v", err)
	}
	for i, err := range sink.errs {
		if err != nil {
			t.Fatalf("statement %q failed: %v", sink.stmts[i].SQL, err)
		}
	}
	if len(sink.errs) != len(stmts) {
		t.Fatalf("ran %d of %d statements:\n%s", len(sink.errs), len(stmts), strings.Join(stmts, "\n"))
	}
}

// roundTrip describes a collection and checks that submitting it unchanged
// generates nothing.
func roundTrip(t *testing.T, c *conn, ref driver.ObjectRef) *driver.Table {
	t.Helper()
	tb, err := c.Describe(context.Background(), ref)
	if err != nil {
		t.Fatalf("describe %s: %v", ref.Name, err)
	}
	stmts, err := c.AlterTableSQL(tb, defFromTable(tb))
	if err != nil || len(stmts) != 0 {
		t.Fatalf("unchanged %s produced %q, %v\nindexes: %+v\noptions: %v", ref.Name, stmts, err, tb.Indexes, tb.Options)
	}
	return tb
}

func indexNamed(tb *driver.Table, name string) *driver.Index {
	for i := range tb.Indexes {
		if tb.Indexes[i].Name == name {
			return &tb.Indexes[i]
		}
	}
	return nil
}

func TestIntegrationDDL(t *testing.T) {
	c := openTestConn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := "rowsmith_it_" + strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 36)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = c.client.Database(db).Drop(ctx)
		_ = c.client.Database(db + "_new").Drop(ctx)
	})
	// A console on another database: generated statements name their own.
	sess, err := c.NewSession(ctx, driver.Scope{Database: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	orders := driver.ObjectRef{Database: db, Name: "orders", Kind: "collection"}
	stmts, err := c.CreateTableSQL(driver.TableDef{Ref: orders,
		Options: map[string]string{
			"validator":       `{$jsonSchema: {bsonType: "object", required: ["customerId"], properties: {total: {bsonType: "double", minimum: 0}}}}`,
			"validationLevel": "moderate"},
		Indexes: []driver.Index{
			{Name: "cust_date", Columns: []string{"customerId", "placedAt"}, Desc: []bool{false, true}, Unique: true,
				Where: `{placedAt: {$gte: ISODate("2024-01-01T00:00:00Z")}}`},
			{Name: "search", Columns: []string{"title", "notes"}, Type: "text"},
			{Name: "geo", Columns: []string{"loc", "category"}, Type: "2dsphere"},
			{Name: "sku_hashed", Columns: []string{"sku"}, Type: "hashed"},
			{Name: "expire", Columns: []string{"expiresAt"}, Comment: "TTL 3600s"},
			{Name: "attrs", Columns: []string{"attrs.$**"}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	runScript(t, sess, stmts)
	tb := roundTrip(t, c, orders)
	if len(tb.Indexes) != 7 || tb.Options["validationLevel"] != "moderate" || !strings.Contains(tb.Options["validator"], `"minimum": 0`) {
		t.Fatalf("created collection = %+v, options %v", tb.Indexes, tb.Options)
	}
	// The recreate script shown as the collection's DDL runs in the console.
	if cmds, err := parseScript(tb.DDL); err != nil || len(cmds) != 7 {
		t.Errorf("DDL does not parse into 7 commands (%v):\n%s", err, tb.DDL)
	}
	if ix := indexNamed(tb, "search"); ix == nil || ix.Type != "text" || strings.Join(ix.Columns, ",") != "notes,title" {
		t.Errorf("text index described as %+v", ix)
	}
	if ix := indexNamed(tb, "expire"); ix == nil || ix.Comment != "TTL 3600s" {
		t.Errorf("TTL index described as %+v", ix)
	}

	// Change indexes, options and the name, then run the statements.
	def := defFromTable(tb)
	var kept []driver.Index
	for _, ix := range def.Indexes {
		switch ix.Name {
		case "cust_date":
			ix.Unique = false
		case "geo":
			ix.Columns, ix.Desc = append(ix.Columns, "name"), append(ix.Desc, true)
		case "expire":
			ix.Comment = "TTL 60s"
		case "sku_hashed":
			continue
		}
		kept = append(kept, ix)
	}
	def.Indexes = append(kept, driver.Index{Name: "by_total", Columns: []string{"total"}, Desc: []bool{true}})
	def.Options["validator"] = `{total: {$gte: 0}}`
	def.Options["validationAction"] = "warn"
	def.Ref.Name = "orders_2025"
	stmts, err = c.AlterTableSQL(tb, def)
	if err != nil {
		t.Fatal(err)
	}
	runScript(t, sess, stmts)
	orders.Name = "orders_2025"
	tb = roundTrip(t, c, orders)
	if ix := indexNamed(tb, "geo"); ix == nil || !strings.Contains(ix.Definition, `{"loc": "2dsphere", "category": 1, "name": -1}`) {
		t.Errorf("geo index = %+v", ix)
	}
	if ix := indexNamed(tb, "cust_date"); ix == nil || ix.Unique || !strings.Contains(ix.Where, "ISODate(") {
		t.Errorf("cust_date index = %+v", ix)
	}
	if indexNamed(tb, "sku_hashed") != nil || indexNamed(tb, "by_total") == nil || indexNamed(tb, "expire").Comment != "TTL 60s" {
		t.Errorf("indexes after alter = %+v", tb.Indexes)
	}
	if tb.Options["validationAction"] != "warn" || !strings.Contains(tb.Options["validator"], `"$gte": 0`) {
		t.Errorf("options after alter = %v", tb.Options)
	}

	// Emptying and dropping.
	runScript(t, sess, []string{`db.getSiblingDB("` + db + `").getCollection("orders_2025").insertMany([{customerId: 1, total: 2.5}, {customerId: 2, total: 1.0}])`})
	stmts, _ = c.TruncateSQL(orders)
	runScript(t, sess, stmts)
	if n, err := c.db(db).Collection("orders_2025").CountDocuments(ctx, bson.D{}); err != nil || n != 0 {
		t.Errorf("documents after truncate = %d, %v", n, err)
	}

	// Capped collections: create, resize, drop the document limit.
	logRef := driver.ObjectRef{Database: db, Name: "log", Kind: "collection"}
	stmts, err = c.CreateTableSQL(driver.TableDef{Ref: logRef, Options: map[string]string{"capped": "true", "size": "65536", "max": "100"}})
	if err != nil {
		t.Fatal(err)
	}
	runScript(t, sess, stmts)
	tb = roundTrip(t, c, logRef)
	def = defFromTable(tb)
	def.Options["size"], def.Options["max"] = "131072", "500"
	stmts, err = c.AlterTableSQL(tb, def)
	if err != nil {
		t.Fatal(err)
	}
	runScript(t, sess, stmts)
	tb = roundTrip(t, c, logRef)
	if tb.Options["size"] != "131072" || tb.Options["max"] != "500" {
		t.Errorf("capped options after collMod = %v", tb.Options)
	}
	def = defFromTable(tb)
	def.Options["max"] = ""
	stmts, _ = c.AlterTableSQL(tb, def)
	runScript(t, sess, stmts)
	roundTrip(t, c, logRef)

	// Converting keeps the documents and re-creates the indexes.
	plainRef := driver.ObjectRef{Database: db, Name: "plain", Kind: "collection"}
	stmts, _ = c.CreateTableSQL(driver.TableDef{Ref: plainRef, Indexes: []driver.Index{{Name: "a_1", Columns: []string{"a"}}}})
	stmts = append(stmts, `db.getSiblingDB("`+db+`").getCollection("plain").insertOne({a: 1})`)
	runScript(t, sess, stmts)
	tb = roundTrip(t, c, plainRef)
	def = defFromTable(tb)
	def.Options["capped"], def.Options["size"] = "true", "1048576"
	stmts, err = c.AlterTableSQL(tb, def)
	if err != nil {
		t.Fatal(err)
	}
	runScript(t, sess, stmts)
	tb = roundTrip(t, c, plainRef)
	if tb.Options["capped"] != "true" || indexNamed(tb, "a_1") == nil || tb.RowEstimate == nil || *tb.RowEstimate != 1 {
		t.Errorf("converted collection = %v, %+v", tb.Options, tb.Indexes)
	}

	// Rename and drop through the navigator statements.
	stmts, _ = c.RenameObjectSQL(logRef, "log_old")
	runScript(t, sess, stmts)
	logRef.Name = "log_old"
	roundTrip(t, c, logRef)
	stmts, _ = c.DropObjectSQL(logRef, false)
	runScript(t, sess, stmts)
	if names, _ := c.db(db).ListCollectionNames(ctx, bson.D{{Key: "name", Value: "log_old"}}); len(names) != 0 {
		t.Errorf("log_old still exists")
	}

	// Views round-trip and drop like collections.
	runScript(t, sess, []string{`db.getSiblingDB("` + db + `").createView("big", "orders_2025", [{$match: {total: {$gt: 100}}}])`})
	view := driver.ObjectRef{Database: db, Name: "big", Kind: "view"}
	roundTrip(t, c, view)
	stmts, _ = c.DropObjectSQL(view, false)
	runScript(t, sess, stmts)

	// Databases appear with their first collection and drop as a whole.
	stmts, err = c.CreateDatabaseSQL(db+"_new", map[string]string{"collection": "first"})
	if err != nil {
		t.Fatal(err)
	}
	runScript(t, sess, stmts)
	if !hasDatabase(t, c, db+"_new") {
		t.Fatalf("%s_new was not created", db)
	}
	for _, name := range []string{db + "_new", db} {
		stmts, _ = c.DropDatabaseSQL(name)
		runScript(t, sess, stmts)
		if hasDatabase(t, c, name) {
			t.Errorf("%s still exists", name)
		}
	}
}

func hasDatabase(t *testing.T, c *conn, name string) bool {
	t.Helper()
	dbs, err := c.Databases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dbs {
		if d.Name == name {
			return true
		}
	}
	return false
}
