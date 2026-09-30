package export

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
)

// QuoteFor returns the identifier quoting rule of a dialect.
func QuoteFor(dialect string) func(string) string {
	switch dialect {
	case MySQL, BigQuery:
		return func(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
	case MSSQL:
		return func(s string) string { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }
	}
	return func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
}

// QualifyFor renders schema-qualified names; database names are left out so
// a dump restores into whatever database it is run against.
func QualifyFor(dialect string) func(driver.ObjectRef) string {
	q := QuoteFor(dialect)
	return func(r driver.ObjectRef) string {
		if r.Schema != "" && dialect != MySQL && dialect != SQLite {
			return q(r.Schema) + "." + q(r.Name)
		}
		return q(r.Name)
	}
}

// Dumper writes a SQL script that recreates a schema: tables (without
// foreign keys), their rows, then foreign keys, views, routines and
// triggers. It works for any engine whose Conn implements
// driver.DDLGenerator.
type Dumper struct {
	Conn    driver.Conn
	Session driver.Session // read-only console session used to stream rows
	Dialect string
	Product string // shown in the header
	// Progress reports the object being written and rows so far.
	Progress func(object string, rows int64)
}

// DumpSummary describes a finished dump.
type DumpSummary struct {
	Tables   int      `json:"tables"`
	Views    int      `json:"views"`
	Routines int      `json:"routines"`
	Rows     int64    `json:"rows"`
	Warnings []string `json:"warnings,omitempty"`
}

var nextval = regexp.MustCompile(`^nextval\('((?:[^']|'')+)'(::regclass)?\)$`)

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func colByName(t *driver.Table, name string) driver.Column {
	for _, c := range t.Columns {
		if c.Name == name {
			return c
		}
	}
	return driver.Column{}
}

var definerClause = regexp.MustCompile("(?i)\\s+DEFINER\\s*=\\s*(`[^`]*`|'[^']*'|[^\\s@]+)@(`[^`]*`|'[^']*'|[^\\s]+)")

func (d *Dumper) portable(r driver.ObjectRef) driver.ObjectRef {
	r.Database = ""
	if d.Dialect == SQLite {
		r.Schema = ""
	}
	return r
}

func (d *Dumper) emit(w io.Writer, stmts ...string) error {
	_, err := io.WriteString(w, sqlsplit.Join(d.Dialect, stmts))
	return err
}

// Dump writes the script for req to w.
func (d *Dumper) Dump(ctx context.Context, req driver.DumpRequest, w io.Writer) (*DumpSummary, error) {
	sum := &DumpSummary{}
	gen, _ := d.Conn.(driver.DDLGenerator)
	if req.Structure && gen == nil {
		return nil, errors.New("this engine cannot generate table definitions; export data only")
	}
	objs, err := d.Conn.Objects(ctx, req.Scope)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, o := range req.Objects {
		want[o.Name] = true
	}
	var tables, views, routines, triggers, prelude []driver.Object
	for _, o := range objs {
		if len(want) > 0 && !want[o.Name] {
			continue
		}
		if o.Extension != "" || o.OwnedBy == "identity" {
			continue // recreated by CREATE EXTENSION or by the identity column
		}
		switch o.Kind {
		case "table", "partitioned_table":
			tables = append(tables, o)
		case "view", "materialized_view":
			views = append(views, o)
		case "procedure", "function", "package", "routine":
			routines = append(routines, o)
		case "trigger", "event":
			triggers = append(triggers, o)
		case "extension", "type", "sequence":
			prelude = append(prelude, o)
		}
	}
	ref := func(o driver.Object) driver.ObjectRef {
		return driver.ObjectRef{Database: req.Scope.Database, Schema: req.Scope.Schema, Name: o.Name, Kind: o.Kind}
	}
	def, _ := d.Conn.(driver.Definer)
	definition := func(o driver.Object) (string, bool) {
		if def == nil {
			return "", false
		}
		s, err := def.Definition(ctx, ref(o))
		if err != nil || strings.TrimSpace(s) == "" {
			if err != nil && !errors.Is(err, driver.ErrNotSupported) {
				sum.Warnings = append(sum.Warnings, fmt.Sprintf("%s %s was skipped: %v", o.Kind, o.Name, err))
			}
			return "", false
		}
		s = strings.TrimSpace(s)
		if d.Dialect == MySQL {
			// Portable: no DEFINER account and no source database prefix.
			s = definerClause.ReplaceAllString(s, "")
			if db := req.Scope.Database; db != "" {
				s = strings.ReplaceAll(s, QuoteFor(MySQL)(db)+".", "")
			}
		}
		return strings.TrimSpace(strings.TrimSuffix(s, ";")), true
	}
	q := QualifyFor(d.Dialect)
	comment := func(s string) error { _, err := io.WriteString(w, "\n-- "+s+"\n"); return err }

	scope := req.Scope.Database
	if req.Scope.Schema != "" {
		scope = strings.Trim(scope+"."+req.Scope.Schema, ".")
	}
	fmt.Fprintf(w, "-- Rowsmith SQL dump\n-- Source: %s%s\n-- Created: %s\n", d.Product, map[bool]string{true: " · " + scope, false: ""}[scope != ""], time.Now().UTC().Format("2006-01-02 15:04:05 UTC"))
	switch d.Dialect {
	case MySQL:
		d.emit(w, "SET NAMES utf8mb4", "SET FOREIGN_KEY_CHECKS = 0", "SET UNIQUE_CHECKS = 0")
	case Postgres:
		d.emit(w, "SET client_encoding = 'UTF8'", "SET standard_conforming_strings = on")
	case SQLite:
		d.emit(w, "PRAGMA foreign_keys = OFF", "BEGIN")
	}

	// Describe every table once; partitions follow their parents.
	described := make([]*driver.Table, 0, len(tables))
	for _, o := range tables {
		t, err := d.Conn.Describe(ctx, ref(o))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", o.Name, err)
		}
		described = append(described, t)
	}
	sort.SliceStable(described, func(i, j int) bool {
		return described[i].Options["partition_of"] == "" && described[j].Options["partition_of"] != ""
	})

	if req.Structure {
		if len(prelude) > 0 {
			comment("Extensions, types and sequences")
			for _, kind := range []string{"extension", "type", "sequence"} {
				for _, o := range prelude {
					if o.Kind != kind {
						continue
					}
					if s, ok := definition(o); ok {
						if err := d.emit(w, s); err != nil {
							return nil, err
						}
					}
				}
			}
		}
		for _, t := range described {
			comment("Table " + t.Ref.Name)
			tdef := d.tableDef(t, false)
			stmts, err := gen.CreateTableSQL(tdef)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", t.Ref.Name, err)
			}
			if req.DropFirst {
				stmts = append([]string{d.dropTable(q(d.portable(t.Ref)))}, stmts...)
			}
			if err := d.emit(w, stmts...); err != nil {
				return nil, err
			}
			sum.Tables++
		}
	}

	if req.Data {
		for _, t := range described {
			if t.Kind == "partitioned_table" {
				continue // rows live in the partitions
			}
			n, err := d.tableData(ctx, t, req.BatchSize, w, sum)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", t.Ref.Name, err)
			}
			sum.Rows += n
		}
	}

	if req.Structure {
		wroteFK := false
		for _, t := range described {
			if len(t.ForeignKeys) == 0 || t.Options["partition_of"] != "" {
				continue // partitions inherit their parent's keys
			}
			from := *t
			from.Ref = d.portable(t.Ref)
			from.ForeignKeys = nil
			stmts, err := gen.AlterTableSQL(&from, d.tableDef(t, true))
			if err != nil {
				sum.Warnings = append(sum.Warnings, fmt.Sprintf("foreign keys of %s were skipped: %v", t.Ref.Name, err))
				continue
			}
			if !wroteFK {
				comment("Foreign keys")
				wroteFK = true
			}
			if err := d.emit(w, stmts...); err != nil {
				return nil, err
			}
		}

		defs := map[string]string{}
		var names []string
		for _, o := range views {
			if s, ok := definition(o); ok {
				defs[o.Name] = s
				names = append(names, o.Name)
			}
		}
		if len(names) > 0 {
			comment("Views")
		}
		for _, name := range orderViews(names, defs) {
			var stmts []string
			if req.DropFirst {
				stmts = append(stmts, d.dropView(q(d.portable(driver.ObjectRef{Schema: req.Scope.Schema, Name: name}))))
			}
			if err := d.emit(w, append(stmts, defs[name])...); err != nil {
				return nil, err
			}
			sum.Views++
		}

		dumped := map[string]bool{}
		for i, group := range [][]driver.Object{routines, triggers} {
			first := true
			for _, o := range group {
				s, ok := definition(o)
				if !ok {
					continue
				}
				dumped[o.Name] = true
				if first {
					comment(map[int]string{0: "Routines", 1: "Triggers and events"}[i])
					first = false
				}
				var err error
				if d.Dialect == MySQL {
					_, err = io.WriteString(w, "DELIMITER ;;\n"+s+";;\nDELIMITER ;\n")
				} else {
					err = d.emit(w, s)
				}
				if err != nil {
					return nil, err
				}
				sum.Routines++
			}
		}
		// Engines that list triggers per table (PostgreSQL) keep the full
		// CREATE TRIGGER statement in the description.
		first := true
		for _, t := range described {
			if t.Options["partition_of"] != "" {
				continue // cloned from the parent
			}
			for _, tr := range t.Triggers {
				st := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(tr.Statement), ";"))
				if dumped[tr.Name] || !strings.HasPrefix(strings.ToUpper(st), "CREATE") {
					continue
				}
				if first {
					comment("Triggers")
					first = false
				}
				if err := d.emit(w, st); err != nil {
					return nil, err
				}
				sum.Routines++
			}
		}
	}

	switch d.Dialect {
	case MySQL:
		d.emit(w, "SET FOREIGN_KEY_CHECKS = 1", "SET UNIQUE_CHECKS = 1")
	case SQLite:
		d.emit(w, "COMMIT", "PRAGMA foreign_keys = ON")
	}
	if len(sum.Warnings) > 0 {
		comment("Warnings")
		for _, s := range sum.Warnings {
			io.WriteString(w, "-- "+s+"\n")
		}
	}
	_, err = io.WriteString(w, "\n-- End of dump\n")
	return sum, err
}

func (d *Dumper) tableDef(t *driver.Table, withFKs bool) driver.TableDef {
	def := driver.TableDef{Ref: d.portable(t.Ref), PrimaryKey: t.PrimaryKey, Indexes: t.Indexes, Checks: t.Checks,
		Comment: t.Comment, Options: t.Options}
	for _, c := range t.Columns {
		def.Columns = append(def.Columns, driver.ColumnDef{Column: c, OriginalName: c.Name})
	}
	if withFKs {
		for _, fk := range t.ForeignKeys {
			fk.RefTable.Database = ""
			if d.Dialect == SQLite {
				fk.RefTable.Schema = ""
			}
			def.ForeignKeys = append(def.ForeignKeys, fk)
		}
	}
	return def
}

func (d *Dumper) dropTable(name string) string {
	switch d.Dialect {
	case Oracle:
		return "BEGIN EXECUTE IMMEDIATE 'DROP TABLE " + strings.ReplaceAll(name, "'", "''") + " CASCADE CONSTRAINTS'; EXCEPTION WHEN OTHERS THEN IF SQLCODE != -942 THEN RAISE; END IF; END;"
	case Postgres:
		return "DROP TABLE IF EXISTS " + name + " CASCADE"
	}
	return "DROP TABLE IF EXISTS " + name
}

func (d *Dumper) dropView(name string) string {
	if d.Dialect == Oracle {
		return "BEGIN EXECUTE IMMEDIATE 'DROP VIEW " + strings.ReplaceAll(name, "'", "''") + "'; EXCEPTION WHEN OTHERS THEN IF SQLCODE != -942 THEN RAISE; END IF; END;"
	}
	return "DROP VIEW IF EXISTS " + name
}

// tableData streams a table's rows as INSERT statements.
func (d *Dumper) tableData(ctx context.Context, t *driver.Table, batch int, w io.Writer, sum *DumpSummary) (int64, error) {
	var cols []string
	alwaysIdentity, identity := false, []string{}
	for _, c := range t.Columns {
		if c.Generated != "" {
			continue // computed from the other columns
		}
		cols = append(cols, c.Name)
		if c.AutoIncrement {
			identity = append(identity, c.Name)
		}
		if c.Default != nil && strings.HasPrefix(strings.ToUpper(*c.Default), "GENERATED ALWAYS") {
			alwaysIdentity = true
		}
	}
	if len(cols) == 0 {
		return 0, nil
	}
	query, args, err := d.selectAll(ctx, t, cols)
	if err != nil {
		return 0, err
	}
	target := d.portable(t.Ref)
	qualified := QualifyFor(d.Dialect)(target)
	opts := Options{Format: SQL, Dialect: d.Dialect, Table: target, BatchRows: batch, QuoteIdent: QuoteFor(d.Dialect), Qualify: QualifyFor(d.Dialect)}
	if alwaysIdentity && d.Dialect == Postgres {
		opts.Values = "OVERRIDING SYSTEM VALUE VALUES"
	}
	iw := newInsertWriter(w, opts)
	s := &Sink{W: iw, Interval: 250 * time.Millisecond}
	name := t.Ref.Name
	if d.Progress != nil {
		s.Progress = func(n int64) { d.Progress(name, n) }
		d.Progress(name, 0)
	}
	io.WriteString(w, "\n-- Data for "+t.Ref.Name+"\n")
	mssqlIdentity := d.Dialect == MSSQL && len(identity) > 0
	if mssqlIdentity {
		d.emit(w, "SET IDENTITY_INSERT "+qualified+" ON")
	}
	err = d.Session.Execute(ctx, query, driver.ExecOptions{Params: args, ReadOnly: true, StopOnError: true}, s)
	if err == nil {
		err = s.Err()
	}
	if cerr := iw.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return s.Count(), err
	}
	if iw.Stats.TooLarge > 0 {
		sum.Warnings = append(sum.Warnings, fmt.Sprintf("%d binary values in %s were too large for an Oracle literal and were written as NULL", iw.Stats.TooLarge, t.Ref.Name))
	}
	if mssqlIdentity {
		d.emit(w, "SET IDENTITY_INSERT "+qualified+" OFF")
	}
	if s.Count() > 0 {
		if err := d.emit(w, IdentityResets(d.Dialect, t, qualified, identity)...); err != nil {
			return s.Count(), err
		}
	}
	if d.Progress != nil {
		d.Progress(name, s.Count())
	}
	return s.Count(), nil
}

// IdentityResets returns statements that move auto-numbering past rows
// loaded with explicit keys (PostgreSQL sequences, Oracle identities).
// qualified is the table name as the statements should reference it.
func IdentityResets(dialect string, t *driver.Table, qualified string, cols []string) []string {
	q := QuoteFor(dialect)
	var out []string
	for _, c := range cols {
		col := colByName(t, c)
		if !col.AutoIncrement {
			continue
		}
		switch dialect {
		case Postgres:
			seq := "pg_get_serial_sequence(" + stringLit(Postgres, qualified, nil) + ", " + stringLit(Postgres, c, nil) + ")"
			if m := nextval.FindStringSubmatch(deref(col.Default)); m != nil {
				seq = stringLit(Postgres, m[1], nil) // serial column: its own sequence
			}
			out = append(out, fmt.Sprintf("SELECT setval(%s, MAX(%s)) FROM %s HAVING MAX(%s) IS NOT NULL", seq, q(c), qualified, q(c)))
		case Oracle:
			out = append(out, "ALTER TABLE "+qualified+" MODIFY ("+q(c)+" GENERATED BY DEFAULT ON NULL AS IDENTITY (START WITH LIMIT VALUE))")
		}
	}
	return out
}

// selectAll reads raw columns (no display conversions, so geometry keeps its
// SRID) in primary-key order for a deterministic dump.
func (d *Dumper) selectAll(_ context.Context, t *driver.Table, cols []string) (string, []any, error) {
	q := QuoteFor(d.Dialect)
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = q(c)
	}
	s := "SELECT " + strings.Join(names, ", ") + " FROM " + QualifyFor(d.Dialect)(t.Ref)
	if len(t.PrimaryKey) > 0 {
		keys := make([]string, len(t.PrimaryKey))
		for i, k := range t.PrimaryKey {
			keys[i] = q(k)
		}
		s += " ORDER BY " + strings.Join(keys, ", ")
	}
	return s, nil, nil
}

// orderViews puts views after the views their definitions mention.
func orderViews(names []string, defs map[string]string) []string {
	sort.Strings(names)
	mentions := func(def, name string) bool {
		re := regexp.MustCompile(`(?i)(^|[^\w$])` + regexp.QuoteMeta(name) + `($|[^\w$])`)
		return re.MatchString(def)
	}
	var out []string
	done := map[string]bool{}
	for len(out) < len(names) {
		progress := false
		for _, n := range names {
			if done[n] {
				continue
			}
			ready := true
			for _, m := range names {
				if m != n && !done[m] && mentions(defs[n], m) {
					ready = false
					break
				}
			}
			if ready {
				out = append(out, n)
				done[n] = true
				progress = true
			}
		}
		if !progress { // a cycle or a false match: keep name order
			for _, n := range names {
				if !done[n] {
					out = append(out, n)
					done[n] = true
				}
			}
		}
	}
	return slices.Clip(out)
}
