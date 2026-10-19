package cassandra

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
)

// schemaColumn is a row of system_schema.columns.
type schemaColumn struct {
	name, kind, typ, order string
	position               int
}

func (c *conn) schemaColumns(ctx context.Context, ks, table string) ([]schemaColumn, error) {
	iter := c.query(ctx, "SELECT column_name, kind, type, clustering_order, position FROM system_schema.columns WHERE keyspace_name = ? AND table_name = ?", ks, table).Iter()
	var out []schemaColumn
	var sc schemaColumn
	for iter.Scan(&sc.name, &sc.kind, &sc.typ, &sc.order, &sc.position) {
		out = append(out, sc)
	}
	if err := iter.Close(); err != nil {
		return nil, mapError(err)
	}
	// Partition key, clustering columns, static then regular columns.
	rank := map[string]int{"partition_key": 0, "clustering": 1, "static": 2, "regular": 3}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].kind] != rank[out[j].kind] {
			return rank[out[i].kind] < rank[out[j].kind]
		}
		if out[i].kind == "partition_key" || out[i].kind == "clustering" {
			return out[i].position < out[j].position
		}
		return out[i].name < out[j].name
	})
	return out, nil
}

func column(sc schemaColumn) driver.Column {
	col := driver.Column{Name: sc.name, Type: sc.typ, Kind: kindOfType(sc.typ), Nullable: sc.kind == "regular" || sc.kind == "static"}
	col.BaseType = strings.ToLower(sc.typ)
	if i := strings.IndexAny(col.BaseType, "<("); i > 0 {
		col.BaseType = col.BaseType[:i]
	}
	col.PrimaryKey = sc.kind == "partition_key" || sc.kind == "clustering"
	return col
}

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	ks := c.ksOf(ref.Database)
	if ks == "" {
		return nil, fmt.Errorf("%s: choose a keyspace", ref.Name)
	}
	t := &driver.Table{Ref: driver.ObjectRef{Database: ks, Name: ref.Name, Kind: ref.Kind}, Options: map[string]string{}, Indexes: []driver.Index{},
		ForeignKeys: []driver.ForeignKey{}, Referenced: []driver.ForeignKey{}, Checks: []driver.Check{}, Triggers: []driver.Trigger{}, PrimaryKey: []string{}}
	switch ref.Kind {
	case "type", "function", "aggregate":
		def, err := c.Definition(ctx, t.Ref)
		if err != nil {
			return nil, err
		}
		t.Kind, t.Definition, t.DDL = ref.Kind, def, def
		return t, nil
	}

	var comment, base, where string
	var ttl, grace int
	var compaction, compression, caching map[string]string
	var includeAll bool
	err := c.query(ctx, `SELECT comment, default_time_to_live, gc_grace_seconds, compaction, compression, caching
		FROM system_schema.tables WHERE keyspace_name = ? AND table_name = ?`, ks, ref.Name).
		Scan(&comment, &ttl, &grace, &compaction, &compression, &caching)
	t.Kind = "table"
	if err != nil {
		if err := c.query(ctx, `SELECT base_table_name, where_clause, include_all_columns, comment, default_time_to_live, gc_grace_seconds, compaction, compression, caching
			FROM system_schema.views WHERE keyspace_name = ? AND view_name = ?`, ks, ref.Name).
			Scan(&base, &where, &includeAll, &comment, &ttl, &grace, &compaction, &compression, &caching); err != nil {
			return nil, fmt.Errorf("%s.%s not found", ks, ref.Name)
		}
		t.Kind = "materialized_view"
		t.Options["base_table"] = base
	}
	t.Ref.Kind = t.Kind
	t.Comment = comment

	cols, err := c.schemaColumns(ctx, ks, ref.Name)
	if err != nil {
		return nil, err
	}
	var partition, clustering, order, static []string
	counter := false
	for _, sc := range cols {
		col := column(sc)
		t.Columns = append(t.Columns, col)
		switch sc.kind {
		case "partition_key":
			partition = append(partition, sc.name)
		case "clustering":
			clustering = append(clustering, sc.name)
			order = append(order, sc.name+" "+strings.ToUpper(sc.order))
		case "static":
			static = append(static, sc.name)
		}
		counter = counter || sc.typ == "counter"
	}
	t.PrimaryKey = append(append([]string{}, partition...), clustering...)
	t.RowKey, t.RowKeyKind = t.PrimaryKey, "primary"
	t.Options["partition_key"] = strings.Join(partition, ", ")
	if len(order) > 0 {
		t.Options["clustering_order"] = strings.Join(order, ", ")
	}
	if len(static) > 0 {
		t.Options["static_columns"] = strings.Join(static, ", ")
	}
	t.Options["default_time_to_live"] = strconv.Itoa(ttl)
	t.Options["gc_grace_seconds"] = strconv.Itoa(grace)
	if cl := compaction["class"]; cl != "" {
		t.Options["compaction"] = cl[strings.LastIndexByte(cl, '.')+1:]
	}
	if counter {
		t.Options["counter"] = "true"
	}
	t.Indexes = append(t.Indexes, driver.Index{Name: "PRIMARY KEY", Columns: t.PrimaryKey, Primary: true, Unique: true})
	if t.Kind == "table" {
		t.Indexes = append(t.Indexes, c.indexes(ctx, ks, ref.Name)...)
	}

	// The server's estimate of partitions (not rows), refreshed every few
	// minutes and rough for small tables.
	iter := c.query(ctx, "SELECT partitions_count FROM system.size_estimates WHERE keyspace_name = ? AND table_name = ?", ks, ref.Name).Iter()
	var parts, total int64
	found := false
	for iter.Scan(&parts) {
		total += parts
		found = true
	}
	iter.Close()
	if found {
		t.Options["estimated_partitions"] = strconv.FormatInt(total, 10)
	}

	// Counter tables take only increments, and views are written by the server.
	t.Editable = t.Kind == "table" && !counter && !c.ro
	if t.Kind == "materialized_view" {
		t.DDL = viewDDL(ks, ref.Name, base, where, includeAll, cols, partition, clustering, order)
	} else {
		t.DDL = tableDDL(ks, ref.Name, cols, partition, clustering, order, comment, ttl, grace, compaction)
		for _, ix := range t.Indexes[1:] {
			t.DDL += ";\n" + ix.Definition
		}
	}
	t.Definition = ""
	return t, nil
}

func (c *conn) indexes(ctx context.Context, ks, table string) []driver.Index {
	iter := c.query(ctx, "SELECT index_name, kind, options FROM system_schema.indexes WHERE keyspace_name = ? AND table_name = ?", ks, table).Iter()
	var out []driver.Index
	var name, kind string
	var opts map[string]string
	for iter.Scan(&name, &kind, &opts) {
		target := opts["target"]
		ix := driver.Index{Name: name, Columns: []string{strings.Trim(target, `"`)}, Type: "secondary"}
		using := ""
		if cl := opts["class_name"]; cl != "" {
			short := cl[strings.LastIndexByte(cl, '.')+1:]
			if strings.Contains(strings.ToLower(short), "storageattachedindex") || strings.EqualFold(short, "sai") {
				ix.Type, using = "sai", " USING 'sai'"
			} else {
				ix.Type, using = short, " USING "+literal(cl)
			}
		}
		// Targets such as keys(m), values(s) or full(f) index a collection.
		col := target
		if !strings.Contains(target, "(") {
			col = quote(strings.Trim(target, `"`))
		}
		ix.Definition = "CREATE INDEX " + quote(name) + " ON " + qualify(ks, table) + " (" + col + ")" + using
		out = append(out, ix)
	}
	iter.Close()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// primaryKeyText writes PRIMARY KEY ((p1, p2), c1, c2), with the partition
// key in parentheses whenever clustering columns follow it.
func primaryKeyText(partition, clustering []string) string {
	parts := make([]string, len(partition))
	for i, p := range partition {
		parts[i] = quote(p)
	}
	key := strings.Join(parts, ", ")
	if len(partition) > 1 || len(clustering) > 0 {
		key = "(" + key + ")"
	}
	for _, cl := range clustering {
		key += ", " + quote(cl)
	}
	return "PRIMARY KEY (" + key + ")"
}

func orderText(order []string) string {
	parts := make([]string, len(order))
	for i, o := range order {
		name, dir, _ := strings.Cut(o, " ")
		parts[i] = quote(name) + " " + dir
	}
	return strings.Join(parts, ", ")
}

func tableDDL(ks, name string, cols []schemaColumn, partition, clustering, order []string, comment string, ttl, grace int, compaction map[string]string) string {
	var b strings.Builder
	b.WriteString("CREATE TABLE " + qualify(ks, name) + " (\n")
	for _, sc := range cols {
		b.WriteString("  " + quote(sc.name) + " " + sc.typ)
		if sc.kind == "static" {
			b.WriteString(" STATIC")
		}
		b.WriteString(",\n")
	}
	b.WriteString("  " + primaryKeyText(partition, clustering) + "\n)")
	var with []string
	if len(order) > 0 {
		with = append(with, "CLUSTERING ORDER BY ("+orderText(order)+")")
	}
	if comment != "" {
		with = append(with, "comment = "+literal(comment))
	}
	if ttl > 0 {
		with = append(with, "default_time_to_live = "+strconv.Itoa(ttl))
	}
	with = append(with, "gc_grace_seconds = "+strconv.Itoa(grace))
	if len(compaction) > 0 {
		with = append(with, "compaction = "+mapLiteral(compaction))
	}
	b.WriteString(" WITH " + strings.Join(with, "\n  AND "))
	return b.String()
}

func viewDDL(ks, name, base, where string, all bool, cols []schemaColumn, partition, clustering, order []string) string {
	sel := "*"
	if !all {
		names := make([]string, len(cols))
		for i, sc := range cols {
			names[i] = quote(sc.name)
		}
		sel = strings.Join(names, ", ")
	}
	s := "CREATE MATERIALIZED VIEW " + qualify(ks, name) + " AS\n  SELECT " + sel + " FROM " + qualify(ks, base) +
		"\n  WHERE " + where + "\n  " + primaryKeyText(partition, clustering)
	if len(order) > 0 {
		s += "\n  WITH CLUSTERING ORDER BY (" + orderText(order) + ")"
	}
	return s
}

func mapLiteral(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = literal(k) + ": " + literal(m[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Definition implements driver.Definer for user types, functions,
// aggregates and materialized views.
func (c *conn) Definition(ctx context.Context, ref driver.ObjectRef) (string, error) {
	ks := c.ksOf(ref.Database)
	switch ref.Kind {
	case "type":
		var names, typs []string
		if err := c.query(ctx, "SELECT field_names, field_types FROM system_schema.types WHERE keyspace_name = ? AND type_name = ?", ks, ref.Name).Scan(&names, &typs); err != nil {
			return "", fmt.Errorf("type %s.%s not found", ks, ref.Name)
		}
		fields := make([]string, len(names))
		for i := range names {
			fields[i] = "  " + quote(names[i]) + " " + typs[i]
		}
		return "CREATE TYPE " + qualify(ks, ref.Name) + " (\n" + strings.Join(fields, ",\n") + "\n)", nil
	case "function":
		iter := c.query(ctx, `SELECT argument_names, argument_types, return_type, language, body, called_on_null_input
			FROM system_schema.functions WHERE keyspace_name = ? AND function_name = ?`, ks, ref.Name).Iter()
		var defs []string
		var names, typs []string
		var ret, lang, body string
		var onNull bool
		for iter.Scan(&names, &typs, &ret, &lang, &body, &onNull) {
			args := make([]string, len(names))
			for i := range names {
				args[i] = quote(names[i]) + " " + typs[i]
			}
			null := "RETURNS NULL ON NULL INPUT"
			if onNull {
				null = "CALLED ON NULL INPUT"
			}
			defs = append(defs, "CREATE FUNCTION "+qualify(ks, ref.Name)+" ("+strings.Join(args, ", ")+")\n  "+null+"\n  RETURNS "+ret+
				"\n  LANGUAGE "+lang+"\n  AS $$"+body+"$$")
		}
		if err := iter.Close(); err != nil || len(defs) == 0 {
			return "", fmt.Errorf("function %s.%s not found", ks, ref.Name)
		}
		return strings.Join(defs, ";\n\n"), nil
	case "aggregate":
		iter := c.query(ctx, `SELECT argument_types, state_func, state_type, final_func, initcond
			FROM system_schema.aggregates WHERE keyspace_name = ? AND aggregate_name = ?`, ks, ref.Name).Iter()
		var defs []string
		var typs []string
		var sfunc, stype, ffunc, init string
		for iter.Scan(&typs, &sfunc, &stype, &ffunc, &init) {
			d := "CREATE AGGREGATE " + qualify(ks, ref.Name) + " (" + strings.Join(typs, ", ") + ")\n  SFUNC " + quote(sfunc) + "\n  STYPE " + stype
			if ffunc != "" {
				d += "\n  FINALFUNC " + quote(ffunc)
			}
			if init != "" {
				d += "\n  INITCOND " + init
			}
			defs = append(defs, d)
		}
		if err := iter.Close(); err != nil || len(defs) == 0 {
			return "", fmt.Errorf("aggregate %s.%s not found", ks, ref.Name)
		}
		return strings.Join(defs, ";\n\n"), nil
	}
	t, err := c.Describe(ctx, driver.ObjectRef{Database: ks, Name: ref.Name})
	if err != nil {
		return "", err
	}
	return t.DDL, nil
}

// CatalogColumns implements driver.Catalog for one keyspace.
func (c *conn) CatalogColumns(ctx context.Context, s driver.Scope) ([]driver.CatalogTable, error) {
	ks := c.ksOf(s.Database)
	if ks == "" {
		return nil, nil
	}
	views := map[string]bool{}
	hidden := c.indexViews(ctx, ks)
	iter := c.query(ctx, "SELECT view_name FROM system_schema.views WHERE keyspace_name = ?", ks).Iter()
	var name string
	for iter.Scan(&name) {
		views[name] = true
	}
	iter.Close()
	iter = c.query(ctx, "SELECT table_name, column_name, type, kind FROM system_schema.columns WHERE keyspace_name = ?", ks).Iter()
	var out []driver.CatalogTable
	idx := map[string]int{}
	var tn, cn, typ, kind string
	for iter.Scan(&tn, &cn, &typ, &kind) {
		if hidden[tn] {
			continue
		}
		i, ok := idx[tn]
		if !ok {
			i = len(out)
			idx[tn] = i
			k := "table"
			if views[tn] {
				k = "materialized_view"
			}
			out = append(out, driver.CatalogTable{Name: tn, Kind: k})
		}
		out[i].Columns = append(out[i].Columns, driver.CatalogColumn{Name: cn, Type: typ, PK: kind == "partition_key" || kind == "clustering"})
	}
	return out, mapError(iter.Close())
}
