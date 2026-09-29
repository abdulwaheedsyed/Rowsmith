package mysql

import (
	"strings"
	"testing"

	"rowsmith/internal/driver"
)

func strp(s string) *string { return &s }

func TestCreateTableSQL(t *testing.T) {
	c := &conn{flavor: "mysql", major: 8}
	stmts, err := c.CreateTableSQL(driver.TableDef{
		Ref: driver.ObjectRef{Database: "shop", Name: "notes"},
		Columns: []driver.ColumnDef{
			{Column: driver.Column{Name: "id", Type: "bigint unsigned", AutoIncrement: true}},
			{Column: driver.Column{Name: "body", Type: "text", Nullable: true, Comment: "it's free text"}},
			{Column: driver.Column{Name: "created_at", Type: "datetime", Default: strp("CURRENT_TIMESTAMP")}},
		},
		PrimaryKey: []string{"id"},
		Indexes:    []driver.Index{{Name: "idx_created", Columns: []string{"created_at"}}},
		Options:    map[string]string{"engine": "InnoDB"},
		Comment:    "Team notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "CREATE TABLE `shop`.`notes` (\n  `id` bigint unsigned NOT NULL AUTO_INCREMENT,\n  `body` text NULL COMMENT 'it''s free text',\n" +
		"  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,\n  PRIMARY KEY (`id`),\n  INDEX `idx_created` (`created_at`)\n) ENGINE=InnoDB COMMENT='Team notes'"
	if stmts[0] != want {
		t.Fatalf("got\n%s\nwant\n%s", stmts[0], want)
	}
}

func TestAlterTableSQL(t *testing.T) {
	c := &conn{flavor: "mysql", major: 8}
	from := &driver.Table{
		Ref: driver.ObjectRef{Database: "shop", Name: "t"},
		Columns: []driver.Column{
			{Name: "id", Type: "int", PrimaryKey: true},
			{Name: "a", Type: "varchar(10)", Nullable: true},
			{Name: "b", Type: "int", Nullable: true},
		},
		PrimaryKey:  []string{"id"},
		Indexes:     []driver.Index{{Name: "PRIMARY", Primary: true, Unique: true, Columns: []string{"id"}}, {Name: "ix_a", Columns: []string{"a"}}},
		ForeignKeys: []driver.ForeignKey{{Name: "fk_b", Columns: []string{"b"}, RefTable: driver.ObjectRef{Name: "other"}, RefColumns: []string{"id"}}},
	}
	to := driver.TableDef{
		Ref: driver.ObjectRef{Database: "shop", Name: "t2"},
		Columns: []driver.ColumnDef{
			{Column: driver.Column{Name: "id", Type: "int"}, OriginalName: "id"},
			{Column: driver.Column{Name: "title", Type: "varchar(200)"}, OriginalName: "a"},
			{Column: driver.Column{Name: "c", Type: "json", Nullable: true}},
		},
		PrimaryKey: []string{"id"},
		Indexes:    []driver.Index{{Name: "ix_title", Columns: []string{"title"}}},
	}
	stmts, err := c.AlterTableSQL(from, to)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(stmts, "\n")
	for _, want := range []string{
		"DROP FOREIGN KEY `fk_b`",
		"DROP COLUMN `b`",
		"CHANGE COLUMN `a` `title` varchar(200) NOT NULL",
		"ADD COLUMN `c` json NULL AFTER `title`",
		"DROP INDEX `ix_a`",
		"ADD INDEX `ix_title` (`title`)",
		"RENAME TABLE `shop`.`t` TO `shop`.`t2`",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "CHANGE COLUMN `id`") {
		t.Errorf("unchanged column altered:\n%s", joined)
	}
	if strings.Contains(joined, "PRIMARY KEY") {
		t.Errorf("unchanged primary key touched:\n%s", joined)
	}
}
