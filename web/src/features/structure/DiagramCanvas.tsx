import { useMemo, useState } from "react";
import { ReactFlow, Background, Controls, MiniMap, Handle, Position, type Node, type Edge, type NodeProps } from "@xyflow/react";
import dagre from "@dagrejs/dagre";
import "@xyflow/react/dist/style.css";
import { KeyRound, Link2, Search } from "lucide-react";
import { useCatalog } from "../../lib/queries";
import type { CatalogTable, Connection } from "../../lib/types";
import { Alert, Empty, Spinner } from "../../components/ui";
import { openObject } from "../workspace/actions";
import "./diagram.css";

type TableNodeData = { t: CatalogTable; fkCols: Set<string>; dim: boolean; onOpen(): void };
const W = 240;
const ROW = 22;
const HEAD = 34;
const MAX_COLS = 14;

function TableNode({ data }: NodeProps<Node<TableNodeData>>) {
  const { t, fkCols, dim, onOpen } = data;
  const cols = t.columns.slice(0, MAX_COLS);
  return (
    <div className={`ernode ${dim ? "is-dim" : ""}`} style={{ width: W }} onDoubleClick={onOpen}>
      <Handle type="target" position={Position.Left} className="erhandle" />
      <div className="ernode__head">
        <span className="ernode__name">{t.name}</span>
        <span className="ernode__kind">{t.kind === "table" ? "" : t.kind.replace("_", " ")}</span>
      </div>
      {cols.map((c) => (
        <div key={c.name} className="ernode__col">
          {c.pk ? <KeyRound className="ernode__icon ernode__icon--pk" /> : fkCols.has(c.name) ? <Link2 className="ernode__icon ernode__icon--fk" /> : <span className="ernode__icon" />}
          <span className="ernode__cname">{c.name}</span>
          <span className="ernode__ctype">{c.type.replace(/\(.*\)/, "")}</span>
        </div>
      ))}
      {t.columns.length > MAX_COLS && <div className="ernode__more">+{t.columns.length - MAX_COLS} more</div>}
      <Handle type="source" position={Position.Right} className="erhandle" />
    </div>
  );
}

const nodeTypes = { table: TableNode };

export default function DiagramCanvas({ conn, database, schema }: { conn: Connection; database?: string; schema?: string }) {
  const cat = useCatalog(conn.id, database, schema);
  const [filter, setFilter] = useState("");
  const [onlyRelated, setOnlyRelated] = useState(false);

  const { nodes, edges } = useMemo(() => {
    const tables = (cat.data ?? []).filter((t) => t.kind !== "view" || !onlyRelated);
    const names = new Set(tables.map((t) => t.name));
    const edges: Edge[] = [];
    const linked = new Set<string>();
    for (const t of tables) {
      for (const fk of t.fks ?? []) {
        if (!names.has(fk.refTable.name)) continue;
        linked.add(t.name);
        linked.add(fk.refTable.name);
        edges.push({
          id: `${t.name}.${fk.name}`,
          source: t.name,
          target: fk.refTable.name,
          label: fk.columns.join(", "),
          type: "smoothstep",
          className: "eredge",
          labelBgPadding: [4, 2],
          labelBgBorderRadius: 3,
        });
      }
    }
    const visible = tables.filter((t) => !onlyRelated || linked.has(t.name));
    const heightOf = (t: CatalogTable) => HEAD + Math.min(MAX_COLS, t.columns.length) * ROW + (t.columns.length > MAX_COLS ? ROW : 0) + 8;
    // Related tables get a layered layout; unrelated ones are packed in a grid beside it.
    const g = new dagre.graphlib.Graph();
    g.setGraph({ rankdir: "LR", nodesep: 28, ranksep: 90, marginx: 20, marginy: 20 });
    g.setDefaultEdgeLabel(() => ({}));
    for (const t of visible) if (linked.has(t.name)) g.setNode(t.name, { width: W, height: heightOf(t) });
    for (const e of edges) if (g.hasNode(e.source) && g.hasNode(e.target)) g.setEdge(e.source, e.target);
    dagre.layout(g);
    let maxX = 0;
    for (const n of g.nodes()) maxX = Math.max(maxX, g.node(n).x + W / 2);
    const loose = visible.filter((t) => !linked.has(t.name));
    const perRow = Math.max(2, Math.ceil(Math.sqrt(loose.length * 1.6)));
    const colHeights = new Array(perRow).fill(20);
    const pos = new Map<string, { x: number; y: number; width: number; height: number }>();
    loose.forEach((t) => {
      const col = colHeights.indexOf(Math.min(...colHeights));
      const h = heightOf(t);
      pos.set(t.name, { x: (maxX ? maxX + 120 : 20) + col * (W + 32) + W / 2, y: colHeights[col] + h / 2, width: W, height: h });
      colHeights[col] += h + 32;
    });
    const needle = filter.trim().toLowerCase();
    const nodes: Node<TableNodeData>[] = visible.map((t) => {
      const p = g.hasNode(t.name) ? g.node(t.name) : pos.get(t.name)!;
      const fkCols = new Set((t.fks ?? []).flatMap((f) => f.columns));
      return {
        id: t.name,
        type: "table",
        position: { x: p.x - p.width / 2, y: p.y - p.height / 2 },
        data: { t, fkCols, dim: !!needle && !t.name.toLowerCase().includes(needle), onOpen: () => openObject(conn, { database, schema, name: t.name, kind: t.kind }, "structure") },
      };
    });
    return { nodes, edges: edges.filter((e) => g.hasNode(e.source) && g.hasNode(e.target)) };
  }, [cat.data, filter, onlyRelated, conn, database, schema]);

  if (cat.isLoading) return <div className="structure__center"><Spinner large /></div>;
  if (cat.error) return <div className="structure__pad"><Alert kind="danger">{(cat.error as Error).message}</Alert></div>;
  if (!nodes.length) return <Empty title="No tables to draw">This schema has no tables{onlyRelated ? " with relationships" : ""}.</Empty>;

  return (
    <div className="diagram">
      <div className="diagram__bar">
        <label className="diagram__search"><Search /><input className="input" placeholder="Highlight tables" value={filter} onChange={(e) => setFilter(e.target.value)} /></label>
        <label className="check"><input type="checkbox" checked={onlyRelated} onChange={(e) => setOnlyRelated(e.target.checked)} /> Only tables with relationships</label>
        <span className="spacer" />
        <span className="faint">{nodes.length} tables · {edges.length} relationships · double-click a table to open it</span>
      </div>
      <div className="diagram__canvas">
        <ReactFlow nodes={nodes} edges={edges} nodeTypes={nodeTypes} fitView minZoom={0.1} maxZoom={1.6} proOptions={{ hideAttribution: true }} nodesConnectable={false} colorMode="system">
          <Background gap={22} size={1} />
          <Controls showInteractive={false} />
          <MiniMap pannable zoomable nodeColor="var(--line-strong)" nodeStrokeWidth={0} maskColor="color-mix(in srgb, var(--bg) 55%, transparent)" />
        </ReactFlow>
      </div>
    </div>
  );
}
