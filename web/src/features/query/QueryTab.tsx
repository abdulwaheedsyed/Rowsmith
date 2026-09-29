import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { format as formatSQL } from "sql-formatter";
import { Play, Square, ChevronDown, Wand2, Save, GitBranch, Undo2, Check, ListTree, PlayCircle, TextSelect, AlertTriangle, ShieldAlert } from "lucide-react";
import { ApiError, get, post, put, stream } from "../../lib/api";
import { useCatalog, useDriver, useDatabases, useSchemas } from "../../lib/queries";
import { useWorkspace, toast, type Tab } from "../../lib/store";
import type { Connection, Plan, SavedQuery, PendingStatement, Danger, StatementKind } from "../../lib/types";
import { modKey } from "../../lib/format";
import { Alert, Button, Dialog, Field, Kbd, Menu, MenuContent, MenuItem, MenuSep, MenuTrigger, Tip } from "../../components/ui";
import { SqlEditor, byteOffset, type EditorHandle, type StatementMark } from "./SqlEditor";
import { Results } from "./Results";
import { useRuns, type RunState, type StmtRun } from "./runs";
import { useStatus } from "../shell/status";
import { HeatBar } from "./HeatBar";
import "./query.css";

const FORMAT_LANG: Record<string, string> = { mysql: "mysql", postgresql: "postgresql", mssql: "transactsql", plsql: "plsql", sqlite: "sqlite", bigquery: "bigquery" };
const ROW_LIMITS = [100, 1000, 10000, 100000];

export function QueryTab({ tab, conn, active }: { tab: Tab; conn: Connection; active: boolean }) {
  const drv = useDriver(conn.driver);
  const qc = useQueryClient();
  const updateTab = useWorkspace((s) => s.updateTab);
  const run = useRuns((s) => s.runs[tab.id]) ?? { status: "idle", stmts: [], lineOffset: 0 } as RunState;
  const setRun = useRuns((s) => s.set);
  const setStatus = useStatus((s) => s.set);
  const editor = useRef<EditorHandle | null>(null);
  const abort = useRef<AbortController | null>(null);
  const [sql, setSql] = useState(tab.sql ?? "");
  const [maxRows, setMaxRows] = useState<number>((tab.state?.maxRows as number) ?? 1000);
  const [split, setSplit] = useState<number>((tab.state?.split as number) ?? 0.46);
  const [saveOpen, setSaveOpen] = useState(false);
  const [typed, setTyped] = useState("");
  const [marks, setMarks] = useState<StatementMark[]>([]);
  const lastRun = useRef<{ mode: "statement" | "all" | "selection"; body: Record<string, unknown> } | null>(null);

  const database = tab.database;
  const schema = tab.schema;
  const hasDbs = !!drv?.caps.databases;
  const hasSchemas = !!drv?.caps.schemas;
  const dbs = useDatabases(conn.id, hasDbs);
  const schemas = useSchemas(conn.id, database, hasSchemas && (!hasDbs || !!database));
  const catalog = useCatalog(conn.id, database, schema, !!drv && (!hasDbs || !!database));

  // Persist the draft (debounced).
  useEffect(() => {
    const t = setTimeout(() => sql !== tab.sql && updateTab(tab.id, { sql }), 400);
    return () => clearTimeout(t);
  }, [sql, tab.id, tab.sql, updateTab]);

  // Statement analysis for gutter markers (debounced, server-side splitter).
  useEffect(() => {
    if (!drv?.caps.sql || !sql.trim()) {
      setMarks([]);
      return;
    }
    const t = setTimeout(async () => {
      try {
        const r = await post<{ statements: { line: number; kind: StatementKind; danger: Danger }[] }>(`c/${conn.id}/analyze`, { sql });
        setMarks(r.statements.map((s) => ({ line: s.line, kind: s.kind, danger: s.danger })));
      } catch {
        /* markers are best-effort */
      }
    }, 700);
    return () => clearTimeout(t);
  }, [sql, conn.id, drv?.caps.sql]);

  useEffect(() => {
    if (!active) return;
    const last = run.stmts[run.stmts.length - 1];
    const set = last?.sets[last.sets.length - 1];
    setStatus(tab.id, {
      ms: run.status === "running" ? undefined : run.ms,
      rows: set ? `${set.rows.length.toLocaleString()} row${set.rows.length === 1 ? "" : "s"}${set.summary?.truncated ? " (limit reached)" : ""}` : undefined,
      inTx: run.inTx,
      running: run.status === "running",
      note: run.status === "running" ? "Running…" : undefined,
    });
  }, [active, run, tab.id, setStatus]);

  const execute = useCallback(
    async (mode: "statement" | "all" | "selection", confirm = false, override?: string) => {
      const view = editor.current?.view;
      const doc = override ?? view?.state.doc.toString() ?? sql;
      let body: Record<string, unknown> = { tab: tab.id, database, schema, maxRows, confirm };
      let lineOffset = 0;
      if (override !== undefined) body = { ...body, sql: override, mode: "all" };
      else if (mode === "selection" || (mode === "statement" && view && !view.state.selection.main.empty)) {
        const sel = view!.state.selection.main;
        body = { ...body, sql: view!.state.sliceDoc(sel.from, sel.to), mode: "all" };
        lineOffset = view!.state.doc.lineAt(sel.from).number - 1;
        mode = "selection";
      } else if (mode === "statement") {
        const cursor = view?.state.selection.main.head ?? 0;
        body = { ...body, sql: doc, mode: "statement", cursor: byteOffset(doc, cursor) };
      } else {
        body = { ...body, sql: doc, mode: "all" };
      }
      if (!String(body.sql ?? "").trim()) return;
      if (confirm && lastRun.current) body = { ...lastRun.current.body, confirm: true };
      lastRun.current = { mode, body };

      abort.current?.abort();
      const ctrl = new AbortController();
      abort.current = ctrl;
      const started = Date.now();
      setRun(tab.id, (r) => ({ ...r, status: "running", startedAt: started, stmts: [], error: undefined, confirm: undefined, lineOffset, plan: undefined, planError: undefined }));

      // Batch streamed rows into ~60fps updates.
      let pending: StmtRun[] = [];
      let raf = 0;
      const flush = () => {
        raf = 0;
        const snapshot = pending.map((s) => ({ ...s, sets: s.sets.map((x) => ({ ...x })) }));
        setRun(tab.id, (r) => ({ ...r, stmts: snapshot }));
      };
      const schedule = () => {
        if (!raf) raf = requestAnimationFrame(flush);
      };
      let ddl = false;
      try {
        for await (const ev of stream(`c/${conn.id}/query`, body, ctrl.signal)) {
          switch (ev.t) {
            case "start":
              setRun(tab.id, (r) => ({ ...r, console: ev.console }));
              break;
            case "stmt":
              pending = [...pending, { index: ev.i, sql: ev.sql, line: ev.line, kind: ev.kind as StatementKind, sets: [], notices: [], done: false }];
              if (ev.kind === "ddl") ddl = true;
              break;
            case "cols": {
              const cur = pending[pending.length - 1];
              if (cur) cur.sets.push({ columns: ev.cols, rows: [] });
              break;
            }
            case "rows": {
              const cur = pending[pending.length - 1];
              const set = cur?.sets[cur.sets.length - 1];
              if (set) set.rows = set.rows.concat(ev.rows);
              break;
            }
            case "end": {
              const cur = pending[pending.length - 1];
              if (!cur) break;
              const set = cur.sets[cur.sets.length - 1];
              if (set && !set.summary && set.columns.length) set.summary = ev.summary;
              else cur.sets.push({ columns: [], rows: [], summary: ev.summary });
              break;
            }
            case "notice": {
              const cur = pending[pending.length - 1];
              if (cur) cur.notices.push({ level: ev.level, text: ev.text });
              break;
            }
            case "stmtEnd": {
              const cur = pending[pending.length - 1];
              if (cur) Object.assign(cur, { error: ev.error, ms: ev.ms, done: true });
              break;
            }
            case "done":
              cancelAnimationFrame(raf);
              flush();
              setRun(tab.id, (r) => ({
                ...r,
                status: ctrl.signal.aborted ? "cancelled" : pending.some((s) => s.error) || ev.error ? "error" : "done",
                ms: ev.ms,
                inTx: ev.inTx,
                console: ev.console,
                error: ev.error?.message,
              }));
              break;
          }
          schedule();
        }
        if (ddl) {
          qc.invalidateQueries({ queryKey: ["objects", conn.id] });
          qc.invalidateQueries({ queryKey: ["describe", conn.id] });
          qc.invalidateQueries({ queryKey: ["catalog", conn.id] });
        }
        qc.invalidateQueries({ queryKey: ["history"] });
      } catch (e) {
        cancelAnimationFrame(raf);
        if (e instanceof ApiError && e.code === "confirm_required") {
          const d = e.detail as { statements: PendingStatement[]; environment: string };
          setTyped("");
          setRun(tab.id, (r) => ({ ...r, status: "idle", confirm: d }));
          return;
        }
        if ((e as Error).name === "AbortError") {
          flush();
          setRun(tab.id, (r) => ({ ...r, status: "cancelled", ms: Date.now() - started }));
          return;
        }
        setRun(tab.id, (r) => ({ ...r, status: "error", error: e instanceof ApiError ? e.message : String(e), ms: Date.now() - started }));
      }
    },
    [sql, tab.id, database, schema, maxRows, conn.id, setRun, qc],
  );

  const cancel = async () => {
    if (run.console) await post(`c/${conn.id}/query/cancel`, { console: run.console }).catch(() => {});
    setTimeout(() => abort.current?.abort(), 1500);
  };

  const explain = async (analyze = false) => {
    const view = editor.current?.view;
    if (!view) return;
    const doc = view.state.doc.toString();
    let stmt = view.state.selection.main.empty ? "" : view.state.sliceDoc(view.state.selection.main.from, view.state.selection.main.to);
    if (!stmt) {
      const r = await post<{ statements: { sql: string; start: number; end: number }[] }>(`c/${conn.id}/analyze`, { sql: doc }).catch(() => null);
      const cur = byteOffset(doc, view.state.selection.main.head);
      stmt = r?.statements.find((s) => cur >= s.start && cur <= s.end + 1)?.sql ?? r?.statements[0]?.sql ?? doc;
    }
    setRun(tab.id, (r) => ({ ...r, planLoading: true, plan: undefined, planError: undefined }));
    try {
      const plan = await post<Plan>(`c/${conn.id}/explain`, { database, schema, sql: stmt, analyze });
      setRun(tab.id, (r) => ({ ...r, plan, planLoading: false }));
    } catch (e) {
      setRun(tab.id, (r) => ({ ...r, planError: e instanceof ApiError ? e.message : String(e), planLoading: false }));
    }
  };

  const format = () => {
    try {
      const lang = FORMAT_LANG[drv?.dialect ?? ""] ?? "sql";
      const view = editor.current?.view;
      const sel = view?.state.selection.main;
      if (view && sel && !sel.empty) {
        const out = formatSQL(view.state.sliceDoc(sel.from, sel.to), { language: lang as any, keywordCase: "upper", tabWidth: 2 });
        view.dispatch({ changes: { from: sel.from, to: sel.to, insert: out } });
      } else {
        setSql(formatSQL(sql, { language: lang as any, keywordCase: "upper", tabWidth: 2 }));
      }
    } catch (e) {
      toast.error("Could not format", (e as Error).message.split("\n")[0]);
    }
  };

  const txAction = (stmt: "COMMIT" | "ROLLBACK") => execute("all", false, stmt);

  // Error line in editor coordinates.
  const errorLine = useMemo(() => {
    const failed = run.stmts.find((s) => s.error);
    if (!failed) return null;
    const line = failed.error?.line || failed.line;
    return line ? line + run.lineOffset : null;
  }, [run.stmts, run.lineOffset]);

  // Split pane drag.
  const box = useRef<HTMLDivElement>(null);
  const onSplitDown = (e: React.PointerEvent) => {
    (e.target as HTMLElement).setPointerCapture(e.pointerId);
    document.body.classList.add("is-resizing-v");
  };
  const onSplitMove = (e: React.PointerEvent) => {
    if (!(e.buttons & 1) || !box.current) return;
    const r = box.current.getBoundingClientRect();
    setSplit(Math.max(0.15, Math.min(0.85, (e.clientY - r.top) / r.height)));
  };
  const onSplitUp = () => {
    document.body.classList.remove("is-resizing-v");
    updateTab(tab.id, { state: { ...(tab.state ?? {}), split } });
  };

  const running = run.status === "running";
  const needsTyping = run.confirm && run.confirm.environment === "production" && run.confirm.statements.some((s) => s.danger.level === "destructive");

  return (
    <div className="query" ref={box}>
      <div className="query__bar">
        {running ? (
          <Button variant="danger" size="sm" onClick={cancel}><Square /> Cancel</Button>
        ) : (
          <div className="splitbtn">
            <Tip label={<>Run statement at cursor <Kbd>{modKey()}↵</Kbd></>}>
              <Button variant="primary" size="sm" onClick={() => execute("statement")}><Play /> Run</Button>
            </Tip>
            <Menu>
              <MenuTrigger asChild>
                <Button variant="primary" size="sm" icon aria-label="More run options"><ChevronDown /></Button>
              </MenuTrigger>
              <MenuContent>
                <MenuItem icon={<Play />} onSelect={() => execute("statement")} hint={`${modKey()}↵`}>Run statement at cursor</MenuItem>
                <MenuItem icon={<PlayCircle />} onSelect={() => execute("all")} hint={`⇧${modKey()}↵`}>Run everything</MenuItem>
                <MenuItem icon={<TextSelect />} onSelect={() => execute("selection")}>Run selection</MenuItem>
                {drv?.caps.explain && (
                  <>
                    <MenuSep />
                    <MenuItem icon={<ListTree />} onSelect={() => explain(false)} hint={`${modKey()}E`}>Explain plan</MenuItem>
                    <MenuItem icon={<ListTree />} onSelect={() => explain(true)}>Explain analyze (executes)</MenuItem>
                  </>
                )}
              </MenuContent>
            </Menu>
          </div>
        )}
        {drv?.caps.explain && !running && (
          <Tip label={<>Explain plan <Kbd>{modKey()}E</Kbd></>}>
            <Button size="sm" variant="ghost" onClick={() => explain(false)}><ListTree /> Explain</Button>
          </Tip>
        )}
        <Tip label={<>Format SQL <Kbd>⇧⌥F</Kbd></>}>
          <Button size="sm" variant="ghost" icon onClick={format} aria-label="Format SQL"><Wand2 /></Button>
        </Tip>
        <Tip label={<>Save query <Kbd>{modKey()}S</Kbd></>}>
          <Button size="sm" variant="ghost" icon onClick={() => setSaveOpen(true)} aria-label="Save query"><Save /></Button>
        </Tip>
        <div className="vdivider" />
        {hasDbs && (
          <select className="select query__scope" value={database ?? ""} onChange={(e) => updateTab(tab.id, { database: e.target.value || undefined, schema: undefined })} aria-label="Database">
            <option value="">No database</option>
            {(dbs.data ?? []).map((d) => <option key={d.name} value={d.name}>{d.name}</option>)}
          </select>
        )}
        {hasSchemas && (
          <select className="select query__scope" value={schema ?? ""} onChange={(e) => updateTab(tab.id, { schema: e.target.value || undefined })} aria-label="Schema">
            <option value="">Default schema</option>
            {(schemas.data ?? []).map((s) => <option key={s.name} value={s.name}>{s.name}</option>)}
          </select>
        )}
        <span className="spacer" />
        {run.inTx && !running && (
          <div className="txbar">
            <GitBranch />
            <span>Transaction open</span>
            <Button size="sm" onClick={() => txAction("ROLLBACK")}><Undo2 /> Roll back</Button>
            <Button size="sm" variant="primary" onClick={() => txAction("COMMIT")}><Check /> Commit</Button>
          </div>
        )}
        <label className="query__limit">
          <span className="faint">Limit</span>
          <select className="select" value={maxRows} onChange={(e) => { const v = Number(e.target.value); setMaxRows(v); updateTab(tab.id, { state: { ...(tab.state ?? {}), maxRows: v } }); }} aria-label="Row limit">
            {ROW_LIMITS.map((n) => <option key={n} value={n}>{n.toLocaleString()}</option>)}
          </select>
        </label>
      </div>
      <HeatBar running={running} startedAt={run.startedAt} finalMs={run.ms} status={run.status} />
      <div className="query__editor" style={{ flexBasis: `${split * 100}%` }}>
        <SqlEditor
          value={sql}
          onChange={setSql}
          dialect={drv?.dialect}
          driverId={conn.driver}
          catalog={catalog.data}
          defaultSchema={schema}
          onRun={(m) => execute(m)}
          onExplain={() => explain(false)}
          onSave={() => setSaveOpen(true)}
          onFormat={format}
          marks={marks}
          errorLine={errorLine}
          handleRef={editor}
        />
      </div>
      <div className="query__split" role="separator" aria-orientation="horizontal" onPointerDown={onSplitDown} onPointerMove={onSplitMove} onPointerUp={onSplitUp} />
      <div className="query__results">
        <Results run={run} conn={conn} />
      </div>

      <Dialog
        open={!!run.confirm}
        onOpenChange={(o) => !o && setRun(tab.id, (r) => ({ ...r, confirm: undefined }))}
        title={run.confirm?.environment === "production" ? "Run on production?" : "Run destructive statements?"}
        description={run.confirm?.environment === "production" ? `${conn.name} is a production database. Review what will run.` : "These statements permanently remove or overwrite data."}
        width="wide"
        footer={
          <>
            <Button onClick={() => setRun(tab.id, (r) => ({ ...r, confirm: undefined }))}>Cancel</Button>
            <Button variant="danger" disabled={!!needsTyping && typed !== conn.name} onClick={() => execute(lastRun.current?.mode === "statement" ? "statement" : "all", true)}>
              <ShieldAlert /> Run {run.confirm?.statements.length} statement{run.confirm?.statements.length === 1 ? "" : "s"}
            </Button>
          </>
        }
      >
        <div className="confirmlist">
          {run.confirm?.statements.map((s) => (
            <div key={s.index} className={`confirmlist__item confirmlist__item--${s.danger.level}`}>
              <div className="confirmlist__head">
                <AlertTriangle />
                <span>{s.danger.reason}</span>
                <span className="spacer" />
                <span className="faint mono">line {s.line + run.lineOffset}</span>
              </div>
              <pre className="confirmlist__sql">{s.sql}</pre>
            </div>
          ))}
          {needsTyping && (
            <Field label={<>Type <b className="mono">{conn.name}</b> to confirm</>}>
              <input className="input" value={typed} onChange={(e) => setTyped(e.target.value)} autoFocus />
            </Field>
          )}
          {!needsTyping && run.confirm?.environment === "production" && (
            <Alert kind="warn">Rowsmith records who ran these statements in the audit log.</Alert>
          )}
        </div>
      </Dialog>

      {saveOpen && <SaveQueryDialog tab={tab} conn={conn} sql={sql} onClose={() => setSaveOpen(false)} />}
    </div>
  );
}

function SaveQueryDialog({ tab, conn, sql, onClose }: { tab: Tab; conn: Connection; sql: string; onClose(): void }) {
  const qc = useQueryClient();
  const updateTab = useWorkspace((s) => s.updateTab);
  const existing = useQuery({ queryKey: ["saved"], queryFn: () => get<SavedQuery[]>("saved-queries") });
  const current = existing.data?.find((q) => q.id === tab.savedQueryId);
  const [name, setName] = useState(current?.name ?? (tab.title.startsWith("Query ") ? "" : tab.title));
  const [description, setDescription] = useState(current?.description ?? "");
  const [visibility, setVisibility] = useState<"private" | "team">(current?.visibility ?? "private");
  const [tags, setTags] = useState((current?.tags ?? []).join(", "));
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (current) {
      setName(current.name);
      setDescription(current.description);
      setVisibility(current.visibility);
      setTags(current.tags.join(", "));
    }
  }, [current?.id]); // eslint-disable-line react-hooks/exhaustive-deps

  const save = async (asNew: boolean) => {
    setBusy(true);
    try {
      const body = { connectionId: conn.id, database: tab.database ?? "", name, description, body: sql, visibility, tags: tags.split(",").map((t) => t.trim()).filter(Boolean) };
      const q = current && !asNew ? await put<SavedQuery>(`saved-queries/${current.id}`, body) : await post<SavedQuery>("saved-queries", body);
      updateTab(tab.id, { savedQueryId: q.id, title: q.name });
      qc.invalidateQueries({ queryKey: ["saved"] });
      toast.success(current && !asNew ? "Saved query updated" : "Query saved", visibility === "team" ? "Visible to your team" : "Only you can see it");
      onClose();
    } catch (e) {
      toast.error("Could not save", (e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} title={current ? "Update saved query" : "Save query"}
      footer={
        <>
          {current && <Button onClick={() => save(true)} disabled={!name.trim()}>Save as new</Button>}
          <span className="spacer" />
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} disabled={!name.trim()} onClick={() => save(false)}>{current ? "Update" : "Save"}</Button>
        </>
      }>
      <div className="col gap-4">
        <Field label="Name" required><input className="input" value={name} onChange={(e) => setName(e.target.value)} autoFocus placeholder="Daily revenue by store" /></Field>
        <Field label="Description"><textarea className="textarea" rows={2} value={description} onChange={(e) => setDescription(e.target.value)} placeholder="What it answers, and when to use it" /></Field>
        <Field label="Tags" help="Comma separated"><input className="input" value={tags} onChange={(e) => setTags(e.target.value)} placeholder="finance, weekly" /></Field>
        <Field label="Who can see it">
          <div className="segmented">
            <button aria-pressed={visibility === "private"} onClick={() => setVisibility("private")}>Only me</button>
            <button aria-pressed={visibility === "team"} onClick={() => setVisibility("team")}>My team</button>
          </div>
        </Field>
      </div>
    </Dialog>
  );
}


