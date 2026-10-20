package dynamodb

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
)

var _ driver.DDLGenerator = (*conn)(nil)

// pretty writes an operation as the console runs it.
func pretty(op string, input any) string {
	b, _ := json.MarshalIndent(input, "", "  ")
	return op + "\n" + string(b)
}

func keySchema(hash, rng string) []any {
	ks := []any{map[string]any{"AttributeName": hash, "KeyType": "HASH"}}
	if rng != "" {
		ks = append(ks, map[string]any{"AttributeName": rng, "KeyType": "RANGE"})
	}
	return ks
}

// projectionOf reads an index's projection from its definition
// ("projects KEYS_ONLY", "projects INCLUDE a, b"); all attributes otherwise.
func projectionOf(ix driver.Index) map[string]any {
	d := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ix.Definition), "projects"))
	switch {
	case strings.EqualFold(d, "KEYS_ONLY"):
		return map[string]any{"ProjectionType": "KEYS_ONLY"}
	case len(d) > 7 && strings.EqualFold(d[:7], "INCLUDE"):
		var attrs []string
		for _, a := range strings.Split(d[7:], ",") {
			if a = strings.TrimSpace(a); a != "" {
				attrs = append(attrs, a)
			}
		}
		return map[string]any{"ProjectionType": "INCLUDE", "NonKeyAttributes": attrs}
	}
	return map[string]any{"ProjectionType": "ALL"}
}

func capacity(opts map[string]string) (map[string]any, error) {
	units := map[string]any{}
	for _, k := range []string{"read_capacity", "write_capacity"} {
		v := strings.TrimSpace(opts[k])
		if v == "" {
			v = "5"
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%s must be a whole number of at least 1", strings.ReplaceAll(k, "_", " "))
		}
		units[map[string]string{"read_capacity": "ReadCapacityUnits", "write_capacity": "WriteCapacityUnits"}[k]] = n
	}
	return units, nil
}

func provisioned(opts map[string]string) bool { return opts["billing"] == "PROVISIONED" }

// typeTags record the types of attributes outside the keys, a tag of up to
// 256 characters each: "total=N items=L". Names a tag can't hold (only
// letters, digits, spaces and _ . : / + - @ are allowed) are left out.
func typeTags(cols []driver.Column, keys map[string]bool) []any {
	var pairs []string
	for _, c := range cols {
		if keys[c.Name] || c.Type == "" || c.Type == tMixed || c.Type == tNULL || strings.ContainsAny(c.Name, " =") || !tagSafe(c.Name) {
			continue
		}
		pairs = append(pairs, c.Name+"="+c.Type)
	}
	var tags []any
	cur := ""
	flush := func() {
		if cur != "" {
			tags = append(tags, map[string]any{"Key": typeTag + strconv.Itoa(len(tags)), "Value": cur})
			cur = ""
		}
	}
	for _, p := range pairs {
		if len(p) > 256 {
			continue
		}
		if cur != "" && len(cur)+1+len(p) > 256 {
			flush()
		}
		if cur != "" {
			cur += " "
		}
		cur += p
	}
	flush()
	return tags
}

func tagSafe(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.:/+-@", r) || r > 127) {
			return false
		}
	}
	return true
}

// keyType is a key attribute's type, which must be S, N or B.
func keyType(cols map[string]driver.Column, name string) (string, error) {
	c, ok := cols[name]
	if !ok {
		return "", fmt.Errorf("key attribute %s is not among the attributes", name)
	}
	switch c.Type {
	case tS, tN, tB:
		return c.Type, nil
	}
	return "", fmt.Errorf("key attribute %s must be text (S), a number (N) or binary (B)", name)
}

func indexKeys(ix driver.Index) (hash, rng string, err error) {
	switch len(ix.Columns) {
	case 1:
		return ix.Columns[0], "", nil
	case 2:
		return ix.Columns[0], ix.Columns[1], nil
	}
	return "", "", fmt.Errorf("index %s: name its partition key and optionally a sort key", ix.Name)
}

// createInput is CreateTable's input for a definition.
func createInput(def driver.TableDef) (map[string]any, error) {
	if strings.TrimSpace(def.Ref.Name) == "" {
		return nil, errors.New("enter the table name")
	}
	if len(def.PrimaryKey) == 0 || len(def.PrimaryKey) > 2 {
		return nil, errors.New("the primary key is a partition key and optionally a sort key")
	}
	cols := map[string]driver.Column{}
	var plain []driver.Column
	for _, c := range def.Columns {
		cols[c.Name] = c.Column
		plain = append(plain, c.Column)
	}
	attrs := map[string]string{}
	addAttr := func(name string) error {
		t, err := keyType(cols, name)
		attrs[name] = t
		return err
	}
	hash, rng := def.PrimaryKey[0], ""
	if len(def.PrimaryKey) == 2 {
		rng = def.PrimaryKey[1]
	}
	for _, k := range def.PrimaryKey {
		if err := addAttr(k); err != nil {
			return nil, err
		}
	}
	in := map[string]any{"TableName": def.Ref.Name, "KeySchema": keySchema(hash, rng)}
	billing := "PAY_PER_REQUEST"
	var units map[string]any
	if provisioned(def.Options) {
		billing = "PROVISIONED"
		var err error
		if units, err = capacity(def.Options); err != nil {
			return nil, err
		}
		in["ProvisionedThroughput"] = units
	}
	in["BillingMode"] = billing
	var gsis, lsis []any
	for _, ix := range def.Indexes {
		if ix.Primary {
			continue
		}
		h, r, err := indexKeys(ix)
		if err != nil {
			return nil, err
		}
		for _, k := range ix.Columns {
			if err := addAttr(k); err != nil {
				return nil, fmt.Errorf("index %s: %w", ix.Name, err)
			}
		}
		spec := map[string]any{"IndexName": ix.Name, "KeySchema": keySchema(h, r), "Projection": projectionOf(ix)}
		if ix.Type == "local" {
			if rng == "" {
				return nil, fmt.Errorf("local index %s: only a table with a sort key takes local indexes", ix.Name)
			}
			if h != hash || r == "" {
				return nil, fmt.Errorf("local index %s: it shares the table's partition key (%s) and has a sort key of its own", ix.Name, hash)
			}
			lsis = append(lsis, spec)
			continue
		}
		if units != nil {
			spec["ProvisionedThroughput"] = units
		}
		gsis = append(gsis, spec)
	}
	names := make([]string, 0, len(attrs))
	for n := range attrs {
		names = append(names, n)
	}
	sort.Strings(names)
	var defs []any
	for _, n := range names {
		defs = append(defs, map[string]any{"AttributeName": n, "AttributeType": attrs[n]})
	}
	in["AttributeDefinitions"] = defs
	if gsis != nil {
		in["GlobalSecondaryIndexes"] = gsis
	}
	if lsis != nil {
		in["LocalSecondaryIndexes"] = lsis
	}
	if def.Options["deletion_protection"] == "on" {
		in["DeletionProtectionEnabled"] = true
	}
	if tags := typeTags(plain, map[string]bool{hash: true, rng: true}); tags != nil {
		in["Tags"] = tags
	}
	return in, nil
}

func ttlRequest(table, attr string, on bool) string {
	return pretty("UpdateTimeToLive", map[string]any{"TableName": table, "TimeToLiveSpecification": map[string]any{"AttributeName": attr, "Enabled": on}})
}

func (c *conn) CreateTableSQL(def driver.TableDef) ([]string, error) {
	if len(def.ForeignKeys) > 0 || len(def.Checks) > 0 {
		return nil, errors.New("DynamoDB tables have no foreign keys or checks")
	}
	in, err := createInput(def)
	if err != nil {
		return nil, err
	}
	out := []string{pretty("CreateTable", in)}
	if a := strings.TrimSpace(def.Options["ttl_attribute"]); a != "" {
		out = append(out, ttlRequest(def.Ref.Name, a, true))
	}
	return out, nil
}

// createRequest shows a table as the calls that would create it.
func createRequest(t *driver.Table, _ *layout) string {
	def := driver.TableDef{Ref: t.Ref, PrimaryKey: t.PrimaryKey, Options: t.Options}
	for _, c := range t.Columns {
		def.Columns = append(def.Columns, driver.ColumnDef{Column: c})
	}
	for _, ix := range t.Indexes {
		if !ix.Primary {
			def.Indexes = append(def.Indexes, ix)
		}
	}
	stmts, err := (&conn{}).CreateTableSQL(def)
	if err != nil {
		return "-- " + err.Error()
	}
	return strings.Join(stmts, "\n\n")
}

// AlterTableSQL changes what DynamoDB lets change: capacity, deletion
// protection, time to live, global indexes (one per UpdateTable) and the
// attribute types Rowsmith records. Keys, local indexes and names are
// fixed when the table is created.
func (c *conn) AlterTableSQL(from *driver.Table, to driver.TableDef) ([]string, error) {
	if from == nil {
		return nil, errors.New("current table definition is missing")
	}
	table := from.Ref.Name
	if to.Ref.Name != "" && to.Ref.Name != table {
		return nil, errors.New("DynamoDB tables can't be renamed; create a new table and copy the items (a migration does that)")
	}
	if !slices.Equal(to.PrimaryKey, from.PrimaryKey) {
		return nil, errors.New("the primary key is fixed when the table is created")
	}
	current := map[string]driver.Column{}
	for _, col := range from.Columns {
		current[col.Name] = col
	}
	keys := map[string]bool{}
	for _, k := range from.PrimaryKey {
		keys[k] = true
	}
	cols := map[string]driver.Column{}
	var plain []driver.Column
	for _, col := range to.Columns {
		if col.OriginalName != "" && col.OriginalName != col.Name {
			return nil, fmt.Errorf("attribute %s: DynamoDB can't rename an attribute in every item; add the new one and copy the values", col.OriginalName)
		}
		if old, ok := current[col.OriginalName]; ok && keys[old.Name] && old.Type != col.Type {
			return nil, fmt.Errorf("key attribute %s keeps the type it was created with", old.Name)
		}
		cols[col.Name] = col.Column
		plain = append(plain, col.Column)
	}
	var out []string
	update := func(in map[string]any) {
		in["TableName"] = table
		out = append(out, pretty("UpdateTable", in))
	}
	// Capacity and deletion protection.
	in := map[string]any{}
	wasProv, isProv := provisioned(from.Options), provisioned(to.Options)
	if isProv {
		units, err := capacity(to.Options)
		if err != nil {
			return nil, err
		}
		if !wasProv || from.Options["read_capacity"] != fmt.Sprint(units["ReadCapacityUnits"]) || from.Options["write_capacity"] != fmt.Sprint(units["WriteCapacityUnits"]) {
			in["ProvisionedThroughput"] = units
			if !wasProv {
				in["BillingMode"] = "PROVISIONED"
				var gsiUnits []any
				for _, ix := range from.Indexes {
					if ix.Type == "global" {
						gsiUnits = append(gsiUnits, map[string]any{"Update": map[string]any{"IndexName": ix.Name, "ProvisionedThroughput": units}})
					}
				}
				if gsiUnits != nil {
					in["GlobalSecondaryIndexUpdates"] = gsiUnits
				}
			}
		}
	} else if wasProv && to.Options["billing"] != "" {
		in["BillingMode"] = "PAY_PER_REQUEST"
	}
	if (to.Options["deletion_protection"] == "on") != (from.Options["deletion_protection"] == "on") {
		in["DeletionProtectionEnabled"] = to.Options["deletion_protection"] == "on"
	}
	if len(in) > 0 {
		update(in)
	}
	// Time to live: off, then on for the new attribute.
	oldTTL, newTTL := strings.TrimSpace(from.Options["ttl_attribute"]), strings.TrimSpace(to.Options["ttl_attribute"])
	if oldTTL != newTTL {
		if oldTTL != "" {
			out = append(out, ttlRequest(table, oldTTL, false))
		}
		if newTTL != "" {
			out = append(out, ttlRequest(table, newTTL, true))
		}
	}
	// Indexes.
	oldIx := map[string]driver.Index{}
	for _, ix := range from.Indexes {
		if !ix.Primary {
			oldIx[ix.Name] = ix
		}
	}
	newIx := map[string]bool{}
	var creates []driver.Index
	for _, ix := range to.Indexes {
		if ix.Primary {
			continue
		}
		newIx[ix.Name] = true
		old, ok := oldIx[ix.Name]
		same := ok && slices.Equal(old.Columns, ix.Columns) && old.Type == ix.Type &&
			fmt.Sprint(projectionOf(old)) == fmt.Sprint(projectionOf(ix))
		if same {
			continue
		}
		if ix.Type == "local" || ok && old.Type == "local" {
			return nil, fmt.Errorf("local index %s is fixed when the table is created", ix.Name)
		}
		if ok {
			update(map[string]any{"GlobalSecondaryIndexUpdates": []any{map[string]any{"Delete": map[string]any{"IndexName": ix.Name}}}})
		}
		creates = append(creates, ix)
	}
	for _, ix := range from.Indexes {
		if ix.Primary || newIx[ix.Name] {
			continue
		}
		if ix.Type == "local" {
			return nil, fmt.Errorf("local index %s is fixed when the table is created", ix.Name)
		}
		update(map[string]any{"GlobalSecondaryIndexUpdates": []any{map[string]any{"Delete": map[string]any{"IndexName": ix.Name}}}})
	}
	for _, ix := range creates {
		h, r, err := indexKeys(ix)
		if err != nil {
			return nil, err
		}
		var defs []any
		for _, k := range ix.Columns {
			t, err := keyType(cols, k)
			if err != nil {
				return nil, fmt.Errorf("index %s: %w", ix.Name, err)
			}
			defs = append(defs, map[string]any{"AttributeName": k, "AttributeType": t})
		}
		spec := map[string]any{"IndexName": ix.Name, "KeySchema": keySchema(h, r), "Projection": projectionOf(ix)}
		if isProv {
			units, _ := capacity(to.Options)
			spec["ProvisionedThroughput"] = units
		}
		update(map[string]any{"AttributeDefinitions": defs, "GlobalSecondaryIndexUpdates": []any{map[string]any{"Create": spec}}})
	}
	// The attribute types Rowsmith records, as tags.
	oldTags, newTags := typeTags(from.Columns, keys), typeTags(plain, keys)
	if fmt.Sprint(oldTags) != fmt.Sprint(newTags) && !strings.Contains(from.Options["arn"], ":ddblocal:") { // DynamoDB Local has no tags
		arn := from.Options["arn"]
		if arn == "" {
			return nil, errors.New("the table's ARN is unknown, so its attribute types can't be recorded")
		}
		if len(oldTags) > len(newTags) {
			var gone []string
			for i := len(newTags); i < len(oldTags); i++ {
				gone = append(gone, typeTag+strconv.Itoa(i))
			}
			out = append(out, pretty("UntagResource", map[string]any{"ResourceArn": arn, "TagKeys": gone}))
		}
		if len(newTags) > 0 {
			out = append(out, pretty("TagResource", map[string]any{"ResourceArn": arn, "Tags": newTags}))
		}
	}
	if len(to.Checks) > 0 || len(to.ForeignKeys) > 0 {
		return nil, errors.New("DynamoDB tables have no foreign keys or checks")
	}
	return out, nil
}

func (c *conn) DropObjectSQL(ref driver.ObjectRef, _ bool) ([]string, error) {
	return []string{pretty("DeleteTable", map[string]any{"TableName": ref.Name})}, nil
}

func (c *conn) TruncateSQL(driver.ObjectRef) ([]string, error) {
	return nil, errors.New("DynamoDB has no truncate: delete the table and create it again from its definition, or import with “replace”, which deletes the items first")
}

func (c *conn) RenameObjectSQL(driver.ObjectRef, string) ([]string, error) {
	return nil, errors.New("DynamoDB tables can't be renamed; create a new table and copy the items (a migration does that)")
}

func (c *conn) CreateDatabaseSQL(string, map[string]string) ([]string, error) {
	return nil, driver.ErrNotSupported
}

func (c *conn) DropDatabaseSQL(string, bool) ([]string, error) { return nil, driver.ErrNotSupported }
