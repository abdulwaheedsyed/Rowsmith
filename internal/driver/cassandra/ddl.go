package cassandra

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"rowsmith/internal/driver"
)

var _ driver.DDLGenerator = (*conn)(nil)

// keyParts splits a primary key into its partition key (the partition_key
// option, or the first column) and clustering columns, with their order.
func keyParts(def driver.TableDef) (partition, clustering, order []string, err error) {
	if len(def.PrimaryKey) == 0 {
		return nil, nil, nil, errors.New("a Cassandra table needs a primary key: its first column (or the partition key) places each row")
	}
	partition = []string{def.PrimaryKey[0]}
	if p := strings.TrimSpace(def.Options["partition_key"]); p != "" {
		partition = nil
		for _, name := range strings.Split(p, ",") {
			if name = strings.TrimSpace(name); name != "" {
				partition = append(partition, name)
			}
		}
	}
	if len(partition) > len(def.PrimaryKey) || !slices.Equal(partition, def.PrimaryKey[:len(partition)]) {
		return nil, nil, nil, fmt.Errorf("the partition key (%s) must be the first columns of the primary key", strings.Join(partition, ", "))
	}
	clustering = def.PrimaryKey[len(partition):]
	dirs := map[string]string{}
	if o := strings.TrimSpace(def.Options["clustering_order"]); o != "" {
		for _, part := range strings.Split(o, ",") {
			name, dir, _ := strings.Cut(strings.TrimSpace(part), " ")
			dir = strings.ToUpper(strings.TrimSpace(dir))
			if dir == "" {
				dir = "ASC"
			}
			if dir != "ASC" && dir != "DESC" {
				return nil, nil, nil, fmt.Errorf("clustering order: %q is not ASC or DESC", dir)
			}
			if !slices.Contains(clustering, name) {
				return nil, nil, nil, fmt.Errorf("clustering order: %s is not a clustering column", name)
			}
			dirs[name] = dir
		}
	}
	if len(dirs) > 0 {
		for _, cl := range clustering {
			d := dirs[cl]
			if d == "" {
				d = "ASC"
			}
			order = append(order, cl+" "+d)
		}
	}
	return partition, clustering, order, nil
}

// tableOptions writes the WITH options the structure editor offers.
func tableOptions(opts map[string]string, comment string, withComment bool) ([]string, error) {
	var with []string
	if withComment {
		with = append(with, "comment = "+literal(comment))
	}
	for _, k := range []string{"default_time_to_live", "gc_grace_seconds"} {
		if v := strings.TrimSpace(opts[k]); v != "" {
			if _, err := strconv.Atoi(v); err != nil {
				return nil, fmt.Errorf("%s must be a whole number of seconds", k)
			}
			with = append(with, k+" = "+v)
		}
	}
	if cl := strings.TrimSpace(opts["compaction"]); cl != "" {
		if strings.ContainsAny(cl, "'\"") {
			return nil, fmt.Errorf("invalid compaction class %q", cl)
		}
		with = append(with, "compaction = {'class': "+literal(cl)+"}")
	}
	return with, nil
}

func indexSQL(ks, table string, ix driver.Index) (string, error) {
	if len(ix.Columns) != 1 {
		return "", fmt.Errorf("index %s: Cassandra indexes one column", ix.Name)
	}
	if ix.Unique {
		return "", fmt.Errorf("index %s: Cassandra indexes can't be unique; only the primary key is", ix.Name)
	}
	q := "CREATE INDEX "
	if ix.Name != "" {
		q += quote(ix.Name) + " "
	}
	q += "ON " + qualify(ks, table) + " (" + quote(ix.Columns[0]) + ")"
	if strings.EqualFold(ix.Type, "sai") {
		q += " USING 'sai'"
	}
	return q, nil
}

func (c *conn) CreateTableSQL(def driver.TableDef) ([]string, error) {
	ks := c.ksOf(def.Ref.Database)
	if ks == "" {
		return nil, errors.New("choose a keyspace")
	}
	if len(def.ForeignKeys) > 0 || len(def.Checks) > 0 {
		return nil, errors.New("Cassandra has no foreign keys or checks")
	}
	partition, clustering, order, err := keyParts(def)
	if err != nil {
		return nil, err
	}
	var cols []string
	names := map[string]bool{}
	for _, col := range def.Columns {
		if strings.TrimSpace(col.Type) == "" {
			return nil, fmt.Errorf("column %s has no type", col.Name)
		}
		if col.Default != nil && *col.Default != "" {
			return nil, fmt.Errorf("column %s: Cassandra columns have no defaults", col.Name)
		}
		names[col.Name] = true
		cols = append(cols, "  "+quote(col.Name)+" "+col.Type)
	}
	for _, k := range def.PrimaryKey {
		if !names[k] {
			return nil, fmt.Errorf("primary key column %s is not a column of the table", k)
		}
	}
	q := "CREATE TABLE " + qualify(ks, def.Ref.Name) + " (\n" + strings.Join(cols, ",\n") + ",\n  " + primaryKeyText(partition, clustering) + "\n)"
	with, err := tableOptions(def.Options, def.Comment, def.Comment != "")
	if err != nil {
		return nil, err
	}
	if len(order) > 0 {
		with = append([]string{"CLUSTERING ORDER BY (" + orderText(order) + ")"}, with...)
	}
	if len(with) > 0 {
		q += " WITH " + strings.Join(with, "\n  AND ")
	}
	out := []string{q}
	for _, ix := range def.Indexes {
		if ix.Primary {
			continue
		}
		s, err := indexSQL(ks, def.Ref.Name, ix)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// AlterTableSQL changes what Cassandra lets change: columns outside the
// primary key are added and dropped, primary key columns renamed, and the
// table's options and indexes changed. Types and the key stay as they are.
func (c *conn) AlterTableSQL(from *driver.Table, to driver.TableDef) ([]string, error) {
	if from == nil {
		return nil, errors.New("current table definition is missing")
	}
	if from.Kind == "materialized_view" {
		return nil, errors.New("change a materialized view by dropping and creating it again")
	}
	ks, target := from.Ref.Database, qualify(from.Ref.Database, from.Ref.Name)
	if to.Ref.Name != "" && to.Ref.Name != from.Ref.Name {
		return nil, errors.New("Cassandra can't rename tables")
	}
	current := map[string]driver.Column{}
	for _, col := range from.Columns {
		current[col.Name] = col
	}
	renamed := map[string]string{}
	kept := map[string]bool{}
	for _, col := range to.Columns {
		if col.OriginalName != "" {
			kept[col.OriginalName] = true
			renamed[col.OriginalName] = col.Name
		}
	}
	// The key must stay the same columns, in the same order.
	wantKey := make([]string, len(from.PrimaryKey))
	for i, k := range from.PrimaryKey {
		wantKey[i] = renamed[k]
	}
	if !slices.Equal(wantKey, to.PrimaryKey) {
		return nil, errors.New("the primary key can't change; create a new table and copy the rows (Migrate can do it)")
	}
	var out []string
	alter := func(clause string) { out = append(out, "ALTER TABLE "+target+" "+clause) }
	for _, col := range from.Columns {
		if !kept[col.Name] {
			if col.PrimaryKey {
				return nil, fmt.Errorf("%s is part of the primary key and can't be dropped", col.Name)
			}
			alter("DROP " + quote(col.Name))
		}
	}
	var renames []string
	for _, col := range to.Columns {
		if col.OriginalName == "" {
			if strings.TrimSpace(col.Type) == "" {
				return nil, fmt.Errorf("column %s has no type", col.Name)
			}
			alter("ADD " + quote(col.Name) + " " + col.Type)
			continue
		}
		old, ok := current[col.OriginalName]
		if !ok {
			return nil, fmt.Errorf("column %s no longer exists", col.OriginalName)
		}
		if !strings.EqualFold(strings.ReplaceAll(col.Type, " ", ""), strings.ReplaceAll(old.Type, " ", "")) {
			return nil, fmt.Errorf("column %s: Cassandra can't change a column's type; add a new column instead", old.Name)
		}
		if col.Name != old.Name {
			if !old.PrimaryKey {
				return nil, fmt.Errorf("column %s: Cassandra renames only primary key columns", old.Name)
			}
			renames = append(renames, quote(old.Name)+" TO "+quote(col.Name))
		}
	}
	if len(renames) > 0 {
		alter("RENAME " + strings.Join(renames, " AND "))
	}
	for _, k := range []string{"partition_key", "clustering_order"} {
		norm := func(s string) string {
			return strings.ToUpper(strings.Join(strings.Fields(strings.ReplaceAll(s, ",", " ")), " "))
		}
		if v, ok := to.Options[k]; ok && norm(v) != norm(from.Options[k]) && !(k == "clustering_order" && v == "" && !strings.Contains(from.Options[k], "DESC")) {
			return nil, fmt.Errorf("the %s can't change once the table exists", strings.ReplaceAll(k, "_", " "))
		}
	}
	changed := map[string]string{}
	for _, k := range []string{"default_time_to_live", "gc_grace_seconds", "compaction"} {
		if v, ok := to.Options[k]; ok && strings.TrimSpace(v) != strings.TrimSpace(from.Options[k]) && strings.TrimSpace(v) != "" {
			changed[k] = v
		}
	}
	with, err := tableOptions(changed, to.Comment, to.Comment != from.Comment)
	if err != nil {
		return nil, err
	}
	if len(with) > 0 {
		alter("WITH " + strings.Join(with, " AND "))
	}
	// Indexes: dropped by name, added as new.
	old := map[string]bool{}
	for _, ix := range from.Indexes {
		if !ix.Primary {
			old[ix.Name] = true
		}
	}
	wanted := map[string]bool{}
	for _, ix := range to.Indexes {
		if ix.Primary {
			continue
		}
		wanted[ix.Name] = true
		if !old[ix.Name] || ix.Name == "" {
			s, err := indexSQL(ks, from.Ref.Name, ix)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
	}
	for _, ix := range from.Indexes {
		if !ix.Primary && !wanted[ix.Name] {
			out = append([]string{"DROP INDEX " + qualify(ks, ix.Name)}, out...)
		}
	}
	return out, nil
}

var dropKeyword = map[string]string{"table": "TABLE", "materialized_view": "MATERIALIZED VIEW", "type": "TYPE", "function": "FUNCTION", "aggregate": "AGGREGATE"}

func (c *conn) DropObjectSQL(ref driver.ObjectRef, _ bool) ([]string, error) {
	kw, ok := dropKeyword[ref.Kind]
	if !ok {
		return nil, fmt.Errorf("cannot drop a %s", ref.Kind)
	}
	return []string{"DROP " + kw + " " + qualify(c.ksOf(ref.Database), ref.Name)}, nil
}

func (c *conn) TruncateSQL(ref driver.ObjectRef) ([]string, error) {
	return []string{"TRUNCATE TABLE " + qualify(c.ksOf(ref.Database), ref.Name)}, nil
}

func (c *conn) RenameObjectSQL(driver.ObjectRef, string) ([]string, error) {
	return nil, errors.New("Cassandra can't rename tables, views or types")
}

// CreateDatabaseSQL creates a keyspace replicated in the local data center:
// three copies, or one per node when it has fewer.
func (c *conn) CreateDatabaseSQL(name string, _ map[string]string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nodes := 1
	iter := c.query(ctx, "SELECT data_center FROM system.peers").Iter()
	var dc string
	for iter.Scan(&dc) {
		if dc == c.dc {
			nodes++
		}
	}
	iter.Close()
	return []string{"CREATE KEYSPACE " + quote(name) + " WITH replication = {'class': 'NetworkTopologyStrategy', " +
		literal(c.dc) + ": " + strconv.Itoa(min(nodes, 3)) + "}"}, nil
}

func (c *conn) DropDatabaseSQL(name string, _ bool) ([]string, error) {
	return []string{"DROP KEYSPACE " + quote(name)}, nil
}
