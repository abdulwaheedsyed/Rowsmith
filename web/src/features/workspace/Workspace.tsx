import { useMemo, useState, type ReactNode } from "react";
import { useShallow } from "zustand/react/shallow";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import * as RContext from "@radix-ui/react-context-menu";
import {
  X, Plus, Table2, TerminalSquare, Columns3, FileCode2, Activity, SlidersHorizontal, UserRound, Server, Network, RefreshCw, Skull, Database, HardDrive,
} from "lucide-react";
import { get, post, qs } from "../../lib/api";
import { useDatabases, useDriver, useServer } from "../../lib/queries";
import { useWorkspace, toast, type Tab } from "../../lib/store";
import type { Connection, HistoryEntry, Me, Result } from "../../lib/types";
import { ago, bytes, duration, temperForMs, modKey, cellText } from "../../lib/format";
import { Alert, Button, Dialog, Empty, EngineBadge, Env, Kbd, Spinner } from "../../components/ui";
import { BrowseTab } from "../browse/BrowseTab";
import { QueryTab } from "../query/QueryTab";
import { StructureTab, DefinitionTab } from "../structure/StructureTab";
import { DataGrid } from "../grid/DataGrid";
import { DiagramTab } from "../structure/DiagramTab";
import { newQueryTab, openPanel, openObject } from "./actions";
import { useRuns } from "../query/runs";
import "./workspace.css";

const TAB_ICONS: Record<Tab["kind"], typeof Table2> = {
  browse: Table2, query: TerminalSquare, structure: Columns3, definition: FileCode2, processes: Activity,
  variables: SlidersHorizontal, users: UserRound, overview: Server, diagram: Network,
};

export function Workspace({ conn, me }: { conn: Connection; me: Me }) {
  const tabs = useWorkspace(useShallow((s) => s.tabs.filter((t) => t.connId === conn.id)));
  const activeId = useWorkspace((s) => s.active[conn.id]);
  const active = tabs.find((t) => t.id === activeId) ?? tabs[tabs.length - 1];
  void me;
  return (
    <div className={`workspace workspace--${conn.environment}`}>
      <TabsBar conn={conn} tabs={tabs} activeId={active?.id} />
      <div className="workspace__body">
        {tabs.length === 0 && <ConnectionHome conn={conn} />}
        {tabs.map((t) => (
          <div key={t.id} className="workspace__pane" hidden={t.id !== active?.id}>
            <TabContent tab={t} conn={conn} active={t.id === active?.id} />
          </div>
        ))}
      </div>
    </div>
  );
}

function TabContent({ tab, conn, active }: { tab: Tab; conn: Connection; active: boolean }) {
  switch (tab.kind) {
    case "browse":
      return <BrowseTab tab={tab} conn={conn} active={active} />;
    case "query":
      return <QueryTab tab={tab} conn={conn} active={active} />;
    case "structure":
      return <StructureTab tab={tab} conn={conn} />;
    case "definition":
      return <DefinitionTab tab={tab} conn={conn} />;
    case "overview":
      return <ConnectionHome conn={conn} />;
    case "processes":
      return <ProcessesPanel conn={conn} />;
    case "variables":
      return <VariablesPanel conn={conn} />;
    case "users":
      return <UsersPanel conn={conn} />;
    case "diagram":
      return <DiagramTab tab={tab} conn={conn} />;
  }
}

function TabsBar({ conn, tabs, activeId }: { conn: Connection; tabs: Tab[]; activeId?: string }) {
  const { setActive, closeTab, closeOthers, pinTab, moveTab } = useWorkspace();
  const allTabs = useWorkspace((s) => s.tabs);
  const runs = useRuns((s) => s.runs);
  const [drag, setDrag] = useState<string | null>(null);
  const [closing, setClosing] = useState<Tab | null>(null);

  const requestClose = (t: Tab) => {
    if (t.kind === "query" && runs[t.id]?.inTx) return setClosing(t);
    doClose(t);
  };
  const doClose = (t: Tab) => {
    if (t.kind === "query") {
      const c = runs[t.id]?.console;
      if (c) post(`c/${conn.id}/console/close`, { console: c }).catch(() => {});
      useRuns.getState().clear(t.id);
    }
    closeTab(t.id);
  };

  return (
    <div className="tabsbar" role="tablist" aria-label="Open tabs">
      <div className="tabsbar__scroll">
        {tabs.map((t) => {
          const Icon = TAB_ICONS[t.kind];
          const running = t.kind === "query" && runs[t.id]?.status === "running";
          const inTx = t.kind === "query" && runs[t.id]?.inTx;
          return (
            <RContext.Root key={t.id}>
              <RContext.Trigger asChild>
                <div
                  role="tab"
                  aria-selected={t.id === activeId}
                  tabIndex={0}
                  draggable
                  onDragStart={() => setDrag(t.id)}
                  onDragOver={(e) => e.preventDefault()}
                  onDrop={() => {
                    if (drag && drag !== t.id) moveTab(drag, allTabs.findIndex((x) => x.id === t.id));
                    setDrag(null);
                  }}
                  className={`tab ${t.id === activeId ? "is-active" : ""} ${t.preview ? "is-preview" : ""}`}
                  onClick={() => setActive(conn.id, t.id)}
                  onDoubleClick={() => pinTab(t.id)}
                  onAuxClick={(e) => e.button === 1 && requestClose(t)}
                  onKeyDown={(e) => e.key === "Enter" && setActive(conn.id, t.id)}
                  title={t.ref ? [t.ref.database, t.ref.schema, t.ref.name].filter(Boolean).join(".") : t.title}
                >
                  {running ? <span className="spinner tab__spin" /> : <Icon className={`tab__icon tab__icon--${t.kind}`} />}
                  <span className="tab__title truncate">{t.title}</span>
                  {inTx && <span className="tab__tx" title="Transaction open">●</span>}
                  <button className="tab__close" aria-label={`Close ${t.title}`} onClick={(e) => { e.stopPropagation(); requestClose(t); }}>
                    <X />
                  </button>
                </div>
              </RContext.Trigger>
              <RContext.Portal>
                <RContext.Content className="menu">
                  <RContext.Item className="menu__item" onSelect={() => requestClose(t)}><X /> Close</RContext.Item>
                  <RContext.Item className="menu__item" onSelect={() => closeOthers(t.id)}><X /> Close others</RContext.Item>
                  {t.preview && <RContext.Item className="menu__item" onSelect={() => pinTab(t.id)}>Keep open</RContext.Item>}
                  {t.ref && t.kind !== "structure" && <RContext.Item className="menu__item" onSelect={() => openObject(conn, t.ref!, "structure")}><Columns3 /> Open structure</RContext.Item>}
                  {t.ref && t.kind !== "browse" && t.kind !== "definition" && <RContext.Item className="menu__item" onSelect={() => openObject(conn, t.ref!, "browse")}><Table2 /> Open data</RContext.Item>}
                </RContext.Content>
              </RContext.Portal>
            </RContext.Root>
          );
        })}
      </div>
      <button className="tabsbar__new" onClick={() => newQueryTab(conn)} aria-label="New query tab" title="New query tab">
        <Plus />
      </button>
      <Dialog open={!!closing} onOpenChange={(o) => !o && setClosing(null)} title="Close with an open transaction?"
        description="Closing this tab ends its database session. Uncommitted changes in the open transaction will be rolled back."
        footer={<><Button onClick={() => setClosing(null)}>Keep tab</Button><Button variant="danger" onClick={() => { doClose(closing!); setClosing(null); }}>Roll back and close</Button></>} />
    </div>
  );
}

// ---- Connection home (no tabs / overview) ----------------------------------------

function ConnectionHome({ conn }: { conn: Connection }) {
  const drv = useDriver(conn.driver);
  const server = useServer(conn.id);
  const dbs = useDatabases(conn.id, !!drv?.caps.databases && !server.isError);
  const setScope = useWorkspace((s) => s.setScope);
  const history = useQuery({ queryKey: ["history", conn.id], queryFn: () => get<HistoryEntry[]>(`history${qs({ connection: conn.id, limit: 8 })}`) });
  const extras = Object.entries(server.data?.server.extras ?? {}).filter(([k]) => k !== "Build");
  const maxSize = Math.max(1, ...(dbs.data ?? []).map((d) => d.size ?? 0));

  return (
    <div className="connhome">
      <header className="connhome__head">
        <EngineBadge driver={conn.driver} size={44} />
        <div className="grow" style={{ minWidth: 0 }}>
          <h1 className="connhome__title display truncate">{conn.name}</h1>
          <div className="connhome__meta">
            <Env env={conn.environment} />
            {server.data && <span className="mono">{server.data.server.product} {server.data.server.version}</span>}
            {server.data && <span className="mono muted">as {server.data.server.user}</span>}
            {extras.map(([k, v]) => <span key={k} className="badge">{k} {v}</span>)}
          </div>
        </div>
        <Button variant="primary" onClick={() => newQueryTab(conn)}><TerminalSquare /> New query</Button>
      </header>
      {server.isError && <Alert kind="danger" title="Cannot connect">{(server.error as Error).message}</Alert>}
      {server.isLoading && <div className="row gap-3 muted"><Spinner /> Connecting…</div>}
      {conn.notes && <div className="connhome__notes">{conn.notes}</div>}

      <div className="connhome__grid">
        {drv?.caps.databases && (
          <section className="card connhome__card">
            <h2 className="eyebrow">Databases</h2>
            {dbs.isLoading && <Spinner />}
            <div className="dblist">
              {(dbs.data ?? []).filter((d) => !d.system).concat((dbs.data ?? []).filter((d) => d.system)).map((d) => (
                <button key={d.name} className={`dblist__row ${d.system ? "is-system" : ""}`} onClick={() => setScope(conn.id, { database: d.name, schema: undefined })}>
                  <Database />
                  <span className="dblist__name truncate">{d.name}</span>
                  <span className="dblist__bar"><span style={{ width: `${((d.size ?? 0) / maxSize) * 100}%` }} /></span>
                  <span className="dblist__size mono">{d.size !== undefined ? bytes(d.size) : ""}</span>
                </button>
              ))}
            </div>
          </section>
        )}
        <section className="card connhome__card">
          <h2 className="eyebrow">Recent here</h2>
          {history.data?.length === 0 && <p className="muted connhome__empty">Nothing run on this connection yet.</p>}
          <ol className="recent">
            {history.data?.map((h) => (
              <li key={h.id}>
                <button className="recent__item" onClick={() => newQueryTab(conn, { sql: h.body, database: h.database || undefined })}>
                  <code className="recent__sql">{h.body.replace(/\s+/g, " ").slice(0, 160)}</code>
                  <span className="recent__meta">
                    <span className="heat" style={{ ["--heat" as string]: temperForMs(h.durationMs) }}>{duration(h.durationMs)}</span>
                    <span className="faint">{h.status === "ok" ? `${h.rowCount} rows` : h.status}</span>
                    <span className="faint">{ago(h.startedAt)}</span>
                  </span>
                </button>
              </li>
            ))}
          </ol>
        </section>
        <section className="card connhome__card connhome__tools">
          <h2 className="eyebrow">Tools</h2>
          {drv?.caps.processes && <ToolButton icon={<Activity />} title="Running processes" desc="See and stop active queries" onClick={() => openPanel(conn, "processes")} />}
          {drv?.caps.variables && <ToolButton icon={<SlidersHorizontal />} title="Variables & status" desc="Server configuration and counters" onClick={() => openPanel(conn, "variables")} />}
          {drv?.caps.users && <ToolButton icon={<UserRound />} title="Database users" desc="Accounts, roles and grants" onClick={() => openPanel(conn, "users")} />}
          <div className="connhome__tip muted">
            <Kbd>{modKey()}</Kbd><Kbd>K</Kbd> jumps to any table · <Kbd>{modKey()}</Kbd><Kbd>B</Kbd> toggles the navigator
          </div>
        </section>
      </div>
    </div>
  );
}

function ToolButton({ icon, title, desc, onClick }: { icon: ReactNode; title: string; desc: string; onClick(): void }) {
  return (
    <button className="toolbtn" onClick={onClick}>
      <span className="toolbtn__icon">{icon}</span>
      <span className="grow">
        <span className="toolbtn__title">{title}</span>
        <span className="toolbtn__desc">{desc}</span>
      </span>
    </button>
  );
}

// ---- Panels ----------------------------------------------------------------------

function ResultPanel({ title, query, actions, onRow }: { title: string; query: ReturnType<typeof useQuery<Result>>; actions?: ReactNode; onRow?(row: number): void }) {
  const cols = useMemo(() => (query.data?.columns ?? []).map((c) => ({ name: c.name, type: c.type.toLowerCase(), kind: c.kind })), [query.data]);
  return (
    <div className="panel">
      <div className="panel__bar">
        <h2 className="panel__title">{title}</h2>
        {query.data && <span className="faint tnum">{query.data.rows.length} rows</span>}
        <span className="spacer" />
        {actions}
        <Button size="sm" variant="ghost" onClick={() => query.refetch()}><RefreshCw className={query.isFetching ? "spin" : ""} /> Refresh</Button>
      </div>
      {query.error ? <div className="panel__pad"><Alert kind="danger">{(query.error as Error).message}</Alert></div> :
        !query.data ? <div className="panel__center"><Spinner large /></div> :
        <DataGrid columns={cols} rows={query.data.rows} onActiveChange={(r) => onRow?.(r)} />}
    </div>
  );
}

function ProcessesPanel({ conn }: { conn: Connection }) {
  const qc = useQueryClient();
  const [auto, setAuto] = useState(false);
  const q = useQuery({ queryKey: ["processes", conn.id], queryFn: () => get<Result>(`c/${conn.id}/processes`), refetchInterval: auto ? 3000 : false });
  const [row, setRow] = useState<number | null>(null);
  const [kill, setKill] = useState<string | null>(null);
  const idCol = q.data?.columns.findIndex((c) => /^(id|pid|session_id|sid)$/i.test(c.name)) ?? -1;
  const selectedId = row !== null && idCol >= 0 ? cellText(q.data!.rows[row]?.[idCol]) : null;
  return (
    <>
      <ResultPanel title="Processes" query={q} onRow={setRow} actions={
        <>
          <label className="switch"><input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} /> Auto-refresh</label>
          {conn.access !== "read" && (
            <Button size="sm" variant="ghost" disabled={!selectedId} onClick={() => setKill(selectedId)}><Skull /> Stop {selectedId ? `#${selectedId}` : "process"}</Button>
          )}
        </>
      } />
      <Dialog open={!!kill} onOpenChange={(o) => !o && setKill(null)} title={`Stop process ${kill}?`} description="The connection is terminated and its current query is aborted. Any open transaction rolls back."
        footer={<><Button onClick={() => setKill(null)}>Cancel</Button><Button variant="danger" onClick={async () => {
          try {
            await post(`c/${conn.id}/processes/kill`, { id: kill });
            toast.success(`Process ${kill} stopped`);
            qc.invalidateQueries({ queryKey: ["processes", conn.id] });
          } catch (e) {
            toast.error("Could not stop process", (e as Error).message);
          }
          setKill(null);
        }}>Stop process</Button></>} />
    </>
  );
}

function VariablesPanel({ conn }: { conn: Connection }) {
  const [kind, setKind] = useState<"variables" | "status">("variables");
  const q = useQuery({ queryKey: ["variables", conn.id, kind], queryFn: () => get<Result>(`c/${conn.id}/variables?kind=${kind}`) });
  return (
    <ResultPanel title={kind === "variables" ? "Server variables" : "Server status"} query={q} actions={
      <div className="segmented">
        <button aria-pressed={kind === "variables"} onClick={() => setKind("variables")}>Variables</button>
        <button aria-pressed={kind === "status"} onClick={() => setKind("status")}>Status</button>
      </div>
    } />
  );
}

function UsersPanel({ conn }: { conn: Connection }) {
  const q = useQuery({ queryKey: ["dbusers", conn.id], queryFn: () => get<Result>(`c/${conn.id}/db-users`) });
  const [row, setRow] = useState<number | null>(null);
  const user = row !== null && q.data ? (() => {
    const r = q.data.rows[row];
    const names = q.data.columns.map((c) => c.name.toLowerCase());
    const u = cellText(r[Math.max(0, names.findIndex((n) => n === "user" || n === "role" || n === "name"))]);
    const hi = names.indexOf("host");
    return hi >= 0 ? `${u}@${cellText(r[hi])}` : u;
  })() : null;
  const grants = useQuery({ queryKey: ["grants", conn.id, user], queryFn: () => get<{ grants: string[] }>(`c/${conn.id}/db-users/grants${qs({ user })}`), enabled: !!user });
  return (
    <div className="panel panel--split">
      <ResultPanel title="Database users" query={q} onRow={setRow} />
      <aside className="grants">
        <h3 className="eyebrow">{user ? `Grants for ${user}` : "Select a user"}</h3>
        {grants.isLoading && user && <Spinner />}
        {grants.error && <Alert kind="danger">{(grants.error as Error).message}</Alert>}
        {grants.data?.grants.length === 0 && <p className="muted">No grants found.</p>}
        {grants.data?.grants.map((g, i) => <pre key={i} className="grants__item mono">{g}</pre>)}
      </aside>
    </div>
  );
}

export function Placeholder({ icon, title, children }: { icon: ReactNode; title: string; children: ReactNode }) {
  return <Empty icon={icon} title={title}>{children}</Empty>;
}

export { HardDrive };
