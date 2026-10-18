import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Check, Minus, X, Users, Search, Undo2, ArrowRight } from "lucide-react";
import { useObjects } from "../../lib/queries";
import { toast } from "../../lib/store";
import type { Connection } from "../../lib/types";
import { pluralize } from "../../lib/format";
import { Alert, Button, Spinner, Tip } from "../../components/ui";
import { fetchGrants, LEVEL, TABLE_KINDS, useGrants, type AccessLevel, type AccessModel, type Principal } from "./api";
import {
  changesOf, copyChanges, coveredBy, hasLevel, held, isOn, matchingPreset, pkey, presetChanges, privsOf, setColumn, setRow, tablesAgg, toggle,
  type Ctx, type Pending,
} from "./model";
import { CopyFromDialog, ReviewDialog } from "./dialogs";

export interface Scope { database?: string; schema?: string }

/** How one privilege stands, for a chip or a matrix cell. */
interface Mark {
  on: boolean;
  was: boolean;
  partial?: boolean;
  via?: string[];
  covered?: string[];
  withGrant?: boolean;
  count?: string;
  /** For partial marks: whether the pending changes touch it. */
  changed?: boolean;
}

const viaText = (v: string) =>
  v === "PUBLIC" ? "everyone (PUBLIC)" : v === "owner" ? "owning it" : v.startsWith("tables created by") ? v : `the ${v} role`;

function describe(m: Mark, what: string): string {
  const lines: string[] = [];
  if (m.partial) lines.push(`${what}: ${m.count}${m.changed ? ", once the changes apply" : ""}`);
  else if (m.on !== m.was) lines.push(m.on ? `${what}: will be granted` : `${what}: will be revoked`);
  else if (m.on) lines.push(`${what}: granted${m.withGrant ? ", and can grant it to others" : ""}`);
  else lines.push(`${what}: not granted directly`);
  if (m.via?.length) lines.push(`Also held through ${[...new Set(m.via)].map(viaText).join(" and ")}`);
  if (m.covered?.length) lines.push(`Covered by the ${m.covered.join(" and ").toLowerCase()} grant`);
  return lines.join("\n");
}

function markClass(m: Mark, base: string) {
  return [
    base,
    m.on && "is-on",
    !m.partial && m.on !== m.was && (m.on ? "is-add" : "is-remove"),
    m.partial && "is-partial",
    m.partial && m.changed && "is-changed",
    !m.on && !m.partial && m.via?.length && "is-via",
    !m.on && !m.partial && !m.via?.length && m.covered?.length && "is-covered",
    m.withGrant && m.on && "is-grantable",
  ].filter(Boolean).join(" ");
}

function MarkIcon({ m }: { m: Mark }) {
  if (m.on) return <Check />;
  if (m.partial) return <Minus />;
  if (m.was) return <X />;
  if (m.via?.length || m.covered?.length) return <Check />;
  return null;
}

/** A privilege as a toggle chip, for levels with their own privileges. */
function Chip({ m, priv, what, editable, onToggle }: { m: Mark; priv: string; what: string; editable: boolean; onToggle(): void }) {
  return (
    <button type="button" role="checkbox" aria-checked={m.partial ? "mixed" : m.on} disabled={!editable}
      className={markClass(m, "achip")} title={describe(m, `${priv} on ${what}`)} onClick={onToggle}>
      <span className="achip__box"><MarkIcon m={m} /></span>
      {priv}
    </button>
  );
}

/** A privilege as a matrix cell. */
function Cell({ m, label, editable, onToggle }: { m: Mark; label: string; editable: boolean; onToggle(): void }) {
  return (
    <td className="amatrix__cell">
      <button type="button" role="checkbox" aria-checked={m.partial ? "mixed" : m.on} aria-label={label} disabled={!editable}
        className={markClass(m, "amark")} title={describe(m, label)} onClick={onToggle}>
        <MarkIcon m={m} />
      </button>
    </td>
  );
}

const SHOWN = 200;

export function AccessEditor({ conn, model, editable, principal, principals, scope, scopeLabel, onPending, onOpenPrincipal }: {
  conn: Connection; model: AccessModel; editable: boolean; principal: Principal; principals: Principal[]; scope: Scope; scopeLabel: string;
  onPending(n: number): void; onOpenPrincipal(name: string): void;
}) {
  const grants = useGrants(conn.id, principal.name, scope.database, scope.schema);
  const withTables = hasLevel(model, LEVEL.table);
  const objs = useObjects(conn.id, scope.database, scope.schema, withTables && (!!scope.database || !!scope.schema));
  const tables = useMemo(() => (objs.data ?? []).filter((o) => TABLE_KINDS.includes(o.kind)).map((o) => o.name), [objs.data]);
  const [pending, setPending] = useState<Pending>(() => new Map());
  const [filter, setFilter] = useState("");
  const [all, setAll] = useState(false);
  const [review, setReview] = useState(false);
  const [copying, setCopying] = useState(false);
  const [serverOpen, setServerOpen] = useState<boolean | null>(null);
  useEffect(() => onPending(pending.size), [pending.size, onPending]);

  const ctx: Ctx = useMemo(() => ({ model, held: held(grants.data ?? []), tables, pending }), [model, grants.data, tables, pending]);
  const match = useMemo(() => (grants.data ? matchingPreset(ctx) : null), [ctx, grants.data]);

  if (grants.error) return <Alert kind="danger" title="Could not read the access">{(grants.error as Error).message}</Alert>;
  if (!grants.data) return <div className="aeditor__loading"><Spinner large /></div>;

  const tablePrivs = privsOf(model, LEVEL.table);
  const inMatrix = (l: AccessLevel) => l.level === LEVEL.table || (tablePrivs.length > 0 && l.privileges.join() === tablePrivs.join());
  const what = (l: AccessLevel) =>
    l.level === LEVEL.server ? "the server" : l.level === LEVEL.database ? `database ${scope.database}` : l.level === LEVEL.schema ? `schema ${scope.schema}` : l.label.toLowerCase();

  const mark = (level: string, object: string, priv: string): Mark => {
    const k = pkey(level, object, priv);
    return {
      on: isOn(ctx, level, object, priv), was: ctx.held.direct.has(k), via: ctx.held.via.get(k),
      covered: coveredBy(ctx, level, priv), withGrant: ctx.held.withGrant.has(k),
    };
  };
  const aggMark = (priv: string): Mark => {
    const now = tablesAgg(ctx, priv);
    const before = tablesAgg({ ...ctx, pending: new Map() }, priv);
    const n = tables.filter((t) => isOn(ctx, LEVEL.table, t, priv)).length;
    return { on: now === "all", was: before === "all", partial: now === "some", changed: now !== before || tables.some((t) => isOn(ctx, LEVEL.table, t, priv) !== ctx.held.direct.has(pkey(LEVEL.table, t, priv))), count: `${n} of ${pluralize(tables.length, "table")}` };
  };
  const set = (p: Pending) => editable && setPending(p);

  // Grants held through roles, to point at where they're changed.
  const through = [...new Set(grants.data.filter((g) => g.via && g.via !== "owner" && !g.via.startsWith("tables created by")).map((g) => g.via!))];
  const owns = grants.data.some((g) => g.via === "owner");

  const blocks: ReactNode[] = [];
  let matrix = false;
  for (const l of model.levels) {
    if (principal.serverOnly && l.level !== LEVEL.server) continue;
    if (inMatrix(l)) {
      if (!matrix) blocks.push(<div key="matrix">{renderMatrix()}</div>);
      matrix = true;
      continue;
    }
    const marks = l.privileges.map((p) => [p, mark(l.level, "", p)] as const);
    const head = (
      <>
        <h3 className="alevel__title">{l.label}{l.level === LEVEL.database && <b className="mono"> {scope.database}</b>}{l.level === LEVEL.schema && <b className="mono"> {scope.schema}</b>}</h3>
        {l.help && <p className="alevel__help">{l.help}</p>}
      </>
    );
    const chips = (
      <div className="alevel__chips">
        {marks.map(([p, m]) => <Chip key={p} priv={p} what={what(l)} m={m} editable={editable} onToggle={() => set(toggle(ctx, l.level, "", p))} />)}
      </div>
    );
    // Server-wide privileges are rare and many: folded away unless some are held.
    const held = marks.filter(([, m]) => m.on || m.was || m.via?.length).map(([p]) => p);
    blocks.push(l.level === LEVEL.server ? (
      <details key={l.level} className="alevel alevel--fold" open={serverOpen ?? held.length > 0} onToggle={(e) => setServerOpen(e.currentTarget.open)}>
        <summary className="alevel__head">
          {head}
          <span className="alevel__sum">{held.length ? `${held.length} granted: ${held.slice(0, 4).join(", ")}${held.length > 4 ? "…" : ""}` : "None granted"}</span>
        </summary>
        {chips}
      </details>
    ) : (
      <section key={l.level} className="alevel">
        <div className="alevel__head">{head}</div>
        {chips}
      </section>
    ));
  }

  function renderMatrix() {
    const aggLevels = model.levels.filter((l) => l.level !== LEVEL.table && inMatrix(l));
    const f = filter.trim().toLowerCase();
    const matching = f ? tables.filter((t) => t.toLowerCase().includes(f)) : tables;
    const visible = all ? matching : matching.slice(0, SHOWN);
    const column = (p: string) => {
      if (!editable) return;
      // The whole schema at once goes through "all tables", as one statement.
      if (!f && hasLevel(model, LEVEL.tables)) return set(toggle(ctx, LEVEL.tables, "", p));
      const want = !matching.every((t) => isOn(ctx, LEVEL.table, t, p));
      set(setColumn(ctx, matching, p, want));
    };
    const rowMark = (t: string): Mark => {
      const n = tablePrivs.filter((p) => isOn(ctx, LEVEL.table, t, p)).length;
      const was = tablePrivs.filter((p) => ctx.held.direct.has(pkey(LEVEL.table, t, p))).length;
      const changed = tablePrivs.some((p) => isOn(ctx, LEVEL.table, t, p) !== ctx.held.direct.has(pkey(LEVEL.table, t, p)));
      return { on: n === tablePrivs.length, was: was === tablePrivs.length, partial: n > 0 && n < tablePrivs.length, changed, count: `${n} of ${tablePrivs.length} privileges` };
    };
    return (
      <section className="alevel alevel--matrix">
        <div className="amatrix-wrap">
          <table className="amatrix" style={{ ["--amatrix-head" as string]: `${Math.max(4, ...tablePrivs.map((p) => p.length)) * 7 + 22}px` }}>
            <thead>
              <tr>
                <th className="amatrix__corner" scope="col">
                  <span className="sr-only">Table</span>
                  {editable && <span className="amatrix__hint" aria-hidden="true">Click a privilege to set it on every {f ? "table shown" : "table"}.</span>}
                </th>
                {tablePrivs.map((p) => (
                  <th key={p} scope="col" className="amatrix__priv">
                    <button type="button" disabled={!editable} onClick={() => column(p)} title={editable ? `Grant or revoke ${p} on ${f ? "every table shown" : "every table"}` : p}>
                      <span>{p}</span>
                    </button>
                  </th>
                ))}
                <th scope="col" className="amatrix__priv amatrix__priv--all" title="Every privilege on the table"><span>ALL</span></th>
              </tr>
            </thead>
            <tbody>
              {aggLevels.map((l) => (
                <tr key={l.level} className="amatrix__agg">
                  <th scope="row" title={l.help}>
                    <span className="amatrix__name">{l.label}</span>
                    <span className="amatrix__sub">{l.level === LEVEL.tables ? `the ${pluralize(tables.length, "table")} there now` : l.level === LEVEL.future ? "default privileges" : ""}</span>
                  </th>
                  {tablePrivs.map((p) => (
                    <Cell key={p} label={`${p} on ${l.label.toLowerCase()}`} editable={editable} m={l.level === LEVEL.tables ? aggMark(p) : mark(l.level, "", p)}
                      onToggle={() => set(toggle(ctx, l.level, "", p))} />
                  ))}
                  <td className="amatrix__cell" />
                </tr>
              ))}
              <tr className="amatrix__sep">
                <th colSpan={tablePrivs.length + 2} scope="colgroup">
                  <div className="amatrix__seprow">
                    <span>{scope.schema ? `Tables in ${scope.schema}` : `Tables in ${scope.database}`}</span>
                    {!objs.isLoading && <span className="faint tnum">{tables.length}</span>}
                    {tables.length > 8 && (
                      <label className="amatrix__filter">
                        <Search aria-hidden="true" />
                        <input className="input" placeholder="Filter tables" value={filter} onChange={(e) => setFilter(e.target.value)} aria-label="Filter tables" />
                      </label>
                    )}
                  </div>
                </th>
              </tr>
              {objs.isLoading && <tr><td colSpan={tablePrivs.length + 2}><Spinner /></td></tr>}
              {!objs.isLoading && matching.length === 0 && (
                <tr><td colSpan={tablePrivs.length + 2} className="amatrix__none">{f ? "No tables match." : "No tables here yet."}</td></tr>
              )}
              {visible.map((t) => {
                const rm = rowMark(t);
                return (
                  <tr key={t}>
                    <th scope="row" className="mono"><span className="amatrix__name">{t}</span></th>
                    {tablePrivs.map((p) => (
                      <Cell key={p} label={`${p} on ${t}`} editable={editable} m={mark(LEVEL.table, t, p)} onToggle={() => set(toggle(ctx, LEVEL.table, t, p))} />
                    ))}
                    <Cell label={`Every privilege on ${t}`} editable={editable} m={rm} onToggle={() => set(setRow(ctx, t, !rm.on))} />
                  </tr>
                );
              })}
              {visible.length < matching.length && (
                <tr><td colSpan={tablePrivs.length + 2}><Button size="sm" variant="ghost" onClick={() => setAll(true)}>Show all {matching.length} tables</Button></td></tr>
              )}
            </tbody>
          </table>
        </div>
      </section>
    );
  }

  const adds = [...pending.values()].filter(Boolean).length;
  const removes = pending.size - adds;
  const changes = changesOf(pending, principal.name, principal.kind, scope);

  return (
    <div className="aeditor">
      {editable && model.presets.length > 0 && !principal.serverOnly && !owns && (
        <div className="apresets" role="group" aria-label={`Quick set access in ${scopeLabel}`}>
          <span className="apresets__label">Quick set</span>
          {model.presets.map((p) => (
            <Tip key={p.id} label={p.description}>
              <button type="button" className="apreset" aria-pressed={match === p.id} onClick={() => set(presetChanges(ctx, p))}>{p.label}</button>
            </Tip>
          ))}
          <Tip label={`Revoke everything granted directly in ${scopeLabel}.`}>
            <button type="button" className="apreset" aria-pressed={match === "none"} onClick={() => set(presetChanges(ctx, null))}>No access</button>
          </Tip>
          <span className="spacer" />
          <Button size="sm" variant="ghost" onClick={() => setCopying(true)}><Users /> Copy from…</Button>
        </div>
      )}
      {through.length > 0 && (
        <p className="athrough">
          Also has access through {through.map((v, i) => (
            <span key={v}>
              {i > 0 && (i === through.length - 1 ? " and " : ", ")}
              {v === "PUBLIC" ? <b>everyone (PUBLIC)</b> : <button type="button" className="alink" onClick={() => onOpenPrincipal(v)}>{v} <ArrowRight /></button>}
            </span>
          ))}. That access is changed on {through.length === 1 && through[0] !== "PUBLIC" ? "the role" : "the role or PUBLIC"}.
        </p>
      )}
      {owns && <p className="athrough">{principal.name} owns {scopeLabel}, so it can do anything there without grants.</p>}
      <Legend covers={!!model.covers} tables={withTables && !principal.serverOnly} />
      {blocks}
      {model.note && <p className="anote">{model.note}</p>}
      {pending.size > 0 && (
        <div className="apending" role="status">
          <span className="apending__count"><b className="tnum">{pluralize(pending.size, "change")}</b>{adds > 0 && <span className="apending__add">+{adds}</span>}{removes > 0 && <span className="apending__remove">−{removes}</span>}</span>
          <span className="faint truncate">for {principal.name} in {scopeLabel}</span>
          <span className="spacer" />
          <Button size="sm" variant="ghost" onClick={() => setPending(new Map())}><Undo2 /> Discard</Button>
          <Button size="sm" variant="primary" onClick={() => setReview(true)}>Review and apply</Button>
        </div>
      )}
      {review && (
        <ReviewDialog conn={conn} title={`Apply ${pluralize(pending.size, "change")} to ${principal.name}?`} req={{ grants: changes, database: scope.database }}
          verb="Apply changes" done={`Access updated for ${principal.name}`} onClose={() => setReview(false)} onDone={() => setPending(new Map())} />
      )}
      {copying && (
        <CopyFromDialog principals={principals} current={principal.name} model={model} where={scopeLabel} onClose={() => setCopying(false)}
          onPick={async (from) => {
            const g = await fetchGrants(conn.id, from, scope.database, scope.schema);
            const next = copyChanges(ctx, g);
            setPending(next);
            if (!next.size) toast.info(`${principal.name} already has the same access as ${from}`);
          }} />
      )}
    </div>
  );
}

function Legend({ covers, tables }: { covers: boolean; tables: boolean }) {
  const items: [Mark, string][] = [
    [{ on: true, was: true }, "Granted"],
    [{ on: false, was: false, via: ["role"] }, "Through a role"],
    ...(covers ? [[{ on: false, was: false, covered: ["level"] }, "Covered by a broader grant"] as [Mark, string]] : []),
    ...(tables ? [[{ on: false, was: false, partial: true }, "Some tables"] as [Mark, string]] : []),
    [{ on: true, was: false }, "To grant"],
    [{ on: false, was: true }, "To revoke"],
  ];
  return (
    <ul className="alegend" aria-label="Legend">
      {items.map(([m, label]) => <li key={label}><span className={markClass(m, "amark amark--legend")} aria-hidden="true"><MarkIcon m={m} /></span>{label}</li>)}
    </ul>
  );
}
