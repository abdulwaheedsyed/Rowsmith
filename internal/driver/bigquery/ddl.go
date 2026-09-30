package bigquery

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"rowsmith/internal/driver"
)

// DDL generation (driver.DDLGenerator, driver.SchemaDDL). BigQuery changes a
// table in place with a narrow set of ALTER TABLE actions: columns can be
// added (nullable), dropped, renamed, relaxed to NULLABLE, widened and given
// defaults and descriptions. Partitioning and clustering are fixed when the
// table is created. Tables have no indexes or check constraints; primary and
// foreign keys exist but are never enforced.
//
// Statements are previewed by the UI and run through the console, so the
// only server call made here is the routine type lookup DROP needs.

// design describes the structure editor. Option keys match what Describe
// puts into Table.Options, so an unchanged table round-trips cleanly.
var design = driver.TableDesign{
	Columns: true, ColumnComments: true, TableComment: true, PrimaryKey: true,
	Options: []driver.Field{
		{Key: "partition_by", Label: "Partition by", Type: driver.FieldText, Span: 3,
			Placeholder: "e.g. DATE(created_at)", Help: "Fixed when the table is created."},
		{Key: "cluster_by", Label: "Cluster by", Type: driver.FieldText, Span: 3,
			Placeholder: "up to four columns, e.g. customer_id, country", Help: "Fixed when the table is created."},
		{Key: "partition_expiration_days", Label: "Partition expiration (days)", Type: driver.FieldNumber, Span: 3},
		{Key: "require_partition_filter", Label: "Require a partition filter in queries", Type: driver.FieldBool, Span: 3},
		{Key: "expiration", Label: "Table expires at", Type: driver.FieldText, Span: 3, Placeholder: "e.g. 2030-01-01 00:00:00 UTC"},
		{Key: "labels", Label: "Labels", Type: driver.FieldText, Span: 3, Placeholder: "env=prod, team=data"},
	},
	Note: "BigQuery applies schema changes in place: types can only be widened, columns cannot become required, " +
		"new columns must allow NULL, and partitioning and clustering are fixed at creation. Keys are not enforced.",
}

func lit(s string) string { return strconv.Quote(s) }

func litOrNull(s string) string {
	if s == "" {
		return "NULL"
	}
	return lit(s)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

func quoteList(cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = quote(c)
	}
	return strings.Join(out, ", ")
}

// tableRef renders the quoted `project.dataset.table` path of an object.
func (c *conn) tableRef(ref driver.ObjectRef) (string, error) {
	if strings.TrimSpace(ref.Name) == "" {
		return "", errors.New("give the table a name")
	}
	project, dataset, err := c.datasetOf(ref.Schema)
	if err != nil {
		return "", err
	}
	return tablePath(project, dataset, ref.Name), nil
}

func isArray(typ string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(typ)), "ARRAY")
}

// columnDef renders a column for CREATE TABLE and ADD COLUMN. ARRAY columns
// are REPEATED and never NOT NULL.
func columnDef(col driver.Column) (string, error) {
	if strings.TrimSpace(col.Name) == "" {
		return "", errors.New("every column needs a name")
	}
	if strings.TrimSpace(col.Type) == "" {
		return "", fmt.Errorf("column %s needs a type", col.Name)
	}
	if col.AutoIncrement {
		return "", fmt.Errorf("BigQuery has no auto-increment columns; give %s a default such as GENERATE_UUID() instead", col.Name)
	}
	if col.Generated != "" {
		return "", fmt.Errorf("BigQuery has no generated columns (%s)", col.Name)
	}
	s := quote(col.Name) + " " + strings.TrimSpace(col.Type)
	if col.Collation != "" {
		s += " COLLATE " + lit(col.Collation)
	}
	if d := deref(col.Default); d != "" {
		s += " DEFAULT " + d
	}
	if !col.Nullable && !isArray(col.Type) {
		s += " NOT NULL"
	}
	if col.Comment != "" {
		s += " OPTIONS(description=" + lit(col.Comment) + ")"
	}
	return s, nil
}

// noIndexes rejects the parts of a definition BigQuery tables cannot have.
func noIndexes(def driver.TableDef) error {
	for _, ix := range def.Indexes {
		if !ix.Primary {
			return errors.New("BigQuery tables have no indexes; use clustering instead")
		}
	}
	if len(def.Checks) > 0 {
		return errors.New("BigQuery does not support check constraints")
	}
	return nil
}

// fkClause renders an unenforced foreign key. A referenced table without a
// dataset lives next to the table that references it.
func (c *conn) fkClause(table driver.ObjectRef, fk driver.ForeignKey) (string, error) {
	if len(fk.Columns) == 0 || len(fk.Columns) != len(fk.RefColumns) {
		return "", fmt.Errorf("foreign key %s needs the same number of columns on both sides", fk.Name)
	}
	ref := fk.RefTable
	if ref.Schema == "" {
		ref.Schema = table.Schema
	}
	path, err := c.tableRef(ref)
	if err != nil {
		return "", err
	}
	s := ""
	if fk.Name != "" {
		s = "CONSTRAINT " + quote(fk.Name) + " "
	}
	return s + "FOREIGN KEY (" + quoteList(fk.Columns) + ") REFERENCES " + path + "(" + quoteList(fk.RefColumns) + ") NOT ENFORCED", nil
}

func (c *conn) sameFK(table driver.ObjectRef, a, b driver.ForeignKey) bool {
	x, errA := c.fkClause(table, a)
	y, errB := c.fkClause(table, b)
	return errA == nil && errB == nil && strings.EqualFold(x, y)
}

func (c *conn) CreateTableSQL(def driver.TableDef) ([]string, error) {
	path, err := c.tableRef(def.Ref)
	if err != nil {
		return nil, err
	}
	if len(def.Columns) == 0 {
		return nil, errors.New("a table needs at least one column")
	}
	if err := noIndexes(def); err != nil {
		return nil, err
	}
	if err := uniqueNames(def.Columns); err != nil {
		return nil, err
	}
	var lines []string
	for _, col := range def.Columns {
		d, err := columnDef(col.Column)
		if err != nil {
			return nil, err
		}
		lines = append(lines, d)
	}
	if len(def.PrimaryKey) > 0 {
		lines = append(lines, "PRIMARY KEY ("+quoteList(def.PrimaryKey)+") NOT ENFORCED")
	}
	for _, fk := range def.ForeignKeys {
		l, err := c.fkClause(def.Ref, fk)
		if err != nil {
			return nil, err
		}
		lines = append(lines, l)
	}
	s := "CREATE TABLE " + path + " (\n  " + strings.Join(lines, ",\n  ") + "\n)"
	if p := strings.TrimSpace(def.Options["partition_by"]); p != "" {
		s += "\nPARTITION BY " + p
	}
	if cl := strings.TrimSpace(def.Options["cluster_by"]); cl != "" {
		s += "\nCLUSTER BY " + cl
	}
	opts, err := tableOptions(nil, "", def.Options, def.Comment)
	if err != nil {
		return nil, err
	}
	if len(opts) > 0 {
		s += "\nOPTIONS(\n  " + strings.Join(opts, ",\n  ") + "\n)"
	}
	return []string{s}, nil
}

func uniqueNames(cols []driver.ColumnDef) error {
	seen := map[string]bool{}
	for _, col := range cols {
		k := strings.ToLower(col.Name)
		if seen[k] {
			return fmt.Errorf("two columns are named %s", col.Name)
		}
		seen[k] = true
	}
	return nil
}

// AlterTableSQL diffs the current table against the desired definition.
// Columns are matched by OriginalName; unmatched current columns are dropped.
// BigQuery cannot reorder columns, so column order is ignored and new
// columns are added at the end.
func (c *conn) AlterTableSQL(from *driver.Table, to driver.TableDef) ([]string, error) {
	if from == nil {
		return nil, errors.New("current table definition is missing")
	}
	if from.Kind != "" && from.Kind != "table" {
		return nil, fmt.Errorf("only tables can be changed here; redefine the %s with CREATE OR REPLACE instead", strings.ReplaceAll(from.Kind, "_", " "))
	}
	target, err := c.tableRef(from.Ref)
	if err != nil {
		return nil, err
	}
	if err := noIndexes(to); err != nil {
		return nil, err
	}
	if err := uniqueNames(to.Columns); err != nil {
		return nil, err
	}
	if !sameExpr(from.Options["partition_by"], to.Options["partition_by"]) {
		return nil, errors.New("partitioning cannot be changed after a table is created; recreate the table with CREATE TABLE … AS SELECT")
	}
	if !sameExpr(from.Options["cluster_by"], to.Options["cluster_by"]) {
		return nil, errors.New("clustering cannot be changed in SQL; recreate the table or run bq update --clustering_fields")
	}
	alter := func(clauses ...string) string {
		if len(clauses) == 1 {
			return "ALTER TABLE " + target + " " + clauses[0]
		}
		return "ALTER TABLE " + target + "\n  " + strings.Join(clauses, ",\n  ")
	}

	current := map[string]driver.Column{}
	for _, col := range from.Columns {
		current[strings.ToLower(col.Name)] = col
	}
	kept := map[string]bool{}
	for _, col := range to.Columns {
		if col.OriginalName == "" {
			continue
		}
		k := strings.ToLower(col.OriginalName)
		if _, ok := current[k]; !ok {
			return nil, fmt.Errorf("column %s no longer exists; reload the structure", col.OriginalName)
		}
		if kept[k] {
			return nil, fmt.Errorf("column %s appears twice", col.OriginalName)
		}
		kept[k] = true
	}

	var out []string
	pkChanged := !equalFold(from.PrimaryKey, to.PrimaryKey)
	if pkChanged && len(from.PrimaryKey) > 0 {
		out = append(out, alter("DROP PRIMARY KEY"))
	}
	toFK := map[string]driver.ForeignKey{}
	for _, fk := range to.ForeignKeys {
		toFK[fk.Name] = fk
	}
	fromFK := map[string]driver.ForeignKey{}
	for _, fk := range from.ForeignKeys {
		fromFK[fk.Name] = fk
		if n, ok := toFK[fk.Name]; !ok || !c.sameFK(from.Ref, fk, n) {
			out = append(out, alter("DROP CONSTRAINT "+quote(fk.Name)))
		}
	}

	var drops, renames, adds []string
	for _, col := range from.Columns {
		if !kept[strings.ToLower(col.Name)] {
			drops = append(drops, "DROP COLUMN "+quote(col.Name))
		}
	}
	var changes []string
	for _, col := range to.Columns {
		if col.OriginalName == "" {
			if !col.Nullable && !isArray(col.Type) {
				return nil, fmt.Errorf("new column %s must allow NULL: BigQuery cannot add required columns to an existing table", col.Name)
			}
			d, err := columnDef(col.Column)
			if err != nil {
				return nil, err
			}
			adds = append(adds, "ADD COLUMN "+d)
			continue
		}
		old := current[strings.ToLower(col.OriginalName)]
		if strings.TrimSpace(col.Name) == "" {
			return nil, errors.New("every column needs a name")
		}
		if col.Name != old.Name {
			renames = append(renames, "RENAME COLUMN "+quote(old.Name)+" TO "+quote(col.Name))
		}
		// Changes address the column by its current name and run before
		// renames: the dry run checks the whole script against the table
		// as it is now.
		qn := quote(old.Name)
		if strings.TrimSpace(col.Type) == "" {
			return nil, fmt.Errorf("column %s needs a type", col.Name)
		}
		if normType(col.Type) != normType(old.Type) {
			changes = append(changes, alter("ALTER COLUMN "+qn+" SET DATA TYPE "+strings.TrimSpace(col.Type)))
		}
		if !strings.EqualFold(col.Collation, old.Collation) {
			return nil, fmt.Errorf("BigQuery cannot change the collation of the existing column %s", col.Name)
		}
		if col.AutoIncrement || col.Generated != "" {
			return nil, fmt.Errorf("BigQuery has no auto-increment or generated columns (%s)", col.Name)
		}
		if !isArray(col.Type) && !isArray(old.Type) && col.Nullable != old.Nullable {
			if !col.Nullable {
				return nil, fmt.Errorf("BigQuery cannot make the existing column %s required", col.Name)
			}
			changes = append(changes, alter("ALTER COLUMN "+qn+" DROP NOT NULL"))
		}
		if oldDef, newDef := deref(old.Default), deref(col.Default); oldDef != newDef {
			if newDef == "" {
				changes = append(changes, alter("ALTER COLUMN "+qn+" DROP DEFAULT"))
			} else {
				changes = append(changes, alter("ALTER COLUMN "+qn+" SET DEFAULT "+newDef))
			}
		}
		if col.Comment != old.Comment {
			changes = append(changes, alter("ALTER COLUMN "+qn+" SET OPTIONS(description="+litOrNull(col.Comment)+")"))
		}
	}
	if len(drops) > 0 {
		out = append(out, alter(drops...))
	}
	out = append(out, changes...)
	if len(renames) > 0 {
		out = append(out, alter(renames...))
	}
	if len(adds) > 0 {
		out = append(out, alter(adds...))
	}

	if pkChanged && len(to.PrimaryKey) > 0 {
		out = append(out, alter("ADD PRIMARY KEY ("+quoteList(to.PrimaryKey)+") NOT ENFORCED"))
	}
	for _, fk := range to.ForeignKeys {
		if old, ok := fromFK[fk.Name]; !ok || !c.sameFK(from.Ref, old, fk) {
			l, err := c.fkClause(from.Ref, fk)
			if err != nil {
				return nil, err
			}
			out = append(out, alter("ADD "+l))
		}
	}
	opts, err := tableOptions(from.Options, from.Comment, to.Options, to.Comment)
	if err != nil {
		return nil, err
	}
	if len(opts) > 0 {
		out = append(out, alter("SET OPTIONS("+strings.Join(opts, ", ")+")"))
	}
	if to.Ref.Name != "" && to.Ref.Name != from.Ref.Name {
		out = append(out, alter("RENAME TO "+quote(to.Ref.Name)))
	}
	return out, nil
}

func equalFold(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// sameExpr compares partitioning or clustering expressions, ignoring
// quoting, spacing and case.
func sameExpr(a, b string) bool {
	squash := func(s string) string {
		return strings.ToUpper(strings.Map(func(r rune) rune {
			if r == '`' || r == ' ' || r == '\t' || r == '\n' || r == '\r' {
				return -1
			}
			return r
		}, s))
	}
	return squash(a) == squash(b)
}

var typeAliases = map[string]string{
	"INT": "INT64", "INTEGER": "INT64", "BIGINT": "INT64", "SMALLINT": "INT64", "TINYINT": "INT64", "BYTEINT": "INT64",
	"FLOAT": "FLOAT64", "BOOLEAN": "BOOL", "DECIMAL": "NUMERIC", "BIGDECIMAL": "BIGNUMERIC",
}

// normType canonicalizes a type for comparison: upper case, aliases
// resolved, no spaces around punctuation. "numeric(10,2)" and the
// described "NUMERIC(10, 2)" are the same type.
func normType(t string) string {
	var toks []string
	word := func(w string) {
		w = strings.ToUpper(w)
		if a, ok := typeAliases[w]; ok {
			w = a
		}
		toks = append(toks, w)
	}
	for i := 0; i < len(t); {
		ch := t[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			i++
		case ch == '`':
			j := strings.IndexByte(t[i+1:], '`')
			if j < 0 {
				j = len(t) - i - 1
			}
			toks = append(toks, strings.ToUpper(t[i+1:i+1+j]))
			i += j + 2
		case ch == '_' || ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z':
			j := i
			for j < len(t) && (t[j] == '_' || t[j] >= '0' && t[j] <= '9' || t[j] >= 'a' && t[j] <= 'z' || t[j] >= 'A' && t[j] <= 'Z') {
				j++
			}
			word(t[i:j])
			i = j
		default:
			toks = append(toks, string(ch))
			i++
		}
	}
	var b strings.Builder
	isWord := func(s string) bool { return len(s) > 1 || s != "" && strings.IndexAny(s, "<>(),") < 0 }
	for i, tk := range toks {
		if i > 0 && isWord(tk) && isWord(toks[i-1]) {
			b.WriteByte(' ')
		}
		b.WriteString(tk)
	}
	return b.String()
}

// tableOptions renders the OPTIONS entries that differ between the current
// table (nil when creating) and the desired one.
func tableOptions(from map[string]string, fromComment string, to map[string]string, toComment string) ([]string, error) {
	var out []string
	if toComment != fromComment {
		out = append(out, "description="+litOrNull(toComment))
	}

	oldDays, _ := daysOpt(from["partition_expiration_days"])
	newDays, err := daysOpt(to["partition_expiration_days"])
	if err != nil {
		return nil, err
	}
	if newDays != oldDays {
		if newDays == 0 {
			out = append(out, "partition_expiration_days=NULL")
		} else {
			out = append(out, "partition_expiration_days="+strconv.FormatFloat(newDays, 'f', -1, 64))
		}
	}

	if a, b := boolOpt(from["require_partition_filter"]), boolOpt(to["require_partition_filter"]); a != b {
		out = append(out, "require_partition_filter="+strconv.FormatBool(b))
	}

	if a, b := strings.TrimSpace(from["expiration"]), strings.TrimSpace(to["expiration"]); a != b {
		if b == "" {
			out = append(out, "expiration_timestamp=NULL")
		} else {
			out = append(out, "expiration_timestamp="+timestampExpr(b))
		}
	}

	oldLabels, _ := parseLabels(from["labels"])
	newLabels, err := parseLabels(to["labels"])
	if err != nil {
		return nil, err
	}
	if !sameLabels(oldLabels, newLabels) {
		if len(newLabels) == 0 {
			out = append(out, "labels=NULL")
		} else {
			out = append(out, "labels="+labelsLiteral(newLabels))
		}
	}
	return out, nil
}

// daysOpt reads partition_expiration_days; 0 means none.
func daysOpt(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, errors.New("partition expiration must be a number of days")
	}
	return f, nil
}

func boolOpt(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// timestampExpr turns a plain timestamp into a TIMESTAMP literal and keeps
// expressions such as TIMESTAMP_ADD(CURRENT_TIMESTAMP(), INTERVAL 7 DAY).
func timestampExpr(s string) string {
	if strings.HasPrefix(strings.ToUpper(s), "TIMESTAMP") || strings.ContainsAny(s, "()") {
		return s
	}
	return "TIMESTAMP " + lit(s)
}

// parseLabels reads "key=value, key2=value2", the form Describe reports.
func parseLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			k, v, ok = strings.Cut(part, ":")
		}
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, errors.New("write labels as key=value pairs separated by commas")
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}

func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func labelsLiteral(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = "(" + lit(k) + ", " + lit(l[k]) + ")"
	}
	return "[" + strings.Join(pairs, ", ") + "]"
}

var dropKeyword = map[string]string{
	"": "TABLE", "table": "TABLE", "view": "VIEW", "materialized_view": "MATERIALIZED VIEW", "external_table": "EXTERNAL TABLE",
	"function": "FUNCTION", "procedure": "PROCEDURE", "table_function": "TABLE FUNCTION",
}

// DropObjectSQL drops a navigator object. BigQuery has no CASCADE for
// tables and routines, so cascade is ignored.
func (c *conn) DropObjectSQL(ref driver.ObjectRef, _ bool) ([]string, error) {
	path, err := c.tableRef(ref)
	if err != nil {
		return nil, err
	}
	kw, ok := dropKeyword[ref.Kind]
	if ref.Kind == "routine" {
		kw, err = c.routineKeyword(ref)
		if err != nil {
			return nil, err
		}
		ok = true
	}
	if !ok {
		return nil, fmt.Errorf("cannot drop objects of kind %q", ref.Kind)
	}
	return []string{"DROP " + kw + " " + path}, nil
}

// routineKeyword looks up whether a routine is a function, table function
// or procedure: each has its own DROP statement.
func (c *conn) routineKeyword(ref driver.ObjectRef) (string, error) {
	project, dataset, err := c.datasetOf(ref.Schema)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	md, err := c.client.DatasetInProject(project, dataset).Routine(ref.Name).Metadata(ctx)
	if err != nil {
		return "", mapError(err)
	}
	switch md.Type {
	case "PROCEDURE":
		return "PROCEDURE", nil
	case "TABLE_VALUED_FUNCTION":
		return "TABLE FUNCTION", nil
	}
	return "FUNCTION", nil // scalar and aggregate functions
}

func (c *conn) TruncateSQL(ref driver.ObjectRef) ([]string, error) {
	if ref.Kind != "" && ref.Kind != "table" {
		return nil, fmt.Errorf("only tables can be emptied, not a %s", strings.ReplaceAll(ref.Kind, "_", " "))
	}
	path, err := c.tableRef(ref)
	if err != nil {
		return nil, err
	}
	return []string{"TRUNCATE TABLE " + path}, nil
}

func (c *conn) RenameObjectSQL(ref driver.ObjectRef, newName string) ([]string, error) {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return nil, errors.New("enter a new name")
	}
	if ref.Kind != "" && ref.Kind != "table" {
		return nil, fmt.Errorf("BigQuery can only rename tables; recreate the %s under the new name", strings.ReplaceAll(ref.Kind, "_", " "))
	}
	path, err := c.tableRef(ref)
	if err != nil {
		return nil, err
	}
	return []string{"ALTER TABLE " + path + " RENAME TO " + quote(newName)}, nil
}

var errProjects = errors.New("BigQuery projects are created and deleted in the Google Cloud console; create a dataset instead")

func (c *conn) CreateDatabaseSQL(string, map[string]string) ([]string, error) {
	return nil, errProjects
}

func (c *conn) DropDatabaseSQL(string) ([]string, error) {
	return nil, errProjects
}

// schemaPath quotes a dataset name; "project.dataset" reaches other projects.
func (c *conn) schemaPath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("enter a dataset name")
	}
	project := c.project
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		project, name = name[:i], name[i+1:]
	}
	return quote(project + "." + name), nil
}

// CreateSchemaSQL creates a dataset in the connection's location, if one is
// configured (BigQuery's default is the US multi-region).
func (c *conn) CreateSchemaSQL(_ string, name string) ([]string, error) {
	path, err := c.schemaPath(name)
	if err != nil {
		return nil, err
	}
	s := "CREATE SCHEMA " + path
	if c.location != "" {
		s += " OPTIONS(location=" + lit(c.location) + ")"
	}
	return []string{s}, nil
}

// DropSchemaSQL drops a dataset; without cascade BigQuery refuses to drop
// a dataset that still holds tables.
func (c *conn) DropSchemaSQL(_ string, name string, cascade bool) ([]string, error) {
	path, err := c.schemaPath(name)
	if err != nil {
		return nil, err
	}
	s := "DROP SCHEMA " + path
	if cascade {
		s += " CASCADE"
	}
	return []string{s}, nil
}
