package mssql

import (
	"encoding/xml"
	"errors"
	"io"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
)

// xnode is a generic XML element; showplans are too irregular to map onto
// fixed structs (RelOps nest inside dozens of operator-specific elements).
type xnode struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Nodes   []xnode    `xml:",any"`
}

func (n *xnode) attr(name string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func (n *xnode) num(name string) *float64 { return parseNum(n.attr(name)) }

func parseNum(s string) *float64 {
	if s == "" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &f
}

// child returns the first direct child with the given local name.
func (n *xnode) child(name string) *xnode {
	for i := range n.Nodes {
		if n.Nodes[i].XMLName.Local == name {
			return &n.Nodes[i]
		}
	}
	return nil
}

// find returns the first descendant with the given local name, depth first.
func (n *xnode) find(name string) *xnode {
	for i := range n.Nodes {
		if n.Nodes[i].XMLName.Local == name {
			return &n.Nodes[i]
		}
		if f := n.Nodes[i].find(name); f != nil {
			return f
		}
	}
	return nil
}

// parsePlan turns one or more ShowPlanXML documents into a plan tree: one
// node per statement, each holding its operator (RelOp) tree.
func parsePlan(docs []string) (*driver.Plan, error) {
	plan := &driver.Plan{Raw: strings.Join(docs, "\n"), Format: "xml", Totals: map[string]any{}}
	var stmts []*driver.PlanNode
	var compile, elapsed, cpu float64
	var timed bool
	for _, doc := range docs {
		var root xnode
		dec := xml.NewDecoder(strings.NewReader(doc))
		// The server declares UTF-16, but the text already arrived as UTF-8.
		dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
		if err := dec.Decode(&root); err != nil {
			return nil, errors.New("the server returned a plan that is not valid XML")
		}
		stmts = append(stmts, planNodes(&root)...)
		walk(&root, func(n *xnode) {
			switch n.XMLName.Local {
			case "QueryPlan":
				if v := n.num("CompileTime"); v != nil {
					compile += *v
				}
			case "QueryTimeStats":
				if v := n.num("ElapsedTime"); v != nil {
					elapsed, timed = elapsed+*v, true
				}
				if v := n.num("CpuTime"); v != nil {
					cpu += *v
				}
			}
		})
	}
	switch len(stmts) {
	case 0:
		return nil, errors.New("the server returned no execution plan")
	case 1:
		plan.Root = stmts[0]
	default:
		plan.Root = &driver.PlanNode{Operation: "Batch", Children: stmts}
	}
	plan.Totals["Compile Time"] = compile
	if timed {
		plan.Totals["Execution Time"] = elapsed
		plan.Totals["CPU Time"] = cpu
	}
	return plan, nil
}

func walk(n *xnode, fn func(*xnode)) {
	fn(n)
	for i := range n.Nodes {
		walk(&n.Nodes[i], fn)
	}
}

// planNodes collects the statement and operator nodes below n, keeping
// their nesting.
func planNodes(n *xnode) []*driver.PlanNode {
	name := n.XMLName.Local
	switch {
	case name == "RelOp":
		return []*driver.PlanNode{relOp(n)}
	case strings.HasPrefix(name, "Stmt"): // StmtSimple, StmtCond, StmtCursor...
		return []*driver.PlanNode{statement(n)}
	}
	return children(n)
}

func children(n *xnode) []*driver.PlanNode {
	var out []*driver.PlanNode
	for i := range n.Nodes {
		out = append(out, planNodes(&n.Nodes[i])...)
	}
	return out
}

func statement(n *xnode) *driver.PlanNode {
	p := &driver.PlanNode{Operation: n.attr("StatementType"), Props: map[string]string{}}
	if p.Operation == "" {
		p.Operation = strings.TrimPrefix(n.XMLName.Local, "Stmt")
	}
	p.Detail = strings.Join(strings.Fields(n.attr("StatementText")), " ")
	p.Cost = n.num("StatementSubTreeCost")
	p.Rows = n.num("StatementEstRows")
	setProp(p.Props, "Optimization level", n.attr("StatementOptmLevel"))
	setProp(p.Props, "Early abort reason", n.attr("StatementOptmEarlyAbortReason"))
	setProp(p.Props, "Cardinality model", n.attr("CardinalityEstimationModelVersion"))
	if qp := n.child("QueryPlan"); qp != nil {
		setProp(p.Props, "Degree of parallelism", qp.attr("DegreeOfParallelism"))
		setProp(p.Props, "Non-parallel reason", qp.attr("NonParallelPlanReason"))
		if mg := qp.child("MemoryGrantInfo"); mg != nil {
			setProp(p.Props, "Memory grant (KB)", mg.attr("GrantedMemory"))
		}
		if w := qp.child("Warnings"); w != nil {
			setProp(p.Props, "Warnings", warnings(w))
		}
	}
	p.Children = children(n)
	return p
}

func relOp(n *xnode) *driver.PlanNode {
	p := &driver.PlanNode{Operation: n.attr("PhysicalOp"), Props: map[string]string{}}
	p.Cost = n.num("EstimatedTotalSubtreeCost")
	p.Rows = n.num("EstimateRows")
	var detail []string
	if logical := n.attr("LogicalOp"); logical != "" && logical != p.Operation {
		detail = append(detail, logical)
	}
	// The operator-specific element (IndexScan, Hash, NestedLoops...) holds
	// the object, predicates and child operators.
	for i := range n.Nodes {
		op := &n.Nodes[i]
		switch op.XMLName.Local {
		case "OutputList", "RunTimeInformation", "Warnings", "MemoryFractions", "RunTimePartitionSummary", "InternalInfo":
			continue
		}
		if obj := op.child("Object"); obj != nil && p.Object == "" {
			p.Object = objectName(obj)
			if ix := unbracket(obj.attr("Index")); ix != "" {
				detail = append(detail, "using "+ix)
			}
		}
		if pr := op.child("Predicate"); pr != nil {
			if so := pr.find("ScalarOperator"); so != nil {
				setProp(p.Props, "Predicate", so.attr("ScalarString"))
			}
		}
		if sp := op.child("SeekPredicates"); sp != nil {
			setProp(p.Props, "Seek predicate", seekText(sp))
		}
	}
	p.Detail = strings.Join(detail, ", ")
	setProp(p.Props, "Estimated I/O", n.attr("EstimateIO"))
	setProp(p.Props, "Estimated CPU", n.attr("EstimateCPU"))
	setProp(p.Props, "Execution mode", n.attr("EstimatedExecutionMode"))
	if n.attr("Parallel") == "1" || n.attr("Parallel") == "true" {
		p.Props["Parallel"] = "yes"
	}
	if r, w := n.num("EstimateRebinds"), n.num("EstimateRewinds"); r != nil && w != nil && *r+*w > 0 {
		p.Props["Estimated executions"] = strconv.FormatFloat(*r+*w+1, 'f', -1, 64)
	}
	if w := n.child("Warnings"); w != nil {
		setProp(p.Props, "Warnings", warnings(w))
	}
	if rt := n.child("RunTimeInformation"); rt != nil {
		runtime(p, rt)
	}
	for i := range n.Nodes {
		p.Children = append(p.Children, children(&n.Nodes[i])...)
	}
	return p
}

// runtime folds per-thread counters into per-execution averages, matching
// how the plan view reads rows and times (value × loops = total).
func runtime(p *driver.PlanNode, rt *xnode) {
	var rows, execs, maxElapsed, reads float64
	var haveTime, haveReads bool
	for i := range rt.Nodes {
		t := &rt.Nodes[i]
		if t.XMLName.Local != "RunTimeCountersPerThread" {
			continue
		}
		if v := t.num("ActualRows"); v != nil {
			rows += *v
		}
		if v := t.num("ActualExecutions"); v != nil {
			execs += *v
		}
		if v := t.num("ActualElapsedms"); v != nil {
			haveTime = true
			if *v > maxElapsed {
				maxElapsed = *v
			}
		}
		if v := t.num("ActualLogicalReads"); v != nil {
			haveReads = true
			reads += *v
		}
	}
	loops := execs
	if loops < 1 {
		loops = 1
	}
	actual := rows / loops
	p.ActualRows = &actual
	p.Loops = &execs
	if haveTime {
		per := maxElapsed / loops
		p.TimeMS = &per
	}
	if haveReads {
		p.Props["Logical reads"] = strconv.FormatFloat(reads, 'f', -1, 64)
	}
}

func objectName(o *xnode) string {
	name := unbracket(o.attr("Table"))
	if s := unbracket(o.attr("Schema")); s != "" && name != "" {
		name = s + "." + name
	}
	if a := unbracket(o.attr("Alias")); a != "" && a != unbracket(o.attr("Table")) {
		name += " " + a
	}
	return name
}

func unbracket(s string) string {
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		return strings.ReplaceAll(s[1:len(s)-1], "]]", "]")
	}
	return s
}

// seekText renders seek predicates as "column = expression" pairs.
func seekText(sp *xnode) string {
	var parts []string
	walk(sp, func(n *xnode) {
		if n.XMLName.Local != "Prefix" && n.XMLName.Local != "StartRange" && n.XMLName.Local != "EndRange" {
			return
		}
		col := ""
		if rc := n.child("RangeColumns"); rc != nil {
			if cr := rc.child("ColumnReference"); cr != nil {
				col = cr.attr("Column")
			}
		}
		expr := ""
		if re := n.child("RangeExpressions"); re != nil {
			if so := re.find("ScalarOperator"); so != nil {
				expr = so.attr("ScalarString")
			}
		}
		op := map[string]string{"EQ": "=", "GT": ">", "GE": ">=", "LT": "<", "LE": "<=", "IS": "IS", "IS NOT": "IS NOT"}[n.attr("ScanType")]
		if op == "" {
			op = n.attr("ScanType")
		}
		parts = append(parts, strings.TrimSpace(col+" "+op+" "+expr))
	})
	return strings.Join(parts, " AND ")
}

func warnings(w *xnode) string {
	var names []string
	for i := range w.Nodes {
		names = append(names, w.Nodes[i].XMLName.Local)
	}
	for _, a := range w.Attrs {
		if a.Value == "1" || a.Value == "true" {
			names = append(names, a.Name.Local)
		}
	}
	return strings.Join(names, ", ")
}

func setProp(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}
