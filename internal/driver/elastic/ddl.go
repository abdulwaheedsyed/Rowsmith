package elastic

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
)

var _ driver.DDLGenerator = (*conn)(nil)

// fieldMapping reads a column's type: a type name such as keyword, or a
// whole mapping as JSON ({"type": "scaled_float", "scaling_factor": 100}).
func fieldMapping(col driver.Column) (any, error) {
	t := strings.TrimSpace(col.Type)
	if t == "" {
		return nil, fmt.Errorf("field %s has no type", col.Name)
	}
	if strings.HasPrefix(t, "{") {
		var m map[string]any
		if err := json.Unmarshal([]byte(t), &m); err != nil {
			return nil, fmt.Errorf("field %s: the mapping is not valid JSON", col.Name)
		}
		return m, nil
	}
	if strings.ContainsAny(t, " \"'{}") {
		return nil, fmt.Errorf("field %s: %q is not a field type", col.Name, t)
	}
	m := map[string]any{"type": t}
	if t == "text" {
		// Text matches words; a keyword sub-field gives exact matches and sorting.
		m["fields"] = map[string]any{"keyword": map[string]any{"type": "keyword", "ignore_above": 256}}
	}
	return m, nil
}

func pretty(method, path string, body any) string {
	b, _ := json.MarshalIndent(body, "", "  ")
	return method + " " + path + "\n" + string(b)
}

func indexSettings(opts map[string]string, create bool) (map[string]any, error) {
	s := map[string]any{}
	for _, k := range []string{"number_of_shards", "number_of_replicas"} {
		if v := strings.TrimSpace(opts[k]); v != "" {
			if _, err := strconv.Atoi(v); err != nil {
				return nil, fmt.Errorf("%s must be a whole number", strings.ReplaceAll(k, "_", " "))
			}
			if k == "number_of_shards" && !create {
				continue
			}
			s[k] = v
		}
	}
	if v := strings.TrimSpace(opts["refresh_interval"]); v != "" {
		s["refresh_interval"] = v
	}
	return s, nil
}

func (c *conn) CreateTableSQL(def driver.TableDef) ([]string, error) {
	name := def.Ref.Name
	if name == "" || name != strings.ToLower(name) || strings.ContainsAny(name, ` "*\<|,>/?#:`) || strings.HasPrefix(name, "_") || strings.HasPrefix(name, "-") {
		return nil, fmt.Errorf("index names are lower case, without spaces or \" * \\ < | , > / ? # :, and don't start with _ or -")
	}
	if len(def.ForeignKeys) > 0 || len(def.Checks) > 0 {
		return nil, errors.New("Elasticsearch has no foreign keys or checks")
	}
	props := map[string]any{}
	for _, col := range def.Columns {
		if col.Name == "_id" {
			continue
		}
		m, err := fieldMapping(col.Column)
		if err != nil {
			return nil, err
		}
		props[col.Name] = m
	}
	mappings := map[string]any{"properties": props}
	switch pk := def.PrimaryKey; {
	case len(pk) > 1:
		return nil, errors.New("a document has one _id, so the primary key can be one field")
	case len(pk) == 1 && pk[0] != "_id":
		mappings["_meta"] = map[string]any{"primary_key": pk[0]}
	}
	body := map[string]any{"mappings": mappings}
	settings, err := indexSettings(def.Options, true)
	if err != nil {
		return nil, err
	}
	if len(settings) > 0 {
		body["settings"] = map[string]any{"index": settings}
	}
	return []string{pretty("PUT", "/"+name, body)}, nil
}

// AlterTableSQL adds fields and changes settings: Elasticsearch can't
// change or remove a field's mapping once documents may use it.
func (c *conn) AlterTableSQL(from *driver.Table, to driver.TableDef) ([]string, error) {
	if from == nil {
		return nil, errors.New("current index definition is missing")
	}
	if to.Ref.Name != "" && to.Ref.Name != from.Ref.Name {
		return nil, errors.New("indices can't be renamed; reindex into a new index (POST _reindex) and add an alias")
	}
	current := map[string]driver.Column{}
	for _, col := range from.Columns {
		current[col.Name] = col
	}
	kept := map[string]bool{}
	props := map[string]any{}
	for _, col := range to.Columns {
		if col.Name == "_id" || col.OriginalName == "_id" {
			kept["_id"] = true
			continue
		}
		if col.OriginalName == "" {
			m, err := fieldMapping(col.Column)
			if err != nil {
				return nil, err
			}
			props[col.Name] = m
			continue
		}
		kept[col.OriginalName] = true
		old := current[col.OriginalName]
		if col.Name != old.Name {
			return nil, fmt.Errorf("field %s can't be renamed; add a new field and reindex", old.Name)
		}
		if strings.TrimSpace(col.Type) != strings.TrimSpace(old.Type) {
			return nil, fmt.Errorf("field %s: a mapping can't change type; create a new index and reindex into it", old.Name)
		}
	}
	for _, col := range from.Columns {
		if !kept[col.Name] {
			return nil, fmt.Errorf("field %s can't be removed from the mapping; create a new index and reindex into it", col.Name)
		}
	}
	var out []string
	body := map[string]any{}
	if len(props) > 0 {
		body["properties"] = props
	}
	oldPK := strings.TrimSuffix(from.Options["primary_key"], " (copied into _id)")
	newPK := ""
	if len(to.PrimaryKey) == 1 && to.PrimaryKey[0] != "_id" {
		newPK = to.PrimaryKey[0]
	} else if len(to.PrimaryKey) > 1 {
		return nil, errors.New("a document has one _id, so the primary key can be one field")
	}
	if newPK != oldPK {
		body["_meta"] = map[string]any{"primary_key": newPK}
	}
	if len(body) > 0 {
		out = append(out, pretty("PUT", "/"+from.Ref.Name+"/_mapping", body))
	}
	if strings.TrimSpace(to.Options["number_of_shards"]) != "" && to.Options["number_of_shards"] != from.Options["number_of_shards"] {
		return nil, errors.New("the number of primary shards is set when the index is created; use the _split or _shrink API to change it")
	}
	changed := map[string]string{}
	for _, k := range []string{"number_of_replicas", "refresh_interval"} {
		if v := strings.TrimSpace(to.Options[k]); v != "" && v != from.Options[k] {
			changed[k] = v
		}
	}
	if len(changed) > 0 {
		s, err := indexSettings(changed, false)
		if err != nil {
			return nil, err
		}
		out = append(out, pretty("PUT", "/"+from.Ref.Name+"/_settings", map[string]any{"index": s}))
	}
	return out, nil
}

func (c *conn) DropObjectSQL(ref driver.ObjectRef, _ bool) ([]string, error) {
	switch ref.Kind {
	case "data_stream":
		return []string{"DELETE /_data_stream/" + ref.Name}, nil
	case "alias":
		return []string{"DELETE /_all/_alias/" + ref.Name}, nil
	}
	return []string{"DELETE /" + ref.Name}, nil
}

func (c *conn) TruncateSQL(ref driver.ObjectRef) ([]string, error) {
	return []string{pretty("POST", "/"+ref.Name+"/_delete_by_query?refresh=true&conflicts=proceed", map[string]any{"query": map[string]any{"match_all": map[string]any{}}})}, nil
}

func (c *conn) RenameObjectSQL(driver.ObjectRef, string) ([]string, error) {
	return nil, errors.New("indices can't be renamed; reindex into a new index (POST _reindex) and add an alias")
}

func (c *conn) CreateDatabaseSQL(string, map[string]string) ([]string, error) {
	return nil, driver.ErrNotSupported
}

func (c *conn) DropDatabaseSQL(string, bool) ([]string, error) { return nil, driver.ErrNotSupported }
