package mongodb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"rowsmith/internal/driver"
)

// Explain runs the explain command for the first command of stmt.
// executionStats evaluates writes without applying them.
func (c *conn) Explain(ctx context.Context, s driver.Scope, stmt string, analyze bool) (*driver.Plan, error) {
	cmds, err := parseScript(stmt)
	if err != nil {
		return nil, err
	}
	if len(cmds) == 0 || cmds[0].target == "" || len(cmds[0].calls) == 0 {
		return nil, errors.New("explain needs a collection command such as db.orders.find({...})")
	}
	cmd := cmds[0]
	coll := cmd.target
	first := cmd.calls[0]
	var inner bson.D
	switch first.name {
	case "find", "findOne":
		f, _ := argDoc(first.args, 0)
		inner = bson.D{{Key: "find", Value: coll}, {Key: "filter", Value: f}}
		for _, m := range cmd.calls[1:] {
			switch m.name {
			case "sort":
				d, _ := argDoc(m.args, 0)
				inner = append(inner, bson.E{Key: "sort", Value: d})
			case "limit":
				inner = append(inner, bson.E{Key: "limit", Value: argInt(m.args, 0)})
			case "skip":
				inner = append(inner, bson.E{Key: "skip", Value: argInt(m.args, 0)})
			case "hint":
				if len(m.args) > 0 {
					inner = append(inner, bson.E{Key: "hint", Value: m.args[0]})
				}
			}
		}
	case "aggregate":
		p, _ := argArray(first.args, 0)
		inner = bson.D{{Key: "aggregate", Value: coll}, {Key: "pipeline", Value: p}, {Key: "cursor", Value: bson.D{}}}
	case "countDocuments", "count":
		f, _ := argDoc(first.args, 0)
		inner = bson.D{{Key: "count", Value: coll}, {Key: "query", Value: f}}
	case "distinct":
		field, _ := firstString(first.args)
		f, _ := argDoc(first.args, 1)
		inner = bson.D{{Key: "distinct", Value: coll}, {Key: "key", Value: field}, {Key: "query", Value: f}}
	case "updateOne", "updateMany":
		f, _ := argDoc(first.args, 0)
		var u any = bson.D{}
		if len(first.args) > 1 {
			u = first.args[1]
		}
		inner = bson.D{{Key: "update", Value: coll}, {Key: "updates", Value: bson.A{bson.D{{Key: "q", Value: f}, {Key: "u", Value: u}, {Key: "multi", Value: first.name == "updateMany"}}}}}
	case "deleteOne", "deleteMany":
		f, _ := argDoc(first.args, 0)
		limit := 0
		if first.name == "deleteOne" {
			limit = 1
		}
		inner = bson.D{{Key: "delete", Value: coll}, {Key: "deletes", Value: bson.A{bson.D{{Key: "q", Value: f}, {Key: "limit", Value: limit}}}}}
	default:
		return nil, fmt.Errorf("%s cannot be explained", first.name)
	}
	verbosity := "queryPlanner"
	if analyze {
		verbosity = "executionStats"
	}
	var out bson.D
	database := s.Database
	if cmd.database != "" {
		database = cmd.database
	}
	if err := c.db(database).RunCommand(ctx, bson.D{{Key: "explain", Value: inner}, {Key: "verbosity", Value: verbosity}}).Decode(&out); err != nil {
		return nil, mapError(err)
	}
	raw, _ := bson.MarshalExtJSONIndent(out, false, false, "", "  ")
	plan := &driver.Plan{Raw: string(raw), Format: "json", Totals: map[string]any{}}
	plan.Root = planFrom(out, plan.Totals)
	return plan, nil
}

func get(d bson.D, key string) any {
	for _, e := range d {
		if e.Key == key {
			return e.Value
		}
	}
	return nil
}

func getDoc(d bson.D, key string) bson.D {
	v, _ := get(d, key).(bson.D)
	return v
}

func planFrom(out bson.D, totals map[string]any) *driver.PlanNode {
	if stages, ok := get(out, "stages").(bson.A); ok {
		root := &driver.PlanNode{Operation: "Aggregation pipeline"}
		for _, st := range stages {
			sd, ok := st.(bson.D)
			if !ok || len(sd) == 0 {
				continue
			}
			if sd[0].Key == "$cursor" {
				if n := planFrom(getDoc(sd, "$cursor"), totals); n != nil {
					root.Children = append(root.Children, n)
				}
				continue
			}
			node := &driver.PlanNode{Operation: sd[0].Key}
			if n, ok := numF(get(sd, "nReturned")); ok {
				node.ActualRows = &n
			}
			if t, ok := numF(get(sd, "executionTimeMillisEstimate")); ok {
				node.TimeMS = &t
			}
			b, _ := bson.MarshalExtJSON(bson.D{sd[0]}, false, false)
			node.Detail = trim(string(b), 160)
			root.Children = append(root.Children, node)
		}
		return root
	}
	qp := getDoc(out, "queryPlanner")
	if ns, ok := get(qp, "namespace").(string); ok {
		totals["namespace"] = ns
	}
	es := getDoc(out, "executionStats")
	if len(es) > 0 {
		for _, k := range []string{"nReturned", "executionTimeMillis", "totalKeysExamined", "totalDocsExamined"} {
			if v, ok := numF(get(es, k)); ok {
				totals[k] = v
			}
		}
		if t, ok := numF(get(es, "executionTimeMillis")); ok {
			totals["Execution Time"] = t
		}
		if st := getDoc(es, "executionStages"); len(st) > 0 {
			return stageNode(st, true)
		}
	}
	wp := getDoc(qp, "winningPlan")
	if q := getDoc(wp, "queryPlan"); len(q) > 0 {
		wp = q // slot-based execution engine (7.0+)
	}
	if len(wp) == 0 {
		return nil
	}
	return stageNode(wp, false)
}

func stageNode(d bson.D, actual bool) *driver.PlanNode {
	n := &driver.PlanNode{Props: map[string]string{}}
	n.Operation, _ = get(d, "stage").(string)
	if ix, ok := get(d, "indexName").(string); ok {
		n.Object = ix
	}
	if kp := getDoc(d, "keyPattern"); len(kp) > 0 {
		b, _ := bson.MarshalExtJSON(kp, false, false)
		n.Detail = string(b)
	}
	if f := getDoc(d, "filter"); len(f) > 0 {
		b, _ := bson.MarshalExtJSON(f, false, false)
		n.Props["filter"] = trim(string(b), 400)
	}
	for _, k := range []string{"direction", "isMultiKey", "docsExamined", "keysExamined", "works", "memUsage"} {
		if v := get(d, k); v != nil {
			n.Props[k] = fmt.Sprint(v)
		}
	}
	if actual {
		if v, ok := numF(get(d, "nReturned")); ok {
			n.ActualRows = &v
		}
		if v, ok := numF(get(d, "executionTimeMillisEstimate")); ok {
			n.TimeMS = &v
		}
	}
	if in := getDoc(d, "inputStage"); len(in) > 0 {
		n.Children = append(n.Children, stageNode(in, actual))
	}
	if ins, ok := get(d, "inputStages").(bson.A); ok {
		for _, x := range ins {
			if xd, ok := x.(bson.D); ok {
				n.Children = append(n.Children, stageNode(xd, actual))
			}
		}
	}
	return n
}

func numF(v any) (float64, bool) {
	switch x := v.(type) {
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---- administration -----------------------------------------------------------

func (c *conn) Processes(ctx context.Context) (*driver.Result, error) {
	var out bson.D
	if err := c.client.Database("admin").RunCommand(ctx, bson.D{{Key: "currentOp", Value: 1}}).Decode(&out); err != nil {
		return nil, mapError(err)
	}
	res := &driver.Result{Columns: []driver.ResultColumn{
		{Name: "opid", Kind: driver.KindInt}, {Name: "active", Kind: driver.KindBool}, {Name: "secs_running", Kind: driver.KindInt},
		{Name: "op", Kind: driver.KindString}, {Name: "ns", Kind: driver.KindString}, {Name: "client", Kind: driver.KindString},
		{Name: "desc", Kind: driver.KindString}, {Name: "command", Kind: driver.KindJSON},
	}, Rows: [][]any{}}
	ops, _ := get(out, "inprog").(bson.A)
	for _, o := range ops {
		d, ok := o.(bson.D)
		if !ok {
			continue
		}
		cmd, _ := bson.MarshalExtJSON(getDoc(d, "command"), false, false)
		res.Rows = append(res.Rows, []any{encode(get(d, "opid")), get(d, "active"), encode(get(d, "secs_running")), get(d, "op"),
			get(d, "ns"), get(d, "client"), get(d, "desc"), trim(string(cmd), 2000)})
	}
	return res, nil
}

func (c *conn) KillProcess(ctx context.Context, id string) error {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid operation id %q", id)
	}
	return mapError(c.client.Database("admin").RunCommand(ctx, bson.D{{Key: "killOp", Value: 1}, {Key: "op", Value: n}}).Err())
}

func (c *conn) Variables(ctx context.Context, kind string) (*driver.Result, error) {
	res := &driver.Result{Columns: []driver.ResultColumn{{Name: "name", Kind: driver.KindString}, {Name: "value", Kind: driver.KindText}}, Rows: [][]any{}}
	var out bson.D
	cmd := bson.D{{Key: "getParameter", Value: "*"}}
	if kind == "status" {
		cmd = bson.D{{Key: "serverStatus", Value: 1}}
	}
	if err := c.client.Database("admin").RunCommand(ctx, cmd).Decode(&out); err != nil {
		return nil, mapError(err)
	}
	var walk func(prefix string, d bson.D, depth int)
	walk = func(prefix string, d bson.D, depth int) {
		for _, e := range d {
			name := e.Key
			if prefix != "" {
				name = prefix + "." + e.Key
			}
			if sub, ok := e.Value.(bson.D); ok && kind == "status" && depth < 2 {
				walk(name, sub, depth+1)
				continue
			}
			var v string
			switch x := e.Value.(type) {
			case bson.D, bson.A:
				b, _ := bson.MarshalExtJSON(bson.D{{Key: "v", Value: x}}, false, false)
				v = strings.TrimSuffix(strings.TrimPrefix(string(b), `{"v":`), "}")
			default:
				v = fmt.Sprint(encode(x))
			}
			res.Rows = append(res.Rows, []any{name, trim(v, 4000)})
		}
	}
	walk("", out, 0)
	sort.Slice(res.Rows, func(i, j int) bool { return res.Rows[i][0].(string) < res.Rows[j][0].(string) })
	return res, nil
}

func (c *conn) Users(ctx context.Context) (*driver.Result, error) {
	var out bson.D
	cmd := bson.D{{Key: "usersInfo", Value: bson.D{{Key: "forAllDBs", Value: true}}}}
	if err := c.client.Database("admin").RunCommand(ctx, cmd).Decode(&out); err != nil {
		// Without cluster privileges, fall back to the default database.
		if err2 := c.db("").RunCommand(ctx, bson.D{{Key: "usersInfo", Value: 1}}).Decode(&out); err2 != nil {
			return nil, mapError(err)
		}
	}
	res := &driver.Result{Columns: []driver.ResultColumn{{Name: "user", Kind: driver.KindString}, {Name: "db", Kind: driver.KindString}, {Name: "roles", Kind: driver.KindText}}, Rows: [][]any{}}
	users, _ := get(out, "users").(bson.A)
	for _, u := range users {
		d, ok := u.(bson.D)
		if !ok {
			continue
		}
		var roles []string
		if rs, ok := get(d, "roles").(bson.A); ok {
			for _, r := range rs {
				if rd, ok := r.(bson.D); ok {
					roles = append(roles, fmt.Sprintf("%v@%v", get(rd, "role"), get(rd, "db")))
				}
			}
		}
		res.Rows = append(res.Rows, []any{get(d, "user"), get(d, "db"), strings.Join(roles, ", ")})
	}
	return res, nil
}

func (c *conn) UserGrants(ctx context.Context, user string) ([]string, error) {
	name, db, _ := strings.Cut(user, "@")
	if db == "" {
		db = "admin"
	}
	var out bson.D
	cmd := bson.D{{Key: "usersInfo", Value: bson.D{{Key: "user", Value: name}, {Key: "db", Value: db}}}, {Key: "showPrivileges", Value: true}}
	if err := c.client.Database(db).RunCommand(ctx, cmd).Decode(&out); err != nil {
		return nil, mapError(err)
	}
	var lines []string
	users, _ := get(out, "users").(bson.A)
	for _, u := range users {
		d, ok := u.(bson.D)
		if !ok {
			continue
		}
		privs, _ := get(d, "inheritedPrivileges").(bson.A)
		for _, p := range privs {
			b, _ := bson.MarshalExtJSON(p, false, false)
			lines = append(lines, string(b))
		}
	}
	return lines, nil
}

// CatalogColumns samples each collection for editor completion and diagrams.
func (c *conn) CatalogColumns(ctx context.Context, s driver.Scope) ([]driver.CatalogTable, error) {
	objs, err := c.Objects(ctx, s)
	if err != nil {
		return nil, err
	}
	budget, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out []driver.CatalogTable
	for i, o := range objs {
		t := driver.CatalogTable{Name: o.Name, Kind: o.Kind}
		if i < 100 && budget.Err() == nil {
			if docs, err := c.sample(budget, s.Database, o.Name, 20); err == nil {
				for _, f := range inferFields(docs) {
					t.Columns = append(t.Columns, driver.CatalogColumn{Name: f.name, Type: f.dominant(), PK: f.name == "_id"})
				}
			}
		}
		out = append(out, t)
	}
	return out, nil
}
