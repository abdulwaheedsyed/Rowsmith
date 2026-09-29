import { useMemo, useState } from "react";
import { ChevronDown, ChevronRight, Copy } from "lucide-react";
import type { Plan, PlanNode } from "../../lib/types";
import { compact, duration, temperForShare } from "../../lib/format";
import { Button } from "../../components/ui";

interface FlatNode {
  node: PlanNode;
  depth: number;
  path: string;
  self: number; // exclusive time or cost
  share: number;
  last: boolean[];
}

// Exclusive ("self") weight: time when the plan was analyzed, cost otherwise.
function weight(n: PlanNode, analyzed: boolean) {
  return analyzed ? (n.timeMs ?? 0) * (n.loops ?? 1) : n.cost ?? 0;
}

function flatten(root: PlanNode, analyzed: boolean): FlatNode[] {
  const out: FlatNode[] = [];
  const walk = (n: PlanNode, depth: number, path: string, last: boolean[]) => {
    const total = weight(n, analyzed);
    const kids = (n.children ?? []).reduce((s, c) => s + weight(c, analyzed), 0);
    out.push({ node: n, depth, path, self: Math.max(0, total - kids), share: 0, last });
    (n.children ?? []).forEach((c, i) => walk(c, depth + 1, `${path}.${i}`, [...last, i === (n.children?.length ?? 0) - 1]));
  };
  walk(root, 0, "0", []);
  const sum = out.reduce((s, f) => s + f.self, 0) || 1;
  for (const f of out) f.share = f.self / sum;
  return out;
}

export function PlanView({ plan }: { plan: Plan }) {
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const [openProps, setOpenProps] = useState<string | null>(null);
  const [raw, setRaw] = useState(false);
  const analyzed = useMemo(() => {
    let found = false;
    const visit = (n?: PlanNode) => {
      if (!n) return;
      if (n.actualRows !== undefined || n.timeMs !== undefined) found = true;
      n.children?.forEach(visit);
    };
    visit(plan.root);
    return found;
  }, [plan.root]);
  const flat = useMemo(() => (plan.root ? flatten(plan.root, analyzed) : []), [plan.root, analyzed]);
  const hottest = flat.reduce((a, b) => (b.share > (a?.share ?? -1) ? b : a), flat[0]);

  const hidden = (path: string) => [...collapsed].some((c) => path !== c && path.startsWith(c + "."));

  return (
    <div className="plan">
      <div className="plan__head">
        <span className="eyebrow">{analyzed ? "Actual execution" : "Estimated plan"}</span>
        {plan.totals && Object.entries(plan.totals).map(([k, v]) => (
          <span key={k} className="plan__total">{k}: <b className="mono">{typeof v === "number" ? duration(v) : String(v)}</b></span>
        ))}
        {hottest && hottest.share > 0 && (
          <span className="plan__hot">Most {analyzed ? "time" : "cost"}: <b>{hottest.node.operation}{hottest.node.object ? ` on ${hottest.node.object}` : ""}</b> ({Math.round(hottest.share * 100)}%)</span>
        )}
        <span className="spacer" />
        <div className="plan__legend" aria-label={`Heat: share of ${analyzed ? "time" : "cost"}`}>
          <span>cool</span><span className="plan__scale" /><span>hot</span>
        </div>
        <div className="segmented">
          <button aria-pressed={!raw} onClick={() => setRaw(false)}>Tree</button>
          <button aria-pressed={raw} onClick={() => setRaw(true)}>Raw</button>
        </div>
      </div>
      {raw ? (
        <div className="plan__rawwrap">
          <Button size="sm" variant="ghost" className="plan__copy" onClick={() => navigator.clipboard?.writeText(plan.raw)}><Copy /> Copy</Button>
          <pre className="plan__raw">{plan.format === "json" ? pretty(plan.raw) : plan.raw}</pre>
        </div>
      ) : (
        <div className="plan__tree" role="tree">
          <div className="plan__row plan__row--header">
            <span>Operation</span>
            <span className="num">{analyzed ? "Rows (actual / est.)" : "Rows (est.)"}</span>
            <span className="num">{analyzed ? "Time" : "Cost"}</span>
            <span>{analyzed ? "Share of time" : "Share of cost"}</span>
          </div>
          {flat.map((f) => {
            if (hidden(f.path)) return null;
            const n = f.node;
            const hasKids = !!n.children?.length;
            const isCollapsed = collapsed.has(f.path);
            const mis = analyzed && n.actualRows !== undefined && n.rows !== undefined && n.rows > 0 && (n.actualRows / n.rows > 10 || n.rows / Math.max(1, n.actualRows) > 10);
            const heat = temperForShare(f.share);
            const props = Object.entries(n.props ?? {});
            return (
              <div key={f.path} className="plan__node" role="treeitem" aria-expanded={hasKids ? !isCollapsed : undefined}>
                <div className="plan__row" onClick={() => props.length && setOpenProps(openProps === f.path ? null : f.path)}>
                  <span className="plan__op" style={{ paddingLeft: f.depth * 18 }}>
                    {hasKids ? (
                      <button className="plan__toggle" aria-label={isCollapsed ? "Expand" : "Collapse"} onClick={(e) => {
                        e.stopPropagation();
                        const s = new Set(collapsed);
                        if (isCollapsed) s.delete(f.path);
                        else s.add(f.path);
                        setCollapsed(s);
                      }}>{isCollapsed ? <ChevronRight /> : <ChevronDown />}</button>
                    ) : <span className="plan__toggle plan__toggle--leaf" />}
                    <span className="plan__swatch" style={{ background: heat, opacity: 0.35 + f.share * 0.65 }} />
                    <span className="plan__name">{n.operation}</span>
                    {n.object && <span className="plan__obj mono">{n.object}</span>}
                    {n.detail && <span className="plan__detail truncate">{n.detail}</span>}
                  </span>
                  <span className={`num mono ${mis ? "plan__mis" : ""}`} title={mis ? "Estimate is off by more than 10×; statistics may be stale" : undefined}>
                    {analyzed && n.actualRows !== undefined ? `${compact(n.actualRows)} / ` : ""}{n.rows !== undefined ? compact(n.rows) : "—"}
                    {n.loops && n.loops > 1 ? <span className="faint"> ×{compact(n.loops)}</span> : null}
                  </span>
                  <span className="num mono">{analyzed ? (n.timeMs !== undefined ? duration(n.timeMs) : "—") : n.cost !== undefined ? compact(Math.round(n.cost)) : "—"}</span>
                  <span className="plan__bar"><span style={{ width: `${Math.max(1, f.share * 100)}%`, background: heat }} /><em className="mono">{Math.round(f.share * 100)}%</em></span>
                </div>
                {openProps === f.path && props.length > 0 && (
                  <dl className="plan__props" style={{ marginLeft: f.depth * 18 + 40 }}>
                    {props.map(([k, v]) => (
                      <div key={k}><dt>{k}</dt><dd className="mono">{v}</dd></div>
                    ))}
                  </dl>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

function pretty(s: string) {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}
