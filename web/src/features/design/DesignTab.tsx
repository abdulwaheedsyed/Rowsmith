import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import {
  ArrowDown, ArrowUp, ChevronDown, ChevronRight, Copy, GripVertical, Info, KeyRound, Link2, ListTree, MoreHorizontal, Plus, RotateCcw, ShieldCheck, Sparkles, Trash2, Undo2, Wand2, X,
} from "lucide-react";
import { ApiError, post, stream } from "../../lib/api";
import { useDescribe, useDriver, useObjects } from "../../lib/queries";
import { toast, useWorkspace, type Tab } from "../../lib/store";
import type { Connection, DriverInfo, Field as DField, ObjectRef, TableDesign } from "../../lib/types";
import { Alert, Button, Dialog, Menu, MenuContent, MenuItem, MenuSep, MenuTrigger, Spinner, Tip } from "../../components/ui";
import { SqlEditor } from "../query/SqlEditor";
import { openObject } from "../workspace/actions";
import { blankColumn, fromTable, key, newDraft, renameColumn, toDef, uniqueName, validate, withoutColumn, type Ck, type Col, type Draft, type Fk, type Ix } from "./model";
import "./design.css";

interface Preview {
  statements: string[];
  script: string;
}

export function DesignTab({ tab, conn }: { tab: Tab; conn: Connection }) {
  const qc = useQueryClient();
  const drv = useDriver(conn.driver);
  const design = drv?.design;
  const creating = !tab.ref;
  const desc = useDescribe(conn.id, tab.ref);
  const [draft, setDraftState] = useState<Draft | null>(() => (tab.state?.draft as Draft | undefined) ?? null);
  const [preview, setPreview] = useState<Preview | null>(null);
  const [previewErr, setPreviewErr] = useState("");
  const [previewing, setPreviewing] = useState(false);
  const [applying, setApplying] = useState(false);
  const [applyErr, setApplyErr] = useState("");
  const [confirm, setConfirm] = useState(false);
  const [showSql, setShowSql] = useState(true);

  // Start from the live table (or a fresh one) unless a draft was kept.
  useEffect(() => {
    if (draft || !drv) return;
    if (creating) setDraftState(newDraft(drv, { database: tab.database, schema: tab.schema, name: "" }));
    else if (desc.data) setDraftState(fromTable(desc.data));
  }, [draft, drv, creating, desc.data, tab.database, tab.schema]);

  // Keep the draft with the tab so it survives tab switches and reloads.
  const persist = useRef<ReturnType<typeof setTimeout>>(undefined);
  const setDraft = (fn: (d: Draft) => Draft) =>
    setDraftState((d) => {
      if (!d) return d;
      const next = fn(d);
      clearTimeout(persist.current);
      persist.current = setTimeout(() => useWorkspace.getState().updateTab(tab.id, { state: { ...tab.state, draft: next }, preview: false }), 400);
      return next;
    });

  const problems = useMemo(() => (draft ? validate(draft, design) : []), [draft, design]);

  // Ask the engine's generator for the SQL as the draft changes.
  useEffect(() => {
    if (!draft || problems.length > 0) {
      setPreview(null);
      setPreviewErr("");
      return;
    }
    const ac = new AbortController();
    setPreviewing(true);
    const t = setTimeout(() => {
      post<Preview>(`c/${conn.id}/ddl`, { action: creating ? "create_table" : "alter_table", ref: tab.ref ?? draft.ref, def: toDef(draft) }, ac.signal)
        .then((r) => {
          setPreview(r);
          setPreviewErr("");
        })
        .catch((e) => {
          if ((e as Error).name !== "AbortError") setPreviewErr(e instanceof ApiError ? e.message : String(e));
        })
        .finally(() => setPreviewing(false));
    }, 350);
    return () => {
      clearTimeout(t);
      ac.abort();
    };
  }, [draft, problems.length, conn.id, creating, tab.ref]);

  if (!drv) return <div className="design__center"><Spinner large /></div>;
  if (!design) return <div className="design__pad"><Alert kind="info" title="No structure editor for this engine">Use the SQL editor to change {drv.name} objects.</Alert></div>;
  if (!creating && desc.error) return <div className="design__pad"><Alert kind="danger" title="Could not load the table">{(desc.error as Error).message}</Alert></div>;
  if (!draft) return <div className="design__center"><Spinner large /></div>;

  const readOnly = conn.access === "read" || conn.readOnly;
  const changes = preview?.statements.length ?? 0;
  const canApply = !readOnly && problems.length === 0 && !previewErr && !previewing && !!preview && changes > 0;
  const docs = !design.columns;

  const discard = () => {
    useWorkspace.getState().updateTab(tab.id, { state: { ...tab.state, draft: undefined } });
    setDraftState(null);
    setApplyErr("");
    if (!creating) desc.refetch();
  };

  const apply = async () => {
    if (!preview) return;
    setConfirm(false);
    setApplying(true);
    setApplyErr("");
    let consoleId = "";
    let ran = 0;
    let failure = "";
    try {
      for await (const ev of stream(`c/${conn.id}/query`, { tab: `design-${tab.id}`, database: draft.ref.database, schema: draft.ref.schema, sql: preview.script, confirm: true })) {
        if (ev.t === "start") consoleId = ev.console;
        else if (ev.t === "stmtEnd") {
          if (ev.error && !failure) failure = ev.error.message;
          else if (!ev.error) ran++;
        }
      }
    } catch (e) {
      failure = e instanceof ApiError ? e.message : String(e);
    } finally {
      // A fresh session per change: a failed script must not leave a transaction or pragma behind.
      if (consoleId) post(`c/${conn.id}/console/close`, { console: consoleId }).catch(() => {});
      setApplying(false);
    }
    for (const k of ["describe", "objects", "catalog", "browse", "count"]) qc.invalidateQueries({ queryKey: [k, conn.id] });
    if (failure) {
      setApplyErr(ran > 0 ? `Statement ${ran + 1} of ${changes} failed: ${failure} The ${ran === 1 ? "statement" : `${ran} statements`} before it ${ran === 1 ? "was" : "were"} applied; review the table before trying again.` : failure);
      return;
    }
    useWorkspace.getState().updateTab(tab.id, { state: { ...tab.state, draft: undefined } });
    if (creating) {
      toast.success(`Created ${draft.ref.name}`);
      const ref = { ...draft.ref, kind: docs ? "collection" : "table" };
      useWorkspace.getState().closeTab(tab.id);
      openObject(conn, ref, "structure");
    } else {
      toast.success(`Saved changes to ${draft.ref.name}`, `${changes} statement${changes === 1 ? "" : "s"} ran.`);
      // Start the next edit from the table as it is now, not the cached description.
      const fresh = await desc.refetch();
      setPreview(null);
      setDraftState(fresh.data ? fromTable(fresh.data) : null);
    }
  };

  const destructive = !creating && draft.dropped.length > 0;
  const needsConfirm = destructive || conn.environment === "production";

  return (
    <div className="design">
      <header className="design__head">
        <div className="grow" style={{ minWidth: 0 }}>
          <div className="eyebrow">{creating ? `New ${docs ? "collection" : "table"}` : `Edit ${docs ? "collection" : "table"}`}{draft.ref.schema ? ` · ${draft.ref.schema}` : draft.ref.database ? ` · ${draft.ref.database}` : ""}</div>
          <input className="design__name display" value={draft.ref.name} placeholder={docs ? "collection_name" : "table_name"} autoFocus={creating} spellCheck={false}
            aria-label="Table name" onChange={(e) => setDraft((d) => ({ ...d, ref: { ...d.ref, name: e.target.value } }))} />
          {design.tableComment && (
            <input className="design__comment" value={draft.comment} placeholder="Add a description" aria-label="Table comment"
              onChange={(e) => setDraft((d) => ({ ...d, comment: e.target.value }))} />
          )}
        </div>
        <div className="design__actions">
          <Button onClick={discard} disabled={applying}><Undo2 /> {creating ? "Reset" : "Discard"}</Button>
          <Button variant={needsConfirm ? "danger" : "primary"} disabled={!canApply || applying} loading={applying}
            onClick={() => (needsConfirm ? setConfirm(true) : apply())}>
            {creating ? "Create" : changes > 0 ? `Apply ${changes} change${changes === 1 ? "" : "s"}` : "No changes"}
          </Button>
        </div>
      </header>

      {design.note && !creating && <div className="design__note"><Info /> {design.note}</div>}
      {readOnly && <Alert kind="warn">You have read-only access to this connection; changes cannot be applied.</Alert>}
      {applyErr && <Alert kind="danger" title="The change did not complete">{applyErr}</Alert>}

      <div className={`design__body ${showSql ? "" : "design__body--nosql"}`}>
        <div className="design__form">
          {design.columns && <ColumnsEditor draft={draft} setDraft={setDraft} design={design} drv={drv} creating={creating} />}
          {design.indexes && <IndexesEditor draft={draft} setDraft={setDraft} design={design} />}
          {design.foreignKeys && <ForeignKeysEditor draft={draft} setDraft={setDraft} design={design} conn={conn} drv={drv} />}
          {design.checks && <ChecksEditor draft={draft} setDraft={setDraft} />}
          {(design.options?.length ?? 0) > 0 && <OptionsEditor draft={draft} setDraft={setDraft} fields={design.options!} />}
        </div>
        <aside className="design__sql" aria-label="SQL preview">
          <button className="design__sql-head" onClick={() => setShowSql(!showSql)} aria-expanded={showSql}>
            {showSql ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
            <span className="eyebrow">SQL that will run</span>
            <span className="spacer" />
            {previewing ? <Spinner /> : preview && <span className="badge tnum">{changes} statement{changes === 1 ? "" : "s"}</span>}
          </button>
          {showSql && (
            <div className="design__sql-body">
              {problems.length > 0 ? (
                <ul className="design__problems">{problems.map((p) => <li key={p}>{p}</li>)}</ul>
              ) : previewErr ? (
                <Alert kind="danger" title={`${drv.name} cannot make this change`}>{previewErr}</Alert>
              ) : preview && changes === 0 ? (
                <p className="faint design__nochange">Nothing to change yet. Edit the {docs ? "indexes or options" : "columns, keys or indexes"} and the statements appear here.</p>
              ) : preview ? (
                <div className="design__script"><SqlEditor value={preview.script.trim()} onChange={() => {}} readOnly dialect={drv.dialect} driverId={conn.driver} language={docs ? "mongo" : "sql"} /></div>
              ) : null}
            </div>
          )}
        </aside>
      </div>

      <Dialog open={confirm} onOpenChange={(o) => !o && setConfirm(false)} title={creating ? `Create ${draft.ref.name} on production?` : `Apply ${changes} change${changes === 1 ? "" : "s"} to ${draft.ref.name}?`}
        description={conn.environment === "production" ? `${conn.name} is a production database.` : undefined}
        footer={<><Button onClick={() => setConfirm(false)}>Keep editing</Button><Button variant="danger" onClick={apply}>{destructive ? `Drop ${draft.dropped.length} column${draft.dropped.length === 1 ? "" : "s"} and apply` : "Apply"}</Button></>}>
        {destructive && (
          <Alert kind="danger" title="Data will be deleted">
            {draft.dropped.map((c) => c.originalName ?? c.name).join(", ")} {draft.dropped.length === 1 ? "is" : "are"} removed with every value {draft.dropped.length === 1 ? "it holds" : "they hold"}.
          </Alert>
        )}
        {drv.dialect !== "postgresql" && drv.dialect !== "mssql" && changes > 1 && <p className="faint">{drv.name} applies structure changes one statement at a time; if one fails, the ones before it stay applied.</p>}
      </Dialog>
    </div>
  );
}

// ---- Columns -----------------------------------------------------------------------

function Section({ title, count, icon, action, children }: { title: string; count?: number; icon: ReactNode; action?: ReactNode; children: ReactNode }) {
  return (
    <section className="dsection">
      <div className="dsection__head">
        <span className="dsection__icon">{icon}</span>
        <h2 className="dsection__title">{title}</h2>
        {count !== undefined && <span className="faint tnum">{count}</span>}
        <span className="spacer" />
        {action}
      </div>
      {children}
    </section>
  );
}

function ColumnsEditor({ draft, setDraft, design, drv, creating }: { draft: Draft; setDraft(fn: (d: Draft) => Draft): void; design: TableDesign; drv: DriverInfo; creating: boolean }) {
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const [dragKey, setDragKey] = useState<string | null>(null);
  const movable = creating || design.reorderColumns;
  const listId = `types-${drv.id}`;

  const update = (k: string, patch: Partial<Col>) =>
    setDraft((d) => {
      const old = d.columns.find((c) => c._k === k);
      let next: Draft = { ...d, columns: d.columns.map((c) => (c._k === k ? { ...c, ...patch } : c)) };
      if (old && patch.name !== undefined && patch.name !== old.name) next = renameColumn(next, old.name, patch.name);
      return next;
    });
  const remove = (c: Col) =>
    setDraft((d) => {
      const next = withoutColumn(d, c.name);
      return { ...next, columns: d.columns.filter((x) => x._k !== c._k), dropped: c.originalName ? [...d.dropped, c] : d.dropped };
    });
  const restore = (c: Col) => setDraft((d) => ({ ...d, dropped: d.dropped.filter((x) => x._k !== c._k), columns: [...d.columns, c] }));
  const move = (k: string, to: number) =>
    setDraft((d) => {
      const from = d.columns.findIndex((c) => c._k === k);
      if (from < 0 || to < 0 || to >= d.columns.length || from === to) return d;
      const cols = [...d.columns];
      const [c] = cols.splice(from, 1);
      cols.splice(to, 0, c);
      return { ...d, columns: cols };
    });
  const add = () => setDraft((d) => ({ ...d, columns: [...d.columns, blankColumn(drv, uniqueName("column", d.columns.map((c) => c.name)))] }));
  const togglePK = (c: Col) =>
    setDraft((d) => ({
      ...d,
      primaryKey: d.primaryKey.includes(c.name) ? d.primaryKey.filter((x) => x !== c.name) : d.columns.map((x) => x.name).filter((n) => d.primaryKey.includes(n) || n === c.name),
      columns: d.primaryKey.includes(c.name) ? d.columns : d.columns.map((x) => (x._k === c._k ? { ...x, nullable: false } : x)),
    }));

  return (
    <Section title="Columns" count={draft.columns.length} icon={<ListTree />} action={<Button size="sm" onClick={add}><Plus /> Column</Button>}>
      <datalist id={listId}>{drv.types.map((t) => <option key={t} value={t} />)}</datalist>
      <datalist id="design-defaults"><option value="NULL" /><option value="CURRENT_TIMESTAMP" /><option value="0" /><option value="''" /></datalist>
      <div className="dcols" role="table" aria-label="Columns">
        <div className="dcols__head" role="row">
          <span /><span>Name</span><span>Type</span><span className="c">Null</span><span>Default</span>
          {design.primaryKey && <span className="c">Key</span>}
          {design.autoIncrement && <span className="c">Auto</span>}
          <span /><span />
        </div>
        {draft.columns.map((c, i) => {
          const pk = draft.primaryKey.includes(c.name);
          const identity = !!c.default && /^GENERATED\b/i.test(c.default);
          const expanded = open[c._k];
          const hasExtra = !!(c.comment || c.collation || c.generated || c.onUpdate);
          return (
            <div key={c._k} className={`dcol ${!c.originalName ? "is-new" : ""} ${dragKey === c._k ? "is-dragging" : ""}`} role="row"
              draggable={movable && dragKey === c._k}
              onDragStart={(e) => { e.dataTransfer.effectAllowed = "move"; e.dataTransfer.setData("text/plain", c.name); }}
              onDragEnd={() => setDragKey(null)}
              onDragOver={(e) => { if (dragKey && movable) { e.preventDefault(); } }}
              onDrop={(e) => { e.preventDefault(); if (dragKey) move(dragKey, i); setDragKey(null); }}>
              <div className="dcol__main">
                <span className={`dcol__grip dc-grip ${movable ? "" : "is-off"}`} onMouseDown={() => movable && setDragKey(c._k)} onMouseUp={() => setDragKey(null)}
                  title={movable ? "Drag to reorder" : "This engine cannot move existing columns"}><GripVertical /></span>
                <div className="dcol__name dc-name">
                  <input className="input input--mono" value={c.name} aria-label="Column name" spellCheck={false} onChange={(e) => update(c._k, { name: e.target.value })} />
                  {c.originalName && c.originalName !== c.name && <span className="dcol__was faint">was {c.originalName}</span>}
                  {!c.originalName && !creating && <span className="dcol__was dcol__was--new">new</span>}
                </div>
                <input className="input input--mono dc-type" list={listId} value={c.type} aria-label="Type" spellCheck={false} onChange={(e) => update(c._k, { type: e.target.value })} />
                <label className="dcol__check dc-null c" title="Allows NULL"><input type="checkbox" checked={c.nullable} disabled={pk} onChange={(e) => update(c._k, { nullable: e.target.checked })} /></label>
                <input className="input input--mono dc-default" list="design-defaults" value={identity ? "identity" : c.default ?? ""} disabled={identity || !!c.generated || (c.autoIncrement && drv.dialect !== "postgresql")}
                  placeholder={c.autoIncrement ? "auto" : c.generated ? "computed" : "none"} aria-label="Default" spellCheck={false}
                  onChange={(e) => update(c._k, { default: e.target.value })} />
                {design.primaryKey && (
                  <Tip label={pk ? "Part of the primary key" : "Add to the primary key"}>
                    <button className={`dcol__toggle dc-key c ${pk ? "is-on is-key" : ""}`} aria-pressed={pk} aria-label="Primary key" onClick={() => togglePK(c)}><KeyRound /></button>
                  </Tip>
                )}
                {design.autoIncrement && (
                  <Tip label="Numbered automatically">
                    <button className={`dcol__toggle dc-auto c ${c.autoIncrement ? "is-on" : ""}`} aria-pressed={!!c.autoIncrement} aria-label="Auto-increment"
                      onClick={() => update(c._k, c.autoIncrement
                        ? { autoIncrement: false, default: identity ? undefined : c.default } // switching off also removes an identity default
                        : { autoIncrement: true, default: undefined, nullable: false })}>
                      <Wand2 />
                    </button>
                  </Tip>
                )}
                <button className={`dcol__toggle dc-more ${expanded ? "is-on" : ""} ${hasExtra ? "has-extra" : ""}`} aria-expanded={!!expanded} aria-label="More settings" onClick={() => setOpen({ ...open, [c._k]: !expanded })}>
                  {expanded ? <ChevronDown /> : <ChevronRight />}
                </button>
                <Menu>
                  <MenuTrigger asChild><button className="dcol__toggle dc-menu" aria-label="Column actions"><MoreHorizontal /></button></MenuTrigger>
                  <MenuContent align="end">
                    {movable && <MenuItem icon={<ArrowUp />} disabled={i === 0} onSelect={() => move(c._k, i - 1)}>Move up</MenuItem>}
                    {movable && <MenuItem icon={<ArrowDown />} disabled={i === draft.columns.length - 1} onSelect={() => move(c._k, i + 1)}>Move down</MenuItem>}
                    <MenuItem icon={<Copy />} onSelect={() => setDraft((d) => {
                      const copy: Col = { ...c, name: uniqueName(c.name + "_copy", d.columns.map((x) => x.name)), originalName: undefined, autoIncrement: false, _k: key() };
                      const cols = [...d.columns];
                      cols.splice(i + 1, 0, copy);
                      return { ...d, columns: cols };
                    })}>Duplicate</MenuItem>
                    <MenuSep />
                    <MenuItem icon={<Trash2 />} danger onSelect={() => remove(c)}>{c.originalName ? "Drop column" : "Remove"}</MenuItem>
                  </MenuContent>
                </Menu>
              </div>
              {expanded && (
                <div className="dcol__more">
                  {design.columnComments && <label className="dfield"><span>Comment</span><input className="input" value={c.comment ?? ""} onChange={(e) => update(c._k, { comment: e.target.value })} /></label>}
                  {design.collation && <label className="dfield"><span>Collation</span><input className="input input--mono" value={c.collation ?? ""} placeholder="table default" onChange={(e) => update(c._k, { collation: e.target.value })} /></label>}
                  {design.generated && (
                    <label className="dfield dfield--wide"><span><Sparkles size={12} /> Computed from</span>
                      <div className="row gap-3">
                        <input className="input input--mono grow" value={c.generated ?? ""} placeholder="expression, e.g. price * quantity" spellCheck={false}
                          onChange={(e) => update(c._k, { generated: e.target.value, generatedStored: e.target.value ? c.generatedStored ?? !design.generatedVirtual : undefined })} />
                        {c.generated && design.generatedStored && design.generatedVirtual && (
                          <div className="segmented" role="group" aria-label="Storage">
                            <button aria-pressed={!!c.generatedStored} onClick={() => update(c._k, { generatedStored: true })}>Stored</button>
                            <button aria-pressed={!c.generatedStored} onClick={() => update(c._k, { generatedStored: false })}>Virtual</button>
                          </div>
                        )}
                      </div>
                    </label>
                  )}
                  {design.onUpdate && <label className="dfield"><span>On update</span><input className="input input--mono" value={c.onUpdate ?? ""} placeholder="e.g. CURRENT_TIMESTAMP" onChange={(e) => update(c._k, { onUpdate: e.target.value })} /></label>}
                </div>
              )}
            </div>
          );
        })}
      </div>
      {draft.dropped.length > 0 && (
        <div className="dcols__dropped">
          <span className="faint">Dropped:</span>
          {draft.dropped.map((c) => (
            <button key={c._k} className="chip chip--dropped" onClick={() => restore(c)} title="Restore this column">
              <span className="mono">{c.originalName}</span><RotateCcw size={12} />
            </button>
          ))}
        </div>
      )}
    </Section>
  );
}

// ---- Column token input -------------------------------------------------------------

function ColumnsInput({ value, desc, onChange, suggestions, allowDesc, placeholder }: {
  value: string[]; desc?: boolean[]; onChange(cols: string[], desc: boolean[]): void; suggestions: string[]; allowDesc?: boolean; placeholder: string;
}) {
  const [text, setText] = useState("");
  const listId = useMemo(() => "cols-" + key(), []);
  const d = value.map((_, i) => !!desc?.[i]);
  const add = (name: string) => {
    const n = name.trim();
    if (!n || value.includes(n)) return;
    onChange([...value, n], [...d, false]);
    setText("");
  };
  return (
    <div className="dchips">
      {value.map((c, i) => (
        <span key={c} className="chip">
          {allowDesc ? (
            <button className="chip__dir" title={d[i] ? "Descending — click for ascending" : "Ascending — click for descending"}
              onClick={() => onChange(value, d.map((x, j) => (j === i ? !x : x)))}>
              <span className="mono">{c}</span>{d[i] ? <ArrowDown size={11} /> : <ArrowUp size={11} />}
            </button>
          ) : <span className="mono">{c}</span>}
          <button className="chip__x" aria-label={`Remove ${c}`} onClick={() => onChange(value.filter((_, j) => j !== i), d.filter((_, j) => j !== i))}><X /></button>
        </span>
      ))}
      <input className="dchips__input" list={listId} value={text} placeholder={value.length ? "" : placeholder} spellCheck={false}
        onChange={(e) => {
          const v = e.target.value;
          if (suggestions.includes(v)) add(v);
          else setText(v);
        }}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === ",") { e.preventDefault(); add(text); }
          if (e.key === "Backspace" && !text && value.length) onChange(value.slice(0, -1), d.slice(0, -1));
        }}
        onBlur={() => add(text)} />
      <datalist id={listId}>{suggestions.filter((s) => !value.includes(s)).map((s) => <option key={s} value={s} />)}</datalist>
    </div>
  );
}

// ---- Indexes -------------------------------------------------------------------------

function IndexesEditor({ draft, setDraft, design }: { draft: Draft; setDraft(fn: (d: Draft) => Draft): void; design: TableDesign }) {
  const cols = draft.columns.map((c) => c.name);
  const types = design.indexTypes ?? [];
  const update = (k: string, patch: Partial<Ix>) => setDraft((d) => ({ ...d, indexes: d.indexes.map((i) => (i._k === k ? { ...i, ...patch } : i)) }));
  const add = () => setDraft((d) => ({
    ...d, indexes: [...d.indexes, { name: uniqueName(`${d.ref.name || "table"}_idx`, d.indexes.map((i) => i.name)), columns: [], unique: false, primary: false, type: types[0] || undefined, _k: key() }],
  }));
  return (
    <Section title="Indexes" count={draft.indexes.length} icon={<ListTree />} action={<Button size="sm" onClick={add}><Plus /> Index</Button>}>
      {draft.indexes.length === 0 && <p className="dsection__empty faint">No indexes besides the primary key.</p>}
      <div className="drows">
        {draft.indexes.map((ix) => (
          <div key={ix._k} className="drow">
            <input className="input input--mono drow__name" value={ix.name} aria-label="Index name" spellCheck={false} onChange={(e) => update(ix._k, { name: e.target.value })} />
            <ColumnsInput value={ix.columns} desc={ix.desc} allowDesc suggestions={cols} placeholder={design.columns ? "Add columns" : "Add fields, e.g. address.city"}
              onChange={(columns, desc) => update(ix._k, { columns, desc, lengths: ix.lengths && columns.map((c) => ix.lengths![ix.columns.indexOf(c)] ?? 0) })} />
            <label className="check drow__flag"><input type="checkbox" checked={ix.unique} onChange={(e) => update(ix._k, { unique: e.target.checked })} /> Unique</label>
            {types.length > 1 && (
              <select className="select drow__type" value={ix.type ?? types[0]} aria-label="Index method" onChange={(e) => update(ix._k, { type: e.target.value || undefined })}>
                {types.map((t) => <option key={t} value={t}>{t || "regular"}</option>)}
              </select>
            )}
            <button className="dcol__toggle" aria-label="Remove index" onClick={() => setDraft((d) => ({ ...d, indexes: d.indexes.filter((i) => i._k !== ix._k) }))}><Trash2 /></button>
            {design.partialIndexes && (
              <input className="input input--mono drow__where" value={ix.where ?? ""} placeholder={design.columns ? "Only rows where… (optional)" : "Partial filter, e.g. { status: \"active\" } (optional)"} spellCheck={false}
                onChange={(e) => update(ix._k, { where: e.target.value || undefined })} />
            )}
          </div>
        ))}
      </div>
    </Section>
  );
}

// ---- Foreign keys --------------------------------------------------------------------

function ForeignKeysEditor({ draft, setDraft, design, conn, drv }: { draft: Draft; setDraft(fn: (d: Draft) => Draft): void; design: TableDesign; conn: Connection; drv: DriverInfo }) {
  const objects = useObjects(conn.id, draft.ref.database, draft.ref.schema, true);
  const tables = (objects.data ?? []).filter((o) => o.kind === "table" || o.kind === "partitioned_table").map((o) => o.name);
  const update = (k: string, patch: Partial<Fk>) => setDraft((d) => ({ ...d, foreignKeys: d.foreignKeys.map((f) => (f._k === k ? { ...f, ...patch } : f)) }));
  const add = () => setDraft((d) => ({
    ...d, foreignKeys: [...d.foreignKeys, { name: uniqueName(`fk_${d.ref.name || "table"}`, d.foreignKeys.map((f) => f.name)), columns: [], refTable: { name: "" }, refColumns: [], _k: key() }],
  }));
  const actions = design.fkActions ?? ["NO ACTION", "CASCADE", "SET NULL"];
  return (
    <Section title="Foreign keys" count={draft.foreignKeys.length} icon={<Link2 />} action={<Button size="sm" onClick={add}><Plus /> Foreign key</Button>}>
      {draft.foreignKeys.length === 0 && <p className="dsection__empty faint">No references to other tables.</p>}
      <div className="drows">
        {draft.foreignKeys.map((fk) => (
          <FkRow key={fk._k} fk={fk} conn={conn} cols={draft.columns.map((c) => c.name)} tables={tables} actions={actions} noUpdate={drv.dialect === "plsql"} self={draft.ref}
            update={(p) => update(fk._k, p)} remove={() => setDraft((d) => ({ ...d, foreignKeys: d.foreignKeys.filter((f) => f._k !== fk._k) }))} />
        ))}
      </div>
    </Section>
  );
}

function FkRow({ fk, conn, cols, tables, actions, noUpdate, self, update, remove }: {
  fk: Fk; conn: Connection; cols: string[]; tables: string[]; actions: string[]; noUpdate: boolean; self: ObjectRef; update(p: Partial<Fk>): void; remove(): void;
}) {
  const ref = fk.refTable.name ? { database: fk.refTable.database ?? self.database, schema: fk.refTable.schema ?? self.schema, name: fk.refTable.name, kind: "table" } : undefined;
  const target = useDescribe(conn.id, ref && ref.name !== self.name ? ref : undefined);
  const refCols = ref?.name === self.name ? cols : (target.data?.columns ?? []).map((c) => c.name);
  const rule = (v?: string) => (v && v.toUpperCase() !== "RESTRICT" ? v.toUpperCase() : "NO ACTION");
  return (
    <div className="drow drow--fk">
      <input className="input input--mono drow__name" value={fk.name} aria-label="Foreign key name" spellCheck={false} onChange={(e) => update({ name: e.target.value })} />
      <ColumnsInput value={fk.columns} suggestions={cols} placeholder="Columns" onChange={(columns) => update({ columns })} />
      <span className="drow__arrow">→</span>
      <select className="select drow__table" value={fk.refTable.name} aria-label="Referenced table"
        onChange={(e) => {
          const name = e.target.value;
          update({ refTable: { ...fk.refTable, name }, refColumns: [] });
        }}>
        <option value="">Table…</option>
        {[...new Set([...tables, ...(fk.refTable.name ? [fk.refTable.name] : [])])].map((t) => <option key={t} value={t}>{t}</option>)}
      </select>
      <ColumnsInput value={fk.refColumns} suggestions={refCols} placeholder="Columns" onChange={(refColumns) => update({ refColumns })} />
      <button className="dcol__toggle" aria-label="Remove foreign key" onClick={remove}><Trash2 /></button>
      <div className="drow__rules">
        <label>On delete <select className="select" value={rule(fk.onDelete)} onChange={(e) => update({ onDelete: e.target.value })}>{actions.map((a) => <option key={a}>{a}</option>)}</select></label>
        {!noUpdate && <label>On update <select className="select" value={rule(fk.onUpdate)} onChange={(e) => update({ onUpdate: e.target.value })}>{actions.map((a) => <option key={a}>{a}</option>)}</select></label>}
      </div>
    </div>
  );
}

// ---- Checks and options ------------------------------------------------------------

function ChecksEditor({ draft, setDraft }: { draft: Draft; setDraft(fn: (d: Draft) => Draft): void }) {
  const update = (k: string, patch: Partial<Ck>) => setDraft((d) => ({ ...d, checks: d.checks.map((c) => (c._k === k ? { ...c, ...patch } : c)) }));
  const add = () => setDraft((d) => ({ ...d, checks: [...d.checks, { name: uniqueName(`${d.ref.name || "table"}_check`, d.checks.map((c) => c.name)), expression: "", _k: key() }] }));
  return (
    <Section title="Checks" count={draft.checks.length} icon={<ShieldCheck />} action={<Button size="sm" onClick={add}><Plus /> Check</Button>}>
      {draft.checks.length === 0 && <p className="dsection__empty faint">No row rules.</p>}
      <div className="drows">
        {draft.checks.map((c) => (
          <div key={c._k} className="drow drow--check">
            <input className="input input--mono drow__name" value={c.name} aria-label="Check name" spellCheck={false} onChange={(e) => update(c._k, { name: e.target.value })} />
            <input className="input input--mono" value={c.expression} placeholder="e.g. price >= 0" aria-label="Condition" spellCheck={false} onChange={(e) => update(c._k, { expression: e.target.value })} />
            <button className="dcol__toggle" aria-label="Remove check" onClick={() => setDraft((d) => ({ ...d, checks: d.checks.filter((x) => x._k !== c._k) }))}><Trash2 /></button>
          </div>
        ))}
      </div>
    </Section>
  );
}

function OptionsEditor({ draft, setDraft, fields }: { draft: Draft; setDraft(fn: (d: Draft) => Draft): void; fields: DField[] }) {
  const set = (k: string, v: string) => setDraft((d) => {
    const options = { ...d.options };
    if (v === "") delete options[k];
    else options[k] = v;
    return { ...d, options };
  });
  const visible = fields.filter((f) => !f.showIf || Object.entries(f.showIf).every(([k, vals]) => vals.includes(draft.options[k] ?? "")));
  return (
    <Section title="Options" icon={<Info />}>
      <div className="doptions">
        {visible.map((f) => {
          const v = draft.options[f.key] ?? "";
          return (
            <label key={f.key} className={`dfield ${f.type === "textarea" ? "dfield--wide" : ""}`}>
              {f.type === "bool" ? (
                <span className="check"><input type="checkbox" checked={v === "true"} onChange={(e) => set(f.key, e.target.checked ? "true" : "")} /> {f.label}</span>
              ) : (
                <>
                  <span>{f.label}</span>
                  {f.type === "select" ? (
                    <select className="select" value={v} onChange={(e) => set(f.key, e.target.value)}>
                      <option value="">Default</option>
                      {f.options?.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
                    </select>
                  ) : f.type === "textarea" ? (
                    <textarea className="textarea input--mono" rows={5} value={v} placeholder={f.placeholder} spellCheck={false} onChange={(e) => set(f.key, e.target.value)} />
                  ) : (
                    <input className="input" type={f.type === "number" ? "number" : "text"} value={v} placeholder={f.placeholder} onChange={(e) => set(f.key, e.target.value)} />
                  )}
                </>
              )}
              {f.help && <span className="faint dfield__help">{f.help}</span>}
            </label>
          );
        })}
      </div>
    </Section>
  );
}
