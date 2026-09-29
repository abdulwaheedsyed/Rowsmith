package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
)

// ddlInfo holds catalog details that only matter for scripting a table.
type ddlInfo struct {
	cols    map[string]colDDL
	indexes []indexDDL
}

type colDDL struct {
	computed, persisted, rowGUID bool
	identity                     string // IDENTITY(seed,increment)
	defName, defExpr             string // default constraint, as stored
	typeRef                      string // declared type, schema-qualified for alias types
}

type indexDDL struct {
	driver.Index
	included   []string
	constraint bool // PRIMARY KEY or UNIQUE constraint rather than an index
}

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	if ref.Name == "" {
		return nil, errors.New("table name is required")
	}
	db, schema := c.dbName(ref.Database), schemaName(ref.Schema)
	t := &driver.Table{Ref: driver.ObjectRef{Database: db, Schema: schema, Name: ref.Name}, Options: map[string]string{}}
	var id int64
	var typ string
	var nrows, size sql.NullInt64
	var dbColl sql.NullString
	err := c.db.QueryRowContext(ctx, `SELECT o.object_id, RTRIM(o.type),
		(SELECT SUM(p.rows) FROM `+sysView(db, "partitions")+` p WHERE p.object_id = o.object_id AND p.index_id IN (0, 1)),
		(SELECT SUM(a.total_pages) FROM `+sysView(db, "partitions")+` p
			JOIN `+sysView(db, "allocation_units")+` a ON a.container_id = p.partition_id WHERE p.object_id = o.object_id) * 8192,
		COALESCE(CAST(ep.value AS nvarchar(max)), ''), (SELECT d.collation_name FROM sys.databases d WHERE d.name = @p3)
		FROM `+sysView(db, "objects")+` o JOIN `+sysView(db, "schemas")+` s ON s.schema_id = o.schema_id
		LEFT JOIN `+sysView(db, "extended_properties")+` ep ON ep.class = 1 AND ep.major_id = o.object_id AND ep.minor_id = 0 AND ep.name = N'MS_Description'
		WHERE s.name = @p1 AND o.name = @p2 AND o.type IN ('U', 'V')`, schema, ref.Name, db).
		Scan(&id, &typ, &nrows, &size, &t.Comment, &dbColl)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("table %s.%s not found in database %s", schema, ref.Name, db)
	}
	if err != nil {
		return nil, mapError(err)
	}
	t.Kind = objectKind(typ)
	t.Ref.Kind = t.Kind
	x := &ddlInfo{cols: map[string]colDDL{}}
	if err := c.describeColumns(ctx, db, id, t, x); err != nil {
		return nil, mapError(err)
	}
	c.describeTriggers(ctx, db, id, t)
	if t.Kind == "view" {
		var def sql.NullString
		_ = c.db.QueryRowContext(ctx, `SELECT definition FROM `+sysView(db, "sql_modules")+` WHERE object_id = @p1`, id).Scan(&def)
		t.Definition, t.DDL = def.String, def.String
		return t, nil
	}
	t.RowEstimate, t.Size = sqlbase.NullInt(nrows), sqlbase.NullInt(size)
	if err := c.describeIndexes(ctx, db, id, t, x); err != nil {
		return nil, mapError(err)
	}
	if t.ForeignKeys, err = c.fks(ctx, db, "fk.parent_object_id = @p1", id); err != nil {
		return nil, mapError(err)
	}
	if t.Referenced, err = c.fks(ctx, db, "fk.referenced_object_id = @p1", id); err != nil {
		return nil, mapError(err)
	}
	c.describeChecks(ctx, db, id, t)
	sqlbase.ChooseRowKey(t, false)
	t.DDL = tableDDL(t, x, dbColl.String)
	return t, nil
}

func (c *conn) describeColumns(ctx context.Context, db string, id int64, t *driver.Table, x *ddlInfo) error {
	rows, err := c.db.QueryContext(ctx, `SELECT c.name, ty.name, ts.name, ty.is_user_defined, ty.is_assembly_type, COALESCE(bt.name, ''),
		c.max_length, c.precision, c.scale, COALESCE(c.is_nullable, 1), c.is_identity, c.is_computed, c.is_rowguidcol,
		COALESCE(c.collation_name, ''), COALESCE(dc.name, ''), COALESCE(dc.definition, ''),
		COALESCE(cc.definition, ''), COALESCE(cc.is_persisted, 0),
		CAST(ic.seed_value AS nvarchar(40)), CAST(ic.increment_value AS nvarchar(40)),
		COALESCE(CAST(ep.value AS nvarchar(max)), '')
		FROM `+sysView(db, "columns")+` c
		JOIN `+sysView(db, "types")+` ty ON ty.user_type_id = c.user_type_id
		JOIN `+sysView(db, "schemas")+` ts ON ts.schema_id = ty.schema_id
		LEFT JOIN `+sysView(db, "types")+` bt ON bt.user_type_id = c.system_type_id
		LEFT JOIN `+sysView(db, "default_constraints")+` dc ON dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id
		LEFT JOIN `+sysView(db, "computed_columns")+` cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
		LEFT JOIN `+sysView(db, "identity_columns")+` ic ON ic.object_id = c.object_id AND ic.column_id = c.column_id
		LEFT JOIN `+sysView(db, "extended_properties")+` ep ON ep.class = 1 AND ep.major_id = c.object_id AND ep.minor_id = c.column_id AND ep.name = N'MS_Description'
		WHERE c.object_id = @p1 ORDER BY c.column_id`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var col driver.Column
		var typName, typSchema, baseName, defName, defExpr, computed string
		var userDefined, assembly, identity, isComputed, rowGUID, persisted bool
		var maxLen, prec, scale int
		var seed, inc sql.NullString
		if err := rows.Scan(&col.Name, &typName, &typSchema, &userDefined, &assembly, &baseName, &maxLen, &prec, &scale,
			&col.Nullable, &identity, &isComputed, &rowGUID, &col.Collation, &defName, &defExpr, &computed, &persisted,
			&seed, &inc, &col.Comment); err != nil {
			return err
		}
		e := colDDL{computed: isComputed, persisted: persisted, rowGUID: rowGUID, defName: defName, defExpr: defExpr}
		finishColumn(&col, typName, baseName, userDefined, assembly, maxLen, prec, scale)
		e.typeRef = col.Type
		if userDefined && typSchema != "dbo" && typSchema != "sys" {
			e.typeRef = quote(typSchema) + "." + quote(typName)
		}
		if identity {
			col.AutoIncrement = true
			e.identity = "IDENTITY(" + seed.String + "," + inc.String + ")"
		}
		switch {
		case isComputed:
			col.Generated = unwrapParens(computed)
		case col.BaseType == "timestamp":
			col.Generated = "row version, set by the server"
		case defExpr != "":
			d := unwrapParens(defExpr)
			col.Default = &d
		}
		x.cols[col.Name] = e
		t.Columns = append(t.Columns, col)
	}
	return rows.Err()
}

// finishColumn derives the declared type string, base type and value kind.
func finishColumn(col *driver.Column, typName, baseName string, userDefined, assembly bool, maxLen, prec, scale int) {
	base := typName
	if userDefined && !assembly && baseName != "" {
		base = baseName // alias type
	}
	col.BaseType = base
	col.Type = typName
	if !userDefined {
		col.Type = typeString(typName, maxLen, prec, scale)
	}
	col.Kind = kindOf(base, maxLen == -1)
	if userDefined && assembly {
		col.Kind = driver.KindOther
	}
	switch base {
	case "char", "varchar", "binary", "varbinary", "nchar", "nvarchar":
		if maxLen > 0 {
			n := int64(maxLen)
			if base == "nchar" || base == "nvarchar" {
				n /= 2
			}
			col.Length = &n
		}
	case "decimal", "numeric":
		p, s := int64(prec), int64(scale)
		col.Precision, col.Scale = &p, &s
	case "datetime2", "time", "datetimeoffset":
		s := int64(scale)
		col.Scale = &s
	case "geometry", "geography":
		col.GeometryType = base
		if base == "geography" {
			col.SRID = 4326 // SQL Server's default; the SRID is stored per value
		}
	}
}

// typeString renders a system type with its length, precision or scale.
func typeString(name string, maxLen, prec, scale int) string {
	switch name {
	case "char", "varchar", "binary", "varbinary", "nchar", "nvarchar":
		if maxLen == -1 {
			return name + "(max)"
		}
		if name == "nchar" || name == "nvarchar" {
			maxLen /= 2
		}
		return name + "(" + strconv.Itoa(maxLen) + ")"
	case "decimal", "numeric":
		return name + "(" + strconv.Itoa(prec) + "," + strconv.Itoa(scale) + ")"
	case "datetime2", "time", "datetimeoffset":
		return name + "(" + strconv.Itoa(scale) + ")"
	case "timestamp":
		return "rowversion"
	}
	return name
}

// unwrapParens strips the redundant parentheses SQL Server stores around
// defaults and expressions: "((0))" becomes "0".
func unwrapParens(s string) string {
	s = strings.TrimSpace(s)
	for len(s) >= 2 && s[0] == '(' && closingParen(s) == len(s)-1 {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// closingParen returns the index of the parenthesis closing s[0], skipping
// string literals and bracketed identifiers, or -1.
func closingParen(s string) int {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0 // a doubled quote reopens on the next byte
			}
		case ch == '\'':
			quote = '\''
		case ch == '[':
			quote = ']'
		case ch == '(':
			depth++
		case ch == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (c *conn) describeIndexes(ctx context.Context, db string, id int64, t *driver.Table, x *ddlInfo) error {
	rows, err := c.db.QueryContext(ctx, `SELECT i.index_id, i.name, i.type_desc, i.is_unique, i.is_primary_key, i.is_unique_constraint,
		COALESCE(i.filter_definition, ''), ic.key_ordinal, ic.is_descending_key, ic.is_included_column, c.name
		FROM `+sysView(db, "indexes")+` i
		JOIN `+sysView(db, "index_columns")+` ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
		JOIN `+sysView(db, "columns")+` c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		WHERE i.object_id = @p1 AND i.type > 0 AND i.is_hypothetical = 0
		ORDER BY i.is_primary_key DESC, i.name, ic.key_ordinal, ic.index_column_id`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := map[int]int{}
	for rows.Next() {
		var ixID, keyOrdinal int
		var name, typeDesc, filter, col string
		var unique, primary, uniqueConstraint, desc, included bool
		if err := rows.Scan(&ixID, &name, &typeDesc, &unique, &primary, &uniqueConstraint, &filter, &keyOrdinal, &desc, &included, &col); err != nil {
			return err
		}
		i, ok := byID[ixID]
		if !ok {
			x.indexes = append(x.indexes, indexDDL{Index: driver.Index{Name: name, Unique: unique, Primary: primary,
				Type: strings.ToLower(typeDesc), Where: unwrapParens(filter)}, constraint: primary || uniqueConstraint})
			i = len(x.indexes) - 1
			byID[ixID] = i
		}
		ix := &x.indexes[i]
		switch {
		case included:
			ix.included = append(ix.included, col)
		case keyOrdinal > 0 || strings.Contains(ix.Type, "columnstore"):
			ix.Columns = append(ix.Columns, col)
			ix.Desc = append(ix.Desc, desc)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	table := qualify("", t.Ref.Schema, t.Ref.Name)
	for i := range x.indexes {
		ix := &x.indexes[i]
		ix.Definition = indexDefinition(table, *ix)
		if ix.Primary {
			t.PrimaryKey = ix.Columns
			for j := range t.Columns {
				for _, k := range ix.Columns {
					if t.Columns[j].Name == k {
						t.Columns[j].PrimaryKey = true
					}
				}
			}
		}
		t.Indexes = append(t.Indexes, ix.Index)
	}
	return nil
}

func keyList(cols []string, desc []bool) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = quote(c) + " ASC"
		if i < len(desc) && desc[i] {
			parts[i] = quote(c) + " DESC"
		}
	}
	return strings.Join(parts, ", ")
}

func nameList(cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = quote(c)
	}
	return strings.Join(parts, ", ")
}

// constraintClause renders a PRIMARY KEY or UNIQUE constraint body.
func constraintClause(ix indexDDL) string {
	kw := "UNIQUE "
	if ix.Primary {
		kw = "PRIMARY KEY "
	}
	return kw + strings.ToUpper(ix.Type) + " (" + keyList(ix.Columns, ix.Desc) + ")"
}

// indexDefinition renders the statement that recreates an index.
func indexDefinition(table string, ix indexDDL) string {
	if ix.constraint {
		return "ALTER TABLE " + table + " ADD CONSTRAINT " + quote(ix.Name) + " " + constraintClause(ix)
	}
	var s string
	switch ix.Type {
	case "clustered", "nonclustered":
		s = "CREATE "
		if ix.Unique {
			s += "UNIQUE "
		}
		s += strings.ToUpper(ix.Type) + " INDEX " + quote(ix.Name) + " ON " + table + " (" + keyList(ix.Columns, ix.Desc) + ")"
		if len(ix.included) > 0 {
			s += " INCLUDE (" + nameList(ix.included) + ")"
		}
	case "clustered columnstore":
		return "CREATE CLUSTERED COLUMNSTORE INDEX " + quote(ix.Name) + " ON " + table
	case "nonclustered columnstore":
		s = "CREATE NONCLUSTERED COLUMNSTORE INDEX " + quote(ix.Name) + " ON " + table + " (" + nameList(ix.Columns) + ")"
	default:
		// XML, spatial and hash indexes need options the catalog query does not read.
		return "-- " + ix.Type + " index " + quote(ix.Name) + " on (" + nameList(ix.Columns) + ") is not scripted"
	}
	if ix.Where != "" {
		s += " WHERE " + ix.Where
	}
	return s
}

var fkRules = map[string]string{"NO_ACTION": "NO ACTION", "CASCADE": "CASCADE", "SET_NULL": "SET NULL", "SET_DEFAULT": "SET DEFAULT"}

func (c *conn) fks(ctx context.Context, db, where string, arg any) ([]driver.ForeignKey, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT fk.name, ss.name, st.name, rs.name, rt.name, pc.name, rc.name,
		fk.update_referential_action_desc, fk.delete_referential_action_desc
		FROM `+sysView(db, "foreign_keys")+` fk
		JOIN `+sysView(db, "foreign_key_columns")+` fkc ON fkc.constraint_object_id = fk.object_id
		JOIN `+sysView(db, "objects")+` st ON st.object_id = fk.parent_object_id
		JOIN `+sysView(db, "schemas")+` ss ON ss.schema_id = st.schema_id
		JOIN `+sysView(db, "objects")+` rt ON rt.object_id = fk.referenced_object_id
		JOIN `+sysView(db, "schemas")+` rs ON rs.schema_id = rt.schema_id
		JOIN `+sysView(db, "columns")+` pc ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
		JOIN `+sysView(db, "columns")+` rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
		WHERE `+where+` ORDER BY ss.name, st.name, fk.name, fkc.constraint_column_id`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.ForeignKey
	idx := map[string]int{}
	for rows.Next() {
		var name, ss, st, rs, rt, col, rcol, upd, del string
		if err := rows.Scan(&name, &ss, &st, &rs, &rt, &col, &rcol, &upd, &del); err != nil {
			return nil, err
		}
		key := ss + "\x00" + st + "\x00" + name
		i, ok := idx[key]
		if !ok {
			out = append(out, driver.ForeignKey{Name: name, RefTable: driver.ObjectRef{Database: db, Schema: rs, Name: rt, Kind: "table"},
				OnUpdate: fkRules[upd], OnDelete: fkRules[del], Table: &driver.ObjectRef{Database: db, Schema: ss, Name: st, Kind: "table"}})
			i = len(out) - 1
			idx[key] = i
		}
		out[i].Columns = append(out[i].Columns, col)
		out[i].RefColumns = append(out[i].RefColumns, rcol)
	}
	return out, rows.Err()
}

func (c *conn) describeChecks(ctx context.Context, db string, id int64, t *driver.Table) {
	rows, err := c.db.QueryContext(ctx, `SELECT name, definition FROM `+sysView(db, "check_constraints")+`
		WHERE parent_object_id = @p1 ORDER BY name`, id)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ch driver.Check
		if rows.Scan(&ch.Name, &ch.Expression) == nil {
			ch.Expression = unwrapParens(ch.Expression)
			t.Checks = append(t.Checks, ch)
		}
	}
}

func (c *conn) describeTriggers(ctx context.Context, db string, id int64, t *driver.Table) {
	rows, err := c.db.QueryContext(ctx, `SELECT tr.name, tr.is_instead_of_trigger,
		CASE WHEN EXISTS (SELECT 1 FROM `+sysView(db, "trigger_events")+` e WHERE e.object_id = tr.object_id AND e.type = 1) THEN 1 ELSE 0 END,
		CASE WHEN EXISTS (SELECT 1 FROM `+sysView(db, "trigger_events")+` e WHERE e.object_id = tr.object_id AND e.type = 2) THEN 1 ELSE 0 END,
		CASE WHEN EXISTS (SELECT 1 FROM `+sysView(db, "trigger_events")+` e WHERE e.object_id = tr.object_id AND e.type = 3) THEN 1 ELSE 0 END,
		COALESCE(m.definition, '')
		FROM `+sysView(db, "triggers")+` tr LEFT JOIN `+sysView(db, "sql_modules")+` m ON m.object_id = tr.object_id
		WHERE tr.parent_id = @p1 ORDER BY tr.name`, id)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var tr driver.Trigger
		var instead, ins, upd, del bool
		if rows.Scan(&tr.Name, &instead, &ins, &upd, &del, &tr.Statement) == nil {
			tr.Timing, tr.Event = triggerTiming(instead, ins, upd, del)
			t.Triggers = append(t.Triggers, tr)
		}
	}
}

// tableDDL reconstructs CREATE TABLE with its constraints, indexes, comments
// and triggers. Triggers must start their own batch, hence the GO lines.
func tableDDL(t *driver.Table, x *ddlInfo, dbCollation string) string {
	name := qualify("", t.Ref.Schema, t.Ref.Name)
	lines := columnLines(t.Columns, x, dbCollation)
	for _, ix := range x.indexes {
		if ix.constraint {
			lines = append(lines, "    CONSTRAINT "+quote(ix.Name)+" "+constraintClause(ix))
		}
	}
	for _, ch := range t.Checks {
		lines = append(lines, "    CONSTRAINT "+quote(ch.Name)+" CHECK ("+ch.Expression+")")
	}
	for _, fk := range t.ForeignKeys {
		l := "    CONSTRAINT " + quote(fk.Name) + " FOREIGN KEY (" + nameList(fk.Columns) + ") REFERENCES " +
			qualify("", fk.RefTable.Schema, fk.RefTable.Name) + " (" + nameList(fk.RefColumns) + ")"
		if fk.OnDelete != "" && fk.OnDelete != "NO ACTION" {
			l += " ON DELETE " + fk.OnDelete
		}
		if fk.OnUpdate != "" && fk.OnUpdate != "NO ACTION" {
			l += " ON UPDATE " + fk.OnUpdate
		}
		lines = append(lines, l)
	}
	var b strings.Builder
	b.WriteString("CREATE TABLE " + name + " (\n" + strings.Join(lines, ",\n") + "\n);")
	for _, ix := range x.indexes {
		if !ix.constraint {
			b.WriteString("\n" + ix.Definition + ";")
		}
	}
	comment := func(text string, level2 ...string) {
		b.WriteString("\nEXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = " + literal(text) +
			", @level0type = N'SCHEMA', @level0name = " + literal(t.Ref.Schema) + ", @level1type = N'TABLE', @level1name = " + literal(t.Ref.Name))
		if len(level2) == 1 {
			b.WriteString(", @level2type = N'COLUMN', @level2name = " + literal(level2[0]))
		}
		b.WriteString(";")
	}
	if t.Comment != "" {
		comment(t.Comment)
	}
	for _, c := range t.Columns {
		if c.Comment != "" {
			comment(c.Comment, c.Name)
		}
	}
	for _, tr := range t.Triggers {
		if tr.Statement != "" {
			b.WriteString("\nGO\n" + strings.TrimSpace(tr.Statement) + "\nGO")
		}
	}
	return b.String()
}

// columnLines renders column definitions for CREATE TABLE and CREATE TYPE.
func columnLines(cols []driver.Column, x *ddlInfo, dbCollation string) []string {
	var lines []string
	for _, c := range cols {
		e := x.cols[c.Name]
		l := "    " + quote(c.Name) + " "
		if e.computed {
			l += "AS (" + c.Generated + ")"
			if e.persisted {
				l += " PERSISTED"
				if !c.Nullable {
					l += " NOT NULL"
				}
			}
			lines = append(lines, l)
			continue
		}
		l += e.typeRef
		if c.Collation != "" && c.Collation != dbCollation {
			l += " COLLATE " + c.Collation
		}
		if e.identity != "" {
			l += " " + e.identity
		}
		if e.rowGUID {
			l += " ROWGUIDCOL"
		}
		if c.Nullable {
			l += " NULL"
		} else {
			l += " NOT NULL"
		}
		if e.defExpr != "" {
			if e.defName != "" {
				l += " CONSTRAINT " + quote(e.defName)
			}
			l += " DEFAULT " + e.defExpr
		}
		lines = append(lines, l)
	}
	return lines
}

// Definition implements driver.Definer. Modules (views, procedures,
// functions, triggers) come from OBJECT_DEFINITION, evaluated inside their
// database; tables, sequences, synonyms and types are scripted.
func (c *conn) Definition(ctx context.Context, ref driver.ObjectRef) (string, error) {
	db, schema := c.dbName(ref.Database), schemaName(ref.Schema)
	switch ref.Kind {
	case "table":
		t, err := c.Describe(ctx, ref)
		if err != nil {
			return "", err
		}
		return t.DDL, nil
	case "sequence":
		return c.sequenceDDL(ctx, db, schema, ref.Name)
	case "synonym":
		var base string
		err := c.db.QueryRowContext(ctx, `SELECT sn.base_object_name FROM `+sysView(db, "synonyms")+` sn
			JOIN `+sysView(db, "schemas")+` s ON s.schema_id = sn.schema_id WHERE s.name = @p1 AND sn.name = @p2`, schema, ref.Name).Scan(&base)
		if err != nil {
			return "", notFound(err, schema, ref.Name)
		}
		return "CREATE SYNONYM " + qualify("", schema, ref.Name) + " FOR " + base + ";", nil
	case "type":
		return c.typeDDL(ctx, db, schema, ref.Name)
	}
	var def sql.NullString
	err := c.db.QueryRowContext(ctx, `EXEC `+quote(db)+`.sys.sp_executesql N'SELECT OBJECT_DEFINITION(OBJECT_ID(@name))',
		N'@name nvarchar(776)', @name = @p1`, qualify("", schema, ref.Name)).Scan(&def)
	if err != nil {
		return "", mapError(err)
	}
	if !def.Valid {
		return "", fmt.Errorf("no definition for %s.%s: it does not exist, is encrypted, or needs VIEW DEFINITION permission", schema, ref.Name)
	}
	return strings.TrimSpace(def.String), nil
}

func notFound(err error, schema, name string) error {
	if err == sql.ErrNoRows {
		return fmt.Errorf("%s.%s not found", schema, name)
	}
	return mapError(err)
}

func (c *conn) sequenceDDL(ctx context.Context, db, schema, name string) (string, error) {
	var typ, start, inc, min, max string
	var prec, scale, cacheSize sql.NullInt64
	var cycle, cached bool
	err := c.db.QueryRowContext(ctx, `SELECT ty.name, CAST(sq.start_value AS nvarchar(40)), CAST(sq.increment AS nvarchar(40)),
		CAST(sq.minimum_value AS nvarchar(40)), CAST(sq.maximum_value AS nvarchar(40)), sq.is_cycling, sq.is_cached, sq.cache_size,
		sq.precision, sq.scale
		FROM `+sysView(db, "sequences")+` sq JOIN `+sysView(db, "schemas")+` s ON s.schema_id = sq.schema_id
		JOIN `+sysView(db, "types")+` ty ON ty.user_type_id = sq.user_type_id
		WHERE s.name = @p1 AND sq.name = @p2`, schema, name).Scan(&typ, &start, &inc, &min, &max, &cycle, &cached, &cacheSize, &prec, &scale)
	if err != nil {
		return "", notFound(err, schema, name)
	}
	s := fmt.Sprintf("CREATE SEQUENCE %s AS %s START WITH %s INCREMENT BY %s MINVALUE %s MAXVALUE %s",
		qualify("", schema, name), typeString(typ, 0, int(prec.Int64), int(scale.Int64)), start, inc, min, max)
	if cycle {
		s += " CYCLE"
	} else {
		s += " NO CYCLE"
	}
	switch {
	case !cached:
		s += " NO CACHE"
	case cacheSize.Valid:
		s += " CACHE " + strconv.FormatInt(cacheSize.Int64, 10)
	}
	return s + ";", nil
}

func (c *conn) typeDDL(ctx context.Context, db, schema, name string) (string, error) {
	var table, assembly, nullable bool
	var base string
	var maxLen, prec, scale int
	var tableID sql.NullInt64
	var dbColl sql.NullString
	err := c.db.QueryRowContext(ctx, `SELECT t.is_table_type, t.is_assembly_type, COALESCE(bt.name, ''), t.max_length, t.precision, t.scale,
		t.is_nullable, tt.type_table_object_id, (SELECT d.collation_name FROM sys.databases d WHERE d.name = @p3)
		FROM `+sysView(db, "types")+` t JOIN `+sysView(db, "schemas")+` s ON s.schema_id = t.schema_id
		LEFT JOIN `+sysView(db, "types")+` bt ON bt.user_type_id = t.system_type_id
		LEFT JOIN `+sysView(db, "table_types")+` tt ON tt.user_type_id = t.user_type_id
		WHERE t.is_user_defined = 1 AND s.name = @p1 AND t.name = @p2`, schema, name, db).
		Scan(&table, &assembly, &base, &maxLen, &prec, &scale, &nullable, &tableID, &dbColl)
	if err != nil {
		return "", notFound(err, schema, name)
	}
	full := qualify("", schema, name)
	switch {
	case assembly:
		return "", driver.ErrNotSupported // CLR types come from assemblies
	case table:
		t := &driver.Table{Ref: driver.ObjectRef{Database: db, Schema: schema, Name: name}}
		x := &ddlInfo{cols: map[string]colDDL{}}
		if err := c.describeColumns(ctx, db, tableID.Int64, t, x); err != nil {
			return "", mapError(err)
		}
		if err := c.describeIndexes(ctx, db, tableID.Int64, t, x); err != nil {
			return "", mapError(err)
		}
		lines := columnLines(t.Columns, x, dbColl.String)
		for _, ix := range x.indexes {
			if ix.constraint {
				lines = append(lines, "    "+constraintClause(ix)) // system-named
			}
		}
		return "CREATE TYPE " + full + " AS TABLE (\n" + strings.Join(lines, ",\n") + "\n);", nil
	}
	s := "CREATE TYPE " + full + " FROM " + typeString(base, maxLen, prec, scale)
	if !nullable {
		s += " NOT NULL"
	}
	return s + ";", nil
}

// CatalogColumns implements driver.Catalog.
func (c *conn) CatalogColumns(ctx context.Context, s driver.Scope) ([]driver.CatalogTable, error) {
	db, schema := c.dbName(s.Database), schemaName(s.Schema)
	rows, err := c.db.QueryContext(ctx, `SELECT o.name, RTRIM(o.type), c.name, ty.name, ty.is_user_defined, c.max_length, c.precision, c.scale,
		CASE WHEN EXISTS (SELECT 1 FROM `+sysView(db, "indexes")+` i
			JOIN `+sysView(db, "index_columns")+` ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
			WHERE i.object_id = o.object_id AND i.is_primary_key = 1 AND ic.column_id = c.column_id) THEN 1 ELSE 0 END
		FROM `+sysView(db, "objects")+` o JOIN `+sysView(db, "schemas")+` s ON s.schema_id = o.schema_id
		JOIN `+sysView(db, "columns")+` c ON c.object_id = o.object_id
		JOIN `+sysView(db, "types")+` ty ON ty.user_type_id = c.user_type_id
		WHERE s.name = @p1 AND o.type IN ('U', 'V') AND o.is_ms_shipped = 0
		ORDER BY o.name, c.column_id`, schema)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []driver.CatalogTable
	idx := map[string]int{}
	for rows.Next() {
		var tn, typ, cn, tyName string
		var userDefined, pk bool
		var maxLen, prec, scale int
		if err := rows.Scan(&tn, &typ, &cn, &tyName, &userDefined, &maxLen, &prec, &scale, &pk); err != nil {
			return nil, err
		}
		i, ok := idx[tn]
		if !ok {
			out = append(out, driver.CatalogTable{Schema: schema, Name: tn, Kind: objectKind(typ)})
			i = len(out) - 1
			idx[tn] = i
		}
		ct := tyName
		if !userDefined {
			ct = typeString(tyName, maxLen, prec, scale)
		}
		out[i].Columns = append(out[i].Columns, driver.CatalogColumn{Name: cn, Type: ct, PK: pk})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if all, err := c.fks(ctx, db, "ss.name = @p1", schema); err == nil {
		for _, fk := range all {
			if i, ok := idx[fk.Table.Name]; ok {
				out[i].FKs = append(out[i].FKs, fk)
			}
		}
	}
	return out, nil
}
