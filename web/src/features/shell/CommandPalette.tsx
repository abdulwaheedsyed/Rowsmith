import { Command } from "cmdk";
import * as RDialog from "@radix-ui/react-dialog";
import { useQuery } from "@tanstack/react-query";
import { Database, Eye, FunctionSquare, Moon, Plus, Server, Sun, Table2, TerminalSquare, Activity, BookMarked, Shield, UserRound, Columns3, SlidersHorizontal, Home, Network } from "lucide-react";
import { useMemo, useState } from "react";
import { get } from "../../lib/api";
import { useConnections, useObjects, useDriver } from "../../lib/queries";
import { useUI, useWorkspace } from "../../lib/store";
import type { Connection, Me, SavedQuery } from "../../lib/types";
import { compact } from "../../lib/format";
import { go } from "../../lib/nav";
import { Env, Kbd } from "../../components/ui";
import { newQueryTab, openObject, openPanel, openDiagram } from "../workspace/actions";

// Every typed word must appear in the item; matches at the start of the
// name rank first. Predictable beats clever for jumping to tables.
function paletteFilter(value: string, search: string) {
  const v = value.toLowerCase();
  const words = search.toLowerCase().split(/\s+/).filter(Boolean);
  if (!words.length) return 1;
  if (!words.every((w) => v.includes(w))) return 0;
  const first = words[0];
  if (v.startsWith(first)) return 1;
  if (v.split(/[\s_.-]+/).some((part) => part.startsWith(first))) return 0.8;
  return 0.5;
}

const kindIcon: Record<string, typeof Table2> = { table: Table2, view: Eye, materialized_view: Eye, function: FunctionSquare, procedure: FunctionSquare };

export function CommandPalette({ me, conn, onNewConnection }: { me: Me; conn?: Connection; onNewConnection(): void }) {
  const { paletteOpen, setPaletteOpen, setTheme } = useUI();
  const [search, setSearch] = useState("");
  const conns = useConnections();
  const scope = useWorkspace((s) => (conn ? s.scope[conn.id] : undefined));
  const drv = useDriver(conn?.driver);
  const objects = useObjects(conn?.id, scope?.database, scope?.schema, paletteOpen && !!conn && (!drv?.caps.databases || !!scope?.database));
  const saved = useQuery({ queryKey: ["saved"], queryFn: () => get<SavedQuery[]>("saved-queries"), enabled: paletteOpen });

  const close = () => {
    setPaletteOpen(false);
    setSearch("");
  };
  const run = (fn: () => void) => () => {
    close();
    fn();
  };

  const browsable = useMemo(() => (objects.data ?? []).filter((o) => ["table", "view", "materialized_view", "partitioned_table", "foreign_table", "collection"].includes(o.kind)), [objects.data]);
  const isAdmin = me.user.role === "owner" || me.user.role === "admin";

  return (
    <RDialog.Root open={paletteOpen} onOpenChange={(o) => (o ? setPaletteOpen(true) : close())}>
      <RDialog.Portal>
        <RDialog.Overlay className="scrim scrim--palette" />
        <RDialog.Content className="palette" aria-label="Command palette">
          <RDialog.Title className="sr-only">Command palette</RDialog.Title>
          <RDialog.Description className="sr-only">Search tables, connections and commands</RDialog.Description>
          <Command loop label="Command palette" filter={paletteFilter}>
            <div className="palette__input">
              <Command.Input value={search} onValueChange={setSearch} placeholder={conn ? `Search ${conn.name}…` : "Search connections and commands…"} autoFocus />
              <Kbd>esc</Kbd>
            </div>
            <Command.List className="palette__list">
              <Command.Empty className="palette__empty">No matches. Try a table name, a connection, or “new query”.</Command.Empty>

              {conn && browsable.length > 0 && (
                <Command.Group heading={`Tables & views${scope?.database ? ` · ${scope.database}${scope.schema ? "." + scope.schema : ""}` : ""}`}>
                  {browsable.slice(0, search ? 400 : 12).map((o) => {
                    const Icon = kindIcon[o.kind] ?? Table2;
                    const ref = { database: scope?.database, schema: scope?.schema, name: o.name, kind: o.kind };
                    return (
                      <Command.Item key={`o:${o.kind}:${o.name}`} value={`${o.name} ${o.kind}`} onSelect={run(() => openObject(conn, ref, "browse"))}>
                        <Icon />
                        <span className="grow truncate">{o.name}</span>
                        {o.rows !== undefined && <span className="palette__meta mono">{compact(o.rows)} rows</span>}
                        <span className="palette__meta">{o.kind.replace("_", " ")}</span>
                      </Command.Item>
                    );
                  })}
                </Command.Group>
              )}
              {conn && search && browsable.length > 0 && (
                <Command.Group heading="Structure">
                  {browsable.slice(0, 200).map((o) => (
                    <Command.Item key={`s:${o.name}`} value={`structure of ${o.name}`} onSelect={run(() => openObject(conn, { database: scope?.database, schema: scope?.schema, name: o.name, kind: o.kind }, "structure"))}>
                      <Columns3 />
                      <span className="grow truncate">Structure of {o.name}</span>
                    </Command.Item>
                  ))}
                </Command.Group>
              )}

              {conn && (
                <Command.Group heading={conn.name}>
                  <Command.Item value="new query sql editor" onSelect={run(() => newQueryTab(conn))}>
                    <TerminalSquare /> New query <span className="palette__meta">SQL editor</span>
                  </Command.Item>
                  <Command.Item value="overview server databases" onSelect={run(() => openPanel(conn, "overview"))}>
                    <Server /> Server overview
                  </Command.Item>
                  {drv?.caps.processes && (
                    <Command.Item value="processes running queries kill" onSelect={run(() => openPanel(conn, "processes"))}>
                      <Activity /> Running processes
                    </Command.Item>
                  )}
                  {drv?.caps.variables && (
                    <Command.Item value="variables settings status" onSelect={run(() => openPanel(conn, "variables"))}>
                      <SlidersHorizontal /> Server variables
                    </Command.Item>
                  )}
                  {drv?.caps.users && (
                    <Command.Item value="database users roles grants privileges" onSelect={run(() => openPanel(conn, "users"))}>
                      <UserRound /> Database users & grants
                    </Command.Item>
                  )}
                  {drv?.caps.foreignKeys && (
                    <Command.Item value="diagram er relationships schema" onSelect={run(() => openDiagram(conn, scope?.database, scope?.schema))}>
                      <Network /> Relationship diagram
                    </Command.Item>
                  )}
                </Command.Group>
              )}

              {(saved.data?.length ?? 0) > 0 && (
                <Command.Group heading="Saved queries">
                  {saved.data!.slice(0, search ? 100 : 5).map((q) => {
                    const target = conns.data?.find((c) => c.id === q.connectionId) ?? conn;
                    return (
                      <Command.Item key={`q:${q.id}`} value={`saved ${q.name} ${q.tags.join(" ")}`} disabled={!target}
                        onSelect={run(() => target && newQueryTab(target, { sql: q.body, title: q.name, database: q.database || undefined, savedQueryId: q.id }))}>
                        <BookMarked />
                        <span className="grow truncate">{q.name}</span>
                        <span className="palette__meta">{q.visibility === "team" ? `team · ${q.ownerName}` : "private"}</span>
                      </Command.Item>
                    );
                  })}
                </Command.Group>
              )}

              <Command.Group heading="Connections">
                {(conns.data ?? []).map((c) => (
                  <Command.Item key={`c:${c.id}`} value={`connection ${c.name} ${c.driver} ${c.environment} ${c.folder}`} onSelect={run(() => go(`/c/${c.id}`))}>
                    <Database />
                    <span className="grow truncate">{c.name}</span>
                    <Env env={c.environment} />
                  </Command.Item>
                ))}
                {me.user.role !== "viewer" && (
                  <Command.Item value="new connection add database" onSelect={run(onNewConnection)}>
                    <Plus /> New connection
                  </Command.Item>
                )}
              </Command.Group>

              <Command.Group heading="Rowsmith">
                <Command.Item value="home all connections" onSelect={run(() => go("/"))}><Home /> All connections</Command.Item>
                <Command.Item value="library saved queries history" onSelect={run(() => go("/library"))}><BookMarked /> Saved queries & history</Command.Item>
                <Command.Item value="account security password two-step mfa" onSelect={run(() => go("/account"))}><UserRound /> Account & security</Command.Item>
                {isAdmin && <Command.Item value="admin users team audit log settings" onSelect={run(() => go("/admin"))}><Shield /> Administration</Command.Item>}
                <Command.Item value="theme dark mode" onSelect={run(() => setTheme("dark"))}><Moon /> Dark theme</Command.Item>
                <Command.Item value="theme light mode" onSelect={run(() => setTheme("light"))}><Sun /> Light theme</Command.Item>
              </Command.Group>
            </Command.List>
          </Command>
        </RDialog.Content>
      </RDialog.Portal>
    </RDialog.Root>
  );
}
