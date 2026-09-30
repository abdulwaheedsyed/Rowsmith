import { useMemo, useState } from "react";
import { Link } from "wouter";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Search, MoreHorizontal, Pencil, Copy, Trash2, TerminalSquare, Route as RouteIcon, Users, Lock, Clock, CircleAlert, CheckCircle2 } from "lucide-react";
import { del, get, post } from "../../lib/api";
import { useConnections, useDrivers } from "../../lib/queries";
import type { Connection, HistoryEntry, Me } from "../../lib/types";
import { ago, duration, temperForMs } from "../../lib/format";
import { toast } from "../../lib/store";
import { Button, Dialog, EngineBadge, Env, Menu, MenuContent, MenuItem, MenuSep, MenuTrigger, Spinner } from "../../components/ui";
import { newQueryTab } from "../workspace/actions";
import { go } from "../../lib/nav";
import "./home.css";

const ENV_ORDER = ["production", "staging", "development", "local"] as const;

function target(c: Connection) {
  const p = c.params as Record<string, any>;
  if (p.project) return `${p.project}${p.dataset ? "." + p.dataset : ""}`;
  if (p.file) return String(p.file);
  const host = p.host ? `${p.host}${p.port && !String(p.host).startsWith("/") ? ":" + p.port : ""}` : "";
  const db = p.database || p.service || "";
  return [host, db].filter(Boolean).join(" / ");
}

export function Home({ me, onAdd, onEdit }: { me: Me; onAdd(driver?: string): void; onEdit(c: Connection): void }) {
  const conns = useConnections();
  const [q, setQ] = useState("");
  const [env, setEnv] = useState<string>("");
  const history = useQuery({ queryKey: ["history", "home"], queryFn: () => get<HistoryEntry[]>("history?limit=40") });

  const filtered = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return (conns.data ?? []).filter(
      (c) => (!env || c.environment === env) && (!needle || `${c.name} ${c.driver} ${c.folder} ${target(c)}`.toLowerCase().includes(needle)),
    );
  }, [conns.data, q, env]);

  const groups = useMemo(() => {
    const m = new Map<string, Connection[]>();
    for (const c of filtered) {
      const k = c.folder || "";
      if (!m.has(k)) m.set(k, []);
      m.get(k)!.push(c);
    }
    return [...m.entries()].sort(([a], [b]) => (a === "" ? 1 : b === "" ? -1 : a.localeCompare(b)));
  }, [filtered]);

  if (conns.isLoading) {
    return (
      <div className="home">
        <div className="home__grid">
          {[0, 1, 2].map((i) => <div key={i} className="skeleton" style={{ height: 132, borderRadius: 11 }} />)}
        </div>
      </div>
    );
  }

  if ((conns.data ?? []).length === 0) return <FirstRun me={me} onAdd={onAdd} />;

  const counts = ENV_ORDER.map((e) => [e, (conns.data ?? []).filter((c) => c.environment === e).length] as const).filter(([, n]) => n > 0);

  return (
    <div className="home">
      <div className="home__main">
        <header className="home__head">
          <div>
            <h1 className="home__title display">Connections</h1>
            <p className="home__sub muted">{conns.data!.length} saved · credentials encrypted at rest</p>
          </div>
          {me.user.role !== "viewer" && (
            <Button variant="primary" onClick={() => onAdd()}>
              <Plus /> New connection
            </Button>
          )}
        </header>
        <div className="home__filters">
          <label className="home__search">
            <Search />
            <input className="input" placeholder="Filter by name, host, engine or folder" value={q} onChange={(e) => setQ(e.target.value)} />
          </label>
          <div className="segmented" role="group" aria-label="Environment">
            <button aria-pressed={env === ""} onClick={() => setEnv("")}>All</button>
            {counts.map(([e, n]) => (
              <button key={e} aria-pressed={env === e} onClick={() => setEnv(e)}>
                <span className={`dot dot--${e}`} /> {e === "development" ? "dev" : e} <span className="faint tnum">{n}</span>
              </button>
            ))}
          </div>
        </div>
        {groups.map(([folder, list]) => (
          <section key={folder} className="home__group">
            {(groups.length > 1 || folder) && <h2 className="eyebrow home__folder">{folder || "Ungrouped"}</h2>}
            <div className="home__grid">
              {list.map((c) => <ConnCard key={c.id} c={c} onEdit={() => onEdit(c)} />)}
            </div>
          </section>
        ))}
        {filtered.length === 0 && <p className="muted home__none">No connections match “{q}”.</p>}
      </div>
      <aside className="home__side">
        <h2 className="eyebrow">Your recent queries</h2>
        {history.isLoading && <Spinner />}
        {history.data?.length === 0 && <p className="muted home__side-empty">Queries you run appear here, so you can pick up where you left off.</p>}
        <ol className="recent">
          {history.data?.filter((h) => conns.data?.some((x) => x.id === h.connectionId)).slice(0, 12).map((h) => {
            const c = conns.data?.find((x) => x.id === h.connectionId);
            return (
              <li key={h.id}>
                <button className="recent__item" disabled={!c} onClick={() => c && newQueryTab(c, { sql: h.body, database: h.database || undefined })}>
                  <code className="recent__sql">{h.body.replace(/\s+/g, " ").slice(0, 140)}</code>
                  <span className="recent__meta">
                    {h.status === "ok" ? <CheckCircle2 className="ok" /> : <CircleAlert className="bad" />}
                    <span className="truncate">{c?.name ?? "deleted connection"}</span>
                    <span className="heat" style={{ ["--heat" as string]: temperForMs(h.durationMs) }}>{duration(h.durationMs)}</span>
                    <span className="faint">{ago(h.startedAt)}</span>
                  </span>
                </button>
              </li>
            );
          })}
        </ol>
      </aside>
    </div>
  );
}

function ConnCard({ c, onEdit }: { c: Connection; onEdit(): void }) {
  const qc = useQueryClient();
  const [confirm, setConfirm] = useState(false);
  const manage = c.access === "manage";
  const tunneled = !!c.ssh?.enabled;
  return (
    <article className={`conncard conncard--${c.environment}`}>
      <Link href={`/c/${c.id}`} className="conncard__open" aria-label={`Open ${c.name}`} />
      <div className="conncard__top">
        <EngineBadge driver={c.driver} size={30} />
        <div className="grow" style={{ minWidth: 0 }}>
          <div className="conncard__name truncate">{c.name}</div>
          <div className="conncard__target mono truncate">{target(c) || c.driver}</div>
        </div>
        <Env env={c.environment} />
      </div>
      <div className="conncard__tags">
        {tunneled && <span className="badge"><RouteIcon /> SSH tunnel</span>}
        {c.teamAccess && <span className="badge"><Users /> team · {c.teamAccess}</span>}
        {(c.readOnly || c.access === "read") && <span className="badge"><Lock /> read-only</span>}
        {c.ownerName && c.access !== "manage" && <span className="badge">from {c.ownerName}</span>}
      </div>
      <div className="conncard__foot">
        <span className="faint row gap-2"><Clock size={12} /> {c.lastUsedAt ? `used ${ago(c.lastUsedAt)}` : "not used yet"}</span>
        <span className="spacer" />
        <Button size="sm" variant="ghost" onClick={() => newQueryTab(c)} className="conncard__action">
          <TerminalSquare /> Query
        </Button>
        {manage && (
          <Menu>
            <MenuTrigger asChild>
              <Button size="sm" variant="ghost" icon aria-label="Connection actions" className="conncard__action"><MoreHorizontal /></Button>
            </MenuTrigger>
            <MenuContent align="end">
              <MenuItem icon={<Pencil />} onSelect={onEdit}>Edit connection</MenuItem>
              <MenuItem icon={<Copy />} onSelect={async () => {
                const copy = await post<Connection>(`connections/${c.id}/duplicate`);
                await qc.invalidateQueries({ queryKey: ["connections"] });
                toast.success("Connection duplicated", copy.name);
              }}>Duplicate</MenuItem>
              <MenuSep />
              <MenuItem icon={<Trash2 />} danger onSelect={() => setConfirm(true)}>Delete…</MenuItem>
            </MenuContent>
          </Menu>
        )}
      </div>
      <Dialog
        open={confirm}
        onOpenChange={setConfirm}
        title={`Delete “${c.name}”?`}
        description="This removes the saved connection and its encrypted credentials from Rowsmith. The database itself is not touched."
        footer={
          <>
            <Button onClick={() => setConfirm(false)}>Cancel</Button>
            <Button variant="danger" onClick={async () => {
              await del(`connections/${c.id}`);
              setConfirm(false);
              await qc.invalidateQueries({ queryKey: ["connections"] });
              toast.success("Connection deleted");
            }}>Delete connection</Button>
          </>
        }
      />
    </article>
  );
}

function FirstRun({ me, onAdd }: { me: Me; onAdd(driver?: string): void }) {
  const drivers = useDrivers();
  return (
    <div className="home home--empty">
      <div className="firstrun">
        <p className="eyebrow">Welcome, {me.user.name.split(" ")[0]}</p>
        <h1 className="firstrun__title display">Connect your first database</h1>
        <p className="firstrun__lede muted">
          {me.user.role === "viewer"
            ? "No connections have been shared with you yet. Ask a teammate to share one."
            : "Pick an engine. Credentials are encrypted before they are stored, and Rowsmith can reach private servers through an SSH tunnel."}
        </p>
        {me.user.role !== "viewer" && (
          <div className="firstrun__engines">
            {(drivers.data ?? []).map((d) => (
              <button key={d.id} className="engine-pick" onClick={() => onAdd(d.id)}>
                <EngineBadge driver={d.id} size={34} />
                <span className="engine-pick__name">{d.name}</span>
                <span className="engine-pick__desc">{d.description}</span>
              </button>
            ))}
          </div>
        )}
        <button className="link-button firstrun__cli" onClick={() => go("/account")}>Set up two-step sign-in while you are here →</button>
      </div>
    </div>
  );
}
