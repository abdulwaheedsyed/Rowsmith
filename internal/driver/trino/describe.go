package trino

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

// kindOf maps a Trino type (bigint, varchar(25), decimal(12,2),
// timestamp(6) with time zone, array(varchar)…) to a value kind.
func kindOf(typ string) driver.ValueKind {
	t := strings.ToLower(strings.TrimSpace(typ))
	base := t
	if i := strings.IndexAny(t, "( "); i > 0 {
		base = t[:i]
	}
	switch base {
	case "tinyint", "smallint", "integer", "int", "bigint":
		return driver.KindInt
	case "real", "double":
		return driver.KindFloat
	case "decimal":
		return driver.KindDecimal
	case "boolean":
		return driver.KindBool
	case "varchar", "char":
		return driver.KindString
	case "varbinary":
		return driver.KindBinary
	case "date":
		return driver.KindDate
	case "time":
		if strings.Contains(t, "with time zone") {
			return driver.KindString
		}
		return driver.KindTime
	case "timestamp":
		if strings.Contains(t, "with time zone") {
			return driver.KindTimestamp
		}
		return driver.KindDateTime
	case "json":
		return driver.KindJSON
	case "array":
		return driver.KindArray
	case "map", "row":
		return driver.KindObject
	case "uuid":
		return driver.KindUUID
	case "interval":
		return driver.KindInterval
	case "geometry", "sphericalgeography":
		return driver.KindText // read as WKT text
	}
	return driver.KindOther
}

func column(name, typ string) driver.Column {
	c := driver.Column{Name: name, Type: typ, Kind: kindOf(typ), Nullable: true}
	c.BaseType = strings.ToLower(typ)
	if i := strings.IndexAny(c.BaseType, "( "); i > 0 {
		c.BaseType = c.BaseType[:i]
	}
	if c.BaseType == "decimal" {
		if m := paramsRe.FindStringSubmatch(typ); m != nil {
			p, _ := strconv.ParseInt(m[1], 10, 64)
			s, _ := strconv.ParseInt(m[2], 10, 64)
			c.Precision, c.Scale = &p, &s
		}
	}
	if c.BaseType == "varchar" || c.BaseType == "char" {
		if m := lengthRe.FindStringSubmatch(typ); m != nil {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			c.Length = &n
		}
	}
	if c.BaseType == "timestamp" || c.BaseType == "time" {
		if m := lengthRe.FindStringSubmatch(typ); m != nil {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			c.Scale = &n
		}
	}
	return c
}

var (
	paramsRe = regexp.MustCompile(`\((\d+)\s*,\s*(\d+)\)`)
	lengthRe = regexp.MustCompile(`^\w+\((\d+)\)`)
	// WITH ( key = value, … ) at the end of SHOW CREATE TABLE.
	withRe = regexp.MustCompile(`(?s)\)\s*(?:COMMENT\s+'(?:[^']|'')*'\s*)?WITH\s*\((.*)\)\s*$`)
	propRe = regexp.MustCompile(`(?m)^\s*(\w+)\s*=\s*(.+?),?\s*$`)
)

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	if ref.Database == "" || ref.Schema == "" {
		return nil, fmt.Errorf("%s: choose a catalog and schema", ref.Name)
	}
	t := &driver.Table{Ref: ref, Options: map[string]string{}, Indexes: []driver.Index{}, ForeignKeys: []driver.ForeignKey{},
		Referenced: []driver.ForeignKey{}, Checks: []driver.Check{}, Triggers: []driver.Trigger{}, Kind: "table"}
	var tableType string
	err := c.db.QueryRowContext(ctx, "SELECT table_type FROM "+quote(ref.Database)+".information_schema.tables WHERE table_schema = ? AND table_name = ?",
		ref.Schema, ref.Name).Scan(&tableType)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%s.%s.%s not found", ref.Database, ref.Schema, ref.Name)
	}
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	if tableType == "VIEW" {
		t.Kind = "view"
	} else {
		var n int
		if c.db.QueryRowContext(ctx, "SELECT count(*) FROM system.metadata.materialized_views WHERE catalog_name = ? AND schema_name = ? AND name = ?",
			ref.Database, ref.Schema, ref.Name).Scan(&n) == nil && n > 0 {
			t.Kind = "materialized_view"
		}
	}
	t.Ref.Kind = t.Kind
	conn := c.connector(ctx, ref.Database)
	t.Options["connector"] = conn

	rows, err := c.db.QueryContext(ctx, "SHOW COLUMNS FROM "+qualify(ref))
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	for rows.Next() {
		var name, typ, extra, comment sql.NullString
		if err := rows.Scan(&name, &typ, &extra, &comment); err != nil {
			rows.Close()
			return nil, err
		}
		col := column(name.String, typ.String)
		col.Comment = comment.String
		if strings.Contains(extra.String, "partition key") {
			t.Options["partitioned_by"] = strings.TrimPrefix(t.Options["partitioned_by"]+", "+col.Name, ", ")
		}
		t.Columns = append(t.Columns, col)
	}
	rows.Close()
	// SHOW COLUMNS leaves out NOT NULL.
	if rows, err := c.db.QueryContext(ctx, "SELECT column_name FROM "+quote(ref.Database)+
		".information_schema.columns WHERE table_schema = ? AND table_name = ? AND is_nullable = 'NO'", ref.Schema, ref.Name); err == nil {
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				for i := range t.Columns {
					if t.Columns[i].Name == name {
						t.Columns[i].Nullable = false
					}
				}
			}
		}
		rows.Close()
	}

	kw := map[string]string{"table": "TABLE", "view": "VIEW", "materialized_view": "MATERIALIZED VIEW"}[t.Kind]
	var ddl string
	if c.db.QueryRowContext(ctx, "SHOW CREATE "+kw+" "+qualify(ref)).Scan(&ddl) == nil {
		t.DDL = ddl
		if t.Kind != "table" {
			t.Definition = ddl
		}
		if m := withRe.FindStringSubmatch(ddl); m != nil && t.Kind == "table" {
			for _, p := range propRe.FindAllStringSubmatch(m[1], -1) {
				v := strings.TrimSpace(p[2])
				if strings.HasPrefix(v, "ARRAY[") {
					// ARRAY['month(placed_at)', 'bucket(id, 4)'] → month(placed_at), bucket(id, 4)
					inner := strings.TrimSuffix(strings.TrimPrefix(v, "ARRAY["), "]")
					var parts []string
					for _, q := range regexp.MustCompile(`'((?:[^']|'')*)'`).FindAllStringSubmatch(inner, -1) {
						parts = append(parts, strings.ReplaceAll(q[1], "''", "'"))
					}
					v = strings.Join(parts, ", ")
				} else {
					v = strings.ReplaceAll(strings.Trim(v, "'"), "''", "'")
				}
				switch p[1] {
				case "external_location":
					t.Options["location"] = v
				default:
					t.Options[p[1]] = v
				}
			}
		}
	}
	var comment sql.NullString
	if c.db.QueryRowContext(ctx, "SELECT comment FROM system.metadata.table_comments WHERE catalog_name = ? AND schema_name = ? AND table_name = ?",
		ref.Database, ref.Schema, ref.Name).Scan(&comment) == nil {
		t.Comment = comment.String
	}

	if t.Kind == "table" && conn == "iceberg" {
		c.icebergFacts(ctx, ref, t)
	}
	if t.Kind == "table" && (conn == "iceberg" || conn == "delta_lake") {
		// No keys: an edit matches a row on all of its values and must find exactly one.
		sqlbase.ChooseRowKey(t, false)
		t.Editable = !c.ro
	}
	return t, nil
}

// icebergFacts reads an Iceberg table's snapshots and files from its
// metadata tables ($snapshots, $files), without scanning data.
func (c *conn) icebergFacts(ctx context.Context, ref driver.ObjectRef, t *driver.Table) {
	meta := func(suffix string) string {
		return qualify(driver.ObjectRef{Database: ref.Database, Schema: ref.Schema, Name: ref.Name + "$" + suffix})
	}
	var snaps int64
	var last sql.NullTime
	if c.db.QueryRowContext(ctx, "SELECT count(*), max(committed_at) FROM "+meta("snapshots")).Scan(&snaps, &last) == nil {
		t.Options["snapshots"] = strconv.FormatInt(snaps, 10)
		if last.Valid {
			t.Options["last_commit"] = last.Time.UTC().Format("2006-01-02 15:04:05 UTC")
		}
	}
	var dataFiles, deleteFiles, records, bytes sql.NullInt64
	if c.db.QueryRowContext(ctx, `SELECT count_if(content = 0), count_if(content <> 0),
		sum(CASE WHEN content = 0 THEN record_count END), sum(file_size_in_bytes) FROM `+meta("files")).
		Scan(&dataFiles, &deleteFiles, &records, &bytes) == nil {
		t.Options["data_files"] = strconv.FormatInt(dataFiles.Int64, 10)
		t.Options["delete_files"] = strconv.FormatInt(deleteFiles.Int64, 10)
		if records.Valid {
			n := records.Int64
			t.RowEstimate = &n
		}
		if bytes.Valid {
			n := bytes.Int64
			t.Size = &n
		}
	}
}

// Definition implements driver.Definer for views and materialized views.
func (c *conn) Definition(ctx context.Context, ref driver.ObjectRef) (string, error) {
	kw := "VIEW"
	if ref.Kind == "materialized_view" {
		kw = "MATERIALIZED VIEW"
	}
	var ddl string
	if err := c.db.QueryRowContext(ctx, "SHOW CREATE "+kw+" "+qualify(ref)).Scan(&ddl); err != nil {
		return "", mapError(err, sqlsplit.Statement{})
	}
	return ddl, nil
}

// CatalogColumns implements driver.Catalog for one schema.
func (c *conn) CatalogColumns(ctx context.Context, s driver.Scope) ([]driver.CatalogTable, error) {
	if s.Database == "" || s.Schema == "" {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, "SELECT table_name, column_name, data_type FROM "+quote(s.Database)+
		".information_schema.columns WHERE table_schema = ? ORDER BY table_name, ordinal_position", s.Schema)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	defer rows.Close()
	var out []driver.CatalogTable
	idx := map[string]int{}
	for rows.Next() {
		var tn, cn, typ string
		if err := rows.Scan(&tn, &cn, &typ); err != nil {
			return nil, err
		}
		i, ok := idx[tn]
		if !ok {
			i = len(out)
			idx[tn] = i
			out = append(out, driver.CatalogTable{Schema: s.Schema, Name: tn, Kind: "table"})
		}
		out[i].Columns = append(out[i].Columns, driver.CatalogColumn{Name: cn, Type: typ})
	}
	return out, mapError(rows.Err(), sqlsplit.Statement{})
}
