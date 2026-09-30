import { useEffect, useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { FileUp, Table2, TriangleAlert, Upload as UploadIcon } from "lucide-react";
import type { Cell, Column, Connection, ObjectRef } from "../../lib/types";
import { ApiError, post } from "../../lib/api";
import { useDescribe, useDriver, useObjects } from "../../lib/queries";
import { bytes, cellText, duration, int } from "../../lib/format";
import { transferStream, uploadFile, type Upload } from "../../lib/transfer";
import { Alert, Button, Dialog, Field, Spinner } from "../../components/ui";
import { openObject } from "../workspace/actions";
import { TransferMeter } from "./ExportDialog";
import { keep, remember, type ImportTarget } from "./store";
import "./transfer.css";

const ACCEPT = ".csv,.tsv,.txt,.json,.ndjson,.jsonl,.xlsx,.sql,.js,.gz";
const FORMAT_NAMES: Record<string, string> = { csv: "CSV", tsv: "TSV", json: "JSON", ndjson: "NDJSON", xlsx: "Excel", sql: "SQL script" };

interface ReadOpts {
  header: boolean;
  delimiter: string;
  nullText: string;
  emptyAsNull: boolean;
}
const readDefaults: ReadOpts = { header: true, delimiter: "", nullText: "", emptyAsNull: false };

interface DataPreview { format: string; columns: string[]; rows: Cell[][]; needsConfirm: boolean }
interface SqlPreview { format: "sql"; statements: number; kinds: Record<string, number>; destructive: number; sample: { line: number; kind: string; sql: string }[]; needsConfirm: boolean }

type Phase =
  | { kind: "pick" }
  | { kind: "uploading"; loaded: number; total: number; name: string }
  | { kind: "setup"; upload: Upload }
  | { kind: "running"; upload: Upload; rows: number; bytes: number; statements?: number; total?: number; failed?: number; started: number }
  | { kind: "done"; upload: Upload; rows: number; ms: number; warnings?: string[]; statements?: number; succeeded?: number; failures?: { line: number; message: string; sql: string }[] }
  | { kind: "error"; upload?: Upload; message: string };

const norm = (s: string) => s.toLowerCase().replace(/[^\p{L}\p{N}]+/gu, "");

export function ImportDialog({ conn, target, onClose }: { conn: Connection; target: ImportTarget; onClose(): void }) {
  const qc = useQueryClient();
  const drv = useDriver(conn.driver);
  const docs = !!drv?.caps.documents;
  const [phase, setPhase] = useState<Phase>({ kind: "pick" });
  const [format, setFormat] = useState("csv");
  const [ro, setRo] = useState<ReadOpts>(() => remember("import", readDefaults));
  const [preview, setPreview] = useState<DataPreview | SqlPreview | null>(null);
  const [previewErr, setPreviewErr] = useState("");
  const [tableRef, setTableRef] = useState<ObjectRef | undefined>(target.kind === "table" ? target.ref : undefined);
  const [mapping, setMapping] = useState<Record<string, string>>({}); // table column -> file column ("" = skip)
  const [empty, setEmpty] = useState(false);
  const [stopOnError, setStopOnError] = useState(true);
  const [confirmed, setConfirmed] = useState(false);
  const [drag, setDrag] = useState(false);
  const abort = useRef<AbortController | null>(null);
  const [, setTick] = useState(0);

  const scopeDb = target.kind === "sql" ? target.database : target.ref?.database ?? target.database;
  const scopeSchema = target.kind === "sql" ? target.schema : target.ref?.schema ?? target.schema;
  const objects = useObjects(conn.id, scopeDb, scopeSchema, target.kind === "table" && !target.ref);
  const tables = (objects.data ?? []).filter((o) => ["table", "partitioned_table", "collection"].includes(o.kind) && !o.extension);
  const described = useDescribe(conn.id, format !== "sql" ? tableRef : undefined);
  const columns: Column[] = useMemo(() => (described.data?.columns ?? []).filter((c) => !c.generated && c.name !== "…"), [described.data]);

  const isSql = format === "sql";
  const upload = phase.kind === "setup" || phase.kind === "running" || phase.kind === "done" ? phase.upload : phase.kind === "error" ? phase.upload : undefined;

  useEffect(() => {
    if (phase.kind !== "running") return;
    const t = setInterval(() => setTick((n) => n + 1), 500);
    return () => clearInterval(t);
  }, [phase.kind]);
  useEffect(() => () => abort.current?.abort(), []);

  // Preview whenever the file or reading options change.
  useEffect(() => {
    if (phase.kind !== "setup") return;
    let live = true;
    setPreviewErr("");
    const body = isSql
      ? { upload: phase.upload.id, database: scopeDb, schema: scopeSchema, options: { format: "sql" } }
      : { upload: phase.upload.id, options: { format, header: ro.header, delimiter: ro.delimiter } };
    post<DataPreview | SqlPreview>(`c/${conn.id}/import/preview`, body)
      .then((r) => live && setPreview(r))
      .catch((e) => live && setPreviewErr(e instanceof ApiError ? e.message : String(e)));
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [phase.kind === "setup" ? phase.upload.id : "", format, ro.header, ro.delimiter]);

  // Match table columns to file columns by name.
  const fileCols = preview && preview.format !== "sql" ? (preview as DataPreview).columns : [];
  useEffect(() => {
    if (!fileCols.length) return;
    const byNorm = new Map(fileCols.map((c) => [norm(c), c]));
    const next: Record<string, string> = {};
    if (docs && columns.length === 0) for (const c of fileCols) next[c] = c;
    for (const c of columns) next[c.name] = byNorm.get(norm(c.name)) ?? "";
    setMapping(next);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileCols.join("\u0000"), columns.map((c) => c.name).join("\u0000")]);

  const pick = async (file: File) => {
    const ac = new AbortController();
    abort.current = ac;
    setPreview(null);
    setPhase({ kind: "uploading", loaded: 0, total: file.size, name: file.name });
    try {
      const up = await uploadFile(file, (loaded, total) => setPhase({ kind: "uploading", loaded, total, name: file.name }), ac.signal);
      const f = target.kind === "sql" ? "sql" : up.format;
      setFormat(f === "js" ? "sql" : f);
      setPhase({ kind: "setup", upload: up });
    } catch (e) {
      if ((e as Error).name === "AbortError") return setPhase({ kind: "pick" });
      setPhase({ kind: "error", message: e instanceof ApiError ? e.message : String(e) });
    }
  };

  const mapped = Object.entries(mapping).filter(([, src]) => src);
  const needsConfirm = !!preview?.needsConfirm;
  const unmappedRequired = columns.filter((c) => !c.nullable && !c.default && !c.autoIncrement && !mapping[c.name]);

  const run = async () => {
    if (!upload) return;
    keep("import", ro);
    const body: Record<string, unknown> = isSql
      ? { upload: upload.id, database: scopeDb, schema: scopeSchema, options: { format: "sql" }, confirm: confirmed, stopOnError }
      : {
          upload: upload.id, ref: tableRef, empty, confirm: confirmed,
          options: { format, header: ro.header, delimiter: ro.delimiter, nullText: ro.nullText, emptyAsNull: ro.emptyAsNull },
          mapping: mapped.map(([column, source]) => ({ column, source })),
        };
    const ac = new AbortController();
    abort.current = ac;
    setPhase({ kind: "running", upload, rows: 0, bytes: 0, started: Date.now() });
    try {
      for await (const ev of transferStream(`c/${conn.id}/import`, body, ac.signal)) {
        if (ev.t === "progress") setPhase((ph) => (ph.kind === "running" ? { ...ph, rows: ev.rows ?? ph.rows, bytes: ev.bytes ?? ph.bytes, statements: ev.statements, total: ev.total, failed: ev.failed } : ph));
        else if (ev.t === "error") return setPhase({ kind: "error", upload, message: ev.message });
        else if (ev.t === "done") {
          setPhase({ kind: "done", upload, rows: ev.rows ?? 0, ms: ev.ms, warnings: ev.warnings, statements: ev.statements, succeeded: ev.succeeded, failures: ev.failures });
          for (const k of ["browse", "count", "objects", "describe", "dbs", "schemas", "catalog"]) qc.invalidateQueries({ queryKey: [k, conn.id] });
          return;
        }
      }
      setPhase({ kind: "error", upload, message: "The import stopped before it finished." });
    } catch (e) {
      if ((e as Error).name === "AbortError") return setPhase({ kind: "setup", upload });
      setPhase({ kind: "error", upload, message: e instanceof ApiError ? e.message : String(e) });
    }
  };

  const busy = phase.kind === "running" || phase.kind === "uploading";
  const title = target.kind === "sql" ? "Run a SQL file" : tableRef ? `Import into ${tableRef.name}` : "Import data";
  const description =
    target.kind === "sql"
      ? `Statements run one by one on ${[scopeDb, scopeSchema].filter(Boolean).join(".") || conn.name}, with the console's safety rules.`
      : docs ? "Documents are inserted in batches; if one fails, the ones already inserted are removed." : "All rows are loaded in one transaction: if any row fails, nothing is kept.";

  const canRun =
    phase.kind === "setup" && !!preview && !previewErr && (!needsConfirm || confirmed) &&
    (isSql ? true : !!tableRef && mapped.length > 0);

  const footer =
    phase.kind === "done" ? (
      <>
        {!isSql && tableRef && <Button onClick={() => { openObject(conn, tableRef, "browse"); onClose(); }}><Table2 /> Open {tableRef.name}</Button>}
        <Button variant="primary" onClick={onClose}>Done</Button>
      </>
    ) : busy ? (
      <Button onClick={() => abort.current?.abort()}>Stop</Button>
    ) : (
      <>
        <Button onClick={onClose}>Cancel</Button>
        {phase.kind === "error" && <Button onClick={() => setPhase(upload ? { kind: "setup", upload } : { kind: "pick" })}>Back</Button>}
        {phase.kind === "setup" && (
          <Button variant={needsConfirm ? "danger" : "primary"} disabled={!canRun} onClick={run}>
            <FileUp /> {isSql ? `Run ${preview && "statements" in preview ? int(preview.statements) + " statements" : "script"}` : "Import"}
          </Button>
        )}
      </>
    );

  return (
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()} title={title} description={description} width="xwide" dismissable={!busy} footer={footer}>
      <div className="xfer">
        {phase.kind === "error" && <Alert kind="danger" title={upload ? "The import failed" : "The upload failed"}>{phase.message}{!isSql && upload && !docs ? " Nothing was imported; the table is unchanged." : ""}</Alert>}

        {(phase.kind === "pick" || (phase.kind === "error" && !upload)) && (
          <label
            className={`xfer-drop ${drag ? "is-over" : ""}`}
            onDragOver={(e) => { e.preventDefault(); setDrag(true); }}
            onDragLeave={() => setDrag(false)}
            onDrop={(e) => { e.preventDefault(); setDrag(false); const f = e.dataTransfer.files?.[0]; if (f) pick(f); }}
          >
            <UploadIcon className="xfer-drop__icon" />
            <span className="xfer-drop__title">Drop a file here, or <u>choose one</u></span>
            <span className="xfer-drop__hint faint">{target.kind === "sql" ? "SQL scripts (.sql), optionally gzipped" : "CSV, TSV, JSON, NDJSON or Excel (.xlsx), optionally gzipped"}</span>
            <input type="file" accept={ACCEPT} hidden onChange={(e) => { const f = e.target.files?.[0]; if (f) pick(f); }} />
          </label>
        )}

        {phase.kind === "uploading" && (
          <TransferMeter done={false} rows={Math.round((phase.loaded / Math.max(1, phase.total)) * 100)} unit="% uploaded"
            detail={`${phase.name} · ${bytes(phase.loaded)} of ${bytes(phase.total)}`} fraction={phase.loaded / Math.max(1, phase.total)} />
        )}

        {phase.kind === "running" && (
          isSql ? (
            <TransferMeter done={false} rows={phase.statements ?? 0} unit={`of ${int(phase.total ?? 0)} statements`}
              detail={[phase.failed ? `${phase.failed} failed` : "", duration(Date.now() - phase.started)].filter(Boolean).join(" · ")}
              fraction={phase.total ? (phase.statements ?? 0) / phase.total : undefined} />
          ) : (
            <TransferMeter done={false} rows={phase.rows} unit={docs ? "documents" : "rows"}
              detail={`${bytes(phase.bytes) || "0 B"} of ${bytes(phase.upload.size)} read · ${duration(Date.now() - phase.started)}`}
              fraction={phase.upload.size ? Math.min(1, phase.bytes / phase.upload.size) : undefined} />
          )
        )}

        {phase.kind === "done" && (
          isSql ? (
            <>
              <TransferMeter done rows={phase.succeeded ?? 0} unit={`of ${int(phase.statements ?? 0)} statements ran`} failed={(phase.failures?.length ?? 0) > 0}
                detail={`${phase.upload.name} · ${duration(phase.ms)}`} warnings={phase.warnings} />
              {(phase.failures?.length ?? 0) > 0 && (
                <div className="xfer-failures">
                  {phase.failures!.map((f, i) => (
                    <div key={i} className="xfer-failure">
                      <div className="row gap-3"><TriangleAlert className="xfer-failure__icon" /><span className="mono faint">line {f.line}</span><span className="grow">{f.message}</span></div>
                      <code className="mono faint truncate">{f.sql}</code>
                    </div>
                  ))}
                </div>
              )}
            </>
          ) : (
            <TransferMeter done rows={phase.rows} unit={docs ? "documents imported" : "rows imported"} detail={`${phase.upload.name} · ${duration(phase.ms)}`} warnings={phase.warnings} />
          )
        )}

        {phase.kind === "setup" && (
          <>
            <div className="xfer-file">
              <FileUp className="xfer-file__icon" />
              <div className="grow" style={{ minWidth: 0 }}>
                <div className="truncate"><b>{phase.upload.name}</b></div>
                <div className="faint">{bytes(phase.upload.size)} · {FORMAT_NAMES[format] ?? format}</div>
              </div>
              <Button size="sm" variant="ghost" onClick={() => { setPreview(null); setPhase({ kind: "pick" }); }}>Choose another file</Button>
            </div>

            {previewErr && <Alert kind="danger" title="The file could not be read">{previewErr}</Alert>}

            {isSql ? (
              preview && "statements" in preview ? <SqlSummary p={preview} stopOnError={stopOnError} setStopOnError={setStopOnError} /> : !previewErr && <Spinner />
            ) : (
              <div className="xfer-setup">
                <div className="xfer-setup__side">
                  <Field label="Read as">
                    <select className="select" value={format} onChange={(e) => setFormat(e.target.value)}>
                      {["csv", "tsv", "json", "ndjson", "xlsx"].map((f) => <option key={f} value={f}>{FORMAT_NAMES[f]}</option>)}
                    </select>
                  </Field>
                  {(format === "csv" || format === "tsv" || format === "xlsx") && (
                    <label className="check"><input type="checkbox" checked={ro.header} onChange={(e) => setRo({ ...ro, header: e.target.checked })} /> First row has column names</label>
                  )}
                  {format === "csv" && (
                    <Field label="Delimiter">
                      <select className="select" value={ro.delimiter} onChange={(e) => setRo({ ...ro, delimiter: e.target.value })}>
                        <option value="">Detect</option><option value=",">Comma</option><option value=";">Semicolon</option><option value="\t">Tab</option><option value="|">Pipe</option>
                      </select>
                    </Field>
                  )}
                  <Field label="Text that means NULL" help="Leave empty if the file has no marker. Empty fields are NULL except in text columns.">
                    <input className="input input--mono" value={ro.nullText} placeholder="e.g. \N or NULL" onChange={(e) => setRo({ ...ro, nullText: e.target.value })} />
                  </Field>
                  <label className="check"><input type="checkbox" checked={ro.emptyAsNull} onChange={(e) => setRo({ ...ro, emptyAsNull: e.target.checked })} /> Empty fields are NULL in text columns too</label>
                  {!docs && (
                    <label className="check check--danger"><input type="checkbox" checked={empty} onChange={(e) => setEmpty(e.target.checked)} /> Delete the table's existing rows first</label>
                  )}
                </div>
                <div className="xfer-setup__main">
                  {target.kind === "table" && !target.ref && (
                    <Field label="Import into">
                      <select className="select" value={tableRef?.name ?? ""} onChange={(e) => setTableRef(e.target.value ? { database: scopeDb, schema: scopeSchema, name: e.target.value, kind: "table" } : undefined)}>
                        <option value="">Choose a {docs ? "collection" : "table"}…</option>
                        {tables.map((t) => <option key={t.name} value={t.name}>{t.name}</option>)}
                      </select>
                    </Field>
                  )}
                  {preview && "columns" in preview && tableRef && (
                    <Mapping columns={columns} fileCols={fileCols} mapping={mapping} setMapping={setMapping} docs={docs} loading={described.isLoading} />
                  )}
                  {unmappedRequired.length > 0 && (
                    <Alert kind="warn">{unmappedRequired.map((c) => c.name).join(", ")} {unmappedRequired.length === 1 ? "is" : "are"} required and not mapped; the import will fail unless the file fills {unmappedRequired.length === 1 ? "it" : "them"}.</Alert>
                  )}
                  {preview && "columns" in preview && <PreviewGrid p={preview} used={new Set(Object.values(mapping).filter(Boolean))} />}
                  {!preview && !previewErr && <Spinner />}
                </div>
              </div>
            )}

            {needsConfirm && (
              <label className="check check--danger xfer-confirm">
                <input type="checkbox" checked={confirmed} onChange={(e) => setConfirmed(e.target.checked)} />
                {conn.environment === "production" ? <>I understand this writes to <b>{conn.name}</b>, a production database.</> : "I have reviewed the statements that need confirmation."}
              </label>
            )}
          </>
        )}
      </div>
    </Dialog>
  );
}

function Mapping({ columns, fileCols, mapping, setMapping, docs, loading }: {
  columns: Column[]; fileCols: string[]; mapping: Record<string, string>; setMapping(m: Record<string, string>): void; docs: boolean; loading: boolean;
}) {
  if (loading) return <Spinner />;
  // Document stores accept any field: offer the file's own columns too.
  const targets = docs ? [...new Set([...columns.map((c) => c.name), ...fileCols])] : columns.map((c) => c.name);
  const byName = new Map(columns.map((c) => [c.name, c]));
  const matched = Object.values(mapping).filter(Boolean).length;
  return (
    <div className="xfer-map">
      <div className="xfer-map__head">
        <span className="eyebrow">Columns</span>
        <span className="faint">{matched} of {targets.length} filled from the file</span>
      </div>
      <div className="xfer-map__rows">
        {targets.map((name) => {
          const c = byName.get(name);
          return (
            <div key={name} className={`xfer-map__row ${mapping[name] ? "" : "is-skipped"}`}>
              <span className="mono xfer-map__col truncate">{name}</span>
              <span className="faint mono xfer-map__type truncate">{c ? c.type : "new field"}{c && !c.nullable && !c.default && !c.autoIncrement ? " · required" : ""}</span>
              <span className="xfer-map__arrow">←</span>
              <select className="select" value={mapping[name] ?? ""} onChange={(e) => setMapping({ ...mapping, [name]: e.target.value })}>
                <option value="">{c?.autoIncrement ? "Automatic" : c?.default ? "Column default" : "Leave empty"}</option>
                {fileCols.map((f) => <option key={f} value={f}>{f}</option>)}
              </select>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function PreviewGrid({ p, used }: { p: DataPreview; used: Set<string> }) {
  if (p.columns.length === 0) return <Alert kind="warn">The file has no rows.</Alert>;
  return (
    <div className="xfer-preview">
      <div className="eyebrow">First {p.rows.length} {p.rows.length === 1 ? "row" : "rows"} of the file</div>
      <div className="xfer-preview__scroll">
        <table className="table">
          <thead><tr>{p.columns.map((c) => <th key={c} className={used.has(c) ? "" : "is-unused"}>{c}</th>)}</tr></thead>
          <tbody>
            {p.rows.map((r, i) => (
              <tr key={i}>{p.columns.map((c, j) => {
                const v = r[j];
                return <td key={c} className={`mono ${used.has(c) ? "" : "is-unused"}`}>{v === null || v === undefined ? <span className="c-null">NULL</span> : cellText(v)}</td>;
              })}</tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

const KIND_LABELS: Record<string, string> = { read: "reads", write: "writes", ddl: "structure changes", session: "session settings", tx: "transaction control", unknown: "other" };

function SqlSummary({ p, stopOnError, setStopOnError }: { p: SqlPreview; stopOnError: boolean; setStopOnError(v: boolean): void }) {
  const kinds = Object.entries(p.kinds).sort((a, b) => b[1] - a[1]);
  return (
    <div className="xfer-sql">
      <div className="xfer-sql__counts">
        <span className="display tnum xfer-sql__total">{int(p.statements)}</span>
        <span className="faint">statements</span>
        {kinds.map(([k, n]) => <span key={k} className="badge">{int(n)} {KIND_LABELS[k] ?? k}</span>)}
        {p.destructive > 0 && <span className="badge badge--danger">{int(p.destructive)} destructive</span>}
      </div>
      <div className="xfer-sql__sample">
        {p.sample.map((s, i) => (
          <div key={i} className="xfer-sql__stmt"><span className="faint mono">L{s.line}</span><code className="mono truncate">{s.sql}</code></div>
        ))}
        {p.statements > p.sample.length && <div className="faint xfer-sql__more">…and {int(p.statements - p.sample.length)} more</div>}
      </div>
      <label className="check"><input type="checkbox" checked={stopOnError} onChange={(e) => setStopOnError(e.target.checked)} /> Stop at the first error</label>
    </div>
  );
}
