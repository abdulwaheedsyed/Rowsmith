import { Fragment, useEffect, useMemo, useState } from "react";
import { Link, useRoute } from "wouter";
import { useQueryClient } from "@tanstack/react-query";
import {
  ArrowLeft, ArrowRight, ChevronDown, ChevronRight, CircleAlert, CircleCheck, CircleX, Code2, Database, History as HistoryIcon, Loader2, Square, TriangleAlert,
} from "lucide-react";
import { ApiError, post } from "../../lib/api";
import { useConnections, useDatabases, useDriver, useSchemas } from "../../lib/queries";
import type { Connection, Me } from "../../lib/types";
import { ago, duration, int } from "../../lib/format";
import { toast } from "../../lib/store";
import { go } from "../../lib/nav";
import { Alert, Button, Dialog, Empty, EngineBadge, Env, Spinner, Tip } from "../../components/ui";
import { highlightSQL } from "../ai/Markdown";
import {
  defaultOptions, ENGINE_NAMES, useMigration, useMigrations, type ColumnPlan, type Endpoint, type MigrateOptions, type Migration, type Plan, type PlanResponse,
  type TablePlan, type TableRun,
} from "./api";
import "./migrate.css";

export function Migrate({ me }: { me: Me }) {
  const [, params] = useRoute<{ id: string }>("/migrate/:id");
  if (params?.id) return <RunView id={params.id} />;
  return <Wizard me={me} />;
}

const empty = (): Endpoint => ({ connectionId: "", database: "", schema: "" });

function fromQuery(): Endpoint {
  const q = new URLSearchParams(location.search);
  return { connectionId: q.get("from") ?? "", database: q.get("db") ?? "", schema: q.get("schema") ?? "" };
}

function Wizard({ me }: { me: Me }) {
  const [src, setSrc] = useState<Endpoint>(fromQuery);
  const [dst, setDst] = useState<Endpoint>(empty);
  const [planning, setPlanning] = useState(false);
  const [error, setError] = useState("");
  const [resp, setResp] = useState<PlanResponse | null>(null);
  const [options, setOptions] = useState<MigrateOptions>(defaultOptions);

  const plan = async (opts = options) => {
    setPlanning(true);
    setError("");
    try {
      const r = await post<PlanResponse>("migrations/plan", { source: src, target: dst, options: opts });
      setResp(r);
      setOptions(r.plan.options);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setPlanning(false);
    }
  };

  if (resp) {
    return (
      <div className="page migrate">
        <PlanReview resp={resp} src={src} dst={dst} onChange={(plan) => setResp({ ...resp, plan })} onBack={() => setResp(null)}
          onReplan={(opts) => plan(opts)} replanning={planning} />
      </div>
    );
  }
  const same = src.connectionId && src.connectionId === dst.connectionId && src.database === dst.database && src.schema === dst.schema;
  return (
    <div className="page migrate">
      <header className="page__head">
        <div>
          <h1 className="page__title display">Migrate</h1>
          <p className="page__sub">Copy tables and their rows to another server, on the same engine or a different one. You review every type before anything runs.</p>
        </div>
      </header>
      <section className="bridge">
        <EndpointPicker label="From" value={src} onChange={setSrc} role="source" />
        <div className="bridge__arrow" aria-hidden><ArrowRight /></div>
        <EndpointPicker label="To" value={dst} onChange={setDst} role="target" />
      </section>
      {error && <Alert kind="danger">{error}</Alert>}
      {same && <Alert kind="warn">The source and the target are the same. Choose another database or schema to copy into.</Alert>}
      <div className="migrate__go">
        <Button variant="primary" size="lg" onClick={() => plan()} loading={planning} disabled={!src.connectionId || !dst.connectionId || !!same}>
          Plan the migration
        </Button>
        <span className="faint">Reads the source's tables and proposes how to create them on the target. Nothing changes yet.</span>
      </div>
      <HistoryList me={me} />
    </div>
  );
}

function EndpointPicker({ label, value, onChange, role }: { label: string; value: Endpoint; onChange(e: Endpoint): void; role: "source" | "target" }) {
  const conns = useConnections();
  const conn = conns.data?.find((c) => c.id === value.connectionId);
  const drv = useDriver(conn?.driver);
  const dbs = useDatabases(conn?.id, !!drv?.caps.databases);
  const schemas = useSchemas(conn?.id, value.database || undefined, !!drv?.caps.schemas && (!drv?.caps.databases || !!value.database));
  const usable = (c: Connection) => role === "source" || (c.access !== "read" && !c.readOnly && c.driver !== "bigquery");
  const list = (conns.data ?? []).slice().sort((a, b) => a.name.localeCompare(b.name));
  useEffect(() => {
    // Pick the connection's default database once its list arrives.
    if (drv?.caps.databases && !value.database && dbs.data?.length && conn) {
      const def = (conn.params?.database as string) || dbs.data.find((d) => !d.system)?.name || "";
      if (def) onChange({ ...value, database: def });
    }
  }, [dbs.data, drv?.caps.databases]); // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <div className={`endpoint endpoint--${role}`}>
      <div className="endpoint__head">
        <span className="endpoint__label">{label}</span>
        {conn && <Env env={conn.environment} />}
      </div>
      <div className="endpoint__id">
        {conn ? <EngineBadge driver={conn.driver} size={34} /> : <span className="endpoint__blank"><Database /></span>}
        <select className="select endpoint__conn" value={value.connectionId} onChange={(e) => onChange({ connectionId: e.target.value, database: "", schema: "" })} aria-label={`${label} connection`}>
          <option value="">Choose a connection</option>
          {list.map((c) => (
            <option key={c.id} value={c.id} disabled={!usable(c)}>
              {c.name}{!usable(c) ? (c.driver === "bigquery" ? " — can only be a source" : " — needs write access") : ""}
            </option>
          ))}
        </select>
      </div>
      {conn && (drv?.caps.databases || drv?.caps.schemas) && (
        <div className="endpoint__scope">
          {drv?.caps.databases && (
            <select className="select" value={value.database} onChange={(e) => onChange({ ...value, database: e.target.value, schema: "" })} aria-label={`${label} database`}>
              <option value="">Default database</option>
              {(dbs.data ?? []).filter((d) => !d.system).map((d) => <option key={d.name} value={d.name}>{d.name}</option>)}
            </select>
          )}
          {drv?.caps.schemas && (
            <select className="select" value={value.schema} onChange={(e) => onChange({ ...value, schema: e.target.value })} aria-label={`${label} schema`}>
              <option value="">Default schema</option>
              {(schemas.data ?? []).filter((s) => !s.system).map((s) => <option key={s.name} value={s.name}>{s.name}</option>)}
            </select>
          )}
        </div>
      )}
      {conn && <p className="endpoint__hint faint">{role === "target" ? "Tables are created here. Create the database or schema first if it does not exist." : `${drv?.name ?? ""} · ${conn.params?.host ?? conn.params?.file ?? ""}`}</p>}
    </div>
  );
}

// ---- Plan review ------------------------------------------------------------------

function applyCase(s: string, mode: string) {
  return mode === "lower" ? s.toLowerCase() : mode === "upper" ? s.toUpperCase() : s;
}

function attention(t: TablePlan) {
  return t.columns.filter((c) => c.include && c.lossy).length + t.indexes.filter((i) => !i.include && i.note).length + t.notes.length;
}

function PlanReview({ resp, src, dst, onChange, onBack, onReplan, replanning }: {
  resp: PlanResponse; src: Endpoint; dst: Endpoint; onChange(p: Plan): void; onBack(): void; onReplan(o: MigrateOptions): void; replanning: boolean;
}) {
  const qc = useQueryClient();
  const plan = resp.plan;
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const [filter, setFilter] = useState("");
  const [preview, setPreview] = useState<{ table: string; statements?: string[]; error?: string } | null>(null);
  const [confirm, setConfirm] = useState<{ name: string; production: boolean; replacing: boolean } | null>(null);
  const [typed, setTyped] = useState("");
  const [starting, setStarting] = useState(false);
  const [error, setError] = useState("");
  const toMongo = plan.targetEngine === "mongodb";

  const update = (fn: (p: Plan) => void) => {
    const next = structuredClone(plan);
    fn(next);
    onChange(next);
  };
  const setOpt = <K extends keyof MigrateOptions>(k: K, v: MigrateOptions[K]) => update((p) => {
    const prev = p.options[k];
    p.options[k] = v;
    if (k === "nameCase") {
      for (const t of p.tables) {
        if (t.target === applyCase(t.source.name, prev as string)) t.target = applyCase(t.source.name, v as string);
        for (const c of t.columns) {
          if (c.target !== "_id" && c.target === applyCase(c.source, prev as string)) c.target = applyCase(c.source, v as string);
        }
      }
    }
    if (k === "indexes") for (const t of p.tables) for (const i of t.indexes) if (!i.note) i.include = !!v;
    if (k === "foreignKeys") for (const t of p.tables) for (const f of t.foreignKeys) if (!f.note) f.include = !!v;
  });

  const included = plan.tables.filter((t) => t.include);
  const rows = included.reduce((n, t) => n + (t.rows ?? 0), 0);
  const flagged = included.reduce((n, t) => n + attention(t), 0);
  const existing = included.filter((t) => t.exists);
  const shown = plan.tables.filter((t) => !filter || `${t.source.name} ${t.target}`.toLowerCase().includes(filter.toLowerCase()));

  const start = async (confirmName?: string) => {
    setStarting(true);
    setError("");
    try {
      const r = await post<{ id: string }>("migrations", { source: src, target: dst, plan, confirm: confirmName });
      qc.invalidateQueries({ queryKey: ["migrations"] });
      go(`/migrate/${r.id}`);
    } catch (e) {
      if (e instanceof ApiError && e.code === "confirm_required") {
        setConfirm(e.detail as { name: string; production: boolean; replacing: boolean });
        setTyped("");
      } else {
        setError(e instanceof ApiError ? e.message : String(e));
      }
    } finally {
      setStarting(false);
    }
  };

  const showSQL = async (t: TablePlan) => {
    setPreview({ table: t.source.name });
    try {
      const r = await post<{ statements: string[] }>("migrations/preview", { target: dst, plan, table: t.source.name });
      setPreview({ table: t.source.name, statements: r.statements });
    } catch (e) {
      setPreview({ table: t.source.name, error: e instanceof ApiError ? e.message : String(e) });
    }
  };

  return (
    <>
      <button className="backlink" onClick={onBack}><ArrowLeft size={14} /> Change source or target</button>
      <header className="planhead">
        <div className="planhead__side">
          <EngineBadge driver={plan.sourceEngine} size={40} />
          <div><b>{resp.source.label}</b><span className="faint">{resp.source.version || ENGINE_NAMES[plan.sourceEngine]}</span></div>
        </div>
        <div className="planhead__flow">
          <span>{included.length} of {plan.tables.length} tables</span>
          <span className="planhead__line" />
          <span className="faint">{rows ? `≈ ${int(rows)} rows` : ""}</span>
        </div>
        <div className="planhead__side planhead__side--to">
          <EngineBadge driver={plan.targetEngine} size={40} />
          <div><b>{resp.target.label}</b><span className="faint">{resp.target.version || ENGINE_NAMES[plan.targetEngine]}</span></div>
          <Env env={resp.target.environment as Connection["environment"]} />
        </div>
      </header>
      {plan.warnings.map((w) => <Alert key={w} kind="warn">{w}</Alert>)}

      <section className="card pcard planopts">
        <div className="planopts__row">
          <span className="planopts__label">When a table already exists</span>
          <div className="segmented" role="group" aria-label="When a table already exists">
            <button aria-pressed={plan.options.existing === "fail"} onClick={() => setOpt("existing", "fail")}>Stop</button>
            <button aria-pressed={plan.options.existing === "replace"} onClick={() => setOpt("existing", "replace")}>Replace it</button>
            <button aria-pressed={plan.options.existing === "keep"} onClick={() => setOpt("existing", "keep")}>Keep it</button>
          </div>
          {plan.options.existing === "keep" && (
            <div className="segmented" role="group" aria-label="Rows in kept tables">
              <button aria-pressed={plan.options.existingRows === "append"} onClick={() => setOpt("existingRows", "append")}>Add rows</button>
              <button aria-pressed={plan.options.existingRows === "replace"} onClick={() => setOpt("existingRows", "replace")}>Replace rows</button>
            </div>
          )}
          {existing.length > 0 && <span className="planopts__hint"><TriangleAlert /> {existing.length} {existing.length === 1 ? "table exists" : "tables exist"} on the target</span>}
        </div>
        {!toMongo && (
          <div className="planopts__row">
            <span className="planopts__label">Names</span>
            <div className="segmented" role="group" aria-label="Name case">
              {(["keep", "lower", "upper"] as const).map((m) => (
                <button key={m} aria-pressed={plan.options.nameCase === m} onClick={() => setOpt("nameCase", m)}>{m === "keep" ? "As they are" : m === "lower" ? "lowercase" : "UPPERCASE"}</button>
              ))}
            </div>
          </div>
        )}
        <div className="planopts__row planopts__checks">
          <label className="check"><input type="checkbox" checked={plan.options.data} onChange={(e) => setOpt("data", e.target.checked)} /> Copy rows</label>
          <label className="check"><input type="checkbox" checked={plan.options.indexes} onChange={(e) => setOpt("indexes", e.target.checked)} /> Indexes</label>
          {!toMongo && <label className="check"><input type="checkbox" checked={plan.options.foreignKeys} onChange={(e) => setOpt("foreignKeys", e.target.checked)} /> Foreign keys</label>}
          {plan.sameEngine && (
            <label className="check"><input type="checkbox" checked={plan.options.objects} disabled={replanning}
              onChange={(e) => onReplan({ ...plan.options, objects: e.target.checked })} /> Views, routines and triggers</label>
          )}
          <label className="check"><input type="checkbox" checked={plan.options.stopOnError} onChange={(e) => setOpt("stopOnError", e.target.checked)} /> Stop at the first failed table</label>
        </div>
        {plan.options.data && (
          <div className="planopts__row">
            <span className="planopts__label">Check afterwards</span>
            <div className="segmented" role="group" aria-label="Verification">
              <button aria-pressed={plan.options.verify === "contents"} onClick={() => setOpt("verify", "contents")}>Every value</button>
              <button aria-pressed={plan.options.verify === "counts"} onClick={() => setOpt("verify", "counts")}>Row counts</button>
            </div>
            <span className="planopts__hint faint">{plan.options.verify === "contents" ? "Reads the copy back and compares each column's values with the source." : "Compares the number of rows only."}</span>
          </div>
        )}
      </section>

      <div className="plantables__bar">
        <h2 className="plantables__title">Tables <span className="faint">{plan.tables.length}</span></h2>
        <button className="linkish" onClick={() => update((p) => p.tables.forEach((t) => (t.include = true)))}>All</button>
        <button className="linkish" onClick={() => update((p) => p.tables.forEach((t) => (t.include = false)))}>None</button>
        <span className="spacer" />
        {flagged > 0 && <span className="plantables__flag"><TriangleAlert /> {flagged} {flagged === 1 ? "thing" : "things"} to look at</span>}
        <input className="input plantables__filter" placeholder="Filter tables" value={filter} onChange={(e) => setFilter(e.target.value)} />
      </div>
      <div className="plantables">
        {shown.map((t) => {
          const ti = plan.tables.indexOf(t);
          const n = attention(t);
          const isOpen = !!open[t.source.name];
          return (
            <div key={t.source.name} className={`ptable ${t.include ? "" : "is-off"} ${isOpen ? "is-open" : ""}`}>
              <div className="ptable__row">
                <input type="checkbox" checked={t.include} onChange={(e) => update((p) => { p.tables[ti].include = e.target.checked; })} aria-label={`Include ${t.source.name}`} />
                <button className="ptable__toggle" onClick={() => setOpen({ ...open, [t.source.name]: !isOpen })} aria-expanded={isOpen}>
                  {isOpen ? <ChevronDown /> : <ChevronRight />}
                  <span className="mono truncate">{t.source.name}</span>
                </button>
                <ArrowRight className="ptable__arrow" />
                <input className="input input--mono ptable__target" value={t.target} onChange={(e) => update((p) => { p.tables[ti].target = e.target.value; })} aria-label={`Target name for ${t.source.name}`} disabled={!t.include} />
                <span className="ptable__rows faint">{t.rows != null ? `${int(t.rows)} rows` : ""}</span>
                <span className="ptable__badges">
                  {t.exists && <Tip label={plan.options.existing === "replace" ? "Exists on the target and will be replaced" : plan.options.existing === "keep" ? "Exists on the target; rows go into it" : "Exists on the target: choose Replace or Keep above"}><span className={`pbadge ${plan.options.existing === "fail" ? "pbadge--danger" : "pbadge--warn"}`}>exists</span></Tip>}
                  {n > 0 && <span className="pbadge pbadge--warn">{n} to check</span>}
                </span>
                <Tip label="Show the SQL that creates it"><Button size="sm" variant="ghost" icon onClick={() => showSQL(t)} aria-label="Preview SQL" disabled={!t.include}><Code2 /></Button></Tip>
              </div>
              {isOpen && <TableDetail t={t} plan={plan} onChange={(fn) => update((p) => fn(p.tables[ti]))} />}
            </div>
          );
        })}
      </div>

      {(plan.skipped.length > 0 || plan.objects.length > 0) && <Leftovers plan={plan} />}

      <div className="planstart">
        {error && <Alert kind="danger">{error}</Alert>}
        <div className="planstart__bar">
          <span>{included.length} {included.length === 1 ? "table" : "tables"}{plan.options.data ? `, ≈ ${int(rows)} rows` : ", structure only"} into <b>{resp.target.label}</b></span>
          <span className="spacer" />
          <Button onClick={onBack}>Cancel</Button>
          <Button variant="primary" onClick={() => start()} loading={starting} disabled={!included.length}>Start migration</Button>
        </div>
      </div>

      <Dialog open={!!preview} onOpenChange={(o) => !o && setPreview(null)} title={`SQL for ${preview?.table ?? ""}`} width="wide"
        description="What Rowsmith runs on the target: the table first, then its indexes and keys once the rows are in.">
        {preview?.error ? <Alert kind="danger">{preview.error}</Alert> : !preview?.statements ? <Spinner /> : (
          <pre className="plansql">{preview.statements.map((s, i) => <Fragment key={i}>{highlightSQL(s)}{";\n\n"}</Fragment>)}</pre>
        )}
      </Dialog>
      <Dialog open={!!confirm} onOpenChange={(o) => !o && setConfirm(null)} title="Confirm the migration"
        description={confirm?.production ? `${confirm.name} is a production connection.` : "Existing tables on the target will be dropped and created again."}
        footer={<><Button onClick={() => setConfirm(null)}>Cancel</Button><Button variant="danger" disabled={typed !== confirm?.name} loading={starting} onClick={() => { const n = typed; setConfirm(null); start(n); }}>Start migration</Button></>}>
        <p className="muted">Type <b className="mono">{confirm?.name}</b> to continue.</p>
        <input className="input" value={typed} onChange={(e) => setTyped(e.target.value)} autoFocus aria-label="Target connection name" />
      </Dialog>
    </>
  );
}

function TableDetail({ t, plan, onChange }: { t: TablePlan; plan: Plan; onChange(fn: (t: TablePlan) => void): void }) {
  const toMongo = plan.targetEngine === "mongodb";
  return (
    <div className="ptable__detail">
      {t.notes.length > 0 && <ul className="ptable__notes">{t.notes.map((n) => <li key={n}><CircleAlert /> {n}</li>)}</ul>}
      <div className="pcols" role="table" aria-label={`Columns of ${t.source.name}`}>
        <div className="pcols__head" role="row">
          <span />
          <span>Source column</span>
          <span />
          <span>Target column</span>
          {!toMongo && <span>Type</span>}
          {!toMongo && <span className="pcols__null">Null</span>}
          <span>Notes</span>
        </div>
        {t.columns.map((c, ci) => (
          <ColumnRow key={c.source} c={c} pk={t.primaryKey.includes(c.source)} toMongo={toMongo} choices={plan.typeChoices}
            onChange={(fn) => onChange((tt) => fn(tt.columns[ci]))} />
        ))}
      </div>
      {(t.indexes.length > 0 || t.foreignKeys.length > 0) && (
        <div className="pkeys">
          {t.indexes.map((ix, i) => (
            <label key={"i" + i} className={`pkey ${ix.include ? "" : "is-off"}`}>
              <input type="checkbox" checked={ix.include} onChange={(e) => onChange((tt) => { tt.indexes[i].include = e.target.checked; })} />
              <span className="pkey__kind">{ix.unique ? "unique" : "index"}</span>
              <span className="mono">{ix.name || "(" + ix.columns.join(", ") + ")"}</span>
              <span className="faint mono">({ix.columns.join(", ")}){ix.type ? ` ${ix.type}` : ""}</span>
              {ix.note && <span className="pkey__note">{ix.note}</span>}
            </label>
          ))}
          {t.foreignKeys.map((fk, i) => (
            <label key={"f" + i} className={`pkey ${fk.include ? "" : "is-off"}`}>
              <input type="checkbox" checked={fk.include} onChange={(e) => onChange((tt) => { tt.foreignKeys[i].include = e.target.checked; })} />
              <span className="pkey__kind">foreign key</span>
              <span className="mono">{fk.name}</span>
              <span className="faint mono">({fk.columns.join(", ")}) → {fk.refTable}({fk.refColumns.join(", ")}){fk.onDelete ? ` on delete ${fk.onDelete.toLowerCase()}` : ""}</span>
              {!plan.tables.find((x) => x.source.name === fk.refTable && x.include) && <span className="pkey__note">{fk.refTable} is not included, so this key will be left out</span>}
            </label>
          ))}
        </div>
      )}
    </div>
  );
}

function ColumnRow({ c, pk, toMongo, choices, onChange }: { c: ColumnPlan; pk: boolean; toMongo: boolean; choices: string[]; onChange(fn: (c: ColumnPlan) => void): void }) {
  const list = `types-${choices.length}`;
  return (
    <div className={`pcol ${c.include ? "" : "is-off"} ${c.lossy ? "is-lossy" : ""}`} role="row">
      <input type="checkbox" checked={c.include} disabled={pk} onChange={(e) => onChange((x) => { x.include = e.target.checked; })} aria-label={`Include ${c.source}`}
        title={pk ? "Part of the primary key" : undefined} />
      <span className="pcol__src"><span className="mono">{c.source}</span>{pk && <span className="pbadge">key</span>}<span className="faint mono pcol__type">{c.sourceType}</span></span>
      <ArrowRight className="pcol__arrow" />
      <input className="input input--mono" value={c.target} onChange={(e) => onChange((x) => { x.target = e.target.value; })} disabled={!c.include} aria-label={`Target name for ${c.source}`} />
      {!toMongo && (
        <>
          <input className="input input--mono" value={c.type} list={list} onChange={(e) => onChange((x) => { x.type = e.target.value; x.lossy = false; })} disabled={!c.include} aria-label={`Type for ${c.source}`} spellCheck={false} />
          <datalist id={list}>{choices.map((t) => <option key={t} value={t} />)}</datalist>
          <input type="checkbox" className="pcols__null" checked={c.nullable} disabled={!c.include || pk} onChange={(e) => onChange((x) => { x.nullable = e.target.checked; })} aria-label={`${c.source} allows NULL`} />
        </>
      )}
      <span className="pcol__notes">
        {c.lossy && <TriangleAlert className="pcol__warn" />}
        {[...c.notes, c.autoIncrement ? "auto-numbered" : "", c.values?.length ? `allowed: ${c.values.join(", ")}` : ""].filter(Boolean).join(" · ")}
      </span>
    </div>
  );
}

function Leftovers({ plan }: { plan: Plan }) {
  const [open, setOpen] = useState(false);
  return (
    <section className="leftovers">
      <button className="section-toggle" onClick={() => setOpen(!open)}>
        {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        {plan.objects.length > 0 && `${plan.objects.length} definitions copied as they are`}{plan.objects.length > 0 && plan.skipped.length > 0 && " · "}
        {plan.skipped.length > 0 && `${plan.skipped.length} objects not copied`}
      </button>
      {open && (
        <ul className="leftovers__list">
          {plan.objects.map((o) => <li key={"o" + o.kind + o.name}><CircleCheck className="ok" /> <span className="pkey__kind">{o.kind}</span> <span className="mono">{o.name}</span></li>)}
          {plan.skipped.map((o) => <li key={"s" + o.kind + o.name}><CircleX /> <span className="pkey__kind">{o.kind}</span> <span className="mono">{o.name}</span> <span className="faint">{o.reason}</span></li>)}
        </ul>
      )}
    </section>
  );
}

// ---- Run --------------------------------------------------------------------------------

const TABLE_LABEL: Record<TableRun["status"], string> = {
  waiting: "Waiting", creating: "Creating", copying: "Copying", indexing: "Adding keys", verifying: "Checking", done: "Done", failed: "Failed", skipped: "Skipped", cancelled: "Cancelled",
};

function RunView({ id }: { id: string }) {
  const q = useMigration(id);
  const qc = useQueryClient();
  const [logOpen, setLogOpen] = useState(false);
  const [, setTick] = useState(0);
  const m = q.data;
  const running = m?.status === "running";
  useEffect(() => {
    if (!running) return;
    const t = setInterval(() => setTick((n) => n + 1), 1000);
    return () => clearInterval(t);
  }, [running]);
  useEffect(() => {
    if (m && !running) qc.invalidateQueries({ queryKey: ["migrations"] });
  }, [running]); // eslint-disable-line react-hooks/exhaustive-deps
  const v = m?.progress;
  const totals = useMemo(() => {
    const t = v?.tables ?? [];
    const rows = t.reduce((n, x) => n + x.rows, 0);
    const expected = t.reduce((n, x) => n + Math.max(x.total, x.rows), 0);
    const finished = t.filter((x) => ["done", "failed", "skipped", "cancelled"].includes(x.status)).length;
    const verified = t.filter((x) => x.verify?.counts === "match" && x.verify.contents !== "differ").length;
    const differ = t.filter((x) => x.verify && (x.verify.counts === "differ" || x.verify.contents === "differ")).length;
    const failed = t.filter((x) => x.status === "failed").length;
    return { rows, expected, finished, verified, differ, failed, count: t.length };
  }, [v]);
  if (q.isLoading) return <div className="page"><Spinner large /></div>;
  if (!m) return <div className="page"><Empty title="Migration not found" /></div>;
  const elapsed = (m.finishedAt || Date.now()) - m.startedAt;
  const frac = totals.count ? (running ? Math.min(0.98, totals.expected ? totals.rows / totals.expected : totals.finished / totals.count) : 1) : 0;
  const cancel = async () => {
    try {
      await post(`migrations/${id}/cancel`, {});
      toast.info("Stopping", "The table being copied rolls back its last chunk.");
    } catch (e) {
      toast.error("Could not stop it", e instanceof ApiError ? e.message : String(e));
    }
  };
  return (
    <div className="page migrate">
      <Link href="/migrate" className="backlink"><ArrowLeft size={14} /> Migrations</Link>
      <header className="runhead">
        <div className="grow" style={{ minWidth: 0 }}>
          <div className="runhead__route">
            <span className="truncate">{m.sourceLabel}</span><ArrowRight /><span className="truncate">{m.targetLabel}</span>
          </div>
          <h1 className="page__title display runhead__title">
            {running ? (v?.phase || "Starting") : m.status === "done" ? "Migration finished" : m.status === "cancelled" ? "Migration stopped" : m.status === "interrupted" ? "Interrupted by a restart" : "Migration finished with errors"}
          </h1>
          <p className="page__sub">Started {ago(m.startedAt)} by {m.userName} · {duration(elapsed)}</p>
        </div>
        {running && <Button onClick={cancel}><Square /> Stop</Button>}
      </header>

      <div className={`runbar runbar--${m.status}`} role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(frac * 100)}>
        <div className="runbar__fill" style={{ width: `${frac * 100}%` }} />
      </div>
      <div className="runstats">
        <div><b>{totals.finished}</b><span>of {totals.count} tables</span></div>
        <div><b>{int(totals.rows)}</b><span>rows copied</span></div>
        {!running && <div className={totals.differ ? "is-bad" : "is-good"}><b>{totals.verified}</b><span>verified{totals.differ ? `, ${totals.differ} differ` : ""}</span></div>}
        {totals.failed > 0 && <div className="is-bad"><b>{totals.failed}</b><span>failed</span></div>}
      </div>
      {v?.error && !running && <Alert kind={m.status === "cancelled" ? "info" : "danger"}>{v.error}</Alert>}

      <section className="runtables">
        {(v?.tables ?? []).map((t) => <RunTable key={t.source} t={t} />)}
      </section>

      {(v?.objects.length ?? 0) > 0 && (
        <section className="card pcard runobjects">
          <h2 className="pcard__title">Views, routines and types</h2>
          {v!.objects.map((o) => (
            <div key={o.kind + o.name} className="runobject">
              {o.status === "done" ? <CircleCheck className="ok" /> : o.status === "failed" ? <CircleX className="bad" /> : <Loader2 className="spin" />}
              <span className="pkey__kind">{o.kind}</span> <span className="mono">{o.name}</span>
              {o.error && <span className="faint">{o.error}</span>}
            </div>
          ))}
        </section>
      )}

      {(v?.log.length ?? 0) > 0 && (
        <section className="runlog">
          <button className="section-toggle" onClick={() => setLogOpen(!logOpen)}>{logOpen ? <ChevronDown size={14} /> : <ChevronRight size={14} />} Log ({v!.log.length})</button>
          {logOpen && (
            <ol className="runlog__lines">
              {v!.log.map((l, i) => <li key={i} className={`is-${l.level}`}><span className="faint">{new Date(l.at).toLocaleTimeString()}</span> {l.text}</li>)}
            </ol>
          )}
        </section>
      )}
    </div>
  );
}

function RunTable({ t }: { t: TableRun }) {
  const [open, setOpen] = useState(false);
  const active = ["creating", "copying", "indexing", "verifying"].includes(t.status);
  const frac = t.status === "done" ? 1 : t.total > 0 ? Math.min(1, t.rows / t.total) : 0;
  const v = t.verify;
  const bad = t.status === "failed" || v?.counts === "differ" || v?.contents === "differ";
  const details = t.error || t.notes.length > 0 || v?.note || v?.columns?.length;
  return (
    <div className={`runtable runtable--${t.status} ${bad ? "is-bad" : ""}`}>
      <button className="runtable__row" onClick={() => details && setOpen(!open)} aria-expanded={open}>
        <span className="runtable__icon">
          {t.status === "done" && !bad ? <CircleCheck /> : bad ? <CircleX /> : active ? <Loader2 className="spin" /> : <span className="runtable__dot" />}
        </span>
        <span className="runtable__names"><span className="mono truncate">{t.source}</span>{t.target !== t.source && <span className="faint mono truncate">→ {t.target}</span>}</span>
        <span className="runtable__meter">
          <span className="runtable__bar"><span style={{ width: `${frac * 100}%` }} /></span>
          <span className="faint">{int(t.rows)}{t.total > t.rows ? ` / ${int(t.total)}` : ""}</span>
        </span>
        <span className={`runpill runpill--${t.status === "done" ? (bad ? "failed" : "ok") : t.status === "failed" ? "failed" : active ? "running" : "quiet"}`}>{TABLE_LABEL[t.status]}</span>
        <span className="runtable__verify">
          {v && (
            <>
              <Tip label={v.counts === "match" ? `${int(v.targetRows)} rows on both sides` : v.counts === "differ" ? `${int(v.sourceRows)} rows read, ${int(v.targetRows)} on the target` : v.note ?? ""}>
                <span className={`vtick ${v.counts === "match" ? "is-ok" : v.counts === "differ" ? "is-bad" : ""}`}>rows</span>
              </Tip>
              <Tip label={v.contents === "match" ? "Every column's values match" : v.contents === "differ" ? `Different values in ${v.columns?.join(", ")}` : v.note || "Not compared"}>
                <span className={`vtick ${v.contents === "match" ? "is-ok" : v.contents === "differ" ? "is-bad" : ""}`}>values</span>
              </Tip>
            </>
          )}
        </span>
        <span className="runtable__more">{details ? (open ? <ChevronDown size={14} /> : <ChevronRight size={14} />) : null}</span>
      </button>
      {open && (
        <div className="runtable__detail">
          {t.error && <pre className="runrow__error">{t.error}</pre>}
          {v?.columns?.length ? <p>Values differ in <b className="mono">{v.columns.join(", ")}</b>. The plan's notes say which types round or shorten values.</p> : null}
          {v?.note && <p className="muted">{v.note}</p>}
          {t.notes.length > 0 && <ul className="ptable__notes">{t.notes.map((n, i) => <li key={i}><CircleAlert /> {n}</li>)}</ul>}
        </div>
      )}
    </div>
  );
}

// ---- History ------------------------------------------------------------------------------

function HistoryList({ me }: { me: Me }) {
  const isAdmin = me.user.role === "owner" || me.user.role === "admin";
  const [all, setAll] = useState(false);
  const list = useMigrations(all);
  if (!list.data?.length && !all) return null;
  return (
    <section className="mhistory">
      <div className="mhistory__head">
        <h2 className="plantables__title"><HistoryIcon size={16} /> Recent migrations</h2>
        <span className="spacer" />
        {isAdmin && (
          <div className="segmented" role="group" aria-label="Whose migrations">
            <button aria-pressed={!all} onClick={() => setAll(false)}>Mine</button>
            <button aria-pressed={all} onClick={() => setAll(true)}>Everyone's</button>
          </div>
        )}
      </div>
      <div className="mhistory__list">
        {(list.data ?? []).map((m) => <HistoryRow key={m.id} m={m} all={all} />)}
      </div>
    </section>
  );
}

function HistoryRow({ m, all }: { m: Migration; all: boolean }) {
  const label = { running: "Running", done: "Done", failed: "Failed", cancelled: "Stopped", interrupted: "Interrupted" }[m.status];
  const pill = m.status === "done" ? "ok" : m.status === "running" ? "running" : m.status === "cancelled" ? "quiet" : "failed";
  return (
    <Link href={`/migrate/${m.id}`} className="mhrow">
      <span className={`runpill runpill--${pill}`}>{m.status === "running" && <span className="spinner" />}{label}</span>
      <span className="mhrow__route"><span className="truncate">{m.sourceLabel}</span><ArrowRight size={13} /><span className="truncate">{m.targetLabel}</span></span>
      <span className="faint">{m.tables} {m.tables === 1 ? "table" : "tables"} · {int(m.rows)} rows</span>
      <span className="faint mhrow__when">{all ? `${m.userName} · ` : ""}{ago(m.startedAt)}{m.finishedAt ? ` · ${duration(m.finishedAt - m.startedAt)}` : ""}</span>
    </Link>
  );
}
