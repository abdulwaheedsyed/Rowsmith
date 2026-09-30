import { useMemo, useState } from "react";
import { Link } from "wouter";
import { useQueryClient } from "@tanstack/react-query";
import { Clock, Link2, MessageSquare, Search, Users, UserRound } from "lucide-react";
import type { User } from "../../lib/types";
import { ago } from "../../lib/format";
import { Empty, Spinner } from "../../components/ui";
import { useShares } from "./api";
import { initialsOf } from "./Comments";
import "./shares.css";

type Filter = "all" | "mine" | "withme";

export function SharedList() {
  const me = useQueryClient().getQueryData<{ user: User }>(["me"])?.user;
  const isAdmin = me?.role === "owner" || me?.role === "admin";
  const [everyone, setEveryone] = useState(false);
  const list = useShares(everyone);
  const [filter, setFilter] = useState<Filter>("all");
  const [q, setQ] = useState("");
  const shown = useMemo(() => {
    const n = q.toLowerCase();
    return (list.data ?? []).filter((s) =>
      (filter === "all" || (filter === "mine") === (s.authorId === me?.id)) &&
      (!n || `${s.title} ${s.description} ${s.authorName} ${s.connectionName} ${s.body}`.toLowerCase().includes(n)));
  }, [list.data, filter, q, me?.id]);

  if (list.isLoading) return <Spinner large />;
  if (!list.data?.length && !everyone) {
    return <Empty icon={<Link2 />} title="Nothing shared yet">Share a query from its tab to send teammates a link. They can open it, see its result, and comment on any line.</Empty>;
  }
  return (
    <>
      <div className="sharedtools">
        <label className="home__search sharedtools__search">
          <Search />
          <input className="input" placeholder="Search shared queries" value={q} onChange={(e) => setQ(e.target.value)} />
        </label>
        <div className="segmented" role="group" aria-label="Show">
          {([["all", "All"], ["mine", "Shared by me"], ["withme", "Shared with me"]] as const).map(([v, l]) => (
            <button key={v} aria-pressed={filter === v} onClick={() => setFilter(v)}>{l}</button>
          ))}
        </div>
        {isAdmin && <label className="check"><input type="checkbox" checked={everyone} onChange={(e) => setEveryone(e.target.checked)} /> Everyone's</label>}
      </div>
      {!shown.length ? <p className="muted">No shared queries match.</p> : (
        <div className="sharedlist">
          {shown.map((s) => (
            <Link key={s.id} href={`/q/${s.id}`} className={`card sharedcard ${s.unread ? "is-unread" : ""}`}>
              <div className="sharedcard__head">
                <span className="avatar avatar--sm">{initialsOf(s.authorName)}</span>
                <div className="grow" style={{ minWidth: 0 }}>
                  <h3 className="sharedcard__title truncate">{s.title}</h3>
                  <div className="sharedcard__meta">
                    <span>{s.authorId === me?.id ? "You" : s.authorName}</span>
                    <span>{s.connectionName}{s.database ? ` · ${s.database}` : ""}</span>
                    {s.audience === "team" ? <span className="row gap-2"><Users size={12} /> team</span> : <span className="row gap-2"><UserRound size={12} /> {s.people.length}</span>}
                    {s.expiresAt > 0 && <span className="row gap-2"><Clock size={12} /> {s.expired ? "expired" : `until ${new Date(s.expiresAt).toLocaleDateString()}`}</span>}
                  </div>
                </div>
                <div className="sharedcard__activity">
                  {s.comments > 0 && <span className="sharedcard__comments"><MessageSquare /> {s.comments}{s.openThreads ? <em>{s.openThreads} open</em> : null}</span>}
                  <span className="faint">{ago(s.lastActivity)}</span>
                </div>
              </div>
              {s.description && <p className="sharedcard__desc">{s.description}</p>}
              <pre className="sharedcard__sql mono">{s.body.split("\n").slice(0, 4).join("\n")}</pre>
            </Link>
          ))}
        </div>
      )}
    </>
  );
}
