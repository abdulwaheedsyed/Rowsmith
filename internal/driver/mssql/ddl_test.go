package mssql

import (
	"slices"
	"strings"
	"testing"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
)

func strp(s string) *string { return &s }

// describedOrders is dbo.orders of the integration fixture as Describe
// returns it.
func describedOrders() *driver.Table {
	return &driver.Table{
		Ref:  driver.ObjectRef{Database: "shop", Schema: "dbo", Name: "orders", Kind: "table"},
		Kind: "table",
		Columns: []driver.Column{
			{Name: "id", Type: "int", BaseType: "int", Kind: driver.KindInt, AutoIncrement: true, PrimaryKey: true},
			{Name: "customer_id", Type: "int", BaseType: "int", Kind: driver.KindInt},
			{Name: "total", Type: "decimal(12,2)", BaseType: "decimal", Kind: driver.KindDecimal, Default: strp("0")},
			{Name: "tax", Type: "numeric(14,3)", BaseType: "numeric", Kind: driver.KindDecimal, Nullable: true, Generated: "[total]*(0.2)", GeneratedStored: true},
			{Name: "price", Type: "money", BaseType: "money", Kind: driver.KindDecimal, Nullable: true},
			{Name: "token", Type: "uniqueidentifier", BaseType: "uniqueidentifier", Kind: driver.KindUUID, Default: strp("newid()")},
			{Name: "note", Type: "nvarchar(max)", BaseType: "nvarchar", Kind: driver.KindText, Nullable: true, Collation: "SQL_Latin1_General_CP1_CI_AS", Comment: "Free text"},
			{Name: "placed", Type: "datetimeoffset(3)", BaseType: "datetimeoffset", Kind: driver.KindTimestamp, Nullable: true},
			{Name: "flag", Type: "bit", BaseType: "bit", Kind: driver.KindBool, Default: strp("0")},
			{Name: "location", Type: "geometry", BaseType: "geometry", Kind: driver.KindGeometry, Nullable: true, GeometryType: "geometry"},
			{Name: "ver", Type: "rowversion", BaseType: "timestamp", Kind: driver.KindBinary, Generated: "row version, set by the server"},
		},
		Indexes: []driver.Index{
			{Name: "PK_orders", Columns: []string{"id"}, Unique: true, Primary: true, Type: "clustered", Desc: []bool{false},
				Definition: "ALTER TABLE [dbo].[orders] ADD CONSTRAINT [PK_orders] PRIMARY KEY CLUSTERED ([id] ASC)"},
			{Name: "IX_orders_customer", Columns: []string{"customer_id"}, Type: "nonclustered", Where: "[total]>(0)", Desc: []bool{true},
				Definition: "CREATE NONCLUSTERED INDEX [IX_orders_customer] ON [dbo].[orders] ([customer_id] DESC) INCLUDE ([total]) WHERE [total]>(0)"},
			{Name: "UQ_orders_token", Columns: []string{"token"}, Unique: true, Type: "nonclustered", Desc: []bool{false},
				Definition: "ALTER TABLE [dbo].[orders] ADD CONSTRAINT [UQ_orders_token] UNIQUE NONCLUSTERED ([token] ASC)"},
			{Name: "IX_orders_placed", Columns: []string{"placed", "price"}, Type: "nonclustered", Desc: []bool{false, false},
				Definition: "CREATE NONCLUSTERED INDEX [IX_orders_placed] ON [dbo].[orders] ([placed] ASC, [price] ASC)"},
		},
		ForeignKeys: []driver.ForeignKey{{Name: "FK_orders_customers", Columns: []string{"customer_id"},
			RefTable: driver.ObjectRef{Database: "shop", Schema: "dbo", Name: "customers", Kind: "table"}, RefColumns: []string{"id"},
			OnUpdate: "NO ACTION", OnDelete: "CASCADE", Table: &driver.ObjectRef{Database: "shop", Schema: "dbo", Name: "orders", Kind: "table"}}},
		Checks:     []driver.Check{{Name: "CK_orders_total", Expression: "[total]>=(0)"}},
		PrimaryKey: []string{"id"},
		RowKey:     []string{"id"},
		RowKeyKind: "primary",
		Comment:    "Customer orders",
		Options:    map[string]string{},
		Editable:   true,
	}
}

// describedCustomers is referenced by orders.
func describedCustomers() *driver.Table {
	return &driver.Table{
		Ref:  driver.ObjectRef{Database: "shop", Schema: "dbo", Name: "customers", Kind: "table"},
		Kind: "table",
		Columns: []driver.Column{
			{Name: "id", Type: "int", BaseType: "int", Kind: driver.KindInt, AutoIncrement: true, PrimaryKey: true},
			{Name: "name", Type: "nvarchar(100)", BaseType: "nvarchar", Kind: driver.KindString, Collation: "SQL_Latin1_General_CP1_CI_AS"},
		},
		Indexes: []driver.Index{{Name: "PK_customers", Columns: []string{"id"}, Unique: true, Primary: true, Type: "clustered", Desc: []bool{false},
			Definition: "ALTER TABLE [dbo].[customers] ADD CONSTRAINT [PK_customers] PRIMARY KEY CLUSTERED ([id] ASC)"}},
		Referenced: []driver.ForeignKey{{Name: "FK_orders_customers", Columns: []string{"customer_id"},
			RefTable: driver.ObjectRef{Database: "shop", Schema: "dbo", Name: "customers", Kind: "table"}, RefColumns: []string{"id"},
			OnUpdate: "NO ACTION", OnDelete: "CASCADE", Table: &driver.ObjectRef{Database: "shop", Schema: "dbo", Name: "orders", Kind: "table"}}},
		PrimaryKey: []string{"id"},
	}
}

// defOf builds the editor's definition of an existing table, as the web UI does.
func defOf(t *driver.Table) driver.TableDef {
	d := driver.TableDef{Ref: t.Ref, PrimaryKey: slices.Clone(t.PrimaryKey), Indexes: slices.Clone(t.Indexes),
		ForeignKeys: slices.Clone(t.ForeignKeys), Checks: slices.Clone(t.Checks), Comment: t.Comment, Options: t.Options}
	for _, c := range t.Columns {
		d.Columns = append(d.Columns, driver.ColumnDef{Column: c, OriginalName: c.Name})
	}
	return d
}

func col(d *driver.TableDef, name string) *driver.ColumnDef {
	for i := range d.Columns {
		if d.Columns[i].OriginalName == name {
			return &d.Columns[i]
		}
	}
	panic("no column " + name)
}

func alterSQL(t *testing.T, from *driver.Table, to driver.TableDef) []string {
	t.Helper()
	stmts, err := (&conn{}).AlterTableSQL(from, to)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if strings.HasSuffix(strings.TrimSpace(s), ";") {
			t.Errorf("trailing semicolon: %s", s)
		}
	}
	return stmts
}

func wantStmts(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got\n%s\n\nwant\n%s", strings.Join(got, "\n---\n"), strings.Join(want, "\n---\n"))
	}
}

func TestDesignAndCaps(t *testing.T) {
	d, _ := driver.Get("mssql")
	info := d.Info()
	if !info.Caps.DDL || !info.Caps.CreateDatabase || info.Design == nil {
		t.Fatalf("caps %+v design %v", info.Caps, info.Design)
	}
	if info.Design.ReorderColumns || !info.Design.PartialIndexes || info.Design.IndexTypes[0] != "nonclustered" {
		t.Errorf("design %+v", info.Design)
	}
	var c driver.Conn = &conn{}
	if _, ok := c.(driver.DDLGenerator); !ok {
		t.Error("not a DDLGenerator")
	}
	if _, ok := c.(driver.SchemaDDL); !ok {
		t.Error("not a SchemaDDL")
	}
}

func TestCreateTableSQL(t *testing.T) {
	stmts, err := (&conn{}).CreateTableSQL(driver.TableDef{
		Ref: driver.ObjectRef{Schema: "sales", Name: "notes"},
		Columns: []driver.ColumnDef{
			{Column: driver.Column{Name: "id", Type: "bigint", AutoIncrement: true, Default: strp("1")}},
			{Column: driver.Column{Name: "body", Type: "nvarchar(max)", Nullable: true, Collation: "Latin1_General_CS_AS", Comment: "it's free text"}},
			{Column: driver.Column{Name: "qty", Type: "int", Collation: "Latin1_General_CS_AS", Default: strp("((1))")}},
			{Column: driver.Column{Name: "created", Type: "datetime2(3)", Default: strp("sysutcdatetime()")}},
			{Column: driver.Column{Name: "double_qty", Generated: "[qty]*2", GeneratedStored: true}},
			{Column: driver.Column{Name: "label", Generated: "N'#' + CAST([id] AS nvarchar(20))", Nullable: true}},
			{Column: driver.Column{Name: "author_id", Type: "int", Nullable: true}},
		},
		PrimaryKey: []string{"id"},
		Indexes: []driver.Index{
			{Name: "IX_notes_created", Columns: []string{"created", "qty"}, Desc: []bool{true}, Where: "[qty] > 0"},
			{Name: "UX_notes_label", Columns: []string{"label"}, Unique: true, Type: "nonclustered"},
		},
		ForeignKeys: []driver.ForeignKey{{Name: "FK_notes_author", Columns: []string{"author_id"}, RefTable: driver.ObjectRef{Schema: "dbo", Name: "people"},
			RefColumns: []string{"id"}, OnDelete: "SET NULL", OnUpdate: "NO ACTION"}},
		Checks:  []driver.Check{{Name: "CK_notes_qty", Expression: "[qty] >= 0"}},
		Comment: "Team notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantStmts(t, stmts, []string{
		"CREATE TABLE [sales].[notes] (\n" +
			"  [id] bigint IDENTITY(1,1) NOT NULL,\n" +
			"  [body] nvarchar(max) COLLATE Latin1_General_CS_AS NULL,\n" +
			"  [qty] int NOT NULL DEFAULT (((1))),\n" +
			"  [created] datetime2(3) NOT NULL DEFAULT (sysutcdatetime()),\n" +
			"  [double_qty] AS ([qty]*2) PERSISTED NOT NULL,\n" +
			"  [label] AS (N'#' + CAST([id] AS nvarchar(20))),\n" +
			"  [author_id] int NULL,\n" +
			"  CONSTRAINT [PK_notes] PRIMARY KEY ([id]),\n" +
			"  CONSTRAINT [CK_notes_qty] CHECK ([qty] >= 0),\n" +
			"  CONSTRAINT [FK_notes_author] FOREIGN KEY ([author_id]) REFERENCES [dbo].[people] ([id]) ON DELETE SET NULL\n" +
			")",
		"CREATE INDEX [IX_notes_created] ON [sales].[notes] ([created] DESC, [qty]) WHERE [qty] > 0",
		"CREATE UNIQUE NONCLUSTERED INDEX [UX_notes_label] ON [sales].[notes] ([label])",
		"EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'Team notes', @level0type = N'SCHEMA', @level0name = N'sales', @level1type = N'TABLE', @level1name = N'notes'",
		"EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'it''s free text', @level0type = N'SCHEMA', @level0name = N'sales', @level1type = N'TABLE', @level1name = N'notes', @level2type = N'COLUMN', @level2name = N'body'",
	})

	// A clustered index makes the primary key nonclustered; the PK forces NOT NULL.
	stmts, err = (&conn{}).CreateTableSQL(driver.TableDef{
		Ref:        driver.ObjectRef{Name: "t"},
		Columns:    []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "int", Nullable: true}}, {Column: driver.Column{Name: "b", Type: "int"}}},
		PrimaryKey: []string{"a"},
		Indexes:    []driver.Index{{Name: "CX_t", Columns: []string{"b"}, Type: "clustered"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantStmts(t, stmts, []string{
		"CREATE TABLE [dbo].[t] (\n  [a] int NOT NULL,\n  [b] int NOT NULL,\n  CONSTRAINT [PK_t] PRIMARY KEY NONCLUSTERED ([a])\n)",
		"CREATE CLUSTERED INDEX [CX_t] ON [dbo].[t] ([b])",
	})

	for _, bad := range []driver.TableDef{
		{Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "int"}}}},
		{Ref: driver.ObjectRef{Name: "t"}},
		{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a"}}}},
		{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "int"}}, {Column: driver.Column{Name: "A", Type: "int"}}}},
		{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "int"}}}, PrimaryKey: []string{"b"}},
		{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "int"}}}, Indexes: []driver.Index{{Name: "ix", Columns: []string{"b"}}}},
		{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "nvarchar(5)", Collation: "x; DROP TABLE y"}}}},
		{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "int"}}},
			ForeignKeys: []driver.ForeignKey{{Name: "fk", Columns: []string{"a"}, RefTable: driver.ObjectRef{Name: "o"}, RefColumns: []string{"id"}, OnDelete: "RESTRICTED"}}},
		{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "int"}}}, Indexes: []driver.Index{{Name: "ix", Columns: []string{"a"}, Type: "xml"}}},
	} {
		if _, err := (&conn{}).CreateTableSQL(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestAlterUnchanged(t *testing.T) {
	for _, tb := range []*driver.Table{describedOrders(), describedCustomers()} {
		if stmts := alterSQL(t, tb, defOf(tb)); len(stmts) != 0 {
			t.Errorf("%s: unchanged definition produced\n%s", tb.Ref.Name, strings.Join(stmts, "\n"))
		}
	}
	// Equivalent spellings are not changes either.
	from := describedOrders()
	to := defOf(from)
	col(&to, "total").Type = "DECIMAL(12, 2)"
	col(&to, "total").Default = strp("((0))")
	col(&to, "token").Default = strp("(NEWID())")
	col(&to, "placed").Type = "datetimeoffset(3)"
	col(&to, "note").Collation = "sql_latin1_general_cp1_ci_as"
	col(&to, "ver").Type = "timestamp"
	to.Indexes[1].Type = "NONCLUSTERED"
	to.Indexes[1].Where = "([total]>(0))"
	to.Indexes[3].Desc = nil
	to.Checks[0].Expression = "(([total]>=(0)))"
	to.ForeignKeys[0].OnUpdate = ""
	to.ForeignKeys[0].RefTable.Schema = ""
	to.PrimaryKey = []string{"id"}
	col(&to, "id").Nullable = true // a primary key column is NOT NULL regardless
	if stmts := alterSQL(t, from, to); len(stmts) != 0 {
		t.Errorf("equivalent definition produced\n%s", strings.Join(stmts, "\n"))
	}
}

func TestAlterRenameColumn(t *testing.T) {
	from := describedOrders()
	to := defOf(from)
	col(&to, "note").Name = "memo"
	col(&to, "placed").Name = "placed_at" // used by an index: renaming is allowed
	wantStmts(t, alterSQL(t, from, to), []string{
		"EXEC sp_rename N'[dbo].[orders].[note]', N'memo', N'COLUMN'",
		"EXEC sp_rename N'[dbo].[orders].[placed]', N'placed_at', N'COLUMN'",
	})
	// Editors that update index columns to the new name get the same result.
	to.Indexes[3].Columns = []string{"placed_at", "price"}
	wantStmts(t, alterSQL(t, from, to), []string{
		"EXEC sp_rename N'[dbo].[orders].[note]', N'memo', N'COLUMN'",
		"EXEC sp_rename N'[dbo].[orders].[placed]', N'placed_at', N'COLUMN'",
	})
}

func TestAlterRenameDependedOnColumn(t *testing.T) {
	// SQL Server refuses to rename a column used by a check, a computed
	// column or an index filter: those are dropped and re-created.
	from := describedOrders()
	to := defOf(from)
	col(&to, "total").Name = "amount"
	wantStmts(t, alterSQL(t, from, to), []string{
		"ALTER TABLE [dbo].[orders] DROP CONSTRAINT [CK_orders_total]",
		"DROP INDEX [IX_orders_customer] ON [dbo].[orders]",
		"ALTER TABLE [dbo].[orders] DROP COLUMN [tax]",
		"EXEC sp_rename N'[dbo].[orders].[total]', N'amount', N'COLUMN'",
		"ALTER TABLE [dbo].[orders] ADD [tax] AS ([amount]*(0.2)) PERSISTED",
		"CREATE NONCLUSTERED INDEX [IX_orders_customer] ON [dbo].[orders] ([customer_id] DESC) INCLUDE ([amount]) WHERE [amount]>(0)",
		"ALTER TABLE [dbo].[orders] ADD CONSTRAINT [CK_orders_total] CHECK ([amount]>=(0))",
	})
}

func TestAlterTypeWithDefault(t *testing.T) {
	from := describedOrders()
	to := defOf(from)
	col(&to, "total").Type = "decimal(14,2)"
	dropDefault := "DECLARE @sql nvarchar(max) = (SELECT N'ALTER TABLE [dbo].[orders] DROP CONSTRAINT ' + QUOTENAME(dc.name)\n" +
		"  FROM sys.default_constraints dc JOIN sys.columns c ON c.object_id = dc.parent_object_id AND c.column_id = dc.parent_column_id\n" +
		"  WHERE dc.parent_object_id = OBJECT_ID(N'[dbo].[orders]') AND c.name = N'total');\n" +
		"IF @sql IS NOT NULL EXEC sp_executesql @sql"
	wantStmts(t, alterSQL(t, from, to), []string{
		"ALTER TABLE [dbo].[orders] DROP CONSTRAINT [CK_orders_total]",
		"DROP INDEX [IX_orders_customer] ON [dbo].[orders]",
		"ALTER TABLE [dbo].[orders] DROP COLUMN [tax]",
		dropDefault,
		"ALTER TABLE [dbo].[orders] ALTER COLUMN [total] decimal(14,2) NOT NULL",
		"ALTER TABLE [dbo].[orders] ADD DEFAULT (0) FOR [total]",
		"ALTER TABLE [dbo].[orders] ADD [tax] AS ([total]*(0.2)) PERSISTED",
		"CREATE NONCLUSTERED INDEX [IX_orders_customer] ON [dbo].[orders] ([customer_id] DESC) INCLUDE ([total]) WHERE [total]>(0)",
		"ALTER TABLE [dbo].[orders] ADD CONSTRAINT [CK_orders_total] CHECK ([total]>=(0))",
	})

	// Nullability alone keeps the default; collation and type are restated.
	to = defOf(from)
	col(&to, "flag").Nullable = true
	col(&to, "note").Nullable = false
	col(&to, "note").Collation = "Latin1_General_CS_AS"
	wantStmts(t, alterSQL(t, from, to), []string{
		"ALTER TABLE [dbo].[orders] ALTER COLUMN [note] nvarchar(max) COLLATE Latin1_General_CS_AS NOT NULL",
		"ALTER TABLE [dbo].[orders] ALTER COLUMN [flag] bit NULL",
	})

	// A type change of a key column rebuilds the key and indexes using it.
	to = defOf(from)
	col(&to, "token").Type = "nvarchar(36)"
	col(&to, "token").Collation = "Latin1_General_CS_AS"
	col(&to, "price").Type = "decimal(19,4)"
	stmts := alterSQL(t, from, to)
	for _, want := range []string{
		"ALTER TABLE [dbo].[orders] DROP CONSTRAINT [UQ_orders_token]",
		"DROP INDEX [IX_orders_placed] ON [dbo].[orders]",
		"ALTER TABLE [dbo].[orders] ALTER COLUMN [token] nvarchar(36) COLLATE Latin1_General_CS_AS NOT NULL",
		"ALTER TABLE [dbo].[orders] ADD DEFAULT (newid()) FOR [token]",
		"ALTER TABLE [dbo].[orders] ALTER COLUMN [price] decimal(19,4) NULL",
		"CREATE NONCLUSTERED INDEX [IX_orders_placed] ON [dbo].[orders] ([placed], [price])",
		"ALTER TABLE [dbo].[orders] ADD CONSTRAINT [UQ_orders_token] UNIQUE NONCLUSTERED ([token])",
	} {
		if !slices.Contains(stmts, want) {
			t.Errorf("missing %q in\n%s", want, strings.Join(stmts, "\n"))
		}
	}
	// A type change drops the collation clause for types without one.
	to = defOf(from)
	col(&to, "note").Type = "int"
	if stmts := alterSQL(t, from, to); !slices.Contains(stmts, "ALTER TABLE [dbo].[orders] ALTER COLUMN [note] int NULL") {
		t.Errorf("got\n%s", strings.Join(stmts, "\n"))
	}
}

func TestAlterDefaults(t *testing.T) {
	from := describedOrders()
	to := defOf(from)
	col(&to, "flag").Default = strp("1")      // change
	col(&to, "token").Default = nil           // drop
	col(&to, "price").Default = strp("(0.0)") // add
	col(&to, "placed").Default = strp("")     // "" means none: unchanged
	stmts := alterSQL(t, from, to)
	want := []string{
		"ALTER TABLE [dbo].[orders] ADD DEFAULT ((0.0)) FOR [price]",
		dropDefaultSQL("[dbo].[orders]", "token"),
		dropDefaultSQL("[dbo].[orders]", "flag"),
		"ALTER TABLE [dbo].[orders] ADD DEFAULT (1) FOR [flag]",
	}
	wantStmts(t, stmts, want)
}

func TestAlterIdentity(t *testing.T) {
	from := describedOrders()
	to := defOf(from)
	col(&to, "customer_id").AutoIncrement = true
	if _, err := (&conn{}).AlterTableSQL(from, to); err == nil || !strings.Contains(err.Error(), "cannot add IDENTITY") {
		t.Errorf("err = %v", err)
	}
	to = defOf(from)
	col(&to, "id").AutoIncrement = false
	if _, err := (&conn{}).AlterTableSQL(from, to); err == nil || !strings.Contains(err.Error(), "cannot remove IDENTITY") {
		t.Errorf("err = %v", err)
	}
	// A new identity column is fine; so is changing an identity column's type.
	c := describedCustomers()
	c.Referenced = nil
	to = defOf(c)
	col(&to, "id").Type = "bigint"
	to.Columns = append(to.Columns, driver.ColumnDef{Column: driver.Column{Name: "seq", Type: "int", AutoIncrement: true}})
	wantStmts(t, alterSQL(t, c, to), []string{
		"ALTER TABLE [dbo].[customers] DROP CONSTRAINT [PK_customers]",
		"ALTER TABLE [dbo].[customers] ALTER COLUMN [id] bigint NOT NULL",
		"ALTER TABLE [dbo].[customers] ADD [seq] int IDENTITY(1,1) NOT NULL",
		"ALTER TABLE [dbo].[customers] ADD CONSTRAINT [PK_customers] PRIMARY KEY CLUSTERED ([id])",
	})
}

func TestAlterReferencedKey(t *testing.T) {
	from := describedCustomers()
	to := defOf(from)
	col(&to, "id").Type = "bigint"
	_, err := (&conn{}).AlterTableSQL(from, to)
	if err == nil || err.Error() != "foreign key FK_orders_customers on dbo.orders references the primary key; drop that foreign key first" {
		t.Errorf("err = %v", err)
	}
}

func TestAlterIndexesKeysChecks(t *testing.T) {
	from := describedOrders()
	to := defOf(from)
	to.Indexes = []driver.Index{
		to.Indexes[0], // primary: ignored
		{Name: "IX_orders_customer", Columns: []string{"customer_id", "placed"}, Type: "nonclustered", Where: "[total]>(0)", Desc: []bool{true},
			Definition: from.Indexes[1].Definition}, // changed: keeps its INCLUDE list
		{Name: "IX_orders_note", Columns: []string{"price"}, Where: "price IS NOT NULL"},
		// UQ_orders_token and IX_orders_placed dropped
	}
	to.ForeignKeys[0].OnDelete = "NO ACTION"
	to.Checks = []driver.Check{{Name: "CK_orders_price", Expression: "[price] >= 0"}}
	to.PrimaryKey = []string{"id", "customer_id"}
	wantStmts(t, alterSQL(t, from, to), []string{
		"ALTER TABLE [dbo].[orders] DROP CONSTRAINT [FK_orders_customers]",
		"ALTER TABLE [dbo].[orders] DROP CONSTRAINT [CK_orders_total]",
		"DROP INDEX [IX_orders_customer] ON [dbo].[orders]",
		"ALTER TABLE [dbo].[orders] DROP CONSTRAINT [UQ_orders_token]",
		"DROP INDEX [IX_orders_placed] ON [dbo].[orders]",
		"ALTER TABLE [dbo].[orders] DROP CONSTRAINT [PK_orders]",
		"ALTER TABLE [dbo].[orders] ADD CONSTRAINT [PK_orders] PRIMARY KEY CLUSTERED ([id], [customer_id])",
		"CREATE NONCLUSTERED INDEX [IX_orders_customer] ON [dbo].[orders] ([customer_id] DESC, [placed]) INCLUDE ([total]) WHERE [total]>(0)",
		"CREATE INDEX [IX_orders_note] ON [dbo].[orders] ([price]) WHERE price IS NOT NULL",
		"ALTER TABLE [dbo].[orders] ADD CONSTRAINT [CK_orders_price] CHECK ([price] >= 0)",
		"ALTER TABLE [dbo].[orders] ADD CONSTRAINT [FK_orders_customers] FOREIGN KEY ([customer_id]) REFERENCES [dbo].[customers] ([id])",
	})

	// Removing the primary key; adding a self-referencing foreign key; unsupported types.
	to = defOf(from)
	to.PrimaryKey = nil
	wantStmts(t, alterSQL(t, from, to), []string{"ALTER TABLE [dbo].[orders] DROP CONSTRAINT [PK_orders]"})
	to = defOf(from)
	to.ForeignKeys = append(to.ForeignKeys, driver.ForeignKey{Name: "FK_orders_self", Columns: []string{"customer_id"},
		RefTable: driver.ObjectRef{Name: "orders"}, RefColumns: []string{"id"}, OnUpdate: "set default"})
	wantStmts(t, alterSQL(t, from, to), []string{
		"ALTER TABLE [dbo].[orders] ADD CONSTRAINT [FK_orders_self] FOREIGN KEY ([customer_id]) REFERENCES [dbo].[orders] ([id]) ON UPDATE SET DEFAULT",
	})
	// A self-referencing key is dropped and re-added around a rebuilt primary key.
	self := describedOrders()
	self.ForeignKeys = append(self.ForeignKeys, driver.ForeignKey{Name: "FK_orders_self", Columns: []string{"customer_id"},
		RefTable: driver.ObjectRef{Database: "shop", Schema: "dbo", Name: "orders"}, RefColumns: []string{"id"}, OnUpdate: "NO ACTION", OnDelete: "NO ACTION"})
	self.Referenced = []driver.ForeignKey{self.ForeignKeys[1]}
	self.Referenced[0].Table = &driver.ObjectRef{Database: "shop", Schema: "dbo", Name: "orders"}
	to = defOf(self)
	to.PrimaryKey = []string{"id", "customer_id"}
	stmts := alterSQL(t, self, to)
	if stmts[0] != "ALTER TABLE [dbo].[orders] DROP CONSTRAINT [FK_orders_self]" ||
		stmts[len(stmts)-1] != "ALTER TABLE [dbo].[orders] ADD CONSTRAINT [FK_orders_self] FOREIGN KEY ([customer_id]) REFERENCES [dbo].[orders] ([id])" {
		t.Errorf("got\n%s", strings.Join(stmts, "\n"))
	}
	to = defOf(from)
	to.Indexes = append(to.Indexes, driver.Index{Name: "SX_orders_location", Columns: []string{"location"}, Type: "spatial"})
	if _, err := (&conn{}).AlterTableSQL(from, to); err == nil || !strings.Contains(err.Error(), "use the console") {
		t.Errorf("err = %v", err)
	}
	to = defOf(from)
	to.Indexes = append(to.Indexes, driver.Index{Name: "IX_bad", Columns: []string{"nope"}})
	if _, err := (&conn{}).AlterTableSQL(from, to); err == nil || err.Error() != "index IX_bad uses column nope, which is not in the table" {
		t.Errorf("err = %v", err)
	}
}

func TestAlterDropColumn(t *testing.T) {
	// Dropping a column drops its default and the objects that use it.
	from := describedOrders()
	to := defOf(from)
	var cols []driver.ColumnDef
	for _, c := range to.Columns {
		if c.Name != "price" && c.Name != "flag" && c.Name != "location" {
			cols = append(cols, c)
		}
	}
	to.Columns = cols
	to.Columns = append(to.Columns, driver.ColumnDef{Column: driver.Column{Name: "flag", Type: "tinyint", Default: strp("0"), Comment: "new flag"}})
	wantStmts(t, alterSQL(t, from, to), []string{
		"DROP INDEX [IX_orders_placed] ON [dbo].[orders]",
		"ALTER TABLE [dbo].[orders] DROP COLUMN [price]",
		dropDefaultSQL("[dbo].[orders]", "flag"),
		"ALTER TABLE [dbo].[orders] DROP COLUMN [flag]",
		"ALTER TABLE [dbo].[orders] DROP COLUMN [location]",
		"ALTER TABLE [dbo].[orders] ADD [flag] tinyint NOT NULL DEFAULT (0)",
		"EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'new flag', @level0type = N'SCHEMA', @level0name = N'dbo', @level1type = N'TABLE', @level1name = N'orders', @level2type = N'COLUMN', @level2name = N'flag'",
	})
	// A computed column that keeps reading a dropped column is an error.
	to = defOf(from)
	to.Columns = slices.DeleteFunc(to.Columns, func(c driver.ColumnDef) bool { return c.Name == "total" })
	if _, err := (&conn{}).AlterTableSQL(from, to); err == nil || err.Error() != "computed column tax uses column total, which is being dropped" {
		t.Errorf("err = %v", err)
	}
}

func TestAlterComputedAndComments(t *testing.T) {
	from := describedOrders()
	to := defOf(from)
	col(&to, "tax").Generated = "[total]*(0.15)"
	col(&to, "tax").GeneratedStored = false
	col(&to, "tax").Comment = "VAT"
	col(&to, "note").Comment = ""
	col(&to, "price").Comment = "Unit price"
	col(&to, "placed").Comment = "When"
	from.Columns[7].Comment = "Placed"
	to.Comment = "All orders"
	wantStmts(t, alterSQL(t, from, to), []string{
		"ALTER TABLE [dbo].[orders] DROP COLUMN [tax]",
		"ALTER TABLE [dbo].[orders] ADD [tax] AS ([total]*(0.15))",
		"EXEC sys.sp_updateextendedproperty @name = N'MS_Description', @value = N'All orders', @level0type = N'SCHEMA', @level0name = N'dbo', @level1type = N'TABLE', @level1name = N'orders'",
		"EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'VAT', @level0type = N'SCHEMA', @level0name = N'dbo', @level1type = N'TABLE', @level1name = N'orders', @level2type = N'COLUMN', @level2name = N'tax'",
		"EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'Unit price', @level0type = N'SCHEMA', @level0name = N'dbo', @level1type = N'TABLE', @level1name = N'orders', @level2type = N'COLUMN', @level2name = N'price'",
		"EXEC sys.sp_dropextendedproperty @name = N'MS_Description', @level0type = N'SCHEMA', @level0name = N'dbo', @level1type = N'TABLE', @level1name = N'orders', @level2type = N'COLUMN', @level2name = N'note'",
		"EXEC sys.sp_updateextendedproperty @name = N'MS_Description', @value = N'When', @level0type = N'SCHEMA', @level0name = N'dbo', @level1type = N'TABLE', @level1name = N'orders', @level2type = N'COLUMN', @level2name = N'placed'",
	})
}

func TestAlterSwapAndTableRename(t *testing.T) {
	from := describedOrders()
	to := defOf(from)
	col(&to, "price").Name = "note"
	col(&to, "note").Name = "price"
	to.Indexes[3].Columns = []string{"placed", "note"} // the editor follows the rename
	to.Ref.Name = "purchase"
	stmts := alterSQL(t, from, to)
	wantStmts(t, stmts, []string{
		"EXEC sp_rename N'[dbo].[orders].[price]', N'rowsmith_tmp_1', N'COLUMN'",
		"EXEC sp_rename N'[dbo].[orders].[note]', N'rowsmith_tmp_2', N'COLUMN'",
		"EXEC sp_rename N'[dbo].[orders].[rowsmith_tmp_1]', N'note', N'COLUMN'",
		"EXEC sp_rename N'[dbo].[orders].[rowsmith_tmp_2]', N'price', N'COLUMN'",
		"EXEC sp_rename N'[dbo].[orders]', N'purchase'",
	})
}

func TestAlterErrors(t *testing.T) {
	from := describedOrders()
	to := defOf(from)
	to.Columns[1].OriginalName = "gone"
	if _, err := (&conn{}).AlterTableSQL(from, to); err == nil || !strings.Contains(err.Error(), "reload the structure") {
		t.Errorf("err = %v", err)
	}
	if _, err := (&conn{}).AlterTableSQL(nil, to); err == nil {
		t.Error("nil table accepted")
	}
	view := &driver.Table{Ref: driver.ObjectRef{Name: "v"}, Kind: "view", Columns: []driver.Column{{Name: "a", Type: "int"}}}
	if _, err := (&conn{}).AlterTableSQL(view, defOf(view)); err == nil {
		t.Error("view accepted")
	}
}

func TestGeneratedScriptSplits(t *testing.T) {
	// The console splits T-SQL on GO lines only, so each generated
	// statement, including multi-statement batches, runs as one batch.
	from := describedOrders()
	to := defOf(from)
	col(&to, "total").Type = "decimal(14,2)"
	col(&to, "token").Default = nil
	stmts := alterSQL(t, from, to)
	got := sqlsplit.Split(strings.Join(stmts, "\nGO\n"), sqlsplit.MSSQL)
	if len(got) != len(stmts) {
		t.Fatalf("%d batches for %d statements", len(got), len(stmts))
	}
	for i := range got {
		if got[i].SQL != stmts[i] {
			t.Errorf("batch %d = %q, want %q", i, got[i].SQL, stmts[i])
		}
	}
}

func TestObjectAndSchemaDDL(t *testing.T) {
	c := &conn{}
	check := func(stmts []string, err error, want ...string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		wantStmts(t, stmts, want)
	}
	s, err := c.DropObjectSQL(driver.ObjectRef{Schema: "sales", Name: "o]rders", Kind: "table"}, false)
	check(s, err, "DROP TABLE [sales].[o]]rders]")
	for kind, kw := range map[string]string{"view": "VIEW", "procedure": "PROCEDURE", "function": "FUNCTION", "trigger": "TRIGGER",
		"sequence": "SEQUENCE", "synonym": "SYNONYM", "type": "TYPE"} {
		s, err := c.DropObjectSQL(driver.ObjectRef{Name: "x", Kind: kind}, false)
		check(s, err, "DROP "+kw+" [dbo].[x]")
	}
	if _, err := c.DropObjectSQL(driver.ObjectRef{Name: "x", Kind: "table"}, true); err == nil {
		t.Error("cascade accepted")
	}
	s, err = c.TruncateSQL(driver.ObjectRef{Name: "orders", Kind: "table"})
	check(s, err, "TRUNCATE TABLE [dbo].[orders]")
	if _, err := c.TruncateSQL(driver.ObjectRef{Name: "v", Kind: "view"}); err == nil {
		t.Error("truncating a view accepted")
	}
	s, err = c.RenameObjectSQL(driver.ObjectRef{Schema: "sales", Name: "orders", Kind: "table"}, "o'rders")
	check(s, err, "EXEC sp_rename N'[sales].[orders]', N'o''rders'")
	s, err = c.RenameObjectSQL(driver.ObjectRef{Name: "email", Kind: "type"}, "mail")
	check(s, err, "EXEC sp_rename N'[dbo].[email]', N'mail', N'USERDATATYPE'")
	if _, err := c.RenameObjectSQL(driver.ObjectRef{Name: "v", Kind: "view"}, "w"); err == nil {
		t.Error("view rename accepted")
	}
	if _, err := c.RenameObjectSQL(driver.ObjectRef{Name: "t"}, " "); err == nil {
		t.Error("empty name accepted")
	}
	s, err = c.CreateDatabaseSQL("shop", map[string]string{"collation": "Latin1_General_100_CI_AS_SC_UTF8"})
	check(s, err, "CREATE DATABASE [shop] COLLATE Latin1_General_100_CI_AS_SC_UTF8")
	if _, err := c.CreateDatabaseSQL("shop", map[string]string{"collation": "x'"}); err == nil {
		t.Error("bad collation accepted")
	}
	s, err = c.DropDatabaseSQL("shop")
	check(s, err, "DROP DATABASE [shop]")
	s, err = c.CreateSchemaSQL("shop", "sales")
	check(s, err, "CREATE SCHEMA [sales]")
	s, err = c.DropSchemaSQL("shop", "sales", false)
	check(s, err, "DROP SCHEMA [sales]")
	if _, err := c.DropSchemaSQL("shop", "sales", true); err == nil {
		t.Error("cascade accepted")
	}
}

func TestNormTypeAndRefs(t *testing.T) {
	for in, want := range map[string]string{
		"NVARCHAR(50)": "nvarchar(50)", "nvarchar": "nvarchar(1)", "datetime2": "datetime2(7)", "Decimal (10, 2)": "decimal(10,2)",
		"numeric(5)": "numeric(5,0)", "dec": "decimal(18,0)", "timestamp": "rowversion", "float(24)": "real", "float(53)": "float",
		"[int]": "int", "varchar(MAX)": "varchar(max)", "integer": "int",
	} {
		if got := normType(in); got != want {
			t.Errorf("normType(%q) = %q, want %q", in, got, want)
		}
	}
	if got := refs("[a]+N'[not]'+[b]]c]*2"); !slices.Equal(got, []string{"a", "b]c"}) {
		t.Errorf("refs = %q", got)
	}
	if got := mapRefs("[a] > 'x''[a]' AND [b]", func(n string) string { return n + "2" }); got != "[a2] > 'x''[a]' AND [b2]" {
		t.Errorf("mapRefs = %q", got)
	}
	ix := driver.Index{Definition: "CREATE NONCLUSTERED INDEX [i] ON [dbo].[t] ([a] ASC) INCLUDE ([b], [c)]]d]) WHERE [a]>(0)"}
	if got := includedColumns(ix); !slices.Equal(got, []string{"b", "c)]d"}) {
		t.Errorf("included = %q", got)
	}
}
