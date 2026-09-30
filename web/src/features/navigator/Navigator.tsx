import { useEffect, useMemo, useState } from "react";
import * as RContext from "@radix-ui/react-context-menu";
import * as RPopover from "@radix-ui/react-popover";
import { Command } from "cmdk";
import { useQueryClient } from "@tanstack/react-query";
import {
  ChevronDown, ChevronRight, Table2, Eye, FunctionSquare, Zap, CalendarClock, Hash, Shapes, Puzzle, Search, RefreshCw,
  MoreHorizontal, Pencil, Server, Activity, SlidersHorizontal, UserRound, Columns3, TerminalSquare, Copy, Scissors, Trash2, Database, Layers, Network, FileCode2, ListTree, Package, Link2, TableCellsSplit, LineChart, Unplug, PencilRuler, Download, FileUp, Plus, ArrowRightLeft,
} from "lucide-react";
import { go } from "../../lib/nav";
import { useDatabases, useDriver, useObjects, useSchemas, useServer, qualified } from "../../lib/queries";
import { useWorkspace } from "../../lib/store";
import type { Connection, DbObject, ObjectRef } from "../../lib/types";
import { bytes, compact } from "../../lib/format";
import { Button, EngineBadge, Menu, MenuContent, MenuItem, MenuSep, MenuTrigger, Spinner, Tip, Alert } from "../../components/ui";
import { newQueryTab, openObject, openPanel, openDiagram, openDesign } from "../workspace/actions";
import { DDLDialog } from "../structure/DDLDialog";
import { NameDDLDialog } from "../structure/NameDDLDialog";
import { openExport, openImport } from "../transfer/store";
import { requestDisconnect } from "../workspace/disconnect";
import "./navigator.css";

const ICONS: Record<string, typeof Table2> = {
  table: Table2, partitioned_table: Layers, view: Eye, materialized_view: Eye, foreign_table: Table2, function: FunctionSquare,
  procedure: FunctionSquare, trigger: Zap, event: CalendarClock, sequence: Hash, type: Shapes, extension: Puzzle, collection: Table2,
  routine: FunctionSquare, index: ListTree, package: Package, synonym: Link2, external_table: TableCellsSplit, timeseries: LineChart,
};

export function kindIcon(kind: string) {
  return ICONS[kind] ?? FileCode2;
}

const DESIGNABLE = new Set(["table", "partitioned_table", "collection"]);
const BROWSABLE = new Set(["table", "partitioned_table", "view", "materialized_view", "foreign_table", "external_table", "collection", "timeseries"]);

export function Navigator({ conn, onEdit }: { conn: Connection; onEdit(): void }) {
  const drv = useDriver(conn.driver);
  const server = useServer(conn.id);
  const scope = useWorkspace((s) => s.scope[conn.id]) ?? {};
  const setScope = useWorkspace((s) => s.setScope);
  const expanded = useWorkspace((s) => s.expanded);
  const toggle = useWorkspace((s) => s.toggle);
  const activeRef = useWorkspace((s) => {
    const t = s.tabs.find((x) => x.id === s.active[conn.id]);
    return t?.ref;
  });
  const qc = useQueryClient();
  const [filter, setFilter] = useState("");
  const [ddl, setDdl] = useState<{ action: "drop" | "truncate"; ref: ObjectRef } | null>(null);
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
                    return (
                      <RContext.Root key={o.name}>
                        <RContext.Trigger asChild>
                          <button
                            role="treeitem"
                            className={`navitem ${active ? "is-active" : ""}`}
                            title={o.comment || o.extra || o.name}
                            onClick={() => openObject(conn, ref, browsable ? "browse" : "definition", true)}
                            onDoubleClick={() => openObject(conn, ref, browsable ? "browse" : "definition", false)}
                          >
                            <Icon className="navitem__icon" />
                            <span className="navitem__name truncate">{o.name}</span>
                            {o.rows !== undefined && o.rows !== null && browsable && <span className="navitem__meta mono">{compact(o.rows)}</span>}
                            {!browsable && o.extra && <span className="navitem__meta truncate mono">{o.extra}</span>}
                          </button>
                        </RContext.Trigger>
                        <RContext.Portal>
                          <RContext.Content className="menu">
                            {browsable && <CItem icon={<Table2 />} onSelect={() => openObject(conn, ref, "browse")}>Open data</CItem>}
                            {browsable && <CItem icon={<Columns3 />} onSelect={() => openObject(conn, ref, "structure")}>Structure</CItem>}
                            {!browsable && <CItem icon={<FileCode2 />} onSelect={() => openObject(conn, ref, "definition")}>Show definition</CItem>}
                            {browsable && (
                              <CItem icon={<TerminalSquare />} onSelect={() => newQueryTab(conn, { sql: sampleQuery(drv?.dialect, ref, q), database: scope.database, schema: scope.schema })}>
                                Query in new tab
                              </CItem>
                            )}
                            {canWrite && drv?.design && DESIGNABLE.has(o.kind) && <CItem icon={<PencilRuler />} onSelect={() => openDesign(conn, { ref })}>Edit structure</CItem>}
                            {browsable && (
                              <>
                                <RContext.Separator className="menu__sep" />
                                <CItem icon={<Download />} onSelect={() => openExport(conn, { kind: "table", ref, rows: o.rows ?? undefined })}>Export…</CItem>
                                {canWrite && drv?.caps.editRows && DESIGNABLE.has(o.kind) && <CItem icon={<FileUp />} onSelect={() => openImport(conn, { kind: "table", ref })}>Import data…</CItem>}
                              </>
                            )}
                            <RContext.Separator className="menu__sep" />
                            {canWrite && drv?.caps.ddl && <CItem icon={<Pencil />} onSelect={() => setNameDlg({ action: "rename", target: ref })}>Rename…</CItem>}
                            <CItem icon={<Copy />} onSelect={() => navigator.clipboard?.writeText(o.name)}>Copy name</CItem>
                            <CItem icon={<Copy />} onSelect={() => navigator.clipboard?.writeText(qualified(ref, q, true))}>Copy qualified name</CItem>
                            {drv?.caps.ddl && conn.access !== "read" && !conn.readOnly && (
                              <>
                                <RContext.Separator className="menu__sep" />
                                {o.kind === "table" && <CItem icon={<Scissors />} danger onSelect={() => setDdl({ action: "truncate", ref })}>Empty table…</CItem>}
                                <CItem icon={<Trash2 />} danger onSelect={() => setDdl({ action: "drop", ref })}>Drop {o.kind.replace("_", " ")}…</CItem>
                              </>
                            )}
                          </RContext.Content>
                        </RContext.Portal>
                      </RContext.Root>
                    );
                  })}
                </div>
              )}
            </div>
          );
        })}
      </div>
      {ddl && <DDLDialog conn={conn} action={ddl.action} target={ddl.ref} onClose={() => setDdl(null)} onDone={refresh} />}
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
