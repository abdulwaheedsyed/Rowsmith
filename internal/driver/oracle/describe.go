package oracle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
)

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	return c.describe(ctx, ref, true)
}

// describe reads a table, view or materialized view. The light form (full =
// false) serves browsing and editing: it skips foreign keys, triggers, sizes
// and DDL but chooses the same row key as the full form.
func (c *conn) describe(ctx context.Context, ref driver.ObjectRef, full bool) (*driver.Table, error) {
	if ref.Name == "" {
		return nil, errors.New("table name is required")
	}
	schema := c.schema(ref.Schema)
	t := &driver.Table{Ref: ref, Options: map[string]string{}}
	t.Ref.Schema = schema
	kind, err := c.relationKind(ctx, schema, ref.Name)
	if err != nil {
		return nil, err
	}
	t.Kind, t.Ref.Kind = kind, kind
	if err := c.describeRelation(ctx, t); err != nil {
		return nil, cleanErr(err)
	}
	if err := c.describeColumns(ctx, t); err != nil {
		return nil, cleanErr(err)
	}
	var cons constraints
	if kind != "view" {
		if cons, err = c.describeConstraints(ctx, t); err != nil {
			return nil, cleanErr(err)
		}
		if err := c.describeIndexes(ctx, t, cons); err != nil {
			return nil, cleanErr(err)
		}
	}
	if kind == "table" {
		sqlbase.ChooseRowKey(t, true)
	}
	if !full {
		return t, nil
	}
	if kind == "table" {
		if t.ForeignKeys, err = c.fks(ctx, "c.OWNER = :1 AND c.TABLE_NAME = :2", schema, ref.Name); err != nil {
			return nil, cleanErr(err)
		}
		if t.Referenced, err = c.fks(ctx, "r.OWNER = :1 AND r.TABLE_NAME = :2", schema, ref.Name); err != nil {
			return nil, cleanErr(err)
		}
	}
	if kind != "materialized_view" {
		c.describeTriggers(ctx, t)
	}
	if kind != "view" {
		if n, ok := c.segmentSizes(ctx, schema, ref.Name)[ref.Name]; ok {
			t.Size = &n
		}
	}
	c.describeDDL(ctx, t, cons)
	return t, nil
}

func (c *conn) relationKind(ctx context.Context, schema, name string) (string, error) {
	found, err := sqlbase.Strings(ctx, c.db, `SELECT OBJECT_TYPE FROM ALL_OBJECTS WHERE OWNER = :1 AND OBJECT_NAME = :2
		AND OBJECT_TYPE IN ('TABLE', 'VIEW', 'MATERIALIZED VIEW')`, schema, name)
	if err != nil {
		return "", cleanErr(err)
	}
	kind := ""
	for _, f := range found {
		switch f {
		case "MATERIALIZED VIEW":
			return "materialized_view", nil // its container table has the same name
		case "VIEW":
			kind = "view"
		case "TABLE":
			kind = "table"
		}
	}
	if kind == "" {
		return "", fmt.Errorf("table or view %s.%s not found", schema, name)
	}
	return kind, nil
}

// describeRelation reads statistics, comments and storage options.
func (c *conn) describeRelation(ctx context.Context, t *driver.Table) error {
	s, n := t.Ref.Schema, t.Ref.Name
	switch t.Kind {
	case "view":
		var comment sql.NullString
		err := c.db.QueryRowContext(ctx, `SELECT COMMENTS FROM ALL_TAB_COMMENTS WHERE OWNER = :1 AND TABLE_NAME = :2`, s, n).Scan(&comment)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		t.Comment = comment.String
		return nil
	case "materialized_view":
		var comment, refresh sql.NullString
		err := c.db.QueryRowContext(ctx, `SELECT c.COMMENTS, m.REFRESH_MODE || ' ' || m.REFRESH_METHOD FROM ALL_MVIEWS m
			LEFT JOIN ALL_MVIEW_COMMENTS c ON c.OWNER = m.OWNER AND c.MVIEW_NAME = m.MVIEW_NAME
			WHERE m.OWNER = :1 AND m.MVIEW_NAME = :2`, s, n).Scan(&comment, &refresh)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		t.Comment = comment.String
		setOpt(t.Options, "refresh", strings.ToLower(strings.TrimSpace(refresh.String)))
	}
	var nrows sql.NullInt64
	var temp, part, iot, space, comment sql.NullString
	err := c.db.QueryRowContext(ctx, `SELECT t.NUM_ROWS, t.TEMPORARY, t.PARTITIONED, t.IOT_TYPE, t.TABLESPACE_NAME, c.COMMENTS
		FROM ALL_TABLES t LEFT JOIN ALL_TAB_COMMENTS c ON c.OWNER = t.OWNER AND c.TABLE_NAME = t.TABLE_NAME
		WHERE t.OWNER = :1 AND t.TABLE_NAME = :2`, s, n).Scan(&nrows, &temp, &part, &iot, &space, &comment)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	t.RowEstimate = sqlbase.NullInt(nrows)
	if t.Kind == "table" {
		t.Comment = comment.String
	}
	setOpt(t.Options, "tablespace", space.String)
	if temp.String == "Y" {
		t.Options["temporary"] = "yes"
	}
	if part.String == "YES" {
		t.Options["partitioned"] = "yes"
	}
	if iot.String == "IOT" {
		t.Options["organization"] = "index"
	}
	return nil
}

func setOpt(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// colMeta holds the ALL_TAB_COLS facts that define a column's type.
type colMeta struct {
	dataType, typeOwner, typeMod, charUsed string
	length, precision, scale, charLength   sql.NullInt64
}

// columnType renders the declared type, e.g. NUMBER(12,2) or VARCHAR2(100 CHAR).
func columnType(m colMeta) string {
	switch dt := m.dataType; dt {
	case "VARCHAR2", "VARCHAR", "CHAR":
		if m.charUsed == "C" {
			return fmt.Sprintf("%s(%d CHAR)", dt, m.charLength.Int64)
		}
		return fmt.Sprintf("%s(%d BYTE)", dt, m.length.Int64)
	case "NVARCHAR2", "NCHAR":
		return fmt.Sprintf("%s(%d)", dt, m.charLength.Int64)
	case "RAW":
		return fmt.Sprintf("RAW(%d)", m.length.Int64)
	case "NUMBER":
		switch {
		case !m.precision.Valid && !m.scale.Valid:
			return "NUMBER"
		case !m.precision.Valid:
			return fmt.Sprintf("NUMBER(*,%d)", m.scale.Int64)
		case m.scale.Int64 == 0:
			return fmt.Sprintf("NUMBER(%d)", m.precision.Int64)
		}
		return fmt.Sprintf("NUMBER(%d,%d)", m.precision.Int64, m.scale.Int64)
	case "FLOAT":
		if m.precision.Valid {
			return fmt.Sprintf("FLOAT(%d)", m.precision.Int64)
		}
		return dt
	}
	t := m.dataType
	switch m.typeOwner {
	case "", "SYS", "PUBLIC", "MDSYS":
	default:
		t = ident(m.typeOwner) + "." + ident(t)
	}
	if m.typeMod != "" {
		t = m.typeMod + " " + t
	}
	return t
}

var typeArgs = regexp.MustCompile(`\(\d+(,\s*\d+)?\)`)

// baseType lowercases a data type without its size arguments, e.g.
// "TIMESTAMP(6) WITH TIME ZONE" -> "timestamp with time zone".
func baseType(dataType string) string {
	return strings.ToLower(strings.Join(strings.Fields(typeArgs.ReplaceAllString(dataType, "")), " "))
}

func columnKind(base string, m colMeta) driver.ValueKind {
	switch base {
	case "number":
		return numberKind(m.precision.Valid, m.precision.Int64, m.scale.Valid, m.scale.Int64)
	case "float":
		return driver.KindDecimal // FLOAT is a NUMBER with binary precision
	case "date":
		return driver.KindDateTime // Oracle DATE carries a time of day
	case "xmltype", "vector":
		return driver.KindText
	case "bfile":
		return driver.KindOther // a locator, not the file contents
	case "boolean":
		return driver.KindBool
	case "json":
		return driver.KindJSON
	case "sdo_geometry":
		return driver.KindGeometry
	}
	if m.typeOwner != "" || m.typeMod != "" {
		return driver.KindOther // object types, collections and REFs
	}
	return driver.KindFromTypeName(base)
}

func (c *conn) describeColumns(ctx context.Context, t *driver.Table) error {
	rows, err := c.db.QueryContext(ctx, `SELECT c.COLUMN_NAME, c.DATA_TYPE, c.DATA_TYPE_OWNER, c.DATA_TYPE_MOD, c.DATA_LENGTH,
		c.DATA_PRECISION, c.DATA_SCALE, c.CHAR_LENGTH, c.CHAR_USED, c.NULLABLE, c.DATA_DEFAULT, c.VIRTUAL_COLUMN, c.DEFAULT_ON_NULL,
		cm.COMMENTS, i.GENERATION_TYPE
		FROM ALL_TAB_COLS c
		LEFT JOIN ALL_COL_COMMENTS cm ON cm.OWNER = c.OWNER AND cm.TABLE_NAME = c.TABLE_NAME AND cm.COLUMN_NAME = c.COLUMN_NAME
		LEFT JOIN ALL_TAB_IDENTITY_COLS i ON i.OWNER = c.OWNER AND i.TABLE_NAME = c.TABLE_NAME AND i.COLUMN_NAME = c.COLUMN_NAME
		WHERE c.OWNER = :1 AND c.TABLE_NAME = :2 AND c.USER_GENERATED = 'YES'
		ORDER BY c.COLUMN_ID NULLS LAST, c.INTERNAL_COLUMN_ID`, t.Ref.Schema, t.Ref.Name)
	if err != nil {
		return err
	}
	defer rows.Close()
	geometry := false
	for rows.Next() {
		var m colMeta
		var name, owner, mod, charUsed, nullable, def, virtual, onNull, comment, identity sql.NullString
		if err := rows.Scan(&name, &m.dataType, &owner, &mod, &m.length, &m.precision, &m.scale, &m.charLength, &charUsed,
			&nullable, &def, &virtual, &onNull, &comment, &identity); err != nil {
			return err
		}
		m.typeOwner, m.typeMod, m.charUsed = owner.String, mod.String, charUsed.String
		col := driver.Column{Name: name.String, Type: columnType(m), BaseType: baseType(m.dataType), Nullable: nullable.String != "N",
			Comment: comment.String}
		col.Kind = columnKind(col.BaseType, m)
		switch col.BaseType {
		case "varchar2", "varchar", "char", "nvarchar2", "nchar":
			col.Length = sqlbase.NullInt(m.charLength)
		case "raw":
			col.Length = sqlbase.NullInt(m.length)
		case "number", "float":
			col.Precision, col.Scale = sqlbase.NullInt(m.precision), sqlbase.NullInt(m.scale)
		case "sdo_geometry":
			geometry = true
		}
		expr := strings.TrimSpace(def.String)
		switch {
		case identity.Valid:
			col.AutoIncrement = true
			g := "GENERATED " + identity.String
			if onNull.String == "YES" {
				g += " ON NULL"
			}
			g += " AS IDENTITY"
			col.Default = &g
		case virtual.String == "YES":
			col.Generated = expr
		case def.Valid && !strings.EqualFold(expr, "NULL"):
			// DEFAULT NULL is how Oracle removes a default; it leaves "NULL" behind.
			if onNull.String == "YES" {
				expr = "ON NULL " + expr
			}
			col.Default = &expr
		}
		t.Columns = append(t.Columns, col)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if geometry {
		c.describeGeometry(ctx, t)
	}
	return nil
}

// describeGeometry reads SRIDs from the spatial metadata. Without Oracle
// Spatial (or access to MDSYS.SDO_UTIL) geometry columns are shown by type
// name only, like other object types.
func (c *conn) describeGeometry(ctx context.Context, t *driver.Table) {
	var n int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER = 'MDSYS' AND OBJECT_NAME = 'SDO_UTIL'`).Scan(&n); err != nil || n == 0 {
		for i := range t.Columns {
			if t.Columns[i].BaseType == "sdo_geometry" {
				t.Columns[i].Kind = driver.KindOther
			}
		}
		return
	}
	srids := map[string]int{}
	if rows, err := c.db.QueryContext(ctx, `SELECT COLUMN_NAME, SRID FROM ALL_SDO_GEOM_METADATA WHERE OWNER = :1 AND TABLE_NAME = :2`,
		t.Ref.Schema, t.Ref.Name); err == nil {
		for rows.Next() {
			var col string
			var srid sql.NullInt64
			if rows.Scan(&col, &srid) == nil {
				srids[col] = int(srid.Int64)
			}
		}
		rows.Close()
	}
	for i := range t.Columns {
		if col := &t.Columns[i]; col.BaseType == "sdo_geometry" {
			col.GeometryType, col.SRID = "geometry", srids[col.Name]
		}
	}
}

// constraints keeps what DDL reconstruction needs beyond driver.Table.
type constraints struct {
	pk, pkIndex string
	unique      []namedColumns
	indexes     map[string]bool // indexes created for constraints
}

type namedColumns struct {
	name    string
	columns []string
	index   string // index enforcing a unique constraint
}

var notNullCheck = regexp.MustCompile(`^"([^"]+)" IS NOT NULL$`)

// notNullConstraint reports check constraints that implement NOT NULL,
// which is shown on the column instead. They are system-named unless the
// column was declared with CONSTRAINT name NOT NULL; on a nullable column
// the same condition is a real (or disabled) check.
func notNullConstraint(expr string, cols []driver.Column) bool {
	m := notNullCheck.FindStringSubmatch(expr)
	if m == nil {
		return false
	}
	for _, col := range cols {
		if col.Name == m[1] {
			return !col.Nullable
		}
	}
	return false
}

func (c *conn) describeConstraints(ctx context.Context, t *driver.Table) (constraints, error) {
	cons := constraints{indexes: map[string]bool{}}
	rows, err := c.db.QueryContext(ctx, `SELECT c.CONSTRAINT_NAME, c.CONSTRAINT_TYPE, c.SEARCH_CONDITION, c.INDEX_NAME, cc.COLUMN_NAME
		FROM ALL_CONSTRAINTS c
		LEFT JOIN ALL_CONS_COLUMNS cc ON cc.OWNER = c.OWNER AND cc.CONSTRAINT_NAME = c.CONSTRAINT_NAME AND cc.TABLE_NAME = c.TABLE_NAME
		WHERE c.OWNER = :1 AND c.TABLE_NAME = :2 AND c.CONSTRAINT_TYPE IN ('P', 'U', 'C')
		ORDER BY c.CONSTRAINT_TYPE, c.CONSTRAINT_NAME, cc.POSITION`, t.Ref.Schema, t.Ref.Name)
	if err != nil {
		return cons, err
	}
	defer rows.Close()
	checks := map[string]bool{}
	for rows.Next() {
		var name, typ, cond, index, col sql.NullString
		if err := rows.Scan(&name, &typ, &cond, &index, &col); err != nil {
			return cons, err
		}
		if index.Valid {
			cons.indexes[index.String] = true
		}
		switch typ.String {
		case "P":
			cons.pk, cons.pkIndex = name.String, index.String
			t.PrimaryKey = append(t.PrimaryKey, col.String)
		case "U":
			if n := len(cons.unique); n == 0 || cons.unique[n-1].name != name.String {
				cons.unique = append(cons.unique, namedColumns{name: name.String})
			}
			u := &cons.unique[len(cons.unique)-1]
			u.columns, u.index = append(u.columns, col.String), index.String
		case "C":
			expr := strings.TrimSpace(cond.String)
			if checks[name.String] || notNullConstraint(expr, t.Columns) {
				continue
			}
			checks[name.String] = true
			t.Checks = append(t.Checks, driver.Check{Name: name.String, Expression: expr})
		}
	}
	for i := range t.Columns {
		for _, k := range t.PrimaryKey {
			if t.Columns[i].Name == k {
				t.Columns[i].PrimaryKey = true
			}
		}
	}
	return cons, rows.Err()
}

var indexTypes = map[string]string{
	"NORMAL": "btree", "NORMAL/REV": "btree (reverse)", "FUNCTION-BASED NORMAL": "btree", "FUNCTION-BASED NORMAL/REV": "btree (reverse)",
	"BITMAP": "bitmap", "FUNCTION-BASED BITMAP": "bitmap", "DOMAIN": "domain", "FUNCTION-BASED DOMAIN": "domain",
	"IOT - TOP": "index-organized", "CLUSTER": "cluster",
}

var quotedName = regexp.MustCompile(`^"([^"]+)"$`)

func (c *conn) describeIndexes(ctx context.Context, t *driver.Table, cons constraints) error {
	pkIndex := cons.pkIndex
	rows, err := c.db.QueryContext(ctx, `SELECT i.OWNER, i.INDEX_NAME, i.INDEX_TYPE, i.UNIQUENESS, ic.COLUMN_NAME, ic.DESCEND, e.COLUMN_EXPRESSION, i.ITYP_NAME
		FROM ALL_INDEXES i
		JOIN ALL_IND_COLUMNS ic ON ic.INDEX_OWNER = i.OWNER AND ic.INDEX_NAME = i.INDEX_NAME
		LEFT JOIN ALL_IND_EXPRESSIONS e ON e.INDEX_OWNER = ic.INDEX_OWNER AND e.INDEX_NAME = ic.INDEX_NAME AND e.COLUMN_POSITION = ic.COLUMN_POSITION
		WHERE i.TABLE_OWNER = :1 AND i.TABLE_NAME = :2 AND i.INDEX_TYPE <> 'LOB'
		ORDER BY CASE WHEN i.INDEX_NAME = :3 THEN 0 ELSE 1 END, i.INDEX_NAME, ic.COLUMN_POSITION`, t.Ref.Schema, t.Ref.Name, pkIndex)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for _, col := range t.Columns {
		columns[col.Name] = true
	}
	type entry struct {
		owner string
		ix    driver.Index
	}
	var list []*entry
	for rows.Next() {
		var owner, name, typ, uniq, col, desc, expr, ityp sql.NullString
		if err := rows.Scan(&owner, &name, &typ, &uniq, &col, &desc, &expr, &ityp); err != nil {
			return err
		}
		if n := len(list); n == 0 || list[n-1].ix.Name != name.String {
			it := indexTypes[typ.String]
			if it == "" {
				it = strings.ToLower(typ.String)
			}
			if strings.HasPrefix(ityp.String, "SPATIAL_INDEX") {
				it = "spatial"
			}
			list = append(list, &entry{owner: owner.String, ix: driver.Index{Name: name.String, Unique: uniq.String == "UNIQUE",
				Primary: pkIndex != "" && name.String == pkIndex, Type: it}})
		}
		ix := &list[len(list)-1].ix
		column, isDesc := col.String, desc.String == "DESC"
		// Keys on virtual columns come with the column's expression, keys
		// on hidden columns (SYS_NC...) are function-based.
		if e := strings.TrimSpace(expr.String); e != "" && !columns[column] {
			// Descending keys are stored as expressions naming the column.
			if m := quotedName.FindStringSubmatch(e); m != nil && isDesc {
				column = m[1]
			} else {
				column = e
			}
		}
		ix.Columns = append(ix.Columns, column)
		ix.Desc = append(ix.Desc, isDesc)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range list {
		e.ix.Definition = indexDDL(e.owner, e.ix, t, columns)
		if e.ix.Primary && cons.pk != "" {
			e.ix.Definition = constraintDDL(t, cons.pk, "PRIMARY KEY", t.PrimaryKey)
		}
		for _, u := range cons.unique {
			if u.index == e.ix.Name {
				e.ix.Definition = constraintDDL(t, u.name, "UNIQUE", u.columns)
			}
		}
		t.Indexes = append(t.Indexes, e.ix)
	}
	return nil
}

// constraintDDL is the definition of an index that enforces a primary key or
// unique constraint: the constraint itself, which DDL generation reads back
// (constraintOf) to drop and re-create the constraint rather than the index.
func constraintDDL(t *driver.Table, name, kind string, cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = quote(c)
	}
	return "ALTER TABLE " + qualify(t.Ref.Schema, t.Ref.Name) + " ADD CONSTRAINT " + quote(name) + " " + kind + " (" + strings.Join(q, ", ") + ")"
}

// indexDDL renders CREATE INDEX; key parts that are not plain columns are
// function-based expressions and are kept verbatim.
func indexDDL(owner string, ix driver.Index, t *driver.Table, columns map[string]bool) string {
	var parts []string
	for i, col := range ix.Columns {
		p := col
		if columns[col] {
			p = quote(col)
		}
		if i < len(ix.Desc) && ix.Desc[i] {
			p += " DESC"
		}
		parts = append(parts, p)
	}
	kw, suffix := "INDEX", ""
	switch {
	case ix.Unique:
		kw = "UNIQUE INDEX"
	case ix.Type == "bitmap":
		kw = "BITMAP INDEX"
	case ix.Type == "spatial":
		suffix = " INDEXTYPE IS MDSYS.SPATIAL_INDEX_V2"
	}
	return "CREATE " + kw + " " + qualify(owner, ix.Name) + " ON " + qualify(t.Ref.Schema, t.Ref.Name) + " (" + strings.Join(parts, ", ") + ")" + suffix
}

const fkQuery = `SELECT c.CONSTRAINT_NAME, c.OWNER, c.TABLE_NAME, cc.COLUMN_NAME, r.OWNER, r.TABLE_NAME, rc.COLUMN_NAME, c.DELETE_RULE
	FROM ALL_CONSTRAINTS c
	JOIN ALL_CONS_COLUMNS cc ON cc.OWNER = c.OWNER AND cc.CONSTRAINT_NAME = c.CONSTRAINT_NAME AND cc.TABLE_NAME = c.TABLE_NAME
	JOIN ALL_CONSTRAINTS r ON r.OWNER = c.R_OWNER AND r.CONSTRAINT_NAME = c.R_CONSTRAINT_NAME
	JOIN ALL_CONS_COLUMNS rc ON rc.OWNER = r.OWNER AND rc.CONSTRAINT_NAME = r.CONSTRAINT_NAME AND rc.TABLE_NAME = r.TABLE_NAME AND rc.POSITION = cc.POSITION
	WHERE c.CONSTRAINT_TYPE = 'R' AND %s
	ORDER BY c.OWNER, c.TABLE_NAME, c.CONSTRAINT_NAME, cc.POSITION`

func (c *conn) fks(ctx context.Context, where string, args ...any) ([]driver.ForeignKey, error) {
	rows, err := c.db.QueryContext(ctx, fmt.Sprintf(fkQuery, where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.ForeignKey
	for rows.Next() {
		var name, owner, table, col, rOwner, rTable, rCol, del string
		if err := rows.Scan(&name, &owner, &table, &col, &rOwner, &rTable, &rCol, &del); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].Name != name || out[n-1].Table.Schema != owner {
			out = append(out, driver.ForeignKey{Name: name, RefTable: driver.ObjectRef{Schema: rOwner, Name: rTable, Kind: "table"},
				OnDelete: del, Table: &driver.ObjectRef{Schema: owner, Name: table, Kind: "table"}})
		}
		fk := &out[len(out)-1]
		fk.Columns = append(fk.Columns, col)
		fk.RefColumns = append(fk.RefColumns, rCol)
	}
	return out, rows.Err()
}

func (c *conn) describeTriggers(ctx context.Context, t *driver.Table) {
	rows, err := c.db.QueryContext(ctx, `SELECT TRIGGER_NAME, TRIGGER_TYPE, TRIGGERING_EVENT, DESCRIPTION, WHEN_CLAUSE, TRIGGER_BODY
		FROM ALL_TRIGGERS WHERE TABLE_OWNER = :1 AND TABLE_NAME = :2 ORDER BY TRIGGER_NAME`, t.Ref.Schema, t.Ref.Name)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name, typ, event, desc, when, body sql.NullString
		if rows.Scan(&name, &typ, &event, &desc, &when, &body) != nil {
			continue
		}
		stmt := "CREATE OR REPLACE TRIGGER " + strings.TrimSpace(desc.String) + "\n"
		if when.String != "" {
			stmt += "WHEN (" + strings.TrimSpace(when.String) + ")\n"
		}
		stmt += strings.TrimSpace(body.String)
		t.Triggers = append(t.Triggers, driver.Trigger{Name: name.String, Timing: triggerTiming(typ.String), Event: event.String, Statement: stmt})
	}
}

func triggerTiming(typ string) string {
	for _, p := range []string{"INSTEAD OF", "BEFORE", "AFTER", "COMPOUND"} {
		if strings.HasPrefix(typ, p) {
			return p
		}
	}
	return typ
}

// metadataBudget bounds DBMS_METADATA in Describe, which also serves the
// data grid: on a busy server GET_DDL can take many seconds, and the
// reconstructed DDL is preferable to a stalled table.
const metadataBudget = 5 * time.Second

// describeDDL fills DDL (and the query of views) from DBMS_METADATA, falling
// back to reconstruction from the catalog when it fails (e.g. missing
// SELECT_CATALOG_ROLE for another user's objects) or is too slow.
func (c *conn) describeDDL(ctx context.Context, t *driver.Table, cons constraints) {
	s, n := t.Ref.Schema, t.Ref.Name
	mctx, cancel := context.WithTimeout(ctx, metadataBudget)
	defer cancel()
	switch t.Kind {
	case "view":
		var text sql.NullString
		_ = c.db.QueryRowContext(ctx, `SELECT TEXT FROM ALL_VIEWS WHERE OWNER = :1 AND VIEW_NAME = :2`, s, n).Scan(&text)
		t.Definition = strings.TrimSpace(text.String)
		if ddl, err := c.metadataDDL(mctx, "VIEW", s, n); err == nil {
			t.DDL = ddl
		} else if t.Definition != "" {
			t.DDL = "CREATE OR REPLACE VIEW " + qualify(s, n) + " AS\n" + t.Definition + ";"
		}
		for _, tr := range t.Triggers {
			t.DDL += "\n\n" + tr.Statement + "\n/"
		}
	case "materialized_view":
		var query sql.NullString
		_ = c.db.QueryRowContext(ctx, `SELECT QUERY FROM ALL_MVIEWS WHERE OWNER = :1 AND MVIEW_NAME = :2`, s, n).Scan(&query)
		t.Definition = strings.TrimSpace(query.String)
		if ddl, err := c.metadataDDL(mctx, "MATERIALIZED_VIEW", s, n); err == nil {
			t.DDL = ddl
		} else if t.Definition != "" {
			t.DDL = "CREATE MATERIALIZED VIEW " + qualify(s, n) + " AS\n" + t.Definition + ";"
		}
	default:
		ddl, err := c.metadataDDL(mctx, "TABLE", s, n)
		if err != nil {
			ddl = buildTableDDL(t, cons)
		}
		t.DDL = ddl + tableExtrasDDL(t, cons)
	}
}

// metadataDDL returns DBMS_METADATA.GET_DDL without storage clauses.
func (c *conn) metadataDDL(ctx context.Context, objType, schema, name string) (string, error) {
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer sc.Close()
	if _, err := sc.ExecContext(ctx, `BEGIN
		DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM, 'SEGMENT_ATTRIBUTES', FALSE);
		DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM, 'STORAGE', FALSE);
		DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM, 'SQLTERMINATOR', TRUE);
		DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM, 'PRETTY', TRUE);
	END;`); err != nil {
		return "", err
	}
	var ddl sql.NullString
	if err := sc.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL(:1, :2, :3) FROM DUAL`, objType, name, schema).Scan(&ddl); err != nil {
		return "", err
	}
	if strings.TrimSpace(ddl.String) == "" {
		return "", errors.New("no DDL returned")
	}
	return strings.TrimSpace(ddl.String), nil
}

// buildTableDDL reconstructs CREATE TABLE from the catalog.
func buildTableDDL(t *driver.Table, cons constraints) string {
	var lines []string
	for _, col := range t.Columns {
		l := "    " + quote(col.Name) + " " + col.Type
		switch {
		case col.Generated != "":
			l += " GENERATED ALWAYS AS (" + col.Generated + ") VIRTUAL"
		case col.Default != nil && strings.HasPrefix(*col.Default, "GENERATED "):
			l += " " + *col.Default
		case col.Default != nil:
			l += " DEFAULT " + *col.Default
		}
		if !col.Nullable {
			l += " NOT NULL"
		}
		lines = append(lines, l)
	}
	cols := func(names []string) string {
		q := make([]string, len(names))
		for i, n := range names {
			q[i] = quote(n)
		}
		return strings.Join(q, ", ")
	}
	if len(t.PrimaryKey) > 0 {
		lines = append(lines, "    CONSTRAINT "+quote(cons.pk)+" PRIMARY KEY ("+cols(t.PrimaryKey)+")")
	}
	for _, u := range cons.unique {
		lines = append(lines, "    CONSTRAINT "+quote(u.name)+" UNIQUE ("+cols(u.columns)+")")
	}
	for _, ch := range t.Checks {
		lines = append(lines, "    CONSTRAINT "+quote(ch.Name)+" CHECK ("+ch.Expression+")")
	}
	for _, fk := range t.ForeignKeys {
		l := "    CONSTRAINT " + quote(fk.Name) + " FOREIGN KEY (" + cols(fk.Columns) + ") REFERENCES " +
			qualify(fk.RefTable.Schema, fk.RefTable.Name) + " (" + cols(fk.RefColumns) + ")"
		if fk.OnDelete != "" && fk.OnDelete != "NO ACTION" {
			l += " ON DELETE " + fk.OnDelete
		}
		lines = append(lines, l)
	}
	create := "CREATE TABLE "
	if t.Options["temporary"] == "yes" {
		create = "CREATE GLOBAL TEMPORARY TABLE "
	}
	return create + qualify(t.Ref.Schema, t.Ref.Name) + " (\n" + strings.Join(lines, ",\n") + "\n);"
}

// tableExtrasDDL adds what CREATE TABLE does not cover: standalone indexes,
// comments and triggers.
func tableExtrasDDL(t *driver.Table, cons constraints) string {
	var b strings.Builder
	name := qualify(t.Ref.Schema, t.Ref.Name)
	for _, ix := range t.Indexes {
		if !ix.Primary && !cons.indexes[ix.Name] && ix.Definition != "" {
			b.WriteString("\n" + ix.Definition + ";")
		}
	}
	if t.Comment != "" {
		b.WriteString("\n\nCOMMENT ON TABLE " + name + " IS " + literal(t.Comment) + ";")
	}
	for _, col := range t.Columns {
		if col.Comment != "" {
			b.WriteString("\nCOMMENT ON COLUMN " + name + "." + quote(col.Name) + " IS " + literal(col.Comment) + ";")
		}
	}
	for _, tr := range t.Triggers {
		b.WriteString("\n\n" + tr.Statement + "\n/")
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n" + b.String()
}

// sourceTypes lists the ALL_SOURCE types that make up each code object.
var sourceTypes = map[string][2]string{
	"procedure": {"PROCEDURE", "PROCEDURE"}, "function": {"FUNCTION", "FUNCTION"}, "trigger": {"TRIGGER", "TRIGGER"},
	"package": {"PACKAGE", "PACKAGE BODY"}, "type": {"TYPE", "TYPE BODY"},
}

// Definition implements driver.Definer.
func (c *conn) Definition(ctx context.Context, ref driver.ObjectRef) (string, error) {
	schema := c.schema(ref.Schema)
	switch ref.Kind {
	case "table", "view", "materialized_view":
		t, err := c.Describe(ctx, ref)
		if err != nil {
			return "", err
		}
		return t.DDL, nil
	case "procedure", "function", "package", "type", "trigger":
		return c.sourceDDL(ctx, ref.Kind, schema, ref.Name)
	case "sequence":
		if ddl, err := c.metadataDDL(ctx, "SEQUENCE", schema, ref.Name); err == nil {
			return ddl, nil
		}
		return c.sequenceDDL(ctx, schema, ref.Name)
	case "synonym":
		var owner, table, link sql.NullString
		err := c.db.QueryRowContext(ctx, `SELECT TABLE_OWNER, TABLE_NAME, DB_LINK FROM ALL_SYNONYMS WHERE OWNER = :1 AND SYNONYM_NAME = :2`,
			schema, ref.Name).Scan(&owner, &table, &link)
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("synonym %s.%s not found", schema, ref.Name)
		}
		if err != nil {
			return "", cleanErr(err)
		}
		return "CREATE OR REPLACE SYNONYM " + qualify(schema, ref.Name) + " FOR " + synonymTarget(owner.String, table.String, link.String) + ";", nil
	}
	return "", driver.ErrNotSupported
}

// sourceDDL rebuilds CREATE OR REPLACE statements from ALL_SOURCE, which
// shows the source of objects the user owns or may execute.
func (c *conn) sourceDDL(ctx context.Context, kind, schema, name string) (string, error) {
	st := sourceTypes[kind]
	rows, err := c.db.QueryContext(ctx, `SELECT TYPE, TEXT FROM ALL_SOURCE WHERE OWNER = :1 AND NAME = :2 AND TYPE IN (:3, :4)
		ORDER BY TYPE, LINE`, schema, name, st[0], st[1])
	if err != nil {
		return "", cleanErr(err)
	}
	defer rows.Close()
	var parts []string
	var cur strings.Builder
	last := ""
	flush := func() {
		if cur.Len() > 0 {
			parts = append(parts, "CREATE OR REPLACE "+strings.TrimRight(cur.String(), " \t\r\n")+"\n/")
			cur.Reset()
		}
	}
	for rows.Next() {
		var typ, text sql.NullString
		if err := rows.Scan(&typ, &text); err != nil {
			return "", err
		}
		if typ.String != last {
			flush()
			last = typ.String
		}
		cur.WriteString(text.String)
	}
	if err := rows.Err(); err != nil {
		return "", cleanErr(err)
	}
	flush()
	if len(parts) > 0 {
		return strings.Join(parts, "\n\n"), nil
	}
	if ddl, err := c.metadataDDL(ctx, st[0], schema, name); err == nil {
		return ddl, nil
	}
	return "", fmt.Errorf("the source of %s.%s is not visible to this user", schema, name)
}

func (c *conn) sequenceDDL(ctx context.Context, schema, name string) (string, error) {
	var min, max, inc, cache, last, cycle, order sql.NullString
	err := c.db.QueryRowContext(ctx, `SELECT TO_CHAR(MIN_VALUE), TO_CHAR(MAX_VALUE), TO_CHAR(INCREMENT_BY), TO_CHAR(CACHE_SIZE),
		TO_CHAR(LAST_NUMBER), CYCLE_FLAG, ORDER_FLAG FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER = :1 AND SEQUENCE_NAME = :2`, schema, name).
		Scan(&min, &max, &inc, &cache, &last, &cycle, &order)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("sequence %s.%s not found", schema, name)
	}
	if err != nil {
		return "", cleanErr(err)
	}
	ddl := "CREATE SEQUENCE " + qualify(schema, name) + " START WITH " + last.String + " INCREMENT BY " + inc.String +
		" MINVALUE " + min.String + " MAXVALUE " + max.String
	if n, _ := strconv.Atoi(cache.String); n > 0 {
		ddl += " CACHE " + cache.String
	} else {
		ddl += " NOCACHE"
	}
	if cycle.String == "Y" {
		ddl += " CYCLE"
	}
	if order.String == "Y" {
		ddl += " ORDER"
	}
	return ddl + ";", nil
}

// CatalogColumns implements driver.Catalog.
func (c *conn) CatalogColumns(ctx context.Context, s driver.Scope) ([]driver.CatalogTable, error) {
	schema := c.schema(s.Schema)
	kinds := map[string]string{} // views and materialized views; the rest are tables
	for q, kind := range map[string]string{
		`SELECT VIEW_NAME FROM ALL_VIEWS WHERE OWNER = :1`:   "view",
		`SELECT MVIEW_NAME FROM ALL_MVIEWS WHERE OWNER = :1`: "materialized_view",
	} {
		names, _ := sqlbase.Strings(ctx, c.db, q, schema)
		for _, n := range names {
			kinds[n] = kind
		}
	}
	pk := map[string]bool{}
	if rows, err := c.db.QueryContext(ctx, `SELECT cc.TABLE_NAME, cc.COLUMN_NAME FROM ALL_CONSTRAINTS c
		JOIN ALL_CONS_COLUMNS cc ON cc.OWNER = c.OWNER AND cc.CONSTRAINT_NAME = c.CONSTRAINT_NAME AND cc.TABLE_NAME = c.TABLE_NAME
		WHERE c.OWNER = :1 AND c.CONSTRAINT_TYPE = 'P'`, schema); err == nil {
		for rows.Next() {
			var t, col string
			if rows.Scan(&t, &col) == nil {
				pk[t+"."+col] = true
			}
		}
		rows.Close()
	}
	rows, err := c.db.QueryContext(ctx, `SELECT c.TABLE_NAME, c.COLUMN_NAME, c.DATA_TYPE, c.DATA_TYPE_OWNER, c.DATA_TYPE_MOD, c.DATA_LENGTH,
		c.DATA_PRECISION, c.DATA_SCALE, c.CHAR_LENGTH, c.CHAR_USED
		FROM ALL_TAB_COLUMNS c WHERE c.OWNER = :1 AND (c.TABLE_NAME IN (SELECT TABLE_NAME FROM ALL_TABLES WHERE OWNER = :2
			AND NESTED = 'NO' AND SECONDARY = 'N' AND DROPPED = 'NO' AND (IOT_TYPE IS NULL OR IOT_TYPE = 'IOT'))
		OR c.TABLE_NAME IN (SELECT VIEW_NAME FROM ALL_VIEWS WHERE OWNER = :3))
		ORDER BY c.TABLE_NAME, c.COLUMN_ID`, schema, schema, schema)
	if err != nil {
		return nil, cleanErr(err)
	}
	defer rows.Close()
	var out []driver.CatalogTable
	idx := map[string]int{}
	for rows.Next() {
		var m colMeta
		var table, col string
		var owner, mod, charUsed sql.NullString
		if err := rows.Scan(&table, &col, &m.dataType, &owner, &mod, &m.length, &m.precision, &m.scale, &m.charLength, &charUsed); err != nil {
			return nil, err
		}
		m.typeOwner, m.typeMod, m.charUsed = owner.String, mod.String, charUsed.String
		i, ok := idx[table]
		if !ok {
			kind := kinds[table]
			if kind == "" {
				kind = "table"
			}
			out = append(out, driver.CatalogTable{Schema: schema, Name: table, Kind: kind})
			i = len(out) - 1
			idx[table] = i
		}
		out[i].Columns = append(out[i].Columns, driver.CatalogColumn{Name: col, Type: columnType(m), PK: pk[table+"."+col]})
	}
	if err := rows.Err(); err != nil {
		return nil, cleanErr(err)
	}
	if all, err := c.fks(ctx, "c.OWNER = :1", schema); err == nil {
		for _, fk := range all {
			if i, ok := idx[fk.Table.Name]; ok {
				out[i].FKs = append(out[i].FKs, fk)
			}
		}
	}
	return out, nil
}
