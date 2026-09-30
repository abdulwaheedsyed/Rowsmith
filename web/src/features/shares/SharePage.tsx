import { Fragment, useEffect, useMemo, useState } from "react";
import { Link } from "wouter";
import { useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, Check, Clock, Copy, Link2, MessageSquare, MoreHorizontal, Plus, Settings2, TerminalSquare, Trash2, Users, UserRound, Lock } from "lucide-react";
import { ApiError, del, post } from "../../lib/api";
import { useConnections } from "../../lib/queries";
import { ago, cellText, duration, int, isNumericKind } from "../../lib/format";
import { toast } from "../../lib/store";
import { go } from "../../lib/nav";
import { Alert, Button, Dialog, Empty, Menu, MenuContent, MenuItem, MenuSep, MenuTrigger, Spinner, Tip } from "../../components/ui";
import { highlightLines } from "../ai/Markdown";
import { newQueryTab } from "../workspace/actions";
import { shareUrl, threads, useShare, type ShareDetail, type Snapshot } from "./api";
import { CommentBody, Composer, initialsOf, Thread } from "./Comments";
import { openShareSettings } from "./ShareDialog";
import "./shares.css";

export function SharePage({ id }: { id: string }) {
  const q = useShare(id);
  const qc = useQueryClient();
  useEffect(() => {
    if (q.data) qc.invalidateQueries({ queryKey: ["notifications"] }); // opening it marks its notifications read
  }, [q.data?.id, qc]); // eslint-disable-line react-hooks/exhaustive-deps

  if (q.isLoading) return <div className="page"><Spinner large /></div>;
  if (q.error || !q.data) {
    const e = q.error as ApiError | null;
    return (
      <div className="page">
        <Empty icon={e?.status === 410 ? <Clock /> : <Lock />} title={e?.status === 410 ? "This link has expired" : "Nothing to see here"}
          action={<Link href="/library?tab=shared" className="btn">Shared queries</Link>}>
          {e?.message ?? "This shared query does not exist, or it is not shared with you."}
        </Empty>
      </div>
    );
  }
  return <ShareView share={q.data} />;
}

function ShareView({ share }: { share: ShareDetail }) {
  const qc = useQueryClient();
  const conns = useConnections();
  const conn = conns.data?.find((c) => c.id === share.connectionId);
  const [line, setLine] = useState<number | null>(null);
  const [removing, setRemoving] = useState(false);
  const [copied, setCopied] = useState<"link" | "sql" | null>(null);
  const lines = useMemo(() => highlightLines(share.body), [share.body]);
  const all = useMemo(() => threads(share.comments), [share.comments]);
  const byLine = useMemo(() => {
    const m = new Map<number, typeof all>();
    for (const t of all) {
      if (!m.has(t.root.line)) m.set(t.root.line, []);
      m.get(t.root.line)!.push(t);
    }
    return m;
  }, [all]);
  const general = byLine.get(0) ?? [];
  const refresh = () => qc.invalidateQueries({ queryKey: ["share", share.id] });

  // Jump to a linked comment once it is on the page.
  useEffect(() => {
    const hash = decodeURIComponent(location.hash.slice(1));
    if (!hash.startsWith("c-")) return;
    const el = document.getElementById(hash);
    if (!el) return;
    el.scrollIntoView({ block: "center" });
    el.classList.add("is-linked");
    const t = setTimeout(() => el.classList.remove("is-linked"), 2400);
    return () => clearTimeout(t);
  }, [share.id, share.comments.length]);

  const copy = async (what: "link" | "sql") => {
    try {
      await navigator.clipboard.writeText(what === "link" ? shareUrl(share.id) : share.body);
      setCopied(what);
      setTimeout(() => setCopied(null), 1600);
    } catch {
      toast.error("Could not copy", "Your browser blocked the clipboard.");
    }
  };

  const openInTab = () => {
    if (!conn) return;
    newQueryTab(conn, { sql: share.body, title: share.title, database: share.database || undefined, schema: share.schema || undefined });
  };

  const post_ = async (body: string, ln: number) => {
    await post(`shares/${share.id}/comments`, { body, line: ln });
    setLine(null);
    refresh();
    qc.invalidateQueries({ queryKey: ["shares"] });
  };

  const scope = [share.database, share.schema].filter(Boolean).join(".");
  const open = all.filter((t) => !t.root.resolvedAt && t.root.line > 0).length;

  return (
    <div className="page sharepage">
      <Link href="/library?tab=shared" className="backlink"><ArrowLeft size={14} /> Shared queries</Link>
      <header className="sharehead">
        <div className="sharehead__main">
          <div className="sharehead__eyebrow">
            {share.audience === "team"
              ? <span className="sharebadge"><Users /> Team</span>
              : <Tip label={share.people.map((p) => p.name).join(", ")}><span className="sharebadge"><UserRound /> {share.people.length} {share.people.length === 1 ? "person" : "people"}</span></Tip>}
            {share.expiresAt > 0 && <span className={`sharebadge ${share.expired ? "sharebadge--expired" : ""}`}><Clock /> {share.expired ? "Expired" : "Expires"} {new Date(share.expiresAt).toLocaleDateString()}</span>}
          </div>
          <h1 className="page__title display sharehead__title">{share.title}</h1>
          <div className="sharehead__meta">
            <span className="avatar avatar--xs">{initialsOf(share.authorName)}</span>
            <span><b>{share.authorName}</b> shared this {ago(share.createdAt)}</span>
            <span className="faint">·</span>
            <span>{share.connectionName}{scope ? ` · ${scope}` : ""}</span>
          </div>
        </div>
        <div className="sharehead__actions">
          <Button variant="primary" onClick={openInTab} disabled={!conn || !share.can.run} title={!conn ? `You don't have access to ${share.connectionName}` : undefined}>
            <TerminalSquare /> Open in a query tab
          </Button>
          <Button onClick={() => copy("link")}>{copied === "link" ? <Check /> : <Link2 />} {copied === "link" ? "Copied" : "Copy link"}</Button>
          {(share.can.edit || share.can.delete) && (
            <Menu>
              <MenuTrigger asChild><Button icon aria-label="More"><MoreHorizontal /></Button></MenuTrigger>
              <MenuContent align="end">
                {share.can.edit && <MenuItem icon={<Settings2 />} onSelect={() => openShareSettings({ ...share, comments: share.comments.length })}>Sharing and expiry</MenuItem>}
                {share.can.edit && share.can.delete && <MenuSep />}
                {share.can.delete && <MenuItem icon={<Trash2 />} danger onSelect={() => setRemoving(true)}>Delete link</MenuItem>}
              </MenuContent>
            </Menu>
          )}
        </div>
      </header>

      {share.expired && <Alert kind="warn">This link has expired, so only you{share.can.edit ? "" : " and admins"} can open it. {share.can.edit ? "Extend it under Sharing and expiry to reopen the discussion." : ""}</Alert>}
      {!share.can.run && <Alert kind="info">You can read and discuss this query, but you don't have access to {share.connectionName} to run it{share.hasResult ? " or see its result" : ""}.</Alert>}
      {share.description && <div className="sharenote"><CommentBody body={share.description} names={share.names} /></div>}

      <section className="sharecode card">
        <div className="sharecode__bar">
          <span className="sharecode__label">Query</span>
          <span className="faint">{lines.length} {lines.length === 1 ? "line" : "lines"}{open ? ` · ${open} open ${open === 1 ? "thread" : "threads"}` : ""}</span>
          <span className="spacer" />
          <span className="faint sharecode__hint">Click a line number to comment on it</span>
          <Button size="sm" variant="ghost" onClick={() => copy("sql")}>{copied === "sql" ? <Check /> : <Copy />} Copy SQL</Button>
        </div>
        <div className="sharecode__lines" role="table" aria-label="Query lines">
          {lines.map((code, i) => {
            const n = i + 1;
            const here = byLine.get(n) ?? [];
            const openHere = here.some((t) => !t.root.resolvedAt);
            return (
              <Fragment key={n}>
                <div className={`codeline ${here.length ? (openHere ? "has-open" : "has-resolved") : ""} ${line === n ? "is-active" : ""}`} role="row">
                  <button className="codeline__num" onClick={() => setLine(line === n ? null : n)} disabled={share.expired} aria-label={`Comment on line ${n}`}>
                    <span>{n}</span><Plus />
                  </button>
                  <code className="codeline__code">{code.length ? code : " "}</code>
                  {here.length > 0 && <span className="codeline__count" aria-label={`${here.length} threads`}><MessageSquare />{here.reduce((a, t) => a + 1 + t.replies.length, 0)}</span>}
                </div>
                {(here.length > 0 || line === n) && (
                  <div className="codeline__threads">
                    {here.map((t) => <Thread key={t.root.id} share={share} root={t.root} replies={t.replies} />)}
                    {line === n && (
                      <div className="thread thread--new">
                        <Composer placeholder={`Comment on line ${n}…`} names={share.names} autoFocus onCancel={() => setLine(null)} onSubmit={(body) => post_(body, n)} />
                      </div>
                    )}
                  </div>
                )}
              </Fragment>
            );
          })}
        </div>
      </section>

      {share.hasResult && <ResultCard share={share} />}

      <section className="discussion">
        <h2 className="discussion__title">Discussion <span className="faint">{share.comments.length ? share.comments.length : ""}</span></h2>
        {general.length === 0 && !share.expired && <p className="muted">Ask a question or add context. Mention someone with @ to notify them.</p>}
        {general.map((t) => <Thread key={t.root.id} share={share} root={t.root} replies={t.replies} />)}
        {!share.expired && (
          <div className="thread thread--new">
            <Composer placeholder="Add to the discussion…" names={share.names} onSubmit={(body) => post_(body, 0)} />
          </div>
        )}
      </section>

      <Dialog open={removing} onOpenChange={setRemoving} title="Delete this link?" description="The link stops working, and its discussion is deleted for everyone."
        footer={<><Button onClick={() => setRemoving(false)}>Cancel</Button><Button variant="danger" onClick={async () => {
          try {
            await del(`shares/${share.id}`);
            qc.invalidateQueries({ queryKey: ["shares"] });
            toast.success("Link deleted");
            go("/library?tab=shared");
          } catch (e) {
            toast.error("Could not delete it", e instanceof ApiError ? e.message : String(e));
          }
        }}><Trash2 /> Delete</Button></>} />
    </div>
  );
}

function ResultCard({ share }: { share: ShareDetail }) {
  const r: Snapshot | undefined = share.result;
  return (
    <section className="card shareresult">
      <div className="sharecode__bar">
        <span className="sharecode__label">Result</span>
        <span className="faint">
          {r ? <>{int(r.rowCount)} {r.rowCount === 1 ? "row" : "rows"}{r.truncated ? `, the first ${int(r.rows.length)} kept` : ""} · took {duration(r.ms)} · </> : null}
          when shared, {ago(share.resultAt)}
        </span>
      </div>
      {!r ? (
        <p className="muted shareresult__hidden"><Lock size={14} /> Hidden: only people who can read {share.connectionName} see the result.</p>
      ) : r.columns.length === 0 ? (
        <p className="muted shareresult__hidden">No columns.</p>
      ) : (
        <div className="shareresult__grid">
          <table>
            <thead>
              <tr><th className="shareresult__n">#</th>{r.columns.map((c, i) => <th key={i} title={c.type}>{c.name}<small>{c.type}</small></th>)}</tr>
            </thead>
            <tbody>
              {r.rows.map((row, i) => (
                <tr key={i}>
                  <td className="shareresult__n">{i + 1}</td>
                  {r.columns.map((c, j) => {
                    const v = row[j];
                    return v === null || v === undefined
                      ? <td key={j} className="is-null">NULL</td>
                      : <td key={j} className={isNumericKind(c.kind) ? "is-num" : undefined}>{cellText(v).slice(0, 300)}</td>;
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
