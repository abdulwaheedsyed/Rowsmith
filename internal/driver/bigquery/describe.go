package bigquery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"

	"rowsmith/internal/driver"
)

// table resolves an object reference to a client table handle.
func (c *conn) table(ref driver.ObjectRef) (*bigquery.Table, error) {
	project, dataset, err := c.datasetOf(ref.Schema)
	if err != nil {
		return nil, err
	}
	if ref.Name == "" {
		return nil, errors.New("table name is required")
	}
	return c.client.DatasetInProject(project, dataset).Table(ref.Name), nil
}

func (c *conn) metadata(ctx context.Context, ref driver.ObjectRef) (*bigquery.Table, *bigquery.TableMetadata, error) {
	tbl, err := c.table(ref)
	if err != nil {
		return nil, nil, err
	}
	md, err := tbl.Metadata(ctx)
	if err != nil {
		return nil, nil, mapError(err)
	}
	return tbl, md, nil
}

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	if ref.Kind == "routine" {
		return c.describeRoutine(ctx, ref)
	}
	tbl, md, err := c.metadata(ctx, ref)
	if err != nil {
		return nil, err
	}
	t := describeMetadata(ref, tbl, md)
	t.DDL = c.tableDDL(ctx, tbl, md, t)
	return t, nil
}

// describeMetadata converts table metadata. Nested RECORD fields stay one
// object column whose type spells out the STRUCT; REPEATED fields are arrays.
func describeMetadata(ref driver.ObjectRef, tbl *bigquery.Table, md *bigquery.TableMetadata) *driver.Table {
	kind := tableKind(string(md.Type))
	if ref.Schema == "" {
		ref.Schema = tbl.DatasetID
	}
	t := &driver.Table{Ref: ref, Kind: kind, Comment: md.Description, Options: map[string]string{},
		Columns: []driver.Column{}, Indexes: []driver.Index{}, ForeignKeys: []driver.ForeignKey{}, Referenced: []driver.ForeignKey{},
		Checks: []driver.Check{}, Triggers: []driver.Trigger{}, PrimaryKey: []string{}, RowKey: []string{}}
	t.Ref.Kind = kind

	pk := map[string]bool{}
	if tc := md.TableConstraints; tc != nil {
		if tc.PrimaryKey != nil {
			t.PrimaryKey = tc.PrimaryKey.Columns
			for _, k := range tc.PrimaryKey.Columns {
				pk[k] = true
			}
		}
		for _, fk := range tc.ForeignKeys {
			if fk.ReferencedTable == nil {
				continue
			}
			f := driver.ForeignKey{Name: fk.Name, RefTable: driver.ObjectRef{Schema: refSchema(tbl, fk.ReferencedTable), Name: fk.ReferencedTable.TableID, Kind: "table"}}
			for _, cr := range fk.ColumnReferences {
				f.Columns = append(f.Columns, cr.ReferencingColumn)
				f.RefColumns = append(f.RefColumns, cr.ReferencedColumn)
			}
			t.ForeignKeys = append(t.ForeignKeys, f)
		}
	}
	for _, fs := range md.Schema {
		t.Columns = append(t.Columns, column(fs, pk[fs.Name]))
	}
	if kind == "table" || kind == "materialized_view" {
		rows, size := int64(md.NumRows), md.NumBytes
		t.RowEstimate, t.Size = &rows, &size
	}

	o := t.Options
	o["type"] = string(md.Type)
	setOpt(o, "location", md.Location)
	setOpt(o, "friendly_name", md.Name)
	setTime(o, "created", md.CreationTime)
	setTime(o, "modified", md.LastModifiedTime)
	setTime(o, "expiration", md.ExpirationTime)
	setOpt(o, "partition_by", partitionExpr(md))
	if tp := md.TimePartitioning; tp != nil && tp.Expiration > 0 {
		o["partition_expiration_days"] = strconv.FormatFloat(tp.Expiration.Hours()/24, 'f', -1, 64)
	}
	if md.RequirePartitionFilter || md.TimePartitioning != nil && md.TimePartitioning.RequirePartitionFilter {
		o["require_partition_filter"] = "true"
	}
	setOpt(o, "cluster_by", clusterBy(md))
	setOpt(o, "labels", labelsText(md.Labels))
	setOpt(o, "default_collation", md.DefaultCollation)
	if md.NumLongTermBytes > 0 {
		o["long_term_bytes"] = strconv.FormatInt(md.NumLongTermBytes, 10)
	}
	if sb := md.StreamingBuffer; sb != nil {
		o["streaming_buffer_rows"] = strconv.FormatUint(sb.EstimatedRows, 10)
	}
	if ec := md.EncryptionConfig; ec != nil {
		setOpt(o, "kms_key_name", ec.KMSKeyName)
	}
	if ed := md.ExternalDataConfig; ed != nil {
		setOpt(o, "format", string(ed.SourceFormat))
		setOpt(o, "uris", strings.Join(ed.SourceURIs, ", "))
	}
	if sd := md.SnapshotDefinition; sd != nil && sd.BaseTableReference != nil {
		o["snapshot_of"] = qualifiedName(sd.BaseTableReference)
		setTime(o, "snapshot_time", sd.SnapshotTime)
	}
	if cd := md.CloneDefinition; cd != nil && cd.BaseTableReference != nil {
		o["clone_of"] = qualifiedName(cd.BaseTableReference)
		setTime(o, "clone_time", cd.CloneTime)
	}
	switch kind {
	case "view":
		t.Definition = md.ViewQuery
	case "materialized_view":
		if mv := md.MaterializedView; mv != nil {
			t.Definition = mv.Query
			o["enable_refresh"] = strconv.FormatBool(mv.EnableRefresh)
			if mv.RefreshInterval > 0 {
				o["refresh_interval_minutes"] = strconv.FormatFloat(mv.RefreshInterval.Minutes(), 'f', -1, 64)
			}
			setTime(o, "last_refresh", mv.LastRefreshTime)
		}
	}
	return t
}

// qualifiedName renders project.dataset.table without quotes, for display.
func qualifiedName(t *bigquery.Table) string {
	return t.ProjectID + "." + t.DatasetID + "." + t.TableID
}

// clusterBy renders the CLUSTER BY column list, "" if unclustered.
func clusterBy(md *bigquery.TableMetadata) string {
	if md.Clustering == nil {
		return ""
	}
	cols := make([]string, len(md.Clustering.Fields))
	for i, f := range md.Clustering.Fields {
		cols[i] = quote(f)
	}
	return strings.Join(cols, ", ")
}

// refSchema names the dataset of a referenced table relative to tbl.
func refSchema(tbl *bigquery.Table, ref *bigquery.Table) string {
	if ref.ProjectID == "" || ref.ProjectID == tbl.ProjectID {
		return ref.DatasetID
	}
	return ref.ProjectID + "." + ref.DatasetID
}

func column(fs *bigquery.FieldSchema, pk bool) driver.Column {
	col := driver.Column{
		Name: fs.Name, Type: typeName(fs), BaseType: baseType(fs), Kind: kindOf(fs),
		Nullable: !fs.Required && !fs.Repeated, PrimaryKey: pk, Comment: fs.Description, Collation: fs.Collation,
	}
	if fs.DefaultValueExpression != "" {
		d := fs.DefaultValueExpression
		col.Default = &d
	}
	if fs.MaxLength > 0 {
		n := fs.MaxLength
		col.Length = &n
	}
	if fs.Precision > 0 {
		p, s := fs.Precision, fs.Scale
		col.Precision, col.Scale = &p, &s
	}
	if col.Kind == driver.KindGeometry {
		col.SRID, col.GeometryType = geographySRID, "geography"
	}
	return col
}

func setOpt(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func setTime(m map[string]string, k string, t time.Time) {
	if !t.IsZero() {
		m[k] = t.UTC().Format("2006-01-02 15:04:05 UTC")
	}
}

func labelsText(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + l[k]
	}
	return strings.Join(parts, ", ")
}

// partitionExpr renders the PARTITION BY expression of a table, "" if unpartitioned.
func partitionExpr(md *bigquery.TableMetadata) string {
	if rp := md.RangePartitioning; rp != nil && rp.Range != nil {
		return fmt.Sprintf("RANGE_BUCKET(%s, GENERATE_ARRAY(%d, %d, %d))", quote(rp.Field), rp.Range.Start, rp.Range.End, rp.Range.Interval)
	}
	tp := md.TimePartitioning
	if tp == nil {
		return ""
	}
	unit := string(tp.Type)
	if unit == "" {
		unit = string(bigquery.DayPartitioningType)
	}
	if tp.Field == "" { // ingestion-time partitioning
		if unit == string(bigquery.DayPartitioningType) {
			return "_PARTITIONDATE"
		}
		return "TIMESTAMP_TRUNC(_PARTITIONTIME, " + unit + ")"
	}
	var fieldType bigquery.FieldType
	for _, fs := range md.Schema {
		if fs.Name == tp.Field {
			fieldType = fs.Type
		}
	}
	f := quote(tp.Field)
	switch fieldType {
	case bigquery.DateFieldType:
		if unit == string(bigquery.DayPartitioningType) {
			return f
		}
		return "DATE_TRUNC(" + f + ", " + unit + ")"
	case bigquery.DateTimeFieldType:
		return "DATETIME_TRUNC(" + f + ", " + unit + ")"
	}
	return "TIMESTAMP_TRUNC(" + f + ", " + unit + ")"
}

// cachedDDL is the DDL of one table as of its last modification.
type cachedDDL struct {
	modified time.Time
	ddl      string
}

// tableDDL returns the CREATE statement BigQuery itself reports in
// INFORMATION_SCHEMA.TABLES.ddl. That lookup is a (tiny) billed query, so the
// answer is cached until the table changes; when it is unavailable the DDL
// is reconstructed from metadata.
func (c *conn) tableDDL(ctx context.Context, tbl *bigquery.Table, md *bigquery.TableMetadata, t *driver.Table) string {
	key := qualifiedName(tbl)
	if v, ok := c.ddl.Load(key); ok && v.(cachedDDL).modified.Equal(md.LastModifiedTime) {
		return v.(cachedDDL).ddl
	}
	ddl, err := c.infoSchemaDDL(ctx, tbl.ProjectID, tbl.DatasetID, "TABLES", "table_name", tbl.TableID, md.Location)
	if err != nil || ddl == "" {
		ddl = buildDDL(tablePath(tbl.ProjectID, tbl.DatasetID, tbl.TableID), t, md)
	}
	if ctx.Err() == nil {
		c.ddl.Store(key, cachedDDL{modified: md.LastModifiedTime, ddl: ddl})
	}
	return ddl
}

// infoSchemaDDL reads the ddl column of a dataset's INFORMATION_SCHEMA view.
func (c *conn) infoSchemaDDL(ctx context.Context, project, dataset, view, nameCol, name, location string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	q := c.newQuery("SELECT ddl FROM "+quote(project+"."+dataset)+".INFORMATION_SCHEMA."+view+" WHERE "+nameCol+" = @name", location)
	q.Parameters = []bigquery.QueryParameter{{Name: "name", Value: name}}
	res, err := c.collect(ctx, q, 1)
	if err != nil {
		return "", err
	}
	if len(res.Rows) == 0 {
		return "", errors.New("no DDL reported")
	}
	s, _ := res.Rows[0][0].(string)
	return strings.TrimSpace(s), nil
}

// buildDDL reconstructs a CREATE statement from table metadata.
func buildDDL(path string, t *driver.Table, md *bigquery.TableMetadata) string {
	var b strings.Builder
	switch t.Kind {
	case "view":
		return "CREATE VIEW " + path + "\nAS " + md.ViewQuery + ";"
	case "materialized_view":
		b.WriteString("CREATE MATERIALIZED VIEW " + path)
		if expr := t.Options["partition_by"]; expr != "" {
			b.WriteString("\nPARTITION BY " + expr)
		}
		if cl := t.Options["cluster_by"]; cl != "" {
			b.WriteString("\nCLUSTER BY " + cl)
		}
		b.WriteString("\nAS " + t.Definition + ";")
		return b.String()
	case "external_table":
		b.WriteString("CREATE EXTERNAL TABLE " + path)
	default:
		b.WriteString("CREATE TABLE " + path)
	}
	if len(md.Schema) > 0 {
		b.WriteString("\n(\n")
		for i, fs := range md.Schema {
			b.WriteString("  " + quote(fs.Name) + " " + typeName(fs))
			if fs.DefaultValueExpression != "" { // GoogleSQL wants DEFAULT before NOT NULL
				b.WriteString(" DEFAULT " + fs.DefaultValueExpression)
			}
			if fs.Required && !fs.Repeated {
				b.WriteString(" NOT NULL")
			}
			if fs.Description != "" {
				b.WriteString(" OPTIONS(description=" + strconv.Quote(fs.Description) + ")")
			}
			if i < len(md.Schema)-1 || len(t.PrimaryKey) > 0 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		if len(t.PrimaryKey) > 0 {
			cols := make([]string, len(t.PrimaryKey))
			for i, k := range t.PrimaryKey {
				cols[i] = quote(k)
			}
			b.WriteString("  PRIMARY KEY (" + strings.Join(cols, ", ") + ") NOT ENFORCED\n")
		}
		b.WriteString(")")
	}
	if expr := t.Options["partition_by"]; expr != "" {
		b.WriteString("\nPARTITION BY " + expr)
	}
	if cl := t.Options["cluster_by"]; cl != "" {
		b.WriteString("\nCLUSTER BY " + cl)
	}
	var opts []string
	if ed := md.ExternalDataConfig; ed != nil {
		opts = append(opts, "format="+strconv.Quote(string(ed.SourceFormat)))
		uris := make([]string, len(ed.SourceURIs))
		for i, u := range ed.SourceURIs {
			uris[i] = strconv.Quote(u)
		}
		opts = append(opts, "uris=["+strings.Join(uris, ", ")+"]")
	}
	if md.Description != "" {
		opts = append(opts, "description="+strconv.Quote(md.Description))
	}
	if d := t.Options["partition_expiration_days"]; d != "" {
		opts = append(opts, "partition_expiration_days="+d)
	}
	if t.Options["require_partition_filter"] == "true" {
		opts = append(opts, "require_partition_filter=true")
	}
	if !md.ExpirationTime.IsZero() {
		opts = append(opts, "expiration_timestamp=TIMESTAMP "+strconv.Quote(md.ExpirationTime.UTC().Format("2006-01-02 15:04:05 UTC")))
	}
	if len(md.Labels) > 0 {
		keys := make([]string, 0, len(md.Labels))
		for k := range md.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]string, len(keys))
		for i, k := range keys {
			pairs[i] = "(" + strconv.Quote(k) + ", " + strconv.Quote(md.Labels[k]) + ")"
		}
		opts = append(opts, "labels=["+strings.Join(pairs, ", ")+"]")
	}
	if len(opts) > 0 {
		b.WriteString("\nOPTIONS(\n  " + strings.Join(opts, ",\n  ") + "\n)")
	}
	b.WriteString(";")
	return b.String()
}

// describeRoutine reports a function or procedure: its arguments become
// columns and the body its definition.
func (c *conn) describeRoutine(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	project, dataset, err := c.datasetOf(ref.Schema)
	if err != nil {
		return nil, err
	}
	r := c.client.DatasetInProject(project, dataset).Routine(ref.Name)
	md, err := r.Metadata(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	t := &driver.Table{Ref: ref, Kind: "routine", Comment: md.Description, Definition: md.Body,
		Columns: []driver.Column{}, Indexes: []driver.Index{}, ForeignKeys: []driver.ForeignKey{}, Referenced: []driver.ForeignKey{},
		Checks: []driver.Check{}, Triggers: []driver.Trigger{}, PrimaryKey: []string{}, RowKey: []string{},
		Options: map[string]string{"type": md.Type, "language": md.Language}}
	setTime(t.Options, "created", md.CreationTime)
	setTime(t.Options, "modified", md.LastModifiedTime)
	if md.ReturnType != nil {
		t.Options["returns"] = sqlTypeName(md.ReturnType)
	}
	for _, a := range md.Arguments {
		typ := "ANY TYPE"
		if a.DataType != nil {
			typ = sqlTypeName(a.DataType)
		}
		col := driver.Column{Name: a.Name, Type: typ, Kind: driver.KindOther, Nullable: true}
		if a.DataType != nil {
			col.BaseType, col.Kind = strings.ToLower(a.DataType.TypeKind), standardKind(a.DataType)
		}
		if a.Mode != "" {
			col.Comment = a.Mode
		}
		t.Columns = append(t.Columns, col)
	}
	t.DDL, err = c.infoSchemaDDL(ctx, project, dataset, "ROUTINES", "routine_name", ref.Name, "")
	if err != nil || t.DDL == "" {
		t.DDL = routineDDL(project, dataset, ref.Name, md)
	}
	return t, nil
}

// sqlTypeName renders a StandardSQL type (used by routine signatures).
func sqlTypeName(t *bigquery.StandardSQLDataType) string {
	switch {
	case t.ArrayElementType != nil:
		return "ARRAY<" + sqlTypeName(t.ArrayElementType) + ">"
	case t.RangeElementType != nil:
		return "RANGE<" + sqlTypeName(t.RangeElementType) + ">"
	case t.StructType != nil:
		parts := make([]string, len(t.StructType.Fields))
		for i, f := range t.StructType.Fields {
			parts[i] = strings.TrimSpace(f.Name + " " + sqlTypeName(f.Type))
		}
		return "STRUCT<" + strings.Join(parts, ", ") + ">"
	}
	return t.TypeKind
}

// standardKind maps a StandardSQL type kind (INT64, BOOL, ARRAY...) to a value kind.
func standardKind(t *bigquery.StandardSQLDataType) driver.ValueKind {
	switch t.TypeKind {
	case "ARRAY":
		return driver.KindArray
	case "STRUCT":
		return driver.KindObject
	case "INT64":
		return driver.KindInt
	case "FLOAT64":
		return driver.KindFloat
	case "BOOL":
		return driver.KindBool
	}
	return scalarKind(bigquery.FieldType(t.TypeKind))
}

// routineDDL is a best-effort CREATE statement when INFORMATION_SCHEMA is unavailable.
func routineDDL(project, dataset, name string, md *bigquery.RoutineMetadata) string {
	args := make([]string, len(md.Arguments))
	for i, a := range md.Arguments {
		typ := "ANY TYPE"
		if a.DataType != nil {
			typ = sqlTypeName(a.DataType)
		}
		args[i] = strings.TrimSpace(a.Mode + " " + a.Name + " " + typ)
	}
	path := tablePath(project, dataset, name)
	sig := path + "(" + strings.Join(args, ", ") + ")"
	switch md.Type {
	case "PROCEDURE":
		return "CREATE PROCEDURE " + sig + "\n" + md.Body + ";"
	case "TABLE_VALUED_FUNCTION":
		return "CREATE TABLE FUNCTION " + sig + "\nAS " + md.Body + ";"
	}
	s := "CREATE FUNCTION " + sig
	if md.ReturnType != nil {
		s += "\nRETURNS " + sqlTypeName(md.ReturnType)
	}
	if md.Language != "" && md.Language != "SQL" {
		return s + "\nLANGUAGE " + strings.ToLower(md.Language) + "\nAS r\"\"\"" + md.Body + "\"\"\";"
	}
	return s + "\nAS (" + md.Body + ");"
}

// Definition implements driver.Definer for views, routines and tables.
func (c *conn) Definition(ctx context.Context, ref driver.ObjectRef) (string, error) {
	t, err := c.Describe(ctx, ref)
	if err != nil {
		return "", err
	}
	return t.DDL, nil
}

// CatalogColumns implements driver.Catalog from table metadata (free API
// calls). Columns are fetched for the first statsLimit tables of the dataset.
func (c *conn) CatalogColumns(ctx context.Context, s driver.Scope) ([]driver.CatalogTable, error) {
	project, dataset, err := c.datasetOf(s.Schema)
	if err != nil {
		return nil, err
	}
	schema := s.Schema
	if schema == "" {
		schema = dataset
	}
	var out []driver.CatalogTable
	err = c.svc.Tables.List(project, dataset).MaxResults(1000).Pages(ctx, func(page *bq.TableList) error {
		for _, t := range page.Tables {
			if t.TableReference != nil {
				out = append(out, driver.CatalogTable{Schema: schema, Name: t.TableReference.TableId, Kind: tableKind(t.Type)})
			}
		}
		return nil
	})
	if err != nil {
		return nil, mapError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ds := c.client.DatasetInProject(project, dataset)
	sem := make(chan struct{}, statsWorkers)
	var wg sync.WaitGroup
	for i := range out[:min(len(out), statsLimit)] {
		wg.Add(1)
		sem <- struct{}{}
		go func(ct *driver.CatalogTable) {
			defer func() { <-sem; wg.Done() }()
			md, err := ds.Table(ct.Name).Metadata(ctx)
			if err != nil {
				return
			}
			pk := map[string]bool{}
			if md.TableConstraints != nil && md.TableConstraints.PrimaryKey != nil {
				for _, k := range md.TableConstraints.PrimaryKey.Columns {
					pk[k] = true
				}
			}
			for _, fs := range md.Schema {
				ct.Columns = append(ct.Columns, driver.CatalogColumn{Name: fs.Name, Type: typeName(fs), PK: pk[fs.Name]})
			}
		}(&out[i])
	}
	wg.Wait()
	return out, nil
}
