import { useEffect, useMemo, useState, type ReactNode } from "react";
import * as RContext from "@radix-ui/react-context-menu";
import * as RPopover from "@radix-ui/react-popover";
import { Command } from "cmdk";
import { useQueryClient } from "@tanstack/react-query";
import {
  ChevronDown, ChevronRight, Table2, Eye, FunctionSquare, Zap, CalendarClock, Hash, Shapes, Puzzle, Search, RefreshCw,
  MoreHorizontal, Pencil, Server, Activity, SlidersHorizontal, UserRound, Columns3, TerminalSquare, Copy, Scissors, Trash2, Database, Layers, Network, FileCode2, ListTree, Package, Link2, TableCellsSplit, LineChart, BookOpen, Unplug, PencilRuler, Download, FileUp, Plus, ArrowRightLeft,
} from "lucide-react";
import { go } from "../../lib/nav";
import { useDatabases, useDriver, useObjects, useSchemas, useServer, qualified } from "../../lib/queries";
import { useUI, useWorkspace } from "../../lib/store";
import type { Connection, DbObject, ObjectRef } from "../../lib/types";
import { bytes, compact, int } from "../../lib/format";
import { Button, EngineBadge, Menu, MenuContent, MenuItem, MenuSep, MenuTrigger, Spinner, Tip, Alert } from "../../components/ui";
import { newQueryTab, openObject, openPanel, openDiagram, openDesign } from "../workspace/actions";
import { DDLDialog, type DDLAction } from "../structure/DDLDialog";
import { NameDDLDialog } from "../structure/NameDDLDialog";
import { openExport, openImport } from "../transfer/store";
import { requestDisconnect } from "../workspace/disconnect";
import "./navigator.css";

const ICONS: Record<string, typeof Table2> = {
  table: Table2, partitioned_table: Layers, view: Eye, materialized_view: Eye, foreign_table: Table2, function: FunctionSquare,
  procedure: FunctionSquare, trigger: Zap, event: CalendarClock, sequence: Hash, type: Shapes, extension: Puzzle, collection: Table2,
  routine: FunctionSquare, index: ListTree, package: Package, synonym: Link2, external_table: TableCellsSplit, timeseries: LineChart,
  dictionary: BookOpen,
};

export function kindIcon(kind: string) {
  return ICONS[kind] ?? FileCode2;
}

const DESIGNABLE = new Set(["table", "partitioned_table", "collection"]);
const BROWSABLE = new Set(["table", "partitioned_table", "view", "materialized_view", "foreign_table", "external_table", "collection", "timeseries", "dictionary"]);

/** A table's rows and size, for its tooltip. */
function facts(o: DbObject) {
  const has = (v?: number | null): v is number => v !== undefined && v !== null;
  return [has(o.rows) && `${int(o.rows)} rows (est.)`, has(o.size) && `${bytes(o.size)} on disk`].filter(Boolean).join(" · ");
}

export function Navigator({ conn, onEdit }: { conn: Connection; onEdit(): void }) {
  const drv = useDriver(conn.driver);
  const server = useServer(conn.id);
  const scope = useWorkspace((s) => s.scope[conn.id]) ?? {};
  const measure = useUI((s) => s.navMeasure);
  const setMeasure = useUI((s) => s.setNavMeasure);
  const setScope = useWorkspace((s) => s.setScope);
  const expanded = useWorkspace((s) => s.expanded);
  const toggle = useWorkspace((s) => s.toggle);
  const activeRef = useWorkspace((s) => {
    const t = s.tabs.find((x) => x.id === s.active[conn.id]);
    return t?.ref;
  });
  const qc = useQueryClient();
  const [filter, setFilter] = useState("");
  const [ddl, setDdl] = useState<{ action: DDLAction; ref: ObjectRef } | null>(null);
  const [nameDlg, setNameDlg] = useState<{ action: "create_database" | "create_schema" | "rename"; target?: ObjectRef } | null>(null);
  const canWrite = conn.access !== "read" && !conn.readOnly;

  const hasDbs = !!drv?.caps.databases;
  const hasSchemas = !!drv?.caps.schemas;
  const dbs = useDatabases(conn.id, hasDbs && !server.isError);
  const schemas = useSchemas(conn.id, scope.database, hasSchemas && (!hasDbs || !!scope.database) && !server.isError);
  const ready = (!hasDbs || !!scope.database) && (!hasSchemas || !!scope.schema);
  const objects = useObjects(conn.id, scope.database, scope.schema, ready && !server.isError);

  // Choose sensible defaults for database and schema.
  useEffect(() => {
    if (!hasDbs || scope.database || !dbs.data) return;
    const configured = (conn.params as Record<string, unknown>).database as string | undefined;
    const current = server.data?.server.database;
    // Prefer the configured database, then the session's, then the biggest user database.
    const user = dbs.data.filter((d) => !d.system).sort((a, b) => (b.size ?? 0) - (a.size ?? 0) || (b.tables ?? 0) - (a.tables ?? 0));
    const pick = dbs.data.find((d) => d.name === configured) ?? dbs.data.find((d) => d.name === current) ?? user[0] ?? dbs.data[0];
    if (pick) setScope(conn.id, { database: pick.name, schema: undefined });
  }, [hasDbs, dbs.data, scope.database, conn, server.data, setScope]);
  useEffect(() => {
    if (!hasSchemas || scope.schema || !schemas.data) return;
    const pick = schemas.data.find((s) => s.name === "public") ?? schemas.data.find((s) => s.name === "dbo") ?? schemas.data.find((s) => !s.system) ?? schemas.data[0];
    if (pick) setScope(conn.id, { schema: pick.name });
  }, [hasSchemas, schemas.data, scope.schema, conn.id, setScope]);

  const groups = useMemo(() => {
    const needle = filter.trim().toLowerCase();
    const byKind = new Map<string, DbObject[]>();
    for (const o of objects.data ?? []) {
      if (needle && !o.name.toLowerCase().includes(needle)) continue;
      if (!byKind.has(o.kind)) byKind.set(o.kind, []);
      byKind.get(o.kind)!.push(o);
    }
    const order = drv?.kinds.map((k) => k.kind) ?? [];
    return [...byKind.entries()].sort(([a], [b]) => (order.indexOf(a) < 0 ? 99 : order.indexOf(a)) - (order.indexOf(b) < 0 ? 99 : order.indexOf(b)));
  }, [objects.data, filter, drv]);

  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["objects", conn.id] });
    qc.invalidateQueries({ queryKey: ["dbs", conn.id] });
    qc.invalidateQueries({ queryKey: ["schemas", conn.id] });
    qc.invalidateQueries({ queryKey: ["describe", conn.id] });
    qc.invalidateQueries({ queryKey: ["catalog", conn.id] });
  };

  const q = drv?.quoteChar ?? '"';
  const refOf = (o: DbObject): ObjectRef => ({ database: scope.database, schema: scope.schema, name: o.name, kind: o.kind });

  // What can be done with an object: shown by right-clicking it and by its ⋯ button.
  type Action = { icon: ReactNode; label: ReactNode; onSelect(): void; danger?: boolean } | "sep";
  const objectActions = (o: DbObject, ref: ObjectRef): Action[] => {
    const browsable = BROWSABLE.has(o.kind);
    const a: Action[] = [];
    if (browsable) {
      a.push({ icon: <Table2 />, label: "Open data", onSelect: () => openObject(conn, ref, "browse") });
      a.push({ icon: <Columns3 />, label: "Structure", onSelect: () => openObject(conn, ref, "structure") });
      a.push({ icon: <TerminalSquare />, label: "Query in new tab", onSelect: () => newQueryTab(conn, { sql: sampleQuery(drv?.dialect, ref, q), database: scope.database, schema: scope.schema }) });
    } else {
      a.push({ icon: <FileCode2 />, label: "Show definition", onSelect: () => openObject(conn, ref, "definition") });
    }
    if (canWrite && drv?.design && DESIGNABLE.has(o.kind)) a.push({ icon: <PencilRuler />, label: "Edit structure", onSelect: () => openDesign(conn, { ref }) });
    if (browsable) {
      a.push("sep", { icon: <Download />, label: "Export…", onSelect: () => openExport(conn, { kind: "table", ref, rows: o.rows ?? undefined }) });
      if (canWrite && drv?.caps.editRows && DESIGNABLE.has(o.kind)) a.push({ icon: <FileUp />, label: "Import data…", onSelect: () => openImport(conn, { kind: "table", ref }) });
    }
    a.push("sep");
    if (canWrite && drv?.caps.ddl) a.push({ icon: <Pencil />, label: "Rename…", onSelect: () => setNameDlg({ action: "rename", target: ref }) });
    a.push({ icon: <Copy />, label: "Copy name", onSelect: () => navigator.clipboard?.writeText(o.name) });
    a.push({ icon: <Copy />, label: "Copy qualified name", onSelect: () => navigator.clipboard?.writeText(qualified(ref, q, true)) });
    if (canWrite && drv?.caps.ddl) {
      a.push("sep");
      if (o.kind === "table") a.push({ icon: <Scissors />, label: "Empty table…", danger: true, onSelect: () => setDdl({ action: "truncate", ref }) });
      a.push({ icon: <Trash2 />, label: `Drop ${o.kind.replace("_", " ")}…`, danger: true, onSelect: () => setDdl({ action: "drop", ref }) });
    }
    return a;
  };

  // After dropping the database or schema in view, move to another one.
  const afterDrop = (action: DDLAction, ref: ObjectRef) => {
    if (action === "drop_database") {
      const configured = (conn.params as Record<string, unknown>).database as string | undefined;
      const rest = (dbs.data ?? []).filter((d) => d.name !== ref.database);
      const pick = rest.find((d) => d.name === configured) ?? rest.find((d) => !d.system) ?? rest[0];
      setScope(conn.id, { database: pick?.name, schema: undefined });
    }
    if (action === "drop_schema") {
      const rest = (schemas.data ?? []).filter((s) => s.name !== ref.schema);
      const pick = rest.find((s) => s.name === "public") ?? rest.find((s) => s.name === "dbo") ?? rest.find((s) => !s.system) ?? rest[0];
      setScope(conn.id, { schema: pick?.name });
    }
    refresh();
  };

  return (
    <div className="navigator">
      <div className="navigator__conn">
        <EngineBadge driver={conn.driver} size={24} />
        <div className="grow" style={{ minWidth: 0 }}>
          <div className="navigator__name truncate">{conn.name}</div>
          <div className="navigator__ver mono truncate">{server.isError ? "unreachable" : server.data ? `${server.data.server.product} ${server.data.server.version}` : "connecting…"}</div>
        </div>
        <Menu>
          <MenuTrigger asChild>
            <Button variant="ghost" size="sm" icon aria-label="Connection menu"><MoreHorizontal /></Button>
          </MenuTrigger>
          <MenuContent align="end">
            <MenuItem icon={<TerminalSquare />} onSelect={() => newQueryTab(conn)}>New query</MenuItem>
            <MenuItem icon={<Server />} onSelect={() => openPanel(conn, "overview")}>Server overview</MenuItem>
            {drv?.caps.foreignKeys && <MenuItem icon={<Network />} onSelect={() => openDiagram(conn, scope.database, scope.schema)}>Relationship diagram</MenuItem>}
            {drv?.caps.processes && <MenuItem icon={<Activity />} onSelect={() => openPanel(conn, "processes")}>Processes</MenuItem>}
            {drv?.caps.variables && <MenuItem icon={<SlidersHorizontal />} onSelect={() => openPanel(conn, "variables")}>Variables & status</MenuItem>}
            {drv?.caps.users && <MenuItem icon={<UserRound />} onSelect={() => openPanel(conn, "users")}>Database users</MenuItem>}
            {canWrite && (drv?.design || drv?.caps.createDatabase || drv?.caps.editRows) && <MenuSep />}
            {canWrite && drv?.design && ready && <MenuItem icon={<PencilRuler />} onSelect={() => openDesign(conn, { database: scope.database, schema: scope.schema })}>New {drv.caps.documents ? "collection" : "table"}…</MenuItem>}
            {canWrite && drv?.caps.createDatabase && <MenuItem icon={<Database />} onSelect={() => setNameDlg({ action: "create_database" })}>New database…</MenuItem>}
            {canWrite && drv?.caps.schemas && drv?.caps.ddl && (!hasDbs || !!scope.database) && <MenuItem icon={<Layers />} onSelect={() => setNameDlg({ action: "create_schema", target: { database: scope.database, name: "" } })}>New schema…</MenuItem>}
            {canWrite && drv?.caps.dropDatabase && scope.database && !dbs.data?.find((d) => d.name === scope.database)?.system && (
              <MenuItem icon={<Trash2 />} danger onSelect={() => setDdl({ action: "drop_database", ref: { database: scope.database, name: scope.database! } })}>Drop database {scope.database}…</MenuItem>
            )}
            {canWrite && hasSchemas && drv?.caps.ddl && scope.schema && !schemas.data?.find((s) => s.name === scope.schema)?.system && (
              <MenuItem icon={<Trash2 />} danger onSelect={() => setDdl({ action: "drop_schema", ref: { database: scope.database, schema: scope.schema, name: scope.schema! } })}>
                Drop {conn.driver === "bigquery" ? "dataset" : "schema"} {scope.schema}…
              </MenuItem>
            )}
            <MenuSep />
            {canWrite && drv?.caps.editRows && ready && <MenuItem icon={<FileUp />} onSelect={() => openImport(conn, { kind: "table", database: scope.database, schema: scope.schema })}>Import data…</MenuItem>}
            {canWrite && drv?.caps.sql && <MenuItem icon={<FileCode2 />} onSelect={() => openImport(conn, { kind: "sql", database: scope.database, schema: scope.schema })}>Run a SQL file…</MenuItem>}
            {drv?.caps.sql && drv?.caps.ddl && !drv.caps.documents && ready && <MenuItem icon={<Download />} onSelect={() => openExport(conn, { kind: "dump", database: scope.database, schema: scope.schema })}>Export as SQL dump…</MenuItem>}
            {ready && <MenuItem icon={<ArrowRightLeft />} onSelect={() => go(`/migrate?from=${conn.id}${scope.database ? `&db=${encodeURIComponent(scope.database)}` : ""}${scope.schema ? `&schema=${encodeURIComponent(scope.schema)}` : ""}`)}>Migrate to another database…</MenuItem>}
            <MenuSep />
            <MenuItem icon={<RefreshCw />} onSelect={refresh}>Refresh</MenuItem>
            {conn.access === "manage" && <MenuItem icon={<Pencil />} onSelect={onEdit}>Edit connection</MenuItem>}
            <MenuSep />
            <MenuItem icon={<Unplug />} onSelect={() => requestDisconnect(conn)}>Disconnect</MenuItem>
          </MenuContent>
        </Menu>
      </div>

      {server.isError && (
        <div className="navigator__error">
          <Alert kind="danger" title="Cannot connect">{(server.error as Error).message}</Alert>
          <div className="row gap-3">
            <Button size="sm" onClick={() => server.refetch()}><RefreshCw /> Retry</Button>
            {conn.access === "manage" && <Button size="sm" onClick={onEdit}><Pencil /> Edit</Button>}
          </div>
        </div>
      )}

      {!server.isError && (hasDbs || hasSchemas) && (
        <div className="navigator__scope">
          {hasDbs && (
            <ScopePicker
              icon={<Database />}
              label="Database"
              value={scope.database}
              loading={dbs.isLoading}
              items={(dbs.data ?? []).map((d) => ({ value: d.name, meta: d.size ? bytes(d.size) : d.tables !== undefined ? `${d.tables} tables` : "", dim: d.system }))}
              onChange={(v) => setScope(conn.id, { database: v, schema: undefined })}
            />
          )}
          {hasSchemas && (
            <ScopePicker
              icon={<Layers />}
              label="Schema"
              value={scope.schema}
              loading={schemas.isLoading}
              items={(schemas.data ?? []).map((s) => ({ value: s.name, meta: s.owner ?? "", dim: s.system }))}
              onChange={(v) => setScope(conn.id, { schema: v })}
            />
          )}
        </div>
      )}

      {!server.isError && (
        <div className="navigator__filter">
          <Search />
          <input className="input" placeholder="Filter objects" value={filter} onChange={(e) => setFilter(e.target.value)} aria-label="Filter objects" />
          <Tip label="Refresh">
            <Button variant="ghost" size="sm" icon onClick={refresh} aria-label="Refresh"><RefreshCw className={objects.isFetching ? "spin" : ""} /></Button>
          </Tip>
        </div>
      )}

      <div className="navigator__tree" role="tree" aria-label="Database objects">
        {objects.isLoading && ready && <div className="navigator__loading"><Spinner /> Loading objects…</div>}
        {objects.error && <div className="navigator__error"><Alert kind="danger">{(objects.error as Error).message}</Alert></div>}
        {objects.data && groups.length === 0 && (
          <p className="navigator__empty muted">{filter ? `Nothing matches “${filter}”.` : "This schema is empty."}</p>
        )}
        {groups.map(([kind, list]) => {
          const key = `nav:${conn.id}:${kind}`;
          // Tables can show their size on disk instead of their row count.
          const sized = BROWSABLE.has(kind) && list.some((o) => o.size !== undefined && o.size !== null);
          const info = drv?.kinds.find((k) => k.kind === kind);
          const isOpen = expanded[key] ?? (BROWSABLE.has(kind) || list.length < 30 || !!filter);
          const Icon = kindIcon(kind);
          return (
            <div key={kind} className="navgroup" role="group">
              <div className="navgroup__bar">
                <button className="navgroup__head" onClick={() => toggle(key, !isOpen)} aria-expanded={isOpen}>
                  {isOpen ? <ChevronDown /> : <ChevronRight />}
                  <span className="grow">{info?.label ?? kind}</span>
                  <span className="navgroup__count tnum">{list.length}</span>
                </button>
                {sized && (
                  <Tip label={measure === "size" ? "Showing size on disk. Show row counts" : "Showing row counts. Show size on disk"}>
                    <button className="navgroup__measure" onClick={() => setMeasure(measure === "size" ? "rows" : "size")}
                      aria-label={measure === "size" ? "Show row counts" : "Show sizes on disk"}>{measure === "size" ? "size" : "rows"}</button>
                  </Tip>
                )}
                {(kind === "table" || kind === "collection") && canWrite && drv?.design && (
                  <Tip label={`New ${kind}`}>
                    <button className="navgroup__add" aria-label={`New ${kind}`} onClick={() => openDesign(conn, { database: scope.database, schema: scope.schema })}><Plus /></button>
                  </Tip>
                )}
              </div>
              {isOpen && (
                <div className="navgroup__items">
                  {list.map((o) => {
                    const ref = refOf(o);
                    const active = activeRef?.name === o.name && (activeRef.schema ?? "") === (scope.schema ?? "") && (activeRef.database ?? "") === (scope.database ?? "");
                    const browsable = BROWSABLE.has(o.kind);
                    const actions = objectActions(o, ref);
                    return (
                      <div key={o.name} className="navrow">
                        <RContext.Root>
                          <RContext.Trigger asChild>
                            <button
                              role="treeitem"
                              className={`navitem ${active ? "is-active" : ""}`}
                              title={[o.name, o.comment || o.extra, facts(o)].filter(Boolean).join("\n")}
                              onClick={() => openObject(conn, ref, browsable ? "browse" : "definition", true)}
                              onDoubleClick={() => openObject(conn, ref, browsable ? "browse" : "definition", false)}
                            >
                              <Icon className="navitem__icon" />
                              <span className="navitem__name truncate">{o.name}</span>
                              {browsable && measure === "size" && sized && o.size !== undefined && o.size !== null
                                ? <span className="navitem__meta mono">{bytes(o.size)}</span>
                                : o.rows !== undefined && o.rows !== null && browsable && <span className="navitem__meta mono">{compact(o.rows)}</span>}
                              {!browsable && o.extra && <span className="navitem__meta truncate mono">{o.extra}</span>}
                            </button>
                          </RContext.Trigger>
                          <RContext.Portal>
                            <RContext.Content className="menu">
                              {actions.map((x, k) => x === "sep"
                                ? <RContext.Separator key={k} className="menu__sep" />
                                : <CItem key={k} icon={x.icon} danger={x.danger} onSelect={x.onSelect}>{x.label}</CItem>)}
                            </RContext.Content>
                          </RContext.Portal>
                        </RContext.Root>
                        <Menu>
                          <MenuTrigger asChild>
                            <button className="navrow__more" aria-label={`Actions for ${o.name}`}><MoreHorizontal /></button>
                          </MenuTrigger>
                          <MenuContent align="start">
                            {actions.map((x, k) => x === "sep"
                              ? <MenuSep key={k} />
                              : <MenuItem key={k} icon={x.icon} danger={x.danger} onSelect={x.onSelect}>{x.label}</MenuItem>)}
                          </MenuContent>
                        </Menu>
                      </div>
                    );
                  })}
                </div>
              )}
            </div>
          );
        })}
      </div>
      {ddl && <DDLDialog conn={conn} action={ddl.action} target={ddl.ref} onClose={() => setDdl(null)} onDone={() => afterDrop(ddl.action, ddl.ref)} />}
      {nameDlg && (
        <NameDDLDialog conn={conn} action={nameDlg.action} target={nameDlg.target} onClose={() => setNameDlg(null)}
          onDone={(name) => {
            if (nameDlg.action === "create_database") setScope(conn.id, { database: name, schema: undefined });
            if (nameDlg.action === "create_schema") setScope(conn.id, { schema: name });
          }} />
      )}
    </div>
  );
}

export function sampleQuery(dialect: string | undefined, ref: ObjectRef, q: string) {
  if (dialect === "mongodb") return `db.getCollection(${JSON.stringify(ref.name)}).find({}).limit(100)`;
  if (dialect === "mssql") return `SELECT TOP (100) *\nFROM ${qualified(ref, q)}`;
  if (dialect === "plsql") return `SELECT *\nFROM ${qualified(ref, q)}\nFETCH FIRST 100 ROWS ONLY`;
  return `SELECT *\nFROM ${qualified(ref, q)}\nLIMIT 100`;
}

export function CItem({ icon, children, onSelect, danger }: { icon: React.ReactNode; children: React.ReactNode; onSelect(): void; danger?: boolean }) {
  return (
    <RContext.Item className={`menu__item ${danger ? "menu__item--danger" : ""}`} onSelect={onSelect}>
      {icon}
      <span className="grow truncate">{children}</span>
    </RContext.Item>
  );
}

function ScopePicker({ icon, label, value, items, onChange, loading }: {
  icon: React.ReactNode; label: string; value?: string; loading?: boolean;
  items: { value: string; meta?: string; dim?: boolean }[]; onChange(v: string): void;
}) {
  const [open, setOpen] = useState(false);
  const sorted = [...items].sort((a, b) => Number(!!a.dim) - Number(!!b.dim));
  return (
    <RPopover.Root open={open} onOpenChange={setOpen}>
      <RPopover.Trigger asChild>
        <button className="scopepick" aria-label={`${label}: ${value ?? "none"}`}>
          {icon}
          <span className="scopepick__label">{label}</span>
          <span className="scopepick__value truncate">{loading ? "…" : value ?? "Choose"}</span>
          <ChevronDown className="scopepick__chev" />
        </button>
      </RPopover.Trigger>
      <RPopover.Portal>
        <RPopover.Content className="popover scopepick__pop" align="start" sideOffset={4}>
          <Command loop>
            <Command.Input className="input" placeholder={`Find ${label.toLowerCase()}…`} autoFocus />
            <Command.List className="scopepick__list">
              <Command.Empty className="muted scopepick__empty">No match</Command.Empty>
              {sorted.map((it) => (
                <Command.Item key={it.value} value={it.value} onSelect={() => { onChange(it.value); setOpen(false); }} className={`scopepick__item ${it.dim ? "is-dim" : ""} ${it.value === value ? "is-current" : ""}`}>
                  <span className="grow truncate">{it.value}</span>
                  {it.meta && <span className="mono faint">{it.meta}</span>}
                </Command.Item>
              ))}
            </Command.List>
          </Command>
        </RPopover.Content>
      </RPopover.Portal>
    </RPopover.Root>
  );
}
