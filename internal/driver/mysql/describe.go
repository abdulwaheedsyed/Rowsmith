package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
)

var (
	enumRe = regexp.MustCompile(`^(?i)(enum|set)\((.*)\)$`)
	sridRe = regexp.MustCompile(`(?i)/\*!80003 SRID (\d+) \*/|SRID (\d+)`)
)

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	if ref.Database == "" || ref.Name == "" {
		return nil, fmt.Errorf("database and table are required")
	}
	t := &driver.Table{Ref: ref, Kind: "table", Options: map[string]string{}}
	var typ, engine, coll, comment, rowFormat, createOpts sql.NullString
	var nrows, size, autoInc sql.NullInt64
	err := c.db.QueryRowContext(ctx, `SELECT TABLE_TYPE, ENGINE, TABLE_COLLATION, TABLE_COMMENT, ROW_FORMAT, CREATE_OPTIONS,
		TABLE_ROWS, DATA_LENGTH + INDEX_LENGTH, AUTO_INCREMENT
		FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, ref.Database, ref.Name).
		Scan(&typ, &engine, &coll, &comment, &rowFormat, &createOpts, &nrows, &size, &autoInc)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("table %s.%s not found", ref.Database, ref.Name)
	}
	if err != nil {
		return nil, err
	}
	if strings.Contains(typ.String, "VIEW") {
		t.Kind = "view"
	} else {
		t.Comment = comment.String
		t.RowEstimate, t.Size = sqlbase.NullInt(nrows), sqlbase.NullInt(size)
		setOpt(t.Options, "engine", engine.String)
		setOpt(t.Options, "collation", coll.String)
		setOpt(t.Options, "row_format", rowFormat.String)
		setOpt(t.Options, "create_options", createOpts.String)
		if autoInc.Valid {
			t.Options["auto_increment"] = strconv.FormatInt(autoInc.Int64, 10)
		}
	}
	t.Ref.Kind = t.Kind

	if err := c.describeColumns(ctx, t); err != nil {
		return nil, err
	}
	if t.Kind == "table" {
		if err := c.describeIndexes(ctx, t); err != nil {
			return nil, err
		}
		if err := c.describeFKs(ctx, t); err != nil {
			return nil, err
		}
		c.describeChecks(ctx, t)
		c.describeTriggers(ctx, t)
		sqlbase.ChooseRowKey(t, false)
	}
	t.DDL, _ = c.showCreate(ctx, t.Kind, ref)
	if t.Kind == "view" {
		t.Definition = t.DDL
	}
	return t, nil
}

func setOpt(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func (c *conn) describeColumns(ctx context.Context, t *driver.Table) error {
	srsCol := "NULL"
	if c.flavor == "mysql" && c.major >= 8 {
		srsCol = "SRS_ID"
	}
	rows, err := c.db.QueryContext(ctx, `SELECT COLUMN_NAME, COLUMN_TYPE, DATA_TYPE, IS_NULLABLE, COLUMN_DEFAULT, EXTRA, COLUMN_KEY,
		COLUMN_COMMENT, COLLATION_NAME, CHARACTER_MAXIMUM_LENGTH, NUMERIC_PRECISION, NUMERIC_SCALE, GENERATION_EXPRESSION, `+srsCol+`
		FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, t.Ref.Database, t.Ref.Name)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, colType, dataType, nullable, extra, key, comment, coll, genExpr sql.NullString
		var def sql.NullString
		var length, prec, scale, srs sql.NullInt64
		if err := rows.Scan(&name, &colType, &dataType, &nullable, &def, &extra, &key, &comment, &coll, &length, &prec, &scale, &genExpr, &srs); err != nil {
			return err
		}
		base := strings.ToLower(dataType.String)
		col := driver.Column{
			Name: name.String, Type: colType.String, BaseType: base, Nullable: nullable.String == "YES",
			Comment: comment.String, Collation: coll.String, PrimaryKey: key.String == "PRI",
			Length: sqlbase.NullInt(length), Precision: sqlbase.NullInt(prec), Scale: sqlbase.NullInt(scale),
			Unsigned: strings.Contains(strings.ToLower(colType.String), "unsigned"),
		}
		col.Kind = kindOf(base, colType.String)
		ex := strings.ToLower(extra.String)
		col.AutoIncrement = strings.Contains(ex, "auto_increment")
		if i := strings.Index(ex, "on update "); i >= 0 {
			col.OnUpdate = strings.TrimSpace(extra.String[i+10:])
		}
		if genExpr.String != "" && (strings.Contains(ex, "generated") || strings.Contains(ex, "virtual") || strings.Contains(ex, "stored") || strings.Contains(ex, "persistent")) {
			col.Generated = genExpr.String
		}
		if def.Valid && col.Generated == "" {
			d := c.normalizeDefault(def.String, ex, col.Kind)
			col.Default = &d
		}
		if m := enumRe.FindStringSubmatch(colType.String); m != nil {
			col.Enum = parseEnum(m[2])
			if col.Kind == driver.KindString && strings.EqualFold(m[1], "enum") {
				col.Kind = driver.KindEnum
			}
		}
		if col.Kind == driver.KindGeometry {
			col.GeometryType = base
			if srs.Valid {
				col.SRID = int(srs.Int64)
			}
		}
		t.Columns = append(t.Columns, col)
		if col.PrimaryKey {
			t.PrimaryKey = append(t.PrimaryKey, col.Name)
		}
	}
	return rows.Err()
}

// normalizeDefault renders COLUMN_DEFAULT as a SQL expression usable in DDL.
func (c *conn) normalizeDefault(v, extra string, kind driver.ValueKind) string {
	if c.flavor == "mariadb" {
		// MariaDB already reports literals quoted and expressions verbatim.
		return v
	}
	if strings.Contains(extra, "default_generated") {
		up := strings.ToUpper(v)
		if strings.HasPrefix(up, "CURRENT_TIMESTAMP") || strings.HasPrefix(up, "NOW(") || strings.HasPrefix(up, "LOCALTIME") {
			return v
		}
		return "(" + v + ")"
	}
	up := strings.ToUpper(v)
	if strings.HasPrefix(up, "CURRENT_TIMESTAMP") {
		return v
	}
	switch kind {
	case driver.KindInt, driver.KindFloat, driver.KindDecimal:
		if _, err := strconv.ParseFloat(v, 64); err == nil {
			return v
		}
	case driver.KindBool:
		if strings.HasPrefix(v, "b'") {
			return v
		}
	}
	return "'" + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), "'", "''") + "'"
}

func parseEnum(s string) []string {
	var out []string
	var cur strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\'' && !in:
			in = true
		case ch == '\'' && in:
			if i+1 < len(s) && s[i+1] == '\'' {
				cur.WriteByte('\'')
				i++
			} else {
				in = false
				out = append(out, cur.String())
				cur.Reset()
			}
		case ch == '\\' && in && i+1 < len(s):
			cur.WriteByte(s[i+1])
			i++
		case in:
			cur.WriteByte(ch)
		}
	}
	return out
}

func kindOf(base, full string) driver.ValueKind {
	switch base {
	case "tinyint":
		if strings.HasPrefix(strings.ToLower(full), "tinyint(1)") {
			return driver.KindBool
		}
		return driver.KindInt
	case "bit":
		if strings.EqualFold(full, "bit(1)") {
			return driver.KindBool
		}
		return driver.KindInt
	case "year":
		return driver.KindInt
	case "timestamp":
		return driver.KindDateTime
	case "set":
		return driver.KindString
	case "vector":
		return driver.KindBinary
	}
	return driver.KindFromTypeName(base)
}

func (c *conn) describeIndexes(ctx context.Context, t *driver.Table) error {
	exprCol := "NULL"
	if c.flavor == "mysql" && (c.major > 8 || c.major == 8 && c.minor >= 0) {
		exprCol = "EXPRESSION"
	}
	rows, err := c.db.QueryContext(ctx, `SELECT INDEX_NAME, NON_UNIQUE, COLUMN_NAME, SUB_PART, INDEX_TYPE, COLLATION, INDEX_COMMENT, `+exprCol+`
		FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY INDEX_NAME = 'PRIMARY' DESC, INDEX_NAME, SEQ_IN_INDEX`,
		t.Ref.Database, t.Ref.Name)
	if err != nil {
		if exprCol != "NULL" && strings.Contains(err.Error(), "EXPRESSION") {
			c.major = 5 // older 8.0 builds lack the column; retry without it
			return c.describeIndexes(ctx, t)
		}
		return err
	}
	defer rows.Close()
	idx := map[string]*driver.Index{}
	var order []string
	for rows.Next() {
		var name, colName, typ, coll, comment, expr sql.NullString
		var nonUnique int
		var sub sql.NullInt64
		if err := rows.Scan(&name, &nonUnique, &colName, &sub, &typ, &coll, &comment, &expr); err != nil {
			return err
		}
		ix, ok := idx[name.String]
		if !ok {
			ix = &driver.Index{Name: name.String, Unique: nonUnique == 0, Primary: name.String == "PRIMARY",
				Type: strings.ToLower(typ.String), Comment: comment.String}
			idx[name.String] = ix
			order = append(order, name.String)
		}
		col := colName.String
		if col == "" && expr.String != "" {
			col = "(" + expr.String + ")"
		}
		ix.Columns = append(ix.Columns, col)
		ix.Lengths = append(ix.Lengths, int(sub.Int64))
		ix.Desc = append(ix.Desc, coll.String == "D")
	}
	for _, n := range order {
		t.Indexes = append(t.Indexes, *idx[n])
	}
	return rows.Err()
}

func (c *conn) describeFKs(ctx context.Context, t *driver.Table) error {
	q := `SELECT k.CONSTRAINT_NAME, k.TABLE_SCHEMA, k.TABLE_NAME, k.COLUMN_NAME, k.REFERENCED_TABLE_SCHEMA, k.REFERENCED_TABLE_NAME,
		k.REFERENCED_COLUMN_NAME, r.UPDATE_RULE, r.DELETE_RULE
		FROM information_schema.KEY_COLUMN_USAGE k
		JOIN information_schema.REFERENTIAL_CONSTRAINTS r
		  ON r.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA AND r.CONSTRAINT_NAME = k.CONSTRAINT_NAME AND r.TABLE_NAME = k.TABLE_NAME
		WHERE k.REFERENCED_TABLE_NAME IS NOT NULL AND %s
		ORDER BY k.CONSTRAINT_NAME, k.ORDINAL_POSITION`
	out, err := c.fks(ctx, fmt.Sprintf(q, "k.TABLE_SCHEMA = ? AND k.TABLE_NAME = ?"), t.Ref.Database, t.Ref.Name)
	if err != nil {
		return err
	}
	t.ForeignKeys = out
	in, err := c.fks(ctx, fmt.Sprintf(q, "k.REFERENCED_TABLE_SCHEMA = ? AND k.REFERENCED_TABLE_NAME = ?"), t.Ref.Database, t.Ref.Name)
	if err != nil {
		return err
	}
	t.Referenced = in
	return nil
}

func (c *conn) fks(ctx context.Context, q string, args ...any) ([]driver.ForeignKey, error) {
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string]*driver.ForeignKey{}
	var order []string
	for rows.Next() {
		var name, schema, table, col, rschema, rtable, rcol, upd, del string
		if err := rows.Scan(&name, &schema, &table, &col, &rschema, &rtable, &rcol, &upd, &del); err != nil {
			return nil, err
		}
		key := schema + "." + table + "." + name
		fk, ok := byName[key]
		if !ok {
			fk = &driver.ForeignKey{Name: name, RefTable: driver.ObjectRef{Database: rschema, Name: rtable, Kind: "table"},
				OnUpdate: upd, OnDelete: del, Table: &driver.ObjectRef{Database: schema, Name: table, Kind: "table"}}
			byName[key] = fk
			order = append(order, key)
		}
		fk.Columns = append(fk.Columns, col)
		fk.RefColumns = append(fk.RefColumns, rcol)
	}
	out := make([]driver.ForeignKey, 0, len(order))
	for _, k := range order {
		out = append(out, *byName[k])
	}
	return out, rows.Err()
}

func (c *conn) describeChecks(ctx context.Context, t *driver.Table) {
	q := `SELECT cc.CONSTRAINT_NAME, cc.CHECK_CLAUSE FROM information_schema.CHECK_CONSTRAINTS cc
		JOIN information_schema.TABLE_CONSTRAINTS tc ON tc.CONSTRAINT_SCHEMA = cc.CONSTRAINT_SCHEMA AND tc.CONSTRAINT_NAME = cc.CONSTRAINT_NAME
		WHERE tc.TABLE_SCHEMA = ? AND tc.TABLE_NAME = ? AND tc.CONSTRAINT_TYPE = 'CHECK'`
	if c.flavor == "mariadb" {
		q = `SELECT CONSTRAINT_NAME, CHECK_CLAUSE FROM information_schema.CHECK_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ? AND TABLE_NAME = ?`
	}
	rows, err := c.db.QueryContext(ctx, q, t.Ref.Database, t.Ref.Name)
	if err != nil {
		return // unsupported before MySQL 8.0.16 / MariaDB 10.2
	}
	defer rows.Close()
	for rows.Next() {
		var ch driver.Check
		if rows.Scan(&ch.Name, &ch.Expression) == nil {
			t.Checks = append(t.Checks, ch)
		}
	}
}

func (c *conn) describeTriggers(ctx context.Context, t *driver.Table) {
	rows, err := c.db.QueryContext(ctx, `SELECT TRIGGER_NAME, ACTION_TIMING, EVENT_MANIPULATION, ACTION_STATEMENT FROM information_schema.TRIGGERS
		WHERE EVENT_OBJECT_SCHEMA = ? AND EVENT_OBJECT_TABLE = ? ORDER BY ACTION_ORDER`, t.Ref.Database, t.Ref.Name)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var tr driver.Trigger
		if rows.Scan(&tr.Name, &tr.Timing, &tr.Event, &tr.Statement) == nil {
			t.Triggers = append(t.Triggers, tr)
		}
	}
}

func (c *conn) showCreate(ctx context.Context, kind string, ref driver.ObjectRef) (string, error) {
	var stmt string
	col := 1
	switch kind {
	case "table":
		stmt = "SHOW CREATE TABLE " + qualify(ref.Database, ref.Name)
	case "view":
		stmt = "SHOW CREATE VIEW " + qualify(ref.Database, ref.Name)
	case "procedure":
		stmt, col = "SHOW CREATE PROCEDURE "+qualify(ref.Database, ref.Name), 2
	case "function":
		stmt, col = "SHOW CREATE FUNCTION "+qualify(ref.Database, ref.Name), 2
	case "trigger":
		stmt, col = "SHOW CREATE TRIGGER "+qualify(ref.Database, ref.Name), 2
	case "event":
		stmt, col = "SHOW CREATE EVENT "+qualify(ref.Database, ref.Name), 3
	default:
		return "", driver.ErrNotSupported
	}
	res, err := sqlbase.Collect(ctx, dialect{c: c}, c.db, 1, stmt)
	if err != nil {
		return "", err
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) <= col {
		return "", fmt.Errorf("no definition returned (missing privileges?)")
	}
	s, _ := res.Rows[0][col].(string)
	if s == "" {
		return "", fmt.Errorf("definition not visible to this user")
	}
	return s, nil
}

// Definition implements driver.Definer.
func (c *conn) Definition(ctx context.Context, ref driver.ObjectRef) (string, error) {
	return c.showCreate(ctx, ref.Kind, ref)
}

// CatalogColumns implements driver.Catalog.
func (c *conn) CatalogColumns(ctx context.Context, s driver.Scope) ([]driver.CatalogTable, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT c.TABLE_NAME, t.TABLE_TYPE, c.COLUMN_NAME, c.COLUMN_TYPE, c.COLUMN_KEY
		FROM information_schema.COLUMNS c JOIN information_schema.TABLES t ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME
		WHERE c.TABLE_SCHEMA = ? ORDER BY c.TABLE_NAME, c.ORDINAL_POSITION`, s.Database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.CatalogTable
	idx := map[string]int{}
	for rows.Next() {
		var tn, tt, cn, ct, key string
		if err := rows.Scan(&tn, &tt, &cn, &ct, &key); err != nil {
			return nil, err
		}
		i, ok := idx[tn]
		if !ok {
			kind := "table"
			if strings.Contains(tt, "VIEW") {
				kind = "view"
			}
			out = append(out, driver.CatalogTable{Name: tn, Kind: kind})
			i = len(out) - 1
			idx[tn] = i
		}
		out[i].Columns = append(out[i].Columns, driver.CatalogColumn{Name: cn, Type: ct, PK: key == "PRI"})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	fks, err := c.fks(ctx, `SELECT k.CONSTRAINT_NAME, k.TABLE_SCHEMA, k.TABLE_NAME, k.COLUMN_NAME, k.REFERENCED_TABLE_SCHEMA, k.REFERENCED_TABLE_NAME,
		k.REFERENCED_COLUMN_NAME, r.UPDATE_RULE, r.DELETE_RULE
		FROM information_schema.KEY_COLUMN_USAGE k
		JOIN information_schema.REFERENTIAL_CONSTRAINTS r ON r.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA AND r.CONSTRAINT_NAME = k.CONSTRAINT_NAME AND r.TABLE_NAME = k.TABLE_NAME
		WHERE k.REFERENCED_TABLE_NAME IS NOT NULL AND k.TABLE_SCHEMA = ? ORDER BY k.CONSTRAINT_NAME, k.ORDINAL_POSITION`, s.Database)
	if err == nil {
		for _, fk := range fks {
			if i, ok := idx[fk.Table.Name]; ok {
				out[i].FKs = append(out[i].FKs, fk)
			}
		}
	}
	return out, nil
}
