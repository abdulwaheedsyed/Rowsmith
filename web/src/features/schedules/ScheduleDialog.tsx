import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { create } from "zustand";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { BellRing, CalendarClock, FlaskConical, Mail, Send, Webhook, X } from "lucide-react";
import { ApiError, get, post, put } from "../../lib/api";
import { useConnections, useDatabases, useDriver, useSchemas } from "../../lib/queries";
import type { User } from "../../lib/types";
import { duration, int } from "../../lib/format";
import { toast } from "../../lib/store";
import { go } from "../../lib/nav";
import { Alert, Button, Dialog, Field, Spinner } from "../../components/ui";
import { SqlEditor } from "../query/SqlEditor";
import { defaultConfig, FORMAT_LABELS, usePolicy, type OutputFormat, type Schedule, type ScheduleConfig, type TestResult } from "./api";
import {
  atZone, DAY_SHORT, defaultTimetable, describeTimetable, fromCron, HOUR_STEPS, localZone, MINUTE_STEPS, toCron, until, WEEK_ORDER, zones,
  type Timetable,
} from "./timetable";
import { WeekStrip } from "./WeekStrip";
import "./schedules.css";

/** Where a new schedule starts from. */
export interface ScheduleSeed {
  connectionId?: string;
  database?: string;
  schema?: string;
  name?: string;
  body?: string;
}

type Req = { kind: "new"; seed: ScheduleSeed } | { kind: "edit"; schedule: Schedule };

export const useScheduleDialog = create<{ req: Req | null; set(r: Req | null): void }>((set) => ({ req: null, set: (req) => set({ req }) }));
export const openNewSchedule = (seed: ScheduleSeed) => useScheduleDialog.getState().set({ kind: "new", seed });
export const openEditSchedule = (schedule: Schedule) => useScheduleDialog.getState().set({ kind: "edit", schedule });

export function ScheduleDialogHost() {
  const req = useScheduleDialog((s) => s.req);
  const close = () => useScheduleDialog.getState().set(null);
  if (!req) return null;
  return <ScheduleDialog key={req.kind === "edit" ? req.schedule.id : "new"} req={req} onClose={close} />;
}

interface Draft {
  connectionId: string;
  database: string;
  schema: string;
  name: string;
  body: string;
  timetable: Timetable;
  timezone: string;
  config: ScheduleConfig;
  webhook: string | null; // null keeps the saved one
  enabled: boolean;
}

const PREVIEW_ROWS = [0, 5, 10, 25, 50];
const FORMATS: OutputFormat[] = ["xlsx", "csv", "json", "ndjson", "tsv"];

function initial(req: Req, fallbackConn?: string): Draft {
  if (req.kind === "edit") {
    const s = req.schedule;
    return { connectionId: s.connectionId, database: s.database, schema: s.schema, name: s.name, body: s.body, timetable: fromCron(s.cron),
      timezone: s.timezone, config: structuredClone(s.config), webhook: null, enabled: s.enabled };
  }
  const seed = req.seed;
  return { connectionId: seed.connectionId ?? fallbackConn ?? "", database: seed.database ?? "", schema: seed.schema ?? "", name: seed.name ?? "",
    body: seed.body ?? "", timetable: defaultTimetable(), timezone: localZone(), config: defaultConfig(), webhook: "", enabled: true };
}

function ScheduleDialog({ req, onClose }: { req: Req; onClose(): void }) {
  const qc = useQueryClient();
  const conns = useConnections();
  const policy = usePolicy();
  const editing = req.kind === "edit" ? req.schedule : null;
  const [d, setD] = useState<Draft>(() => initial(req, conns.data?.[0]?.id));
  const set = (p: Partial<Draft>) => setD((x) => ({ ...x, ...p }));
  const setCfg = <K extends keyof ScheduleConfig>(k: K, p: Partial<ScheduleConfig[K]>) => setD((x) => ({ ...x, config: { ...x.config, [k]: { ...x.config[k], ...p } } }));
  useEffect(() => {
    if (!d.connectionId && conns.data?.length) set({ connectionId: conns.data[0].id });
  }, [conns.data, d.connectionId]);

  const conn = conns.data?.find((c) => c.id === d.connectionId);
  const drv = useDriver(conn?.driver);
  const dbs = useDatabases(conn?.id, !!drv?.caps.databases);
  const schemas = useSchemas(conn?.id, d.database || undefined, !!drv?.caps.schemas);
  const users = useQuery({ queryKey: ["users"], queryFn: () => get<User[]>("users"), staleTime: 60_000 });

  const cron = toCron(d.timetable);
  const [preview, setPreview] = useState<{ runs: number[]; week: number[]; error?: string } | null>(null);
  useEffect(() => {
    let live = true;
    const t = setTimeout(async () => {
      try {
        const r = await post<{ runs: number[]; week: number[] }>("schedules/preview", { cron, timezone: d.timezone });
        if (live) setPreview(r);
      } catch (e) {
        if (live) setPreview({ runs: [], week: [], error: e instanceof Error ? e.message : String(e) });
      }
    }, 250);
    return () => {
      live = false;
      clearTimeout(t);
    };
  }, [cron, d.timezone]);

  const [test, setTest] = useState<{ state: "idle" } | { state: "running" } | { state: "done"; result: TestResult } | { state: "error"; message: string }>({ state: "idle" });
  const runTest = async () => {
    setTest({ state: "running" });
    try {
      const result = await post<TestResult>("schedules/test", { scheduleId: editing?.id, connectionId: d.connectionId, database: d.database, schema: d.schema, body: d.body, config: d.config });
      setTest({ state: "done", result });
    } catch (e) {
      setTest({ state: "error", message: e instanceof Error ? e.message : String(e) });
    }
  };
  const columns = test.state === "done" ? test.result.columns : [];

  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const alertOn = d.config.alert.kind !== "";
  const emails = d.config.delivery.emails;
  const hookSaved = !!editing?.webhook.set && d.webhook === null;
  const hasHook = hookSaved || !!d.webhook?.trim();
  const problem = !d.name.trim() ? "Name the schedule." : !d.body.trim() ? "Write the query to run." : preview?.error ? "Fix the timetable." : !conn ? "Choose a connection." : "";

  const save = async () => {
    setSaving(true);
    setError("");
    const body: Record<string, unknown> = { connectionId: d.connectionId, database: d.database, schema: d.schema, name: d.name.trim(), body: d.body,
      cron, timezone: d.timezone, config: d.config, enabled: d.enabled };
    if (d.webhook !== null) body.webhook = d.webhook.trim();
    try {
      const saved = editing ? await put<Schedule>(`schedules/${editing.id}`, body) : await post<Schedule>("schedules", body);
      qc.invalidateQueries({ queryKey: ["schedules"] });
      qc.setQueryData(["schedule", saved.id], saved);
      toast.success(editing ? "Schedule saved" : "Schedule created", saved.enabled && saved.nextRunAt ? `Next run ${atZone(saved.nextRunAt, saved.timezone)} (${until(saved.nextRunAt)}).` : "It is paused.");
      onClose();
      if (!editing) go(`/schedules/${saved.id}`);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  const summary = useMemo(() => {
    const who = emails.length ? `${emails.length} ${emails.length === 1 ? "person" : "people"}` : "";
    const where = [who, hasHook && "a webhook"].filter(Boolean).join(" and ");
    const what = alertOn ? "alerts" : `sends ${FORMAT_LABELS[d.config.output.format]} results`;
    return where ? `${what[0].toUpperCase() + what.slice(1)} to ${where}.` : "Keeps each result in Rowsmith; no one is notified.";
  }, [emails.length, hasHook, alertOn, d.config.output.format]);

  const footer = (
    <>
      <label className="check sched__active"><input type="checkbox" checked={d.enabled} onChange={(e) => set({ enabled: e.target.checked })} /> Active</label>
      <span className="spacer" />
      <Button onClick={onClose}>Cancel</Button>
      <Button variant="primary" onClick={save} loading={saving} disabled={!!problem} title={problem || undefined}><CalendarClock /> {editing ? "Save schedule" : "Create schedule"}</Button>
    </>
  );

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} title={editing ? `Edit “${editing.name}”` : "Schedule a query"}
      description="It runs on its own with your access, reads only, and sends the result or an alert." width="xwide" footer={footer}>
      <div className="sched">
        <div className="sched__form">
          {error && <Alert kind="danger">{error}</Alert>}

          <section className="sched__sec">
            <h3 className="sched__h">Query</h3>
            <Field label="Name" htmlFor="sched-name">
              <input id="sched-name" className="input" value={d.name} onChange={(e) => set({ name: e.target.value })} placeholder="Low stock report" maxLength={200} autoFocus={!d.name} />
            </Field>
            <div className="sched__scope">
              <Field label="Connection">
                <select className="select" value={d.connectionId} onChange={(e) => set({ connectionId: e.target.value, database: "", schema: "" })}>
                  {(conns.data ?? []).map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
                </select>
              </Field>
              {drv?.caps.databases && (
                <Field label="Database">
                  <select className="select" value={d.database} onChange={(e) => set({ database: e.target.value, schema: "" })}>
                    <option value="">Default</option>
                    {d.database && !(dbs.data ?? []).some((x) => x.name === d.database) && <option value={d.database}>{d.database}</option>}
                    {(dbs.data ?? []).map((x) => <option key={x.name} value={x.name}>{x.name}</option>)}
                  </select>
                </Field>
              )}
              {drv?.caps.schemas && (
                <Field label="Schema">
                  <select className="select" value={d.schema} onChange={(e) => set({ schema: e.target.value })}>
                    <option value="">Default</option>
                    {d.schema && !(schemas.data ?? []).some((x) => x.name === d.schema) && <option value={d.schema}>{d.schema}</option>}
                    {(schemas.data ?? []).map((x) => <option key={x.name} value={x.name}>{x.name}</option>)}
                  </select>
                </Field>
              )}
            </div>
            <div className="sched__editor">
              <SqlEditor value={d.body} onChange={(v) => set({ body: v })} dialect={drv?.dialect} driverId={conn?.driver}
                language={drv?.dialect === "mongodb" ? "mongo" : "sql"} placeholder="SELECT sku, stock FROM products WHERE stock < 5" />
            </div>
            <p className="sched__note faint">Only statements that read run here; the session is read-only. The first result set is what gets sent.</p>
          </section>

          <section className="sched__sec">
            <h3 className="sched__h">When</h3>
            <TimetablePicker value={d.timetable} onChange={(timetable) => set({ timetable })} />
            <Field label="Time zone">
              <select className="select" value={d.timezone} onChange={(e) => set({ timezone: e.target.value })}>
                {zones().map((z) => <option key={z} value={z}>{z.replace(/_/g, " ")}</option>)}
              </select>
            </Field>
          </section>

          <section className="sched__sec">
            <h3 className="sched__h">What to send</h3>
            <div className="sched__mode" role="radiogroup" aria-label="When to send">
              <button role="radio" aria-checked={!alertOn} className="sched__modeopt" onClick={() => setCfg("alert", { kind: "" })}>
                <Send /> <span><b>Every run</b><small>Send the result each time it runs</small></span>
              </button>
              <button role="radio" aria-checked={alertOn} className="sched__modeopt" onClick={() => setCfg("alert", { kind: "rows" })}>
                <BellRing /> <span><b>Only when…</b><small>Alert when a condition is met</small></span>
              </button>
            </div>
            {alertOn && (
              <div className="sched__cond">
                <select className="select" value={d.config.alert.kind} onChange={(e) => setCfg("alert", { kind: e.target.value as ScheduleConfig["alert"]["kind"] })} aria-label="Condition">
                  <option value="rows">the row count</option>
                  <option value="value">a value in the first row</option>
                  <option value="changed">the result changes</option>
                </select>
                {d.config.alert.kind === "value" && (
                  <>
                    <input className="input input--mono" value={d.config.alert.column} onChange={(e) => setCfg("alert", { column: e.target.value })} placeholder="first column" list="sched-cols" aria-label="Column" />
                    <datalist id="sched-cols">{columns.map((c) => <option key={c} value={c} />)}</datalist>
                  </>
                )}
                {d.config.alert.kind !== "changed" && (
                  <>
                    <select className="select" value={d.config.alert.op} onChange={(e) => setCfg("alert", { op: e.target.value })} aria-label="Comparison">
                      <option value=">">is more than</option>
                      <option value=">=">is at least</option>
                      <option value="<">is less than</option>
                      <option value="<=">is at most</option>
                      <option value="=">is</option>
                      <option value="!=">is not</option>
                    </select>
                    <input className="input input--mono sched__condval" value={d.config.alert.value} onChange={(e) => setCfg("alert", { value: e.target.value })} aria-label="Value" />
                  </>
                )}
                <label className="check sched__edge"><input type="checkbox" checked={d.config.alert.edge} onChange={(e) => setCfg("alert", { edge: e.target.checked })} /> Notify once when it starts, not again until it clears</label>
              </div>
            )}
            <div className="sched__file">
              <Field label="File" group>
                <div className="segmented">
                  {FORMATS.map((f) => <button key={f} aria-pressed={d.config.output.format === f} onClick={() => setCfg("output", { format: f })}>{FORMAT_LABELS[f]}</button>)}
                </div>
              </Field>
              <Field label="Rows in the file">
                <input className="input" type="number" min={1} max={1000000} value={d.config.output.maxRows} onChange={(e) => setCfg("output", { maxRows: Math.max(1, Math.min(1000000, Number(e.target.value) || 1)) })} />
              </Field>
            </div>
            <div className="sched__checks">
              <label className="check"><input type="checkbox" checked={d.config.output.attach} onChange={(e) => setCfg("output", { attach: e.target.checked })} /> Attach the file to emails <span className="faint">(up to {policy.data?.maxAttachMB ?? 10} MB)</span></label>
              <label className="check"><input type="checkbox" checked={d.config.output.gzip} onChange={(e) => setCfg("output", { gzip: e.target.checked })} /> Compress (gzip)</label>
              <label className="check">
                Show
                <select className="select select--inline" value={d.config.output.preview} onChange={(e) => setCfg("output", { preview: Number(e.target.value) })} aria-label="Rows in the message">
                  {PREVIEW_ROWS.map((n) => <option key={n} value={n}>{n === 0 ? "no" : n}</option>)}
                </select>
                rows in the message
              </label>
            </div>
          </section>

          <section className="sched__sec">
            <h3 className="sched__h">Send to</h3>
            {policy.data && !policy.data.email && (
              <Alert kind="warn">Email isn't set up yet, so nothing can be emailed. An admin can add a mail server under Administration → Email.</Alert>
            )}
            <Field label={<><Mail size={13} /> Email</>} help={policy.data ? (policy.data.anyone ? "Any address." : `Team members${policy.data.domains.length ? ` and addresses at ${policy.data.domains.join(", ")}` : " only, as your admin set it"}.`) : undefined} htmlFor="schedule-emails">
              <EmailChips id="schedule-emails" value={emails} onChange={(v) => setCfg("delivery", { emails: v })} suggestions={(users.data ?? []).map((u) => u.email)} />
            </Field>
            {policy.data?.webhooks !== false && (
              <Field label={<><Webhook size={13} /> Webhook</>} help={editing?.webhook.set && d.webhook !== null ? "Enter the new URL, or leave it empty to stop sending to a webhook." : "Slack, Microsoft Teams, Google Chat and Discord get a message; any other URL gets the result as JSON."} group={!!hookSaved}>
                {hookSaved ? (
                  <div className="sched__hook">
                    <span className="mono truncate">Saved · {editing?.webhook.host}</span>
                    <Button size="sm" onClick={() => set({ webhook: "" })}>Change</Button>
                  </div>
                ) : (
                  <input className="input input--mono" type="url" value={d.webhook ?? ""} onChange={(e) => set({ webhook: e.target.value })} placeholder="https://hooks.slack.com/services/…" spellCheck={false} autoComplete="off" />
                )}
              </Field>
            )}
            <label className="check"><input type="checkbox" checked={d.config.delivery.onFailure} onChange={(e) => setCfg("delivery", { onFailure: e.target.checked })} /> Tell me when a run fails</label>
          </section>
        </div>

        <aside className="sched__side">
          <div className="sched__card">
            <div className="sched__cardhead"><CalendarClock /> <span>{describeTimetable(d.timetable)}</span></div>
            {preview?.error ? (
              <p className="sched__err">{preview.error}</p>
            ) : preview ? (
              <>
                <WeekStrip runs={preview.week} timezone={d.timezone} />
                <ol className="sched__next">
                  {preview.runs.map((r, i) => <li key={r}><span>{atZone(r, d.timezone)}</span><span className="faint">{i === 0 ? until(r) : ""}</span></li>)}
                </ol>
              </>
            ) : <Spinner />}
            <p className="sched__summary">{summary}</p>
          </div>

          <div className="sched__card">
            <div className="sched__cardhead"><FlaskConical /> <span>Try it</span></div>
            <p className="faint sched__small">Runs the query now and checks the condition. Nothing is sent.</p>
            <Button onClick={runTest} loading={test.state === "running"} disabled={!d.body.trim() || !conn} block>Run the query</Button>
            {test.state === "error" && <p className="sched__err">{test.message}</p>}
            {test.state === "done" && <TestView r={test.result} />}
          </div>
        </aside>
      </div>
    </Dialog>
  );
}

function TestView({ r }: { r: TestResult }) {
  const cols = r.columns.slice(0, 6);
  return (
    <div className="sched__test">
      <div className="sched__testhead">
        <b>{int(r.rowCount)} {r.rowCount === 1 ? "row" : "rows"}</b>
        <span className="faint">{r.ms < 1 ? "under 1 ms" : duration(r.ms)}{r.truncated ? " · the file keeps the first rows only" : ""}</span>
      </div>
      {r.alert && (
        r.alert.error ? <p className="sched__err">{r.alert.error}</p> : (
          <p className={`sched__verdict ${r.alert.fired ? "is-fired" : ""}`}>
            {r.alert.fired ? "Would alert now" : "Would stay quiet now"}: {r.alert.condition}{r.alert.observed ? ` (saw ${r.alert.observed})` : ""}.
          </p>
        )
      )}
      {cols.length > 0 && (
        <div className="sched__testgrid">
          <table>
            <thead><tr>{cols.map((c) => <th key={c}>{c}</th>)}</tr></thead>
            <tbody>{r.rows.slice(0, 5).map((row, i) => <tr key={i}>{cols.map((_, j) => <td key={j}>{row[j]}</td>)}</tr>)}</tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function TimetablePicker({ value, onChange }: { value: Timetable; onChange(t: Timetable): void }) {
  const kinds: { id: Timetable["kind"]; label: string; make(): Timetable }[] = [
    { id: "minutes", label: "Minutes", make: () => ({ kind: "minutes", every: 15 }) },
    { id: "hourly", label: "Hourly", make: () => ({ kind: "hourly", every: 1, minute: 0 }) },
    { id: "days", label: "Days", make: () => defaultTimetable() },
    { id: "monthly", label: "Monthly", make: () => ({ kind: "monthly", day: 1, time: "08:00" }) },
    { id: "cron", label: "Custom", make: () => ({ kind: "cron", expr: toCron(value) }) },
  ];
  return (
    <div className="ttpick">
      <div className="segmented" role="group" aria-label="Repeat">
        {kinds.map((k) => <button key={k.id} aria-pressed={value.kind === k.id} onClick={() => value.kind !== k.id && onChange(k.make())}>{k.label}</button>)}
      </div>
      <div className="ttpick__row">
        {value.kind === "minutes" && (
          <label className="ttpick__inline">Every
            <select className="select select--inline" value={value.every} onChange={(e) => onChange({ ...value, every: Number(e.target.value) })}>
              {MINUTE_STEPS.map((n) => <option key={n} value={n}>{n}</option>)}
            </select> minutes
          </label>
        )}
        {value.kind === "hourly" && (
          <label className="ttpick__inline">Every
            <select className="select select--inline" value={value.every} onChange={(e) => onChange({ ...value, every: Number(e.target.value) })}>
              {HOUR_STEPS.map((n) => <option key={n} value={n}>{n === 1 ? "hour" : `${n} hours`}</option>)}
            </select>
            at minute
            <input className="input ttpick__num" type="number" min={0} max={59} value={value.minute} onChange={(e) => onChange({ ...value, minute: Math.max(0, Math.min(59, Number(e.target.value) || 0)) })} />
          </label>
        )}
        {value.kind === "days" && (
          <>
            <div className="ttpick__days" role="group" aria-label="Days">
              {WEEK_ORDER.map((day) => {
                const on = value.days.includes(day);
                return <button key={day} aria-pressed={on} onClick={() => onChange({ ...value, days: on ? value.days.filter((x) => x !== day) : [...value.days, day] })}>{DAY_SHORT[day]}</button>;
              })}
            </div>
            <div className="ttpick__quick">
              <button onClick={() => onChange({ ...value, days: [1, 2, 3, 4, 5] })}>Weekdays</button>
              <button onClick={() => onChange({ ...value, days: [0, 1, 2, 3, 4, 5, 6] })}>Every day</button>
            </div>
            <label className="ttpick__inline">at <input className="input ttpick__time" type="time" value={value.time} onChange={(e) => onChange({ ...value, time: e.target.value || "08:00" })} /></label>
          </>
        )}
        {value.kind === "monthly" && (
          <label className="ttpick__inline">On
            <select className="select select--inline" value={String(value.day)} onChange={(e) => onChange({ ...value, day: e.target.value === "L" ? "L" : Number(e.target.value) })}>
              {Array.from({ length: 28 }, (_, i) => i + 1).map((n) => <option key={n} value={n}>day {n}</option>)}
              <option value="L">the last day</option>
            </select>
            at <input className="input ttpick__time" type="time" value={value.time} onChange={(e) => onChange({ ...value, time: e.target.value || "08:00" })} />
          </label>
        )}
        {value.kind === "cron" && (
          <Field label="Cron expression" help="minute · hour · day of month · month · day of week, e.g. 30 7 * * 1-5. L means the last day of the month.">
            <input className="input input--mono" value={value.expr} onChange={(e) => onChange({ kind: "cron", expr: e.target.value })} spellCheck={false} />
          </Field>
        )}
      </div>
      {value.kind === "days" && value.days.length === 0 && <p className="sched__err">Pick at least one day.</p>}
    </div>
  );
}

function EmailChips({ id, value, onChange, suggestions }: { id?: string; value: string[]; onChange(v: string[]): void; suggestions: string[] }) {
  const [text, setText] = useState("");
  const input = useRef<HTMLInputElement>(null);
  const add = (raw: string) => {
    const parts = raw.split(/[\s,;]+/).map((s) => s.trim().toLowerCase()).filter(Boolean);
    if (!parts.length) return;
    const bad = parts.filter((p) => !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(p));
    if (bad.length) {
      toast.error("Not an email address", bad.join(", "));
      return;
    }
    onChange([...new Set([...value, ...parts])]);
    setText("");
  };
  const onKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter" || e.key === "," || e.key === ";" || (e.key === " " && text.includes("@"))) {
      e.preventDefault();
      add(text);
    } else if (e.key === "Backspace" && !text && value.length) {
      onChange(value.slice(0, -1));
    }
  };
  return (
    <div className="emailchips" onClick={() => input.current?.focus()}>
      {value.map((e) => (
        <span key={e} className="emailchip">
          {e}
          <button aria-label={`Remove ${e}`} onClick={(ev) => { ev.stopPropagation(); onChange(value.filter((x) => x !== e)); }}><X /></button>
        </span>
      ))}
      <input ref={input} id={id} className="emailchips__input" value={text} onChange={(e) => setText(e.target.value)} onKeyDown={onKey} onBlur={() => text && add(text)}
        placeholder={value.length ? "" : "name@example.com"} list="sched-people" type="email" autoComplete="off" aria-label="Add an email address" />
      <datalist id="sched-people">{suggestions.filter((s) => !value.includes(s)).map((s) => <option key={s} value={s} />)}</datalist>
    </div>
  );
}
