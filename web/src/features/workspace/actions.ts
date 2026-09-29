import { useWorkspace, type Tab } from "../../lib/store";
import type { Connection, ObjectRef } from "../../lib/types";
import { go } from "../../lib/nav";
import { refLabel } from "../../lib/queries";

function ensureRoute(conn: Connection) {
  if (!window.location.pathname.includes(`/c/${conn.id}`)) go(`/c/${conn.id}`);
}

let queryCounter = 0;

export function newQueryTab(conn: Connection, opts: { sql?: string; database?: string; schema?: string; title?: string; savedQueryId?: string } = {}) {
  const ws = useWorkspace.getState();
  const scope = ws.scope[conn.id] ?? {};
  const n = ws.tabs.filter((t) => t.connId === conn.id && t.kind === "query").length + 1 + queryCounter++ * 0;
  const id = ws.openTab({
    connId: conn.id,
    kind: "query",
    title: opts.title ?? `Query ${n}`,
    sql: opts.sql ?? "",
    database: opts.database ?? scope.database,
    schema: opts.schema ?? scope.schema,
    savedQueryId: opts.savedQueryId,
  });
  ensureRoute(conn);
  return id;
}

export function openObject(conn: Connection, ref: ObjectRef, kind: Tab["kind"] = "browse", preview = false) {
  const ws = useWorkspace.getState();
  const id = ws.openTab({ connId: conn.id, kind, title: refLabel(ref), ref, database: ref.database, schema: ref.schema, preview });
  ensureRoute(conn);
  return id;
}

export function openPanel(conn: Connection, kind: "overview" | "processes" | "variables" | "users") {
  const titles = { overview: "Overview", processes: "Processes", variables: "Variables", users: "Database users" };
  const id = useWorkspace.getState().openTab({ connId: conn.id, kind, title: titles[kind] });
  ensureRoute(conn);
  return id;
}

export function openDiagram(conn: Connection, database?: string, schema?: string) {
  const id = useWorkspace.getState().openTab({ connId: conn.id, kind: "diagram", title: `Diagram · ${schema || database || "schema"}`, database, schema });
  ensureRoute(conn);
  return id;
}
