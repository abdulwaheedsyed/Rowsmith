package sqlsplit

import (
	"testing"

	"rowsmith/internal/driver"
)

func sqls(st []Statement) []string {
	out := make([]string, len(st))
	for i, s := range st {
		out[i] = s.SQL
	}
	return out
}

func expect(t *testing.T, name string, got []Statement, want ...string) {
	t.Helper()
	g := sqls(got)
	if len(g) != len(want) {
		t.Fatalf("%s: got %d statements %q, want %d %q", name, len(g), g, len(want), want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("%s: statement %d = %q, want %q", name, i, g[i], want[i])
		}
	}
}

func TestSplitBasics(t *testing.T) {
	expect(t, "simple", Split("SELECT 1; SELECT 2;", Generic), "SELECT 1", "SELECT 2")
	expect(t, "no trailing", Split("SELECT 1;\n\nSELECT 2", Generic), "SELECT 1", "SELECT 2")
	expect(t, "comments only", Split("-- hi\n/* x */", Generic))
	expect(t, "string with ;", Split("SELECT 'a;b'; SELECT \"c;d\";", Postgres), "SELECT 'a;b'", `SELECT "c;d"`)
	expect(t, "doubled quote", Split("SELECT 'it''s;'; SELECT 2", Postgres), "SELECT 'it''s;'", "SELECT 2")
	expect(t, "line comment ;", Split("SELECT 1 -- a;b\n; SELECT 2", Postgres), "SELECT 1 -- a;b", "SELECT 2")
}

func TestSplitMySQL(t *testing.T) {
	expect(t, "backslash", Split(`SELECT 'a\';b'; SELECT 2`, MySQL), `SELECT 'a\';b'`, "SELECT 2")
	expect(t, "hash comment", Split("SELECT 1 # x;y\n; SELECT 2", MySQL), "SELECT 1 # x;y", "SELECT 2")
	expect(t, "dash no space is minus", Split("SELECT 5--1; SELECT 2", MySQL), "SELECT 5--1", "SELECT 2")
	script := "DELIMITER $$\nCREATE PROCEDURE p() BEGIN SELECT 1; SELECT 2; END$$\nDELIMITER ;\nCALL p();"
	expect(t, "delimiter", Split(script, MySQL), "CREATE PROCEDURE p() BEGIN SELECT 1; SELECT 2; END", "CALL p()")
	body := "CREATE DEFINER=`root`@`%` PROCEDURE p(IN x INT)\nBEGIN\n  IF x > 0 THEN SELECT 1; END IF;\n  CASE x WHEN 1 THEN SELECT 2; ELSE SELECT 3; END CASE;\n  SELECT CASE WHEN x THEN 1 END;\nEND;\nSELECT 9;"
	expect(t, "routine without delimiter", Split(body, MySQL),
		"CREATE DEFINER=`root`@`%` PROCEDURE p(IN x INT)\nBEGIN\n  IF x > 0 THEN SELECT 1; END IF;\n  CASE x WHEN 1 THEN SELECT 2; ELSE SELECT 3; END CASE;\n  SELECT CASE WHEN x THEN 1 END;\nEND", "SELECT 9")
	expect(t, "begin txn", Split("BEGIN; UPDATE t SET a=1; COMMIT;", MySQL), "BEGIN", "UPDATE t SET a=1", "COMMIT")
}

func TestSplitPostgres(t *testing.T) {
	fn := "CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql;"
	expect(t, "dollar", Split(fn+" SELECT f();", Postgres), fn[:len(fn)-1], "SELECT f()")
	tagged := "DO $body$ BEGIN RAISE NOTICE 'x;'; END $body$; SELECT 1"
	expect(t, "tagged dollar", Split(tagged, Postgres), "DO $body$ BEGIN RAISE NOTICE 'x;'; END $body$", "SELECT 1")
	expect(t, "param not dollar", Split("SELECT $1; SELECT 2", Postgres), "SELECT $1", "SELECT 2")
	expect(t, "E string", Split(`SELECT E'a\';b'; SELECT 2`, Postgres), `SELECT E'a\';b'`, "SELECT 2")
	expect(t, "nested comment", Split("SELECT /* a /* b; */ c; */ 1; SELECT 2", Postgres), "SELECT /* a /* b; */ c; */ 1", "SELECT 2")
	atomic := "CREATE FUNCTION g() RETURNS int LANGUAGE sql BEGIN ATOMIC SELECT 1; SELECT 2; END"
	expect(t, "begin atomic", Split(atomic+"; SELECT 3", Postgres), atomic, "SELECT 3")
	expect(t, "rule parens", Split("CREATE RULE r AS ON INSERT TO t DO ALSO (INSERT INTO a VALUES (1); INSERT INTO b VALUES (2)); SELECT 1", Postgres),
		"CREATE RULE r AS ON INSERT TO t DO ALSO (INSERT INTO a VALUES (1); INSERT INTO b VALUES (2))", "SELECT 1")
}

func TestSplitMSSQL(t *testing.T) {
	script := "DECLARE @x int = 1;\nSELECT @x;\nGO\nSELECT [a;b] FROM t\ngo 2\nSELECT 'x'"
	expect(t, "go batches", Split(script, MSSQL), "DECLARE @x int = 1;\nSELECT @x;", "SELECT [a;b] FROM t", "SELECT 'x'")
}

func TestSplitOracle(t *testing.T) {
	script := "CREATE OR REPLACE PROCEDURE p IS\nBEGIN\n  NULL;\nEND;\n/\nSELECT q'[it's;]' FROM dual;\nSELECT 2 FROM dual"
	expect(t, "slash", Split(script, Oracle), "CREATE OR REPLACE PROCEDURE p IS\nBEGIN\n  NULL;\nEND;", "SELECT q'[it's;]' FROM dual", "SELECT 2 FROM dual")
}

func TestSplitSQLiteTrigger(t *testing.T) {
	trg := "CREATE TRIGGER tr AFTER INSERT ON t BEGIN UPDATE t SET a = CASE WHEN 1 THEN 2 END; DELETE FROM u; END"
	expect(t, "trigger", Split(trg+"; SELECT 1", SQLite), trg, "SELECT 1")
}

func TestSplitBigQuery(t *testing.T) {
	expect(t, "triple", Split("SELECT '''a;b'''; SELECT r\"c\\;\"; SELECT 3", BigQuery), "SELECT '''a;b'''", `SELECT r"c\;"`, "SELECT 3")
}

func TestStatementAt(t *testing.T) {
	s := "SELECT 1;\nSELECT 2;\nSELECT 3;"
	st, ok := StatementAt(s, Generic, 12)
	if !ok || st.SQL != "SELECT 2" || st.Line != 2 {
		t.Fatalf("got %+v", st)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		sql  string
		d    Dialect
		kind driver.StatementKind
		lvl  string
	}{
		{"SELECT * FROM t", Postgres, driver.StmtRead, ""},
		{"select replace(name,'a','b'), left(x, 2) from t", MySQL, driver.StmtRead, ""},
		{"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d", Postgres, driver.StmtWrite, "caution"},
		{"SELECT * INTO backup FROM t", Postgres, driver.StmtWrite, "caution"},
		{"SELECT a INTO @v FROM t", MySQL, driver.StmtRead, ""},
		{"SELECT * FROM t INTO OUTFILE '/tmp/x'", MySQL, driver.StmtWrite, "caution"},
		{"SELECT * FROM t FOR UPDATE", Postgres, driver.StmtWrite, "caution"},
		{"SELECT t.update FROM t", Postgres, driver.StmtRead, ""},
		{"DELETE FROM t", Postgres, driver.StmtWrite, "destructive"},
		{"DELETE FROM t WHERE id = 1", Postgres, driver.StmtWrite, "caution"},
		{"UPDATE t SET a = (SELECT 1 WHERE true)", Postgres, driver.StmtWrite, "destructive"},
		{"DROP TABLE t", MySQL, driver.StmtDDL, "destructive"},
		{"ALTER TABLE t DROP COLUMN a", MySQL, driver.StmtDDL, "destructive"},
		{"ALTER TABLE t ADD COLUMN a int", MySQL, driver.StmtDDL, "caution"},
		{"SET search_path = app", Postgres, driver.StmtSession, ""},
		{"SET default_transaction_read_only = off", Postgres, driver.StmtWrite, "caution"},
		{"SET SESSION TRANSACTION READ WRITE", MySQL, driver.StmtWrite, "caution"},
		{"SET GLOBAL max_connections = 10", MySQL, driver.StmtWrite, "caution"},
		{"EXPLAIN SELECT 1", MySQL, driver.StmtRead, ""},
		{"EXPLAIN ANALYZE DELETE FROM t", Postgres, driver.StmtWrite, "caution"},
		{"SELECT 1 /*!50000 ; DROP TABLE t */", MySQL, driver.StmtWrite, "caution"},
		{"SELECT 1\nDELETE FROM t", MSSQL, driver.StmtWrite, "caution"},
		{"BEGIN TRAN", MSSQL, driver.StmtTCL, ""},
		{"BEGIN NULL; END;", Oracle, driver.StmtUnknown, "caution"},
		{"SHOW TABLES", MySQL, driver.StmtRead, ""},
		{"PRAGMA table_info(t)", SQLite, driver.StmtRead, ""},
		{"START SLAVE", MySQL, driver.StmtWrite, "caution"},
		{"(SELECT 1) UNION (SELECT 2)", Postgres, driver.StmtRead, ""},
		{"CALL p()", MySQL, driver.StmtWrite, "caution"},
	}
	for _, c := range cases {
		k, dg := Classify(c.sql, c.d)
		if k != c.kind || dg.Level != c.lvl {
			t.Errorf("Classify(%q, %s) = %s/%q, want %s/%q", c.sql, c.d, k, dg.Level, c.kind, c.lvl)
		}
	}
}
