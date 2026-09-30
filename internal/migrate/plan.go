package migrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"rowsmith/internal/driver"
)

// Endpoint is one side of a migration.
type Endpoint struct {
	Conn   driver.Conn
	Info   driver.Info
	Server *driver.ServerInfo
	Scope  driver.Scope
}

func (e Endpoint) engine() string { return e.Info.ID }

// Options shape a migration.
type Options struct {
	NameCase     string `json:"nameCase"`     // keep, lower or upper
	Existing     string `json:"existing"`     // when a target table exists: fail, replace or keep
	ExistingRows string `json:"existingRows"` // for kept tables: append or replace
	Data         bool   `json:"data"`         // copy rows (false: structure only)
	Indexes      bool   `json:"indexes"`
	ForeignKeys  bool   `json:"foreignKeys"`
	Objects      bool   `json:"objects"` // views, routines and triggers (same engine only)
	Verify       string `json:"verify"`  // counts or contents
	StopOnError  bool   `json:"stopOnError"`
}

func (o *Options) normalize() {
	if o.NameCase != "lower" && o.NameCase != "upper" {
		o.NameCase = "keep"
	}
	if o.Existing != "replace" && o.Existing != "keep" {
		o.Existing = "fail"
	}
	if o.ExistingRows != "replace" {
		o.ExistingRows = "append"
	}
	if o.Verify != "counts" {
		o.Verify = "contents"
	}
}

// Plan is what will be copied, as the person reviewed it.
type Plan struct {
	SourceEngine string              `json:"sourceEngine"`
	TargetEngine string              `json:"targetEngine"`
	SameEngine   bool                `json:"sameEngine"`
	Options      Options             `json:"options"`
	Tables       []*TablePlan        `json:"tables"`
	Objects      []ObjectNote        `json:"objects"` // definitions copied as they are (same engine)
	Skipped      []ObjectNote        `json:"skipped"` // not copied, and why
	Warnings     []string            `json:"warnings"`
	TypeChoices  []string            `json:"typeChoices"` // the target's column types, for the editor
	Design       *driver.TableDesign `json:"design,omitempty"`
}

type ObjectNote struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Reason string `json:"reason,omitempty"`
}

type TablePlan struct {
	Source      driver.ObjectRef  `json:"source"`
	Target      string            `json:"target"`
	Include     bool              `json:"include"`
	Exists      bool              `json:"exists"`
	Rows        *int64            `json:"rows,omitempty"`
	Columns     []*ColumnPlan     `json:"columns"`
	PrimaryKey  []string          `json:"primaryKey"` // source column names
	Indexes     []*IndexPlan      `json:"indexes"`
	ForeignKeys []*FKPlan         `json:"foreignKeys"`
	Checks      []driver.Check    `json:"checks"` // same engine only
	Comment     string            `json:"comment,omitempty"`
	Options     map[string]string `json:"options,omitempty"` // same engine only
	Notes       []string          `json:"notes"`
}

type ColumnPlan struct {
	Source          string   `json:"source"`
	SourceType      string   `json:"sourceType"`
	Target          string   `json:"target"`
	Type            string   `json:"type"`
	Nullable        bool     `json:"nullable"`
	Default         *string  `json:"default,omitempty"`
	AutoIncrement   bool     `json:"autoIncrement,omitempty"`
	Generated       string   `json:"generated,omitempty"` // same engine only; not copied
	GeneratedStored bool     `json:"generatedStored,omitempty"`
	OnUpdate        string   `json:"onUpdate,omitempty"`
	Comment         string   `json:"comment,omitempty"`
	Collation       string   `json:"collation,omitempty"`
	SRID            int      `json:"srid,omitempty"`
	Values          []string `json:"values,omitempty"` // allowed values, checked on engines without enums
	NoCopy          bool     `json:"noCopy,omitempty"` // the engine fills it (rowversion)
	Include         bool     `json:"include"`
	Notes           []string `json:"notes"`
	Lossy           bool     `json:"lossy,omitempty"`
}

type IndexPlan struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"` // source column names, or expressions
	Unique  bool     `json:"unique"`
	Type    string   `json:"type,omitempty"`
	Where   string   `json:"where,omitempty"`
	Lengths []int    `json:"lengths,omitempty"`
	Desc    []bool   `json:"desc,omitempty"`
	Include bool     `json:"include"`
	Note    string   `json:"note,omitempty"`
}

type FKPlan struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns"`    // source column names
	RefTable   string   `json:"refTable"`   // source table name
	RefColumns []string `json:"refColumns"` // source column names
	OnDelete   string   `json:"onDelete,omitempty"`
	OnUpdate   string   `json:"onUpdate,omitempty"`
	Include    bool     `json:"include"`
	Note       string   `json:"note,omitempty"`
}

// MongoExtra holds fields of documents that were not in the sampled schema.
const MongoExtra = driver.OtherFields

const maxTables = 1000

// CanTarget reports why rows cannot be loaded into an engine, if so.
func CanTarget(dst Endpoint) error {
	if _, ok := dst.Conn.(driver.DDLGenerator); !ok {
		return fmt.Errorf("Rowsmith cannot create tables in %s", dst.Info.Name)
	}
	if _, ok := dst.Conn.(driver.BulkImporter); !ok {
		return fmt.Errorf("Rowsmith cannot load rows into %s yet; it can be a source", dst.Info.Name)
	}
	return nil
}

func targetOf(dst Endpoint) target {
	t := target{Engine: dst.engine()}
	if dst.Server != nil {
		v := dst.Server.Version
		if i := strings.IndexAny(v, ". "); i > 0 {
			v = v[:i]
		}
		t.Version, _ = strconv.Atoi(v)
		for k := range dst.Server.Extras {
			if strings.EqualFold(k, "PostGIS") {
				t.PostGIS = true
			}
		}
	}
	return t
}

// BuildPlan describes the source tables and proposes how to create them on
// the target.
func BuildPlan(ctx context.Context, src, dst Endpoint, opts Options) (*Plan, error) {
	if err := CanTarget(dst); err != nil {
		return nil, err
	}
	if _, ok := src.Conn.(driver.BrowseQuerier); !ok {
		return nil, fmt.Errorf("Rowsmith cannot read whole tables from %s", src.Info.Name)
	}
	if opts.NameCase == "" {
		opts.NameCase = DefaultCase(src.engine(), dst.engine())
	}
	opts.normalize()
	p := &Plan{SourceEngine: src.engine(), TargetEngine: dst.engine(), SameEngine: family(src.engine()) == family(dst.engine()),
		Options: opts, Tables: []*TablePlan{}, Objects: []ObjectNote{}, Skipped: []ObjectNote{}, Warnings: []string{},
		TypeChoices: dst.Info.Types, Design: dst.Info.Design}

	objs, err := src.Conn.Objects(ctx, src.Scope)
	if err != nil {
		return nil, err
	}
	var tables []driver.Object
	for _, o := range objs {
		switch {
		case o.Extension != "":
			p.Skipped = append(p.Skipped, ObjectNote{o.Name, o.Kind, "created by the " + o.Extension + " extension"})
		case o.Kind == "table" || o.Kind == "partitioned_table" || o.Kind == "collection" || o.Kind == "timeseries":
			tables = append(tables, o)
		case o.OwnedBy != "":
			// Sequences behind identity and serial columns come with the column.
		case p.SameEngine && (o.Kind == "type" || o.Kind == "sequence"):
			p.Objects = append(p.Objects, ObjectNote{Name: o.Name, Kind: o.Kind}) // tables may need them
		case p.SameEngine && opts.Objects && isDefinable(o.Kind):
			p.Objects = append(p.Objects, ObjectNote{Name: o.Name, Kind: o.Kind})
		case isDefinable(o.Kind):
			reason := "written in " + src.Info.Name + " SQL, which " + dst.Info.Name + " does not run"
			if p.SameEngine {
				reason = "turn on “Views, routines and triggers” to copy it"
			}
			p.Skipped = append(p.Skipped, ObjectNote{o.Name, o.Kind, reason})
		}
	}
	if len(tables) > maxTables {
		return nil, fmt.Errorf("this scope has %d tables; migrate at most %d at a time", len(tables), maxTables)
	}

	existing := map[string]bool{}
	if dobjs, err := dst.Conn.Objects(ctx, dst.Scope); err == nil {
		for _, o := range dobjs {
			existing[strings.ToLower(o.Name)] = true
		}
	}

	described := make([]*driver.Table, len(tables))
	errs := make([]error, len(tables))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, o := range tables {
		wg.Add(1)
		go func(i int, o driver.Object) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ref := driver.ObjectRef{Database: src.Scope.Database, Schema: src.Scope.Schema, Name: o.Name, Kind: o.Kind}
			described[i], errs[i] = src.Conn.Describe(ctx, ref)
		}(i, o)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	t := targetOf(dst)
	names := map[string]int{}
	anyGeo := false
	for i, o := range tables {
		if errs[i] != nil {
			p.Skipped = append(p.Skipped, ObjectNote{o.Name, o.Kind, "could not be read: " + errs[i].Error()})
			continue
		}
		st := described[i]
		if parent := st.Options["partition_of"]; parent != "" {
			p.Skipped = append(p.Skipped, ObjectNote{o.Name, "partition", "its rows are copied with " + parent})
			continue
		}
		tp := planTable(src, dst, t, st, opts, names)
		tp.Rows = o.Rows
		if tp.Rows == nil {
			tp.Rows = st.RowEstimate
		}
		tp.Exists = existing[strings.ToLower(tp.Target)]
		for _, c := range tp.Columns {
			anyGeo = anyGeo || strings.Contains(strings.ToLower(c.SourceType), "geo") || strings.Contains(strings.ToLower(c.SourceType), "point")
		}
		p.Tables = append(p.Tables, tp)
	}
	sort.SliceStable(p.Tables, func(i, j int) bool { return p.Tables[i].Source.Name < p.Tables[j].Source.Name })
	if !p.SameEngine {
		matchKeyTypes(p.Tables)
	}
	if family(dst.engine()) == Postgres && anyGeo && !t.PostGIS {
		p.Warnings = append(p.Warnings, "PostGIS is not installed on the target, so geometry columns are stored as GeoJSON. Run CREATE EXTENSION postgis there first to keep them as shapes.")
	}
	if !p.SameEngine && len(p.Skipped) > 0 {
		n := 0
		for _, s := range p.Skipped {
			if isDefinable(s.Kind) {
				n++
			}
		}
		if n > 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%d views, routines or triggers are not copied: their SQL is specific to %s.", n, src.Info.Name))
		}
	}
	return p, nil
}

func isDefinable(kind string) bool {
	switch kind {
	case "view", "materialized_view", "procedure", "function", "routine", "trigger", "event", "sequence", "type", "package", "synonym":
		return true
	}
	return false
}

func planTable(src, dst Endpoint, t target, st *driver.Table, opts Options, names map[string]int) *TablePlan {
	sEng, dEng := src.engine(), dst.engine()
	same := family(sEng) == family(dEng)
	design := dst.Info.Design
	if design == nil {
		design = &driver.TableDesign{}
	}
	tp := &TablePlan{Source: st.Ref, Target: clipIdent(dEng, applyCase(st.Ref.Name, opts.NameCase)), Include: true,
		PrimaryKey: slices.Clone(st.PrimaryKey), Indexes: []*IndexPlan{}, ForeignKeys: []*FKPlan{}, Checks: []driver.Check{}, Notes: []string{}}
	if tp.PrimaryKey == nil {
		tp.PrimaryKey = []string{}
	}
	if design.TableComment {
		tp.Comment = st.Comment
	}
	if same {
		tp.Options = map[string]string{}
		for k, v := range st.Options {
			if !strings.HasPrefix(k, "partition") {
				tp.Options[k] = v
			}
		}
		if st.Options["partition_by"] != "" || st.Kind == "partitioned_table" {
			tp.Notes = append(tp.Notes, "partitioning is not recreated; all rows go into one table")
		}
		if design.Checks {
			tp.Checks = slices.Clone(st.Checks)
		}
	} else if len(st.Checks) > 0 {
		tp.Notes = append(tp.Notes, fmt.Sprintf("%d check constraints were left out: their expressions are %s SQL", len(st.Checks), src.Info.Name))
	}
	if len(st.Triggers) > 0 && !same {
		tp.Notes = append(tp.Notes, fmt.Sprintf("%d triggers were left out", len(st.Triggers)))
	}

	indexed := map[string]bool{}
	for _, c := range st.PrimaryKey {
		indexed[c] = true
	}
	for _, ix := range st.Indexes {
		for _, c := range ix.Columns {
			indexed[c] = true
		}
	}
	colNames := map[string]bool{}
	for _, c := range st.Columns {
		colNames[strings.ToLower(c.Name)] = true
	}
	for _, c := range st.Columns {
		cp := &ColumnPlan{Source: c.Name, SourceType: c.Type, Target: clipIdent(dEng, applyCase(c.Name, opts.NameCase)), Nullable: c.Nullable,
			Include: true, Notes: []string{}}
		k := canonOf(sEng, c)
		if same {
			cp.Type = c.Type
			if family(sEng) == MySQL && c.Kind == driver.KindGeometry && c.SRID > 0 && !strings.Contains(strings.ToLower(c.Type), "srid") {
				if sEng == MariaDB {
					cp.Type += fmt.Sprintf(" REF_SYSTEM_ID=%d", c.SRID)
				} else {
					cp.Type += fmt.Sprintf(" SRID %d", c.SRID)
				}
			}
			cp.Default, cp.AutoIncrement = c.Default, c.AutoIncrement
			if c.AutoIncrement && c.Default != nil && strings.HasPrefix(strings.ToLower(*c.Default), "nextval(") {
				cp.Default = nil // serial: its sequence is not copied, so it becomes an identity column
				cp.Notes = append(cp.Notes, "serial becomes an identity column")
			}
			cp.Generated, cp.GeneratedStored = c.Generated, c.GeneratedStored
			cp.OnUpdate, cp.Collation, cp.SRID = c.OnUpdate, c.Collation, c.SRID
			if family(sEng) == MSSQL && (c.BaseType == "rowversion" || c.BaseType == "timestamp") {
				cp.NoCopy = true
				cp.Notes = append(cp.Notes, "filled in by SQL Server; not copied")
			}
		} else {
			if c.AutoIncrement && k.T == "int" && k.Unsigned && k.Bits == 64 && family(dEng) != MySQL {
				// Identity columns must be integers; numbering never nears 2^63.
				k.Unsigned = false
				cp.Notes = append(cp.Notes, "auto-numbered, so stored as a signed bigint")
			}
			m := typeFor(t, k, use{Indexed: indexed[c.Name]})
			cp.Type, cp.Notes, cp.Lossy = m.Type, append(cp.Notes, m.Notes...), m.Lossy
			cp.SRID = k.SRID
			if k.T == "enum" && !strings.HasPrefix(strings.ToLower(m.Type), "enum") {
				cp.Values = slices.Clone(k.Values)
			}
			if family(sEng) == MSSQL && k.T == "geometry" && !k.Geog && family(dEng) != MSSQL {
				cp.Notes = append(cp.Notes, "SQL Server keeps a SRID on each shape; set it in the type (e.g. geometry(Geometry,4326)) if they all share one")
			}
			if c.AutoIncrement {
				if design.AutoIncrement && k.T == "int" {
					cp.AutoIncrement = true
				} else {
					cp.Notes = append(cp.Notes, "auto-numbering is not recreated")
				}
			}
			if c.Default != nil && !c.AutoIncrement {
				d, note := translateDefault(sEng, dEng, *c.Default, k, m.Type)
				cp.Default = d
				if note != "" {
					cp.Notes = append(cp.Notes, note)
				}
			}
			if c.Generated != "" {
				cp.Notes = append(cp.Notes, "computed from "+c.Generated+" on the source; copied as plain values")
			}
			if design.ColumnComments {
				cp.Comment = c.Comment
			}
		}
		if dEng == MongoDB {
			cp.Nullable = true
			if len(st.PrimaryKey) == 1 && st.PrimaryKey[0] == c.Name && c.Name != "_id" {
				cp.Target = "_id"
				cp.Notes = append(cp.Notes, "becomes the document _id")
			}
		}
		if sEng == MongoDB && dEng != MongoDB && c.Name == "_id" && !colNames["id"] {
			cp.Target = applyCase("id", opts.NameCase)
		}
		tp.Columns = append(tp.Columns, cp)
	}
	if sEng == MongoDB && dEng != MongoDB {
		m := typeFor(t, canon{T: "object"}, use{})
		tp.Columns = append(tp.Columns, &ColumnPlan{Source: MongoExtra, SourceType: "other fields", Target: applyCase("_extra", opts.NameCase),
			Type: m.Type, Nullable: true, Include: true, Notes: []string{"fields that were not in the sampled documents, as JSON"}})
	}
	if sEng == MongoDB && dEng == MongoDB {
		tp.Columns = append(tp.Columns, &ColumnPlan{Source: MongoExtra, SourceType: "other fields", Target: MongoExtra, Type: "object",
			Nullable: true, Include: true, Notes: []string{"fields that were not in the sampled documents are copied as they are"}})
	}
	if dEng == MongoDB {
		tp.PrimaryKey = []string{}
		if len(st.ForeignKeys) > 0 {
			tp.Notes = append(tp.Notes, "foreign keys have no MongoDB equivalent and were left out")
		}
	}

	byName := map[string]driver.Column{}
	for _, c := range st.Columns {
		byName[c.Name] = c
	}
	for _, ix := range st.Indexes {
		if ix.Primary {
			continue
		}
		ip := &IndexPlan{Name: ix.Name, Columns: slices.Clone(ix.Columns), Unique: ix.Unique, Where: ix.Where, Include: opts.Indexes}
		if same {
			ip.Lengths, ip.Desc, ip.Type = slices.Clone(ix.Lengths), slices.Clone(ix.Desc), ix.Type
		} else {
			ip.Desc = slices.Clone(ix.Desc)
		}
		geo := false
		expression := len(ix.Columns) == 0
		for _, c := range ix.Columns {
			if _, isCol := byName[c]; !isCol || strings.HasPrefix(c, "(") {
				expression = true
			}
			if byName[c].Kind == driver.KindGeometry {
				geo = true
			}
		}
		if expression && !same {
			ip.Include, ip.Note = false, "an expression index; create it on the target by hand"
		}
		if ix.Where != "" && (!same || !design.PartialIndexes) {
			ip.Include, ip.Note, ip.Where = false, "a partial index (WHERE "+ix.Where+"); create it on the target by hand", ""
		}
		if dEng == MongoDB && len(ix.Columns) == 1 && ix.Columns[0] == "_id" {
			continue
		}
		if sEng == MongoDB && ix.Name == "_id_" {
			continue
		}
		typ, note, ok := indexType(sEng, dEng, ix, geo, design.IndexTypes)
		if !ok {
			ip.Include, ip.Note = false, note
		} else if !same {
			ip.Type = typ
		}
		if dEng == MongoDB {
			ip.Name = ""
		} else {
			ip.Name = uniqueNames(names, tp.Target, clipIdent(dEng, applyCase(ix.Name, opts.NameCase)), dEng)
		}
		tp.Indexes = append(tp.Indexes, ip)
	}
	if dEng != MongoDB && design.ForeignKeys {
		for _, fk := range st.ForeignKeys {
			tp.ForeignKeys = append(tp.ForeignKeys, &FKPlan{
				Name: uniqueNames(names, tp.Target, clipIdent(dEng, applyCase(fk.Name, opts.NameCase)), dEng), Columns: slices.Clone(fk.Columns),
				RefTable: fk.RefTable.Name, RefColumns: slices.Clone(fk.RefColumns),
				OnDelete: fkAction(fk.OnDelete, design.FKActions), OnUpdate: fkAction(fk.OnUpdate, design.FKActions), Include: opts.ForeignKeys})
		}
	}
	if family(dEng) == MySQL && !same {
		fitMySQLRow(tp)
	}
	return tp
}

// matchKeyTypes gives foreign key columns the type of the columns they
// reference: a key into an auto-numbered bigint must be a bigint too.
func matchKeyTypes(tables []*TablePlan) {
	cols := map[string]*ColumnPlan{}
	for _, t := range tables {
		for _, c := range t.Columns {
			cols[t.Source.Name+"\x00"+c.Source] = c
		}
	}
	for _, t := range tables {
		for _, fk := range t.ForeignKeys {
			for i, name := range fk.Columns {
				if i >= len(fk.RefColumns) {
					break
				}
				child, parent := cols[t.Source.Name+"\x00"+name], cols[fk.RefTable+"\x00"+fk.RefColumns[i]]
				if child == nil || parent == nil || child.Type == parent.Type || child.SourceType != parent.SourceType {
					continue
				}
				child.Type = parent.Type
				child.Notes = append(child.Notes, "same type as "+fk.RefTable+"."+parent.Source+", which it references")
			}
		}
	}
}

// fitMySQLRow keeps a MySQL table under the 65,535-byte row limit by
// turning the widest unindexed varchar columns into text.
func fitMySQLRow(tp *TablePlan) {
	width := func() int {
		n := 0
		for _, c := range tp.Columns {
			if p, _ := typeParams(c.Type); strings.HasPrefix(c.Type, "varchar") && p > 0 {
				n += p*4 + 2
			}
		}
		return n
	}
	for width() > 65000 {
		var widest *ColumnPlan
		for _, c := range tp.Columns {
			if p, _ := typeParams(c.Type); strings.HasPrefix(c.Type, "varchar") && p > 255 && (widest == nil || p > mustParam(widest.Type)) {
				if !slices.Contains(tp.PrimaryKey, c.Source) {
					widest = c
				}
			}
		}
		if widest == nil {
			return
		}
		widest.Type = "text"
		widest.Notes = append(widest.Notes, "stored as text to keep the row under MySQL's size limit")
	}
}

func mustParam(t string) int { p, _ := typeParams(t); return p }

// ErrNothing is returned when a plan copies no tables.
var ErrNothing = errors.New("choose at least one table to migrate")
