import { useEffect, useMemo, useRef, useState } from "react";
import { Braces, CheckCircle2, Database, Download, FileSpreadsheet, FileText, Rows3, SquareStack, TableProperties } from "lucide-react";
import type { Connection } from "../../lib/types";
import { useDriver, useObjects } from "../../lib/queries";
import { ApiError } from "../../lib/api";
import { bytes, duration, int } from "../../lib/format";
import { download, transferStream } from "../../lib/transfer";
import { Alert, Button, Dialog, Field } from "../../components/ui";
import { keep, remember, type ExportSource } from "./store";
import "./transfer.css";

type Format = "csv" | "xlsx" | "json" | "ndjson" | "sql" | "tsv";

const FORMATS: { id: Format; name: string; hint: string; icon: typeof FileText }[] = [
  { id: "csv", name: "CSV", hint: "Spreadsheets and data tools", icon: FileText },
  { id: "xlsx", name: "Excel", hint: "A workbook with one sheet", icon: FileSpreadsheet },
  { id: "json", name: "JSON", hint: "An array of objects", icon: Braces },
  { id: "ndjson", name: "NDJSON", hint: "One object per line", icon: Rows3 },
  { id: "sql", name: "SQL", hint: "INSERT statements", icon: Database },
  { id: "tsv", name: "TSV", hint: "Tab-separated text", icon: TableProperties },
];

interface Prefs {
  format: Format;
  header: boolean;
  delimiter: string;
  nullText: string;
  bom: boolean;
  batchRows: number;
  gzip: boolean;
  structure: boolean;
  data: boolean;
  dropFirst: boolean;
}

const defaults: Prefs = { format: "csv", header: true, delimiter: ",", nullText: "", bom: false, batchRows: 100, gzip: false, structure: true, data: true, dropFirst: false };

type Phase =
  | { kind: "options" }
  | { kind: "running"; rows: number; bytes: number; object?: string; started: number }
  | { kind: "done"; rows: number; ms: number; file: { name: string; size: number; url: string }; warnings?: string[] }
  | { kind: "error"; message: string };

export function ExportDialog({ conn, source, onClose }: { conn: Connection; source: ExportSource; onClose(): void }) {
  const drv = useDriver(conn.driver);
  const docs = !!drv?.caps.documents;
  const [p, setP] = useState<Prefs>(() => remember("export", defaults));
  const set = (patch: Partial<Prefs>) => setP((x) => ({ ...x, ...patch }));
  const dump = source.kind === "dump";
  const formats = FORMATS.filter((f) => !(docs && f.id === "sql"));
  const format: Format = dump ? "sql" : formats.some((f) => f.id === p.format) ? p.format : "csv";
  const baseName = source.kind === "table" ? source.ref.name : source.kind === "dump" ? [conn.name, source.database, source.schema].filter(Boolean).join("-") : "query";
  const [fileName, setFileName] = useState(() => baseName.replace(/[^\p{L}\p{N}._-]+/gu, "_"));
  const [target, setTarget] = useState(source.kind === "table" ? source.ref.name : "exported_rows");
  const [phase, setPhase] = useState<Phase>({ kind: "options" });
  const abort = useRef<AbortController | null>(null);
  const [, setTick] = useState(0); // re-render for the elapsed time

  // Dump: choose objects.
  const objects = useObjects(conn.id, dump ? source.database : undefined, dump ? source.schema : undefined, dump);
  const dumpable = useMemo(() => (objects.data ?? []).filter((o) => ["table", "partitioned_table", "view", "materialized_view"].includes(o.kind) && !o.extension), [objects.data]);
  const [picked, setPicked] = useState<Set<string> | null>(null); // null = everything
  const [pickFilter, setPickFilter] = useState("");

  useEffect(() => {
    if (phase.kind !== "running") return;
    const t = setInterval(() => setTick((n) => n + 1), 500);
    return () => clearInterval(t);
  }, [phase.kind]);
  useEffect(() => () => abort.current?.abort(), []);

  const title = source.kind === "table" ? `Export ${source.ref.name}` : source.kind === "dump" ? `SQL dump${source.database ? ` of ${source.database}` : ""}${source.schema ? `.${source.schema}` : ""}` : "Export query results";
  const description =
    source.kind === "table"
      ? source.filtered ? "Every row that matches the grid's filters and sort." : `Every row${source.rows ? `, about ${int(source.rows)}` : ""}.`
      : source.kind === "query" ? "Runs the statement again and writes every row it returns, not only the ones on screen." : "A script that recreates the tables, their rows, keys, views, routines and triggers.";

  const run = async () => {
    keep("export", { ...p, format: dump ? p.format : format });
    const options: Record<string, unknown> = { format, header: p.header, batchRows: p.batchRows };
    if (format === "csv") Object.assign(options, { delimiter: p.delimiter === "tab" ? "\t" : p.delimiter, nullText: p.nullText, bom: p.bom });
    if (format === "tsv") Object.assign(options, { nullText: p.nullText });
    if (format === "sql" && !dump) options.table = { name: target.trim() || baseName };
    const body: Record<string, unknown> = { source: source.kind, options, gzip: p.gzip, fileName };
    if (source.kind === "table") Object.assign(body, { ref: source.ref, browse: source.browse });
    if (source.kind === "query") Object.assign(body, { sql: source.sql, database: source.database, schema: source.schema });
    if (source.kind === "dump") {
      Object.assign(body, { database: source.database, schema: source.schema, structure: p.structure, data: p.data, dropFirst: p.dropFirst });
      if (picked) body.objects = [...picked].map((name) => ({ name }));
    }
    const ac = new AbortController();
    abort.current = ac;
    setPhase({ kind: "running", rows: 0, bytes: 0, started: Date.now() });
    try {
      for await (const ev of transferStream(`c/${conn.id}/export`, body, ac.signal)) {
        if (ev.t === "progress") setPhase((ph) => (ph.kind === "running" ? { ...ph, rows: ev.rows ?? ph.rows, bytes: ev.bytes ?? ph.bytes, object: ev.object || ph.object } : ph));
        else if (ev.t === "error") return setPhase({ kind: "error", message: ev.message });
        else if (ev.t === "done" && ev.file) {
          setPhase({ kind: "done", rows: ev.rows ?? 0, ms: ev.ms, file: ev.file, warnings: ev.warnings });
          download(ev.file.url, ev.file.name);
          return;
        }
      }
      setPhase({ kind: "error", message: "The export stopped before it finished." });
    } catch (e) {
      if ((e as Error).name === "AbortError") return setPhase({ kind: "options" });
      setPhase({ kind: "error", message: e instanceof ApiError ? e.message : String(e) });
    }
  };

  const ext = (dump ? "sql" : format) + (p.gzip ? ".gz" : "");
  const busy = phase.kind === "running";
  const noParts = dump && !p.structure && !p.data;

  const footer =
    phase.kind === "done" ? (
      <>
        <Button onClick={() => download(phase.file.url, phase.file.name)}><Download /> Download again</Button>
        <Button variant="primary" onClick={onClose}>Done</Button>
      </>
    ) : phase.kind === "running" ? (
      <Button onClick={() => abort.current?.abort()}>Stop</Button>
    ) : (
      <>
        <Button onClick={onClose}>Cancel</Button>
        {phase.kind === "error" && <Button onClick={() => setPhase({ kind: "options" })}>Back</Button>}
        <Button variant="primary" onClick={run} disabled={noParts || (dump && picked?.size === 0)}><Download /> {phase.kind === "error" ? "Try again" : "Export"}</Button>
      </>
    );

  return (
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()} title={title} description={description} width="wide" dismissable={!busy} footer={footer}>
      {phase.kind === "running" || phase.kind === "done" ? (
        <TransferMeter
          done={phase.kind === "done"}
          rows={phase.rows}
          unit={docs ? "documents" : "rows"}
          detail={phase.kind === "running"
            ? [bytes(phase.bytes) || "0 B", phase.object && `writing ${phase.object}`, duration(Date.now() - phase.started)].filter(Boolean).join(" · ")
            : `${phase.file.name} · ${bytes(phase.file.size)} · ${duration(phase.ms)}`}
          fraction={phase.kind === "running" && source.kind === "table" && source.rows && !source.filtered ? Math.min(1, phase.rows / source.rows) : undefined}
          warnings={phase.kind === "done" ? phase.warnings : undefined}
        />
      ) : (
        <div className="xfer">
          {phase.kind === "error" && <Alert kind="danger" title="The export failed">{phase.message}</Alert>}
          {!dump ? (
            <div className="xfer-formats" role="radiogroup" aria-label="File format">
              {formats.map((f) => (
                <button key={f.id} role="radio" aria-checked={format === f.id} className="xfer-format" onClick={() => set({ format: f.id })}>
                  <f.icon className="xfer-format__icon" />
                  <span className="xfer-format__name">{f.name}</span>
                  <span className="xfer-format__hint">{f.hint}</span>
                </button>
              ))}
            </div>
          ) : (
            <div className="xfer-formats xfer-formats--parts" role="group" aria-label="What to include">
              <PartTile on={p.structure} onToggle={() => set({ structure: !p.structure })} icon={SquareStack} name="Structure" hint="Tables, keys, views, routines, triggers" />
              <PartTile on={p.data} onToggle={() => set({ data: !p.data })} icon={Rows3} name="Data" hint={docs ? "Every document" : "Every row, as INSERT statements"} />
            </div>
          )}

          <div className="xfer-options">
            {(format === "csv" || format === "tsv" || format === "xlsx") && (
              <label className="check"><input type="checkbox" checked={p.header} onChange={(e) => set({ header: e.target.checked })} /> Column names in the first row</label>
            )}
            {format === "csv" && (
              <>
                <Field label="Delimiter" group>
                  <div className="segmented">
                    {[[",", "Comma"], [";", "Semicolon"], ["tab", "Tab"], ["|", "Pipe"]].map(([v, l]) => (
                      <button key={v} aria-pressed={p.delimiter === v} onClick={() => set({ delimiter: v })}>{l}</button>
                    ))}
                  </div>
                </Field>
                <label className="check"><input type="checkbox" checked={p.bom} onChange={(e) => set({ bom: e.target.checked })} /> Mark as UTF-8 for Excel <span className="faint">(byte order mark)</span></label>
              </>
            )}
            {(format === "csv" || format === "tsv") && (
              <Field label="Write NULL as" help="Empty text and NULL look the same in an empty field; a marker keeps them apart." group>
                <div className="segmented">
                  {[["", "Empty"], ["\\N", "\\N"], ["NULL", "NULL"]].map(([v, l]) => (
                    <button key={l} aria-pressed={p.nullText === v} onClick={() => set({ nullText: v })}>{l}</button>
                  ))}
                </div>
              </Field>
            )}
            {format === "sql" && !dump && (
              <div className="xfer-grid">
                <Field label="INSERT into table"><input className="input input--mono" value={target} onChange={(e) => setTarget(e.target.value)} spellCheck={false} /></Field>
                <Field label="Rows per statement"><input className="input" type="number" min={1} max={1000} value={p.batchRows} onChange={(e) => set({ batchRows: Math.max(1, Math.min(1000, Number(e.target.value) || 100)) })} /></Field>
              </div>
            )}
            {dump && (
              <>
                <label className="check"><input type="checkbox" checked={p.dropFirst} disabled={!p.structure} onChange={(e) => set({ dropFirst: e.target.checked })} /> Drop existing tables and views before creating them</label>
                <DumpPicker items={dumpable} loading={objects.isLoading} picked={picked} setPicked={setPicked} filter={pickFilter} setFilter={setPickFilter} />
              </>
            )}
            <div className="xfer-grid">
              <Field label="File name" htmlFor="export-file-name">
                <div className="xfer-filename">
                  <input id="export-file-name" className="input" value={fileName} onChange={(e) => setFileName(e.target.value)} spellCheck={false} />
                  <span className="xfer-filename__ext mono">.{ext}</span>
                </div>
              </Field>
              <div className="xfer-gzip">
                <label className="check"><input type="checkbox" checked={p.gzip} onChange={(e) => set({ gzip: e.target.checked })} /> Compress (gzip)</label>
              </div>
            </div>
            <p className="xfer-note faint">The file is prepared on the server, encrypted until you download it, and deleted after 15 minutes.</p>
          </div>
        </div>
      )}
    </Dialog>
  );
}

function PartTile({ on, onToggle, icon: Icon, name, hint }: { on: boolean; onToggle(): void; icon: typeof FileText; name: string; hint: string }) {
  return (
    <button role="checkbox" aria-checked={on} className="xfer-format" onClick={onToggle}>
      <Icon className="xfer-format__icon" />
      <span className="xfer-format__name">{name}</span>
      <span className="xfer-format__hint">{hint}</span>
      <CheckCircle2 className="xfer-format__check" />
    </button>
  );
}

function DumpPicker({ items, loading, picked, setPicked, filter, setFilter }: {
  items: { name: string; kind: string; rows?: number }[]; loading: boolean; picked: Set<string> | null; setPicked(s: Set<string> | null): void; filter: string; setFilter(s: string): void;
}) {
  const [open, setOpen] = useState(false);
  const shown = items.filter((o) => o.name.toLowerCase().includes(filter.toLowerCase()));
  const count = picked ? picked.size : items.length;
  return (
    <div className="xfer-pick">
      <button className="xfer-pick__toggle" onClick={() => setOpen(!open)} aria-expanded={open}>
        <span>{picked ? `${int(count)} of ${int(items.length)} tables and views` : `All ${loading ? "" : int(items.length) + " "}tables and views`}</span>
        <span className="faint">{open ? "Done" : "Choose…"}</span>
      </button>
      {open && (
        <div className="xfer-pick__panel">
          <div className="row gap-3">
            <input className="input grow" placeholder="Filter" value={filter} onChange={(e) => setFilter(e.target.value)} />
            <Button size="sm" variant="ghost" onClick={() => setPicked(null)}>All</Button>
            <Button size="sm" variant="ghost" onClick={() => setPicked(new Set())}>None</Button>
          </div>
          <div className="xfer-pick__list">
            {shown.map((o) => {
              const on = picked ? picked.has(o.name) : true;
              return (
                <label key={o.name} className="check xfer-pick__item">
                  <input type="checkbox" checked={on} onChange={() => {
                    const next = new Set(picked ?? items.map((i) => i.name));
                    if (on) next.delete(o.name);
                    else next.add(o.name);
                    setPicked(next.size === items.length ? null : next);
                  }} />
                  <span className="mono truncate">{o.name}</span>
                  <span className="faint">{o.kind.replace("_", " ")}</span>
                </label>
              );
            })}
          </div>
        </div>
      )}
    </div>
  );
}

/** The live counter shown while a transfer runs and after it finishes. */
export function TransferMeter({ done, rows, unit, detail, fraction, warnings, failed }: {
  done: boolean; rows: number; unit: string; detail: string; fraction?: number; warnings?: string[]; failed?: boolean;
}) {
  return (
    <div className={`xfer-meter ${done ? "is-done" : ""} ${failed ? "is-failed" : ""}`} aria-live="polite">
      <div className="xfer-meter__count">
        {done && !failed && <CheckCircle2 className="xfer-meter__ok" />}
        <span className="display tnum">{int(rows)}</span>
        <span className="xfer-meter__unit">{unit}</span>
      </div>
      <div className={`xfer-bar ${fraction === undefined && !done ? "xfer-bar--busy" : ""}`}>
        <div className="xfer-bar__fill" style={{ width: done ? "100%" : fraction !== undefined ? `${Math.round(fraction * 100)}%` : undefined }} />
      </div>
      <div className="xfer-meter__detail faint tnum">{detail}</div>
      {warnings && warnings.length > 0 && (
        <Alert kind="warn" title="Finished with warnings">
          <ul className="xfer-warnings">{warnings.map((w, i) => <li key={i}>{w}</li>)}</ul>
        </Alert>
      )}
    </div>
  );
}
