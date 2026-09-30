package sqlsplit

import (
	"strings"
	"testing"
)

func TestJoinRoundTrip(t *testing.T) {
	cases := map[Dialect][]string{
		MySQL:    {"CREATE TABLE `a` (`x` int COMMENT 'semi; colon')", "ALTER TABLE `a` ADD COLUMN `y` text"},
		Postgres: {`CREATE TABLE "a" ("x" int DEFAULT 1)`, `COMMENT ON TABLE "a" IS 'it''s; fine'`},
		SQLite:   {"PRAGMA foreign_keys = OFF", "BEGIN", `CREATE TABLE "n" ("x" INTEGER)`, "COMMIT"},
		MSSQL: {
			"CREATE SCHEMA [s]",
			"DECLARE @n sysname = (SELECT 1); IF @n IS NOT NULL EXEC(N'ALTER TABLE [s].[t] DROP CONSTRAINT x')",
			"EXEC sp_rename N's.t.a', N'b', N'COLUMN'",
		},
		Oracle: {
			`CREATE TABLE "T" ("ID" NUMBER)`,
			"BEGIN EXECUTE IMMEDIATE 'DROP TABLE x'; EXCEPTION WHEN OTHERS THEN NULL; END;",
			`COMMENT ON TABLE "T" IS 'a; b'`,
		},
	}
	for d, stmts := range cases {
		got := Split(Join(string(d), stmts), d)
		if len(got) != len(stmts) {
			t.Errorf("%s: %d statements back, want %d: %#v", d, len(got), len(stmts), got)
			continue
		}
		for i, st := range got {
			if strings.TrimRight(strings.TrimSpace(st.SQL), ";") != strings.TrimRight(stmts[i], ";") {
				t.Errorf("%s[%d] = %q, want %q", d, i, st.SQL, stmts[i])
			}
		}
	}
}

func TestDelimiterAfterComment(t *testing.T) {
	script := "-- Routines\nDELIMITER ;;\nCREATE PROCEDURE p()\nBEGIN\n  SELECT 1;\nEND;;\nDELIMITER ;\n-- tail\nSELECT 2;\n"
	got := Split(script, MySQL)
	if len(got) != 2 || !strings.HasPrefix(got[0].SQL, "CREATE PROCEDURE") || !strings.HasSuffix(got[0].SQL, "END") || got[1].SQL != "-- tail\nSELECT 2" && got[1].SQL != "SELECT 2" {
		t.Fatalf("%#v", got)
	}
}
