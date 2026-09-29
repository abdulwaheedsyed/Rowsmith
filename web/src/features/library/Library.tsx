import { useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { BookMarked, History, Search, Trash2, Play, Users, Lock, CheckCircle2, CircleAlert, Ban } from "lucide-react";
import { del, get, qs } from "../../lib/api";
import { useConnections } from "../../lib/queries";
import type { HistoryEntry, SavedQuery } from "../../lib/types";
import { ago, duration, int, temperForMs } from "../../lib/format";
import { toast } from "../../lib/store";
import { Button, Dialog, Empty, Env, Spinner } from "../../components/ui";
import { newQueryTab } from "../workspace/actions";

export function Library() {
  const [tab, setTab] = useState<"saved" | "history">("saved");
  return (
    <div className="page">
      <header className="page__head">
        <div>
          <h1 className="page__title display">Library</h1>
          <p className="page__sub">Queries you saved or that your team shared, and everything you have run.</p>
        </div>
      </header>
      <div className="page__tabs" role="tablist">
        <button className="page__tab" role="tab" aria-selected={tab === "saved"} onClick={() => setTab("saved")}><BookMarked /> Saved queries</button>
        <button className="page__tab" role="tab" aria-selected={tab === "history"} onClick={() => setTab("history")}><History /> History</button>
      </div>
      {tab === "saved" ? <Saved /> : <HistoryList />}
    </div>
  );
}

function Saved() {
  const qc = useQueryClient();
  const conns = useConnections();
  const saved = useQuery({ queryKey: ["saved"], queryFn: () => get<SavedQuery[]>("saved-queries") });
  const [q, setQ] = useState("");
  const [removing, setRemoving] = useState<SavedQuery | null>(null);
  const list = useMemo(() => {
    const n = q.toLowerCase();
    return (saved.data ?? []).filter((s) => !n || `${s.name} ${s.description} ${s.tags.join(" ")} ${s.body}`.toLowerCase().includes(n));
  }, [saved.data, q]);
  if (saved.isLoading) return <Spinner large />;
  if (!saved.data?.length) return <Empty icon={<BookMarked />} title="No saved queries yet">Press ⌘S in a query tab to save it here. Share it with your team so everyone starts from the same SQL.</Empty>;
  return (
    <>
      <label className="home__search" style={{ marginBottom: 14, display: "block" }}>
        <Search />
        <input className="input" placeholder="Search saved queries" value={q} onChange={(e) => setQ(e.target.value)} />
      </label>
      <div className="savedlist">
        {list.map((s) => {
          const conn = conns.data?.find((c) => c.id === s.connectionId);
          return (
            <article key={s.id} className="card saved">
              <div className="saved__head">
                <div className="grow" style={{ minWidth: 0 }}>
                  <h3 className="saved__name truncate">{s.name}</h3>
                  <div className="saved__meta">
                    {s.visibility === "team" ? <span className="badge"><Users /> team</span> : <span className="badge"><Lock /> private</span>}
                    <span>by {s.ownerName}</span>
                    <span>updated {ago(s.updatedAt)}</span>
                    {conn && <span className="row gap-2">{conn.name} <Env env={conn.environment} /></span>}
                    {s.tags.map((t) => <span key={t} className="badge badge--accent">{t}</span>)}
                  </div>
                </div>
                <Button size="sm" variant="primary" disabled={!conn} onClick={() => conn && newQueryTab(conn, { sql: s.body, title: s.name, database: s.database || undefined, savedQueryId: s.id })}>
                  <Play /> Open
                </Button>
                <Button size="sm" variant="ghost" icon aria-label="Delete" onClick={() => setRemoving(s)}><Trash2 /></Button>
              </div>
              {s.description && <p className="saved__desc">{s.description}</p>}
              <pre className="saved__sql mono">{s.body.slice(0, 600)}</pre>
            </article>
          );
        })}
      </div>
      <Dialog open={!!removing} onOpenChange={(o) => !o && setRemoving(null)} title={`Delete “${removing?.name}”?`} description={removing?.visibility === "team" ? "Your team will no longer see it." : undefined}
        footer={<><Button onClick={() => setRemoving(null)}>Cancel</Button><Button variant="danger" onClick={async () => {
          try {
            await del(`saved-queries/${removing!.id}`);
            qc.invalidateQueries({ queryKey: ["saved"] });
            toast.success("Saved query deleted");
          } catch (e) {
            toast.error("Could not delete", (e as Error).message);
          }
          setRemoving(null);
        }}>Delete</Button></>} />
    </>
  );
}

function HistoryList() {
  const conns = useConnections();
  const [q, setQ] = useState("");
  const [conn, setConn] = useState("");
  const [team, setTeam] = useState(false);
  const selected = conns.data?.find((c) => c.id === conn);
  const hist = useQuery({
    queryKey: ["history", "library", q, conn, team],
    queryFn: () => get<HistoryEntry[]>(`history${qs({ q, connection: conn, scope: team && conn ? "team" : undefined, limit: 200 })}`),
  });
  return (
    <>
      <div className="home__filters">
        <label className="home__search">
          <Search />
          <input className="input" placeholder="Search SQL" value={q} onChange={(e) => setQ(e.target.value)} />
        </label>
        <select className="select" style={{ width: 240 }} value={conn} onChange={(e) => { setConn(e.target.value); setTeam(false); }} aria-label="Connection">
          <option value="">All connections</option>
          {conns.data?.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
        </select>
        {selected?.access === "manage" && (
          <label className="switch"><input type="checkbox" checked={team} onChange={(e) => setTeam(e.target.checked)} /> Everyone's queries</label>
        )}
      </div>
      {hist.isLoading && <Spinner />}
      {hist.data?.length === 0 && <Empty icon={<History />} title="Nothing here yet">Queries appear in your history as soon as you run them.</Empty>}
      <div className="histlist">
        {hist.data?.map((h) => {
          const c = conns.data?.find((x) => x.id === h.connectionId);
          return (
            <button key={h.id} className="histrow" disabled={!c} onClick={() => c && newQueryTab(c, { sql: h.body, database: h.database || undefined })}>
              <span className="histrow__status">{h.status === "ok" ? <CheckCircle2 className="ok" /> : h.status === "cancelled" ? <Ban /> : <CircleAlert className="bad" />}</span>
              <code className="histrow__sql">{h.body.replace(/\s+/g, " ").slice(0, 220)}</code>
              <span className="histrow__meta">
                {team && <span>{h.userName}</span>}
                <span className="truncate">{c?.name ?? "deleted"}{h.database ? ` · ${h.database}` : ""}</span>
                <span className="tnum">{int(h.rowCount)} rows</span>
                <span className="heat" style={{ ["--heat" as string]: temperForMs(h.durationMs) }}>{duration(h.durationMs)}</span>
                <span className="faint">{ago(h.startedAt)}</span>
              </span>
              {h.error && <span className="histrow__err truncate">{h.error}</span>}
            </button>
          );
        })}
      </div>
    </>
  );
}
