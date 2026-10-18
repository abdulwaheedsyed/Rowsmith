import { useMemo, useState } from "react";
import { Check, Plus, Undo2, Users, UserRound, X } from "lucide-react";
import type { Connection, ObjectRef } from "../../lib/types";
import { pluralize } from "../../lib/format";
import { Alert, Button, Spinner } from "../../components/ui";
import { LEVEL, TABLE_KINDS, splitName, useAccessModel, useObjectAccess, usePrincipals, type GrantChange } from "./api";
import { privsOf } from "./model";
import { ReviewDialog } from "./dialogs";
import "./access.css";

const SEP = "\u0001";

function viaText(v: string) {
  if (v === "owner") return "owns it";
  if (v === "the whole database" || v === "server-wide") return `a grant on ${v === "server-wide" ? "the whole server" : v}`;
  return `the ${v} role`;
}

/** Who can do what on one table, on its Structure tab. */
export function ObjectAccess({ conn, target, kind }: { conn: Connection; target: ObjectRef; kind: string }) {
  const m = useAccessModel(conn.id);
  const model = m.data?.model;
  const privs = model ? privsOf(model, LEVEL.table) : [];
  const enabled = !!model && privs.length > 0 && TABLE_KINDS.includes(kind);
  const canChange = enabled && !!m.data?.canChange && conn.access !== "read" && !conn.readOnly;
  const g = useObjectAccess(conn.id, target, enabled);
  const ps = usePrincipals(conn.id, canChange);
  const [pending, setPending] = useState<Map<string, boolean>>(() => new Map());
  const [added, setAdded] = useState<string[]>([]);
  const [adding, setAdding] = useState("");
  const [review, setReview] = useState(false);

  const rows = useMemo(() => {
    const by = new Map<string, { direct: Set<string>; via: Map<string, string[]>; owner: boolean }>();
    const row = (who: string) => {
      if (!by.has(who)) by.set(who, { direct: new Set(), via: new Map(), owner: false });
      return by.get(who)!;
    };
    for (const x of g.data ?? []) {
      const r = row(x.principal);
      if (x.via === "owner") r.owner = true;
      if (x.via && x.via !== "owner") r.via.set(x.privilege, [...(r.via.get(x.privilege) ?? []), x.via]);
      else r.direct.add(x.privilege);
    }
    for (const a of added) row(a);
    return [...by].sort(([a, x], [b, y]) => Number(y.owner) - Number(x.owner) || a.localeCompare(b));
  }, [g.data, added]);

  if (!enabled) return null;
  const isOn = (who: string, p: string, direct: Set<string>) => pending.get(who + SEP + p) ?? direct.has(p);
  const toggle = (who: string, p: string, direct: Set<string>) => {
    const next = new Map(pending);
    const want = !isOn(who, p, direct);
    if (want === direct.has(p)) next.delete(who + SEP + p);
    else next.set(who + SEP + p, want);
    setPending(next);
  };
  const changes: GrantChange[] = [...pending].map(([k, grant]) => {
    const [principal, privilege] = k.split(SEP);
    const kind = ps.data?.find((p) => p.name === principal)?.kind;
    return { principal, kind, level: LEVEL.table, database: target.database, schema: target.schema, object: target.name, privilege, ...(grant ? {} : { revoke: true }) };
  });
  const shown = new Set(rows.map(([who]) => who));
  const others = (ps.data ?? []).filter((p) => !p.system && !p.serverOnly && !shown.has(p.name));
  const name = (who: string) => {
    const [u, h] = splitName(who, !!model?.hostPart);
    return <>{u}{h && <span className="faint">@{h}</span>}</>;
  };

  return (
    <section className="ssection oaccess">
      <div className="row gap-3">
        <h2 className="ssection__title">Access</h2>
        <span className="faint">Who can do what on {target.name}</span>
        <span className="spacer" />
        {canChange && others.length > 0 && (
          <div className="row gap-2">
            <select className="select select--sm" value={adding} onChange={(e) => setAdding(e.target.value)} aria-label="Give access to">
              <option value="">Give access to…</option>
              {others.map((p) => <option key={p.name} value={p.name}>{p.name}{p.kind === "role" ? " (role)" : ""}</option>)}
            </select>
            <Button size="sm" disabled={!adding} onClick={() => { setAdded([...added, adding]); setAdding(""); }}><Plus /> Add</Button>
          </div>
        )}
      </div>
      {g.isLoading && <Spinner />}
      {g.error && <Alert kind="danger">{(g.error as Error).message}</Alert>}
      {g.data && rows.length === 0 && <p className="muted">Nobody has been granted anything on {target.name} directly.</p>}
      {rows.length > 0 && (
        <div className="stable-wrap">
          <table className="table stable oaccess__table">
            <thead>
              <tr>
                <th>Who</th>
                {privs.map((p) => <th key={p} className="oaccess__priv">{p}</th>)}
              </tr>
            </thead>
            <tbody>
              {rows.map(([who, r]) => {
                const p = ps.data?.find((x) => x.name === who);
                const editable = canChange && !r.owner && !p?.system;
                return (
                  <tr key={who}>
                    <td className="stable__name">
                      {p?.kind === "role" || who === "PUBLIC" ? <Users className="sicon" /> : <UserRound className="sicon" />}
                      <span className="mono">{who === "PUBLIC" ? "PUBLIC" : name(who)}</span>
                      {r.owner && <span className="badge badge--accent">owner</span>}
                      {who === "PUBLIC" && <span className="faint">everyone</span>}
                    </td>
                    {privs.map((priv) => {
                      const on = isOn(who, priv, r.direct);
                      const was = r.direct.has(priv);
                      const via = r.via.get(priv);
                      const cls = ["amark", on && "is-on", on !== was && (on ? "is-add" : "is-remove"), !on && via && "is-via"].filter(Boolean).join(" ");
                      const label = `${priv} for ${who}`;
                      const title = [
                        on !== was ? `${label}: will be ${on ? "granted" : "revoked"}` : on ? `${label}: granted${r.owner ? " (owner)" : ""}` : `${label}: not granted directly`,
                        via && `Also held through ${[...new Set(via)].map(viaText).join(" and ")}`,
                      ].filter(Boolean).join("\n");
                      return (
                        <td key={priv} className="amatrix__cell">
                          <button type="button" role="checkbox" aria-checked={on} aria-label={label} title={title} className={cls} disabled={!editable} onClick={() => toggle(who, priv, r.direct)}>
                            {on ? <Check /> : was ? <X /> : via ? <Check /> : null}
                          </button>
                        </td>
                      );
                    })}
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {pending.size > 0 && (
        <div className="apending apending--inline" role="status">
          <span className="apending__count"><b className="tnum">{pluralize(pending.size, "change")}</b></span>
          <span className="faint">on {target.name}</span>
          <span className="spacer" />
          <Button size="sm" variant="ghost" onClick={() => setPending(new Map())}><Undo2 /> Discard</Button>
          <Button size="sm" variant="primary" onClick={() => setReview(true)}>Review and apply</Button>
        </div>
      )}
      {review && (
        <ReviewDialog conn={conn} title={`Apply ${pluralize(pending.size, "change")} on ${target.name}?`} req={{ grants: changes, database: target.database }}
          verb="Apply changes" done={`Access updated on ${target.name}`} onClose={() => setReview(false)} onDone={() => { setPending(new Map()); setAdded([]); }} />
      )}
    </section>
  );
}
