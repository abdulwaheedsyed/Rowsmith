package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
)

var geoTypeRe = regexp.MustCompile(`(?i)^(geometry|geography)\((\w+)(?:,\s*(\d+))?\)`)

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	p, err := c.pool(ctx, ref.Database)
	if err != nil {
		return nil, err
	}
	db := p.db
	schema := ref.Schema
	if schema == "" {
		schema = "public"
	}
	t := &driver.Table{Ref: ref, Options: map[string]string{}}
	t.Ref.Schema = schema

	var oid uint32
	var kind, comment string
	var n, size sql.NullInt64
	var persistence string
	err = db.QueryRowContext(ctx, `SELECT c.oid, c.relkind::text, COALESCE(obj_description(c.oid, 'pg_class'), ''),
		CASE WHEN c.reltuples >= 0 THEN c.reltuples::bigint END,
		CASE WHEN c.relkind IN ('r','m','p','t') THEN pg_total_relation_size(c.oid) END, c.relpersistence::text
		FROM pg_class c JOIN pg_namespace ns ON ns.oid = c.relnamespace WHERE ns.nspname = $1 AND c.relname = $2`, schema, ref.Name).
		Scan(&oid, &kind, &comment, &n, &size, &persistence)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("relation %s.%s not found", schema, ref.Name)
	}
	if err != nil {
		return nil, err
	}
	t.Kind = relkind(kind)
	t.Ref.Kind = t.Kind
	t.Comment, t.RowEstimate, t.Size = comment, sqlbase.NullInt(n), sqlbase.NullInt(size)
	if persistence == "u" {
		t.Options["unlogged"] = "true"
	}
	var partKey, parent, bound sql.NullString
	if db.QueryRowContext(ctx, `SELECT CASE WHEN c.relkind = 'p' THEN pg_get_partkeydef(c.oid) END,
		(SELECT quote_ident(pn.nspname) || '.' || quote_ident(pc.relname) FROM pg_inherits i JOIN pg_class pc ON pc.oid = i.inhparent
			JOIN pg_namespace pn ON pn.oid = pc.relnamespace WHERE i.inhrelid = c.oid AND c.relispartition LIMIT 1),
		CASE WHEN c.relispartition THEN pg_get_expr(c.relpartbound, c.oid) END
		FROM pg_class c WHERE c.oid = $1`, oid).Scan(&partKey, &parent, &bound) == nil {
		if partKey.Valid {
			t.Options["partition_by"] = partKey.String
		}
		if parent.Valid && bound.Valid {
			t.Options["partition_of"], t.Options["partition_bound"] = parent.String, bound.String
		}
	}

	if err := describeColumns(ctx, db, oid, t); err != nil {
		return nil, err
	}
	if t.Kind == "table" || t.Kind == "partitioned_table" || t.Kind == "materialized_view" {
		if err := describeIndexes(ctx, db, oid, t); err != nil {
			return nil, err
		}
	}
	if t.Kind == "table" || t.Kind == "partitioned_table" || t.Kind == "foreign_table" {
		if t.ForeignKeys, err = fks(ctx, db, "con.conrelid = $1", oid); err != nil {
			return nil, err
		}
		if t.Referenced, err = fks(ctx, db, "con.confrelid = $1", oid); err != nil {
			return nil, err
		}
		describeChecks(ctx, db, oid, t)
		describeTriggers(ctx, db, oid, t)
		sqlbase.ChooseRowKey(t, t.Kind == "table")
	}
	switch t.Kind {
	case "view", "materialized_view":
		var def string
		if db.QueryRowContext(ctx, `SELECT pg_get_viewdef($1::oid, true)`, oid).Scan(&def) == nil {
			t.Definition = def
			kw := "VIEW"
			if t.Kind == "materialized_view" {
				kw = "MATERIALIZED VIEW"
			}
			t.DDL = "CREATE " + kw + " " + qualify(schema, ref.Name) + " AS\n" + def
		}
	case "table", "partitioned_table":
		t.DDL = buildTableDDL(ctx, db, oid, t)
	}
	return t, nil
}

func describeColumns(ctx context.Context, db *sql.DB, oid uint32, t *driver.Table) error {
	rows, err := db.QueryContext(ctx, `SELECT a.attname, format_type(a.atttypid, a.atttypmod), t.typname, t.typtype::text, t.typcategory::text,
		NOT a.attnotnull, pg_get_expr(d.adbin, d.adrelid), a.attidentity::text, COALESCE(a.attgenerated::text, ''),
		COALESCE(col_description(a.attrelid, a.attnum), ''), COALESCE(co.collname, ''),
		CASE WHEN t.typtype = 'e' THEN (SELECT json_agg(e.enumlabel ORDER BY e.enumsortorder)::text FROM pg_enum e WHERE e.enumtypid = t.oid) END,
		COALESCE(bt.typname, '')
		FROM pg_attribute a
		JOIN pg_type t ON t.oid = a.atttypid
		LEFT JOIN pg_type bt ON t.typtype = 'd' AND bt.oid = t.typbasetype
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		LEFT JOIN pg_collation co ON co.oid = a.attcollation AND a.attcollation <> t.typcollation
		WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, oid)
	if err != nil {
		// attgenerated does not exist before PostgreSQL 12.
		if strings.Contains(err.Error(), "attgenerated") {
			return describeColumnsLegacy(ctx, db, oid, t)
		}
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var col driver.Column
		var typname, typtype, category, identity, generated string
		var def, enums sql.NullString
		var baseType string
		if err := rows.Scan(&col.Name, &col.Type, &typname, &typtype, &category, &col.Nullable, &def, &identity, &generated,
			&col.Comment, &col.Collation, &enums, &baseType); err != nil {
			return err
		}
		finishColumn(&col, typname, typtype, category, baseType, def, identity, generated, enums)
		t.Columns = append(t.Columns, col)
	}
	return rows.Err()
}

func describeColumnsLegacy(ctx context.Context, db *sql.DB, oid uint32, t *driver.Table) error {
	rows, err := db.QueryContext(ctx, `SELECT a.attname, format_type(a.atttypid, a.atttypmod), t.typname, t.typtype::text, t.typcategory::text,
		NOT a.attnotnull, pg_get_expr(d.adbin, d.adrelid), COALESCE(col_description(a.attrelid, a.attnum), '')
		FROM pg_attribute a JOIN pg_type t ON t.oid = a.atttypid
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, oid)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var col driver.Column
		var typname, typtype, category string
		var def sql.NullString
		if err := rows.Scan(&col.Name, &col.Type, &typname, &typtype, &category, &col.Nullable, &def, &col.Comment); err != nil {
			return err
		}
		finishColumn(&col, typname, typtype, category, "", def, "", "", sql.NullString{})
		t.Columns = append(t.Columns, col)
	}
	return rows.Err()
}

func finishColumn(col *driver.Column, typname, typtype, category, baseType string, def sql.NullString, identity, generated string, enums sql.NullString) {
	col.BaseType = typname
	switch {
	case category == "A":
		col.Kind = driver.KindArray
	case typtype == "e":
		col.Kind = driver.KindEnum
	case typtype == "d" && baseType != "":
		col.Kind = driver.KindFromTypeName(baseType)
	case typtype == "c":
		col.Kind = driver.KindObject
	default:
		col.Kind = driver.KindFromTypeName(typname)
	}
	if typname == "bpchar" {
		col.Kind = driver.KindString
	}
	if m := geoTypeRe.FindStringSubmatch(col.Type); m != nil {
		col.Kind = driver.KindGeometry
		col.GeometryType = strings.ToLower(m[2])
		if m[3] != "" {
			fmt.Sscanf(m[3], "%d", &col.SRID)
		}
		if strings.EqualFold(m[1], "geography") && col.SRID == 0 {
			col.SRID = 4326
		}
	} else if typname == "geometry" || typname == "geography" {
		col.Kind = driver.KindGeometry
	}
	if generated == "s" {
		col.Generated = def.String
		col.GeneratedStored = true
	} else if def.Valid {
		d := def.String
		col.Default = &d
		if strings.HasPrefix(d, "nextval(") {
			col.AutoIncrement = true
		}
	}
	if identity == "a" || identity == "d" {
		col.AutoIncrement = true
		v := "GENERATED " + map[string]string{"a": "ALWAYS", "d": "BY DEFAULT"}[identity] + " AS IDENTITY"
		col.Default = &v
	}
	if enums.Valid {
		_ = json.Unmarshal([]byte(enums.String), &col.Enum)
	}
}

func describeIndexes(ctx context.Context, db *sql.DB, oid uint32, t *driver.Table) error {
	rows, err := db.QueryContext(ctx, `SELECT i.relname, ix.indisunique, ix.indisprimary, am.amname, pg_get_indexdef(ix.indexrelid),
		COALESCE(pg_get_expr(ix.indpred, ix.indrelid), ''),
		(SELECT json_agg(pg_get_indexdef(ix.indexrelid, k, true) ORDER BY k)::text FROM generate_series(1, ix.indnatts) AS k),
		COALESCE(obj_description(i.oid, 'pg_class'), '')
		FROM pg_index ix JOIN pg_class i ON i.oid = ix.indexrelid JOIN pg_am am ON am.oid = i.relam
		WHERE ix.indrelid = $1 ORDER BY ix.indisprimary DESC, i.relname`, oid)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ix driver.Index
		var cols string
		if err := rows.Scan(&ix.Name, &ix.Unique, &ix.Primary, &ix.Type, &ix.Definition, &ix.Where, &cols, &ix.Comment); err != nil {
			return err
		}
		var raw []string
		_ = json.Unmarshal([]byte(cols), &raw)
		for _, c := range raw {
			desc := strings.HasSuffix(c, " DESC")
			ix.Columns = append(ix.Columns, unquoteIdent(strings.TrimSuffix(c, " DESC")))
			ix.Desc = append(ix.Desc, desc)
		}
		if ix.Primary {
			t.PrimaryKey = ix.Columns
			for i := range t.Columns {
				for _, k := range ix.Columns {
					if t.Columns[i].Name == k {
						t.Columns[i].PrimaryKey = true
					}
				}
			}
		}
		t.Indexes = append(t.Indexes, ix)
	}
	return rows.Err()
}

func unquoteIdent(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

var fkActions = map[string]string{"a": "NO ACTION", "r": "RESTRICT", "c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT"}

func fks(ctx context.Context, db *sql.DB, where string, arg any) ([]driver.ForeignKey, error) {
	rows, err := db.QueryContext(ctx, `SELECT con.conname, sn.nspname, sc.relname, rn.nspname, rc.relname,
		(SELECT json_agg(a.attname ORDER BY k.o)::text FROM unnest(con.conkey) WITH ORDINALITY k(n, o) JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.n),
		(SELECT json_agg(a.attname ORDER BY k.o)::text FROM unnest(con.confkey) WITH ORDINALITY k(n, o) JOIN pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = k.n),
		con.confupdtype::text, con.confdeltype::text
		FROM pg_constraint con
		JOIN pg_class sc ON sc.oid = con.conrelid JOIN pg_namespace sn ON sn.oid = sc.relnamespace
		JOIN pg_class rc ON rc.oid = con.confrelid JOIN pg_namespace rn ON rn.oid = rc.relnamespace
		WHERE con.contype = 'f' AND `+where+` ORDER BY con.conname`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.ForeignKey
	for rows.Next() {
		var fk driver.ForeignKey
		var ss, st, rs, rt, cols, rcols, upd, del string
		if err := rows.Scan(&fk.Name, &ss, &st, &rs, &rt, &cols, &rcols, &upd, &del); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(cols), &fk.Columns)
		_ = json.Unmarshal([]byte(rcols), &fk.RefColumns)
		fk.RefTable = driver.ObjectRef{Schema: rs, Name: rt, Kind: "table"}
		fk.Table = &driver.ObjectRef{Schema: ss, Name: st, Kind: "table"}
		fk.OnUpdate, fk.OnDelete = fkActions[upd], fkActions[del]
		out = append(out, fk)
	}
	return out, rows.Err()
}

func describeChecks(ctx context.Context, db *sql.DB, oid uint32, t *driver.Table) {
	rows, err := db.QueryContext(ctx, `SELECT conname, pg_get_constraintdef(oid, true) FROM pg_constraint WHERE conrelid = $1 AND contype = 'c' ORDER BY conname`, oid)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ch driver.Check
		if rows.Scan(&ch.Name, &ch.Expression) == nil {
			ch.Expression = strings.TrimSuffix(strings.TrimPrefix(ch.Expression, "CHECK ("), ")")
			t.Checks = append(t.Checks, ch)
		}
	}
}

func describeTriggers(ctx context.Context, db *sql.DB, oid uint32, t *driver.Table) {
	rows, err := db.QueryContext(ctx, `SELECT tgname, tgtype, pg_get_triggerdef(oid, true) FROM pg_trigger WHERE tgrelid = $1 AND NOT tgisinternal ORDER BY tgname`, oid)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var tr driver.Trigger
		var typ int
		if rows.Scan(&tr.Name, &typ, &tr.Statement) != nil {
			continue
		}
		switch {
		case typ&64 != 0:
			tr.Timing = "INSTEAD OF"
		case typ&2 != 0:
			tr.Timing = "BEFORE"
		default:
			tr.Timing = "AFTER"
		}
		var ev []string
		for bit, name := range map[int]string{4: "INSERT", 8: "DELETE", 16: "UPDATE", 32: "TRUNCATE"} {
			if typ&bit != 0 {
				ev = append(ev, name)
			}
		}
		tr.Event = strings.Join(sortedEvents(ev), " OR ")
		t.Triggers = append(t.Triggers, tr)
	}
}

func sortedEvents(ev []string) []string {
	order := map[string]int{"INSERT": 0, "UPDATE": 1, "DELETE": 2, "TRUNCATE": 3}
	for i := 1; i < len(ev); i++ {
		for j := i; j > 0 && order[ev[j]] < order[ev[j-1]]; j-- {
			ev[j], ev[j-1] = ev[j-1], ev[j]
		}
	}
	return ev
}

// buildTableDDL reconstructs CREATE TABLE (PostgreSQL has no SHOW CREATE).
func buildTableDDL(ctx context.Context, db *sql.DB, oid uint32, t *driver.Table) string {
	var b strings.Builder
	name := qualify(t.Ref.Schema, t.Ref.Name)
	b.WriteString("CREATE ")
	if t.Options["unlogged"] == "true" {
		b.WriteString("UNLOGGED ")
	}
	b.WriteString("TABLE " + name + " (\n")
	var lines []string
	for _, c := range t.Columns {
		l := "    " + quote(c.Name) + " " + c.Type
		if c.Collation != "" {
			l += " COLLATE " + quote(c.Collation)
		}
		if c.Generated != "" {
			l += " GENERATED ALWAYS AS (" + c.Generated + ") STORED"
		} else if c.Default != nil {
			if strings.HasPrefix(*c.Default, "GENERATED ") {
				l += " " + *c.Default
			} else {
				l += " DEFAULT " + *c.Default
			}
		}
		if !c.Nullable {
			l += " NOT NULL"
		}
		lines = append(lines, l)
	}
	rows, err := db.QueryContext(ctx, `SELECT conname, pg_get_constraintdef(oid, true) FROM pg_constraint
		WHERE conrelid = $1 AND contype IN ('p','u','f','c','x') ORDER BY CASE contype WHEN 'p' THEN 0 WHEN 'u' THEN 1 WHEN 'c' THEN 2 WHEN 'f' THEN 3 ELSE 4 END, conname`, oid)
	constraintIdx := map[string]bool{}
	if err == nil {
		for rows.Next() {
			var n, def string
			if rows.Scan(&n, &def) == nil {
				lines = append(lines, "    CONSTRAINT "+quote(n)+" "+def)
				constraintIdx[n] = true
			}
		}
		rows.Close()
	}
	b.WriteString(strings.Join(lines, ",\n"))
	b.WriteString("\n)")
	var partKey sql.NullString
	if t.Kind == "partitioned_table" && db.QueryRowContext(ctx, `SELECT pg_get_partkeydef($1::oid)`, oid).Scan(&partKey) == nil && partKey.Valid {
		b.WriteString(" PARTITION BY " + partKey.String)
	}
	b.WriteString(";\n")
	for _, ix := range t.Indexes {
		if !constraintIdx[ix.Name] && ix.Definition != "" {
			b.WriteString("\n" + ix.Definition + ";")
		}
	}
	if t.Comment != "" {
		b.WriteString("\n\nCOMMENT ON TABLE " + name + " IS " + literal(t.Comment) + ";")
	}
	for _, c := range t.Columns {
		if c.Comment != "" {
			b.WriteString("\nCOMMENT ON COLUMN " + name + "." + quote(c.Name) + " IS " + literal(c.Comment) + ";")
		}
	}
	for _, tr := range t.Triggers {
		b.WriteString("\n\n" + tr.Statement + ";")
	}
	return strings.TrimSpace(b.String())
}

// Definition implements driver.Definer for routines, sequences, types and views.
func (c *conn) Definition(ctx context.Context, ref driver.ObjectRef) (string, error) {
	p, err := c.pool(ctx, ref.Database)
	if err != nil {
		return "", err
	}
	schema := ref.Schema
	if schema == "" {
		schema = "public"
	}
	var def string
	switch ref.Kind {
	case "function", "procedure":
		rows, err := p.db.QueryContext(ctx, `SELECT pg_get_functiondef(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = $1 AND p.proname = $2 ORDER BY p.oid`, schema, ref.Name)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		var defs []string
		for rows.Next() {
			var d string
			if rows.Scan(&d) == nil {
				defs = append(defs, strings.TrimSpace(d)+";")
			}
		}
		return strings.Join(defs, "\n\n"), rows.Err()
	case "sequence":
		var start, inc, min, max, cache int64
		var cycle bool
		var typ string
		err = p.db.QueryRowContext(ctx, `SELECT data_type::text, start_value, increment_by, min_value, max_value, cache_size, cycle
			FROM pg_sequences WHERE schemaname = $1 AND sequencename = $2`, schema, ref.Name).Scan(&typ, &start, &inc, &min, &max, &cache, &cycle)
		if err != nil {
			return "", err
		}
		def = fmt.Sprintf("CREATE SEQUENCE %s AS %s INCREMENT BY %d MINVALUE %d MAXVALUE %d START WITH %d CACHE %d%s;",
			qualify(schema, ref.Name), typ, inc, min, max, start, cache, map[bool]string{true: " CYCLE", false: ""}[cycle])
		return def, nil
	case "view", "materialized_view", "table", "partitioned_table":
		t, err := c.Describe(ctx, ref)
		if err != nil {
			return "", err
		}
		return t.DDL, nil
	case "type":
		var typtype string
		var oid uint32
		if err := p.db.QueryRowContext(ctx, `SELECT t.oid, t.typtype::text FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace WHERE n.nspname = $1 AND t.typname = $2`,
			schema, ref.Name).Scan(&oid, &typtype); err != nil {
			return "", err
		}
		switch typtype {
		case "e":
			var labels string
			_ = p.db.QueryRowContext(ctx, `SELECT string_agg(quote_literal(enumlabel), ', ' ORDER BY enumsortorder) FROM pg_enum WHERE enumtypid = $1`, oid).Scan(&labels)
			return "CREATE TYPE " + qualify(schema, ref.Name) + " AS ENUM (" + labels + ");", nil
		case "d":
			var base string
			_ = p.db.QueryRowContext(ctx, `SELECT format_type(typbasetype, typtypmod) FROM pg_type WHERE oid = $1`, oid).Scan(&base)
			return "CREATE DOMAIN " + qualify(schema, ref.Name) + " AS " + base + ";", nil
		case "c":
			var attrs string
			_ = p.db.QueryRowContext(ctx, `SELECT string_agg(quote_ident(a.attname) || ' ' || format_type(a.atttypid, a.atttypmod), ', ' ORDER BY a.attnum)
				FROM pg_type t JOIN pg_attribute a ON a.attrelid = t.typrelid WHERE t.oid = $1 AND a.attnum > 0 AND NOT a.attisdropped`, oid).Scan(&attrs)
			return "CREATE TYPE " + qualify(schema, ref.Name) + " AS (" + attrs + ");", nil
		}
	case "extension":
		var ver string
		_ = p.db.QueryRowContext(ctx, `SELECT extversion FROM pg_extension WHERE extname = $1`, ref.Name).Scan(&ver)
		return "CREATE EXTENSION IF NOT EXISTS " + quote(ref.Name) + " WITH SCHEMA " + quote(schema) + " VERSION " + literal(ver) + ";", nil
	}
	return "", driver.ErrNotSupported
}

// CatalogColumns implements driver.Catalog.
func (c *conn) CatalogColumns(ctx context.Context, s driver.Scope) ([]driver.CatalogTable, error) {
	p, err := c.pool(ctx, s.Database)
	if err != nil {
		return nil, err
	}
	schema := s.Schema
	if schema == "" {
		schema = "public"
	}
	rows, err := p.db.QueryContext(ctx, `SELECT c.oid, c.relname, c.relkind::text, a.attname, format_type(a.atttypid, a.atttypmod),
		COALESCE(a.attnum = ANY(pk.conkey), false)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		LEFT JOIN pg_constraint pk ON pk.conrelid = c.oid AND pk.contype = 'p'
		WHERE n.nspname = $1 AND c.relkind IN ('r','p','v','m','f')
		ORDER BY c.relname, a.attnum`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.CatalogTable
	idx := map[string]int{}
	for rows.Next() {
		var oid uint32
		var tn, kind, cn, ct string
		var pk bool
		if err := rows.Scan(&oid, &tn, &kind, &cn, &ct, &pk); err != nil {
			return nil, err
		}
		i, ok := idx[tn]
		if !ok {
			out = append(out, driver.CatalogTable{Schema: schema, Name: tn, Kind: relkind(kind)})
			i = len(out) - 1
			idx[tn] = i
		}
		out[i].Columns = append(out[i].Columns, driver.CatalogColumn{Name: cn, Type: ct, PK: pk})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	all, err := fks(ctx, p.db, "sn.nspname = $1", schema)
	if err == nil {
		for _, fk := range all {
			if i, ok := idx[fk.Table.Name]; ok {
				out[i].FKs = append(out[i].FKs, fk)
			}
		}
	}
	return out, nil
}
