import { useEffect, useState } from "react";
import { Link, useRoute } from "wouter";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowLeft, BellRing, CalendarClock, CircleAlert, CircleCheck, CirclePause, Download, Mail, MoreHorizontal, Pause, Pencil, Play, Plus, Trash2, Webhook, Moon,
} from "lucide-react";
import { ApiError, del, patch, post } from "../../lib/api";
import { useConnections } from "../../lib/queries";
import type { Me } from "../../lib/types";
import { ago, bytes, duration, int } from "../../lib/format";
import { toast } from "../../lib/store";
import { go } from "../../lib/nav";
import { download } from "../../lib/transfer";
import { Alert, Button, Dialog, Empty, Menu, MenuContent, MenuItem, MenuSep, MenuTrigger, Spinner, Tip } from "../../components/ui";
import { describeAlert, FORMAT_LABELS, usePolicy, useRuns, useSchedule, useSchedules, type RunStatus, type Schedule, type ScheduleRun } from "./api";
import { atZone, describeCron, until } from "./timetable";
import { openEditSchedule, openNewSchedule } from "./ScheduleDialog";
import { WeekStrip } from "./WeekStrip";
import "./schedules.css";

export function Schedules({ me }: { me: Me }) {
  const [, params] = useRoute<{ id: string }>("/schedules/:id");
  if (params?.id) return <ScheduleDetail id={params.id} me={me} />;
  return <ScheduleList me={me} />;
}

const isAdmin = (me: Me) => me.user.role === "owner" || me.user.role === "admin";

function state(s: Schedule): { cls: string; label: string } {
  if (s.runningSince) return { cls: "running", label: "Running" };
  if (!s.enabled) return { cls: "paused", label: "Paused" };
  if (s.lastStatus === "failed") return { cls: "failing", label: "Failing" };
  return { cls: "active", label: "Active" };
}

const RUN_LABEL: Record<RunStatus, string> = { running: "Running", ok: "Done", alert: "Alert", quiet: "Quiet", failed: "Failed" };

function RunPill({ status }: { status: RunStatus | "" }) {
  if (!status) return <span className="faint">Not run yet</span>;
  const Icon = status === "failed" ? CircleAlert : status === "alert" ? BellRing : status === "quiet" ? Moon : CircleCheck;
  return <span className={`runpill runpill--${status}`}>{status === "running" ? <span className="spinner" /> : <Icon />}{RUN_LABEL[status]}</span>;
}

function sendsText(s: Schedule) {
  const n = s.config.delivery.emails.length;
  const parts = [];
  if (n) parts.push(`${n} ${n === 1 ? "person" : "people"}`);
  if (s.webhook.set) parts.push(s.webhook.host ?? "webhook");
  const where = parts.length ? parts.join(" + ") : "kept in Rowsmith";
  return s.config.alert.kind ? `Alert when ${describeAlert(s.config.alert)} → ${where}` : `${FORMAT_LABELS[s.config.output.format]} → ${where}`;
}

async function runNow(qc: ReturnType<typeof useQueryClient>, s: Schedule) {
  try {
    await post(`schedules/${s.id}/run`, {});
    toast.info("Running now", s.config.alert.kind ? "It alerts only if the condition is met." : "The result goes out when it finishes.");
    qc.invalidateQueries({ queryKey: ["schedules"] });
    qc.invalidateQueries({ queryKey: ["schedule", s.id] });
    qc.invalidateQueries({ queryKey: ["schedule-runs", s.id] });
  } catch (e) {
    toast.error("Could not run it", e instanceof ApiError ? e.message : String(e));
  }
}

async function setEnabled(qc: ReturnType<typeof useQueryClient>, s: Schedule, enabled: boolean) {
  try {
    const next = await patch<Schedule>(`schedules/${s.id}`, { enabled });
    qc.setQueryData(["schedule", s.id], next);
    qc.invalidateQueries({ queryKey: ["schedules"] });
    toast.success(enabled ? "Schedule resumed" : "Schedule paused", enabled && next.nextRunAt ? `Next run ${atZone(next.nextRunAt, next.timezone)}.` : undefined);
  } catch (e) {
    toast.error(enabled ? "Could not resume it" : "Could not pause it", e instanceof ApiError ? e.message : String(e));
  }
}

function ScheduleList({ me }: { me: Me }) {
  const qc = useQueryClient();
  const [all, setAll] = useState(false);
  const list = useSchedules(all);
  const policy = usePolicy();
  const conns = useConnections();
  const [removing, setRemoving] = useState<Schedule | null>(null);
  const canCreate = me.user.role !== "viewer";
  const newOne = () => openNewSchedule({ connectionId: conns.data?.[0]?.id });

  return (
    <div className="page">
      <header className="page__head">
        <div>
          <h1 className="page__title display">Schedules</h1>
          <p className="page__sub">Queries that run on their own, then send the result or raise an alert.</p>
        </div>
        <div className="row gap-3 schedpage__actions">
          {isAdmin(me) && (
            <div className="segmented" role="group" aria-label="Whose schedules">
              <button aria-pressed={!all} onClick={() => setAll(false)}>Mine</button>
              <button aria-pressed={all} onClick={() => setAll(true)}>Everyone's</button>
            </div>
          )}
          {canCreate && <Button variant="primary" onClick={newOne} disabled={!conns.data?.length}><Plus /> New schedule</Button>}
        </div>
      </header>

      {policy.data && !policy.data.email && (
        <Alert kind="warn" title="Email isn't set up">
          Schedules still run and keep their results, but nothing can be emailed. {isAdmin(me) ? <Link href="/admin/email">Add a mail server</Link> : "Ask an admin to add a mail server."}
        </Alert>
      )}

      {list.isLoading ? <Spinner large /> : !list.data?.length ? (
        <Empty icon={<CalendarClock />} title={all ? "No one has scheduled anything yet" : "Nothing scheduled yet"}
          action={canCreate && conns.data?.length ? <Button variant="primary" onClick={newOne}><Plus /> New schedule</Button> : undefined}>
          Send a report every morning, or get an alert when a query finds something. You can also schedule the query in any query tab.
        </Empty>
      ) : (
        <div className="schedlist">
          {list.data.map((s) => {
            const st = state(s);
            return (
              <article key={s.id} className={`schedrow schedrow--${st.cls}`} onClick={() => go(`/schedules/${s.id}`)}>
                <span className={`schedrow__state schedstate--${st.cls}`} title={st.label} />
                <div className="schedrow__main">
                  <h3 className="schedrow__name truncate">{s.name}</h3>
                  <div className="schedrow__meta">
                    <span>{describeCron(s.cron)}</span>
                    <span className="faint">{s.timezone.replace(/_/g, " ")}</span>
                    <span className="row gap-2"><span className={`dot dot--${s.environment}`} />{s.connectionName}{s.database ? ` · ${s.database}` : ""}</span>
                    {all && <span className="faint">by {s.ownerName}</span>}
                  </div>
                </div>
                <div className="schedrow__sends">
                  {s.config.alert.kind ? <BellRing /> : s.config.delivery.emails.length ? <Mail /> : s.webhook.set ? <Webhook /> : <CalendarClock />}
                  <span className="truncate">{sendsText(s)}</span>
                </div>
                <div className="schedrow__last">
                  <RunPill status={s.runningSince ? "running" : s.lastStatus} />
                  {s.lastRunAt > 0 && !s.runningSince && <span className="faint">{ago(s.lastRunAt)}</span>}
                </div>
                <div className="schedrow__next">
                  {s.enabled ? (s.nextRunAt ? <><span>{until(s.nextRunAt)}</span><span className="faint">{atZone(s.nextRunAt, s.timezone)}</span></> : <span className="faint">—</span>) : <span className="faint">{st.label}</span>}
                </div>
                <div className="schedrow__actions" onClick={(e) => e.stopPropagation()}>
                  <Tip label="Run now"><Button size="sm" variant="ghost" icon onClick={() => runNow(qc, s)} disabled={!!s.runningSince} aria-label="Run now"><Play /></Button></Tip>
                  <Menu>
                    <MenuTrigger asChild><Button size="sm" variant="ghost" icon aria-label="More actions"><MoreHorizontal /></Button></MenuTrigger>
                    <MenuContent align="end">
                      {s.isOwner && <MenuItem icon={<Pencil />} onSelect={() => openEditSchedule(s)}>Edit</MenuItem>}
                      {s.enabled ? <MenuItem icon={<Pause />} onSelect={() => setEnabled(qc, s, false)}>Pause</MenuItem> : <MenuItem icon={<Play />} onSelect={() => setEnabled(qc, s, true)}>Resume</MenuItem>}
                      <MenuSep />
                      <MenuItem icon={<Trash2 />} danger onSelect={() => setRemoving(s)}>Delete</MenuItem>
                    </MenuContent>
                  </Menu>
                </div>
              </article>
            );
          })}
        </div>
      )}
      <DeleteDialog schedule={removing} onClose={() => setRemoving(null)} />
    </div>
  );
}

function DeleteDialog({ schedule, onClose, after }: { schedule: Schedule | null; onClose(): void; after?(): void }) {
  const qc = useQueryClient();
  return (
    <Dialog open={!!schedule} onOpenChange={(o) => !o && onClose()} title={`Delete “${schedule?.name}”?`}
      description="It stops running, and its history and result files are deleted."
      footer={<><Button onClick={onClose}>Cancel</Button><Button variant="danger" onClick={async () => {
        try {
          await del(`schedules/${schedule!.id}`);
          qc.invalidateQueries({ queryKey: ["schedules"] });
          toast.success("Schedule deleted");
          onClose();
          after?.();
        } catch (e) {
          toast.error("Could not delete it", e instanceof ApiError ? e.message : String(e));
        }
      }}><Trash2 /> Delete</Button></>} />
  );
}

function ScheduleDetail({ id, me }: { id: string; me: Me }) {
  const qc = useQueryClient();
  const q = useSchedule(id);
  const s = q.data;
  const running = !!s?.runningSince;
  const runs = useRuns(id, running);
  const [removing, setRemoving] = useState<Schedule | null>(null);
  useEffect(() => {
    if (!running) qc.invalidateQueries({ queryKey: ["schedule-runs", id] }); // a run just finished
  }, [running, id, qc]);
  if (q.isLoading) return <div className="page"><Spinner large /></div>;
  if (!s) return <div className="page"><Empty title="Schedule not found">It may have been deleted.</Empty></div>;
  const st = state(s);
  const a = s.config.alert;
  const o = s.config.output;
  return (
    <div className="page">
      <Link href="/schedules" className="backlink"><ArrowLeft size={14} /> Schedules</Link>
      <header className="page__head schedhead">
        <div style={{ minWidth: 0 }}>
          <h1 className="page__title display schedhead__title">{s.name}</h1>
          <p className="page__sub row gap-3">
            <span className={`schedbadge schedstate--${st.cls}`}>{st.label}</span>
            <span>{describeCron(s.cron)} · {s.timezone.replace(/_/g, " ")}</span>
          </p>
        </div>
        <div className="row gap-3 schedhead__actions">
          <Button onClick={() => runNow(qc, s)} disabled={!!s.runningSince}><Play /> Run now</Button>
          {s.isOwner && <Button onClick={() => openEditSchedule(s)}><Pencil /> Edit</Button>}
          {s.enabled ? <Button onClick={() => setEnabled(qc, s, false)}><CirclePause /> Pause</Button> : <Button variant="primary" onClick={() => setEnabled(qc, s, true)}><Play /> Resume</Button>}
          <Tip label="Delete"><Button variant="ghost" icon onClick={() => setRemoving(s)} aria-label="Delete"><Trash2 /></Button></Tip>
        </div>
      </header>
      {s.pausedReason && <Alert kind={s.lastStatus === "failed" ? "danger" : "info"}>{s.pausedReason}</Alert>}
      {!s.isOwner && <Alert kind="info">This schedule belongs to {s.ownerName} and runs with their access. You can run, pause or delete it; only they can change it.</Alert>}

      <div className="scheddetail">
        <section className="card pcard scheddetail__runs">
          <h2 className="pcard__title">Runs</h2>
          <RunList schedule={s} runs={runs.data} loading={runs.isLoading} />
        </section>
        <div className="scheddetail__side">
          <section className="card pcard">
            <h2 className="pcard__title">Next</h2>
            {s.enabled && s.nextRunAt ? (
              <>
                <p className="scheddetail__next"><b>{atZone(s.nextRunAt, s.timezone, true)}</b> <span className="faint">{until(s.nextRunAt)}</span></p>
                <NextWeek schedule={s} />
              </>
            ) : <p className="muted">Paused: it will not run until it is resumed.</p>}
          </section>
          <section className="card pcard">
            <h2 className="pcard__title">Sends</h2>
            <dl className="scheddl">
              <dt>When</dt><dd>{a.kind ? <>Only when {describeAlert(a)}{a.edge ? ", once until it clears" : ""}</> : "Every run"}</dd>
              <dt>File</dt><dd>{FORMAT_LABELS[o.format]}{o.gzip ? ", gzipped" : ""}, up to {int(o.maxRows)} rows{o.attach ? ", attached" : ""}</dd>
              <dt>Email</dt><dd>{s.config.delivery.emails.length ? s.config.delivery.emails.join(", ") : <span className="faint">No one</span>}</dd>
              <dt>Webhook</dt><dd>{s.webhook.set ? <span className="mono">{s.webhook.host}</span> : <span className="faint">None</span>}</dd>
              <dt>Failures</dt><dd>{s.config.delivery.onFailure ? `Emails ${s.isOwner ? "you" : s.ownerName}` : "Not reported"}</dd>
            </dl>
          </section>
          <section className="card pcard">
            <h2 className="pcard__title">Query</h2>
            <p className="muted row gap-2 scheddetail__conn"><span className={`dot dot--${s.environment}`} /> {s.connectionName}{[s.database, s.schema].filter(Boolean).length ? ` · ${[s.database, s.schema].filter(Boolean).join(".")}` : ""}</p>
            <pre className="saved__sql mono">{s.body}</pre>
            <p className="faint scheddetail__owner">Created by {s.ownerName} {ago(s.createdAt)}{isAdmin(me) && !s.isOwner ? ` · ${s.ownerEmail}` : ""}</p>
          </section>
        </div>
      </div>
      <DeleteDialog schedule={removing} onClose={() => setRemoving(null)} after={() => go("/schedules")} />
    </div>
  );
}

function NextWeek({ schedule }: { schedule: Schedule }) {
  const week = useQuery({
    queryKey: ["schedule-week", schedule.cron, schedule.timezone],
    queryFn: () => post<{ week: number[] }>("schedules/preview", { cron: schedule.cron, timezone: schedule.timezone }),
    staleTime: 5 * 60_000,
  });
  return week.data ? <WeekStrip runs={week.data.week} timezone={schedule.timezone} /> : null;
}

function RunList({ schedule, runs, loading }: { schedule: Schedule; runs?: ScheduleRun[]; loading: boolean }) {
  const [open, setOpen] = useState<string | null>(null);
  if (loading) return <Spinner />;
  if (!runs?.length) return <p className="muted">No runs yet. The first one is {schedule.nextRunAt && schedule.enabled ? `${until(schedule.nextRunAt)}, ${atZone(schedule.nextRunAt, schedule.timezone)}` : "when you run it"}.</p>;
  return (
    <div className="runlist">
      {runs.map((r) => {
        const d = r.delivery ?? {};
        const problem = r.error || d.emailError || (d.webhook && d.webhook !== "sent" ? d.webhook : "") || (d.skipped?.length ? `Not sent to ${d.skipped.join(", ")}: outside the allowed recipients.` : "");
        const expanded = open === r.id;
        return (
          <div key={r.id} className={`runrow ${expanded ? "is-open" : ""}`}>
            <button className="runrow__head" onClick={() => setOpen(expanded ? null : r.id)} aria-expanded={expanded}>
              <span className="runrow__when"><span>{atZone(r.startedAt, schedule.timezone)}</span><span className="faint">{r.trigger === "manual" ? "run by hand" : ago(r.startedAt)}</span></span>
              <RunPill status={r.status} />
              <span className="runrow__rows">{r.status === "failed" || r.status === "running" ? "" : `${int(r.rowCount)} ${r.rowCount === 1 ? "row" : "rows"}`}</span>
              <span className="runrow__took faint">{r.finishedAt ? duration(r.finishedAt - r.startedAt) : ""}</span>
              <span className="runrow__sent">
                {d.emailed?.length ? <Tip label={`Emailed ${d.emailed.join(", ")}`}><Mail /></Tip> : null}
                {d.webhook === "sent" ? <Tip label="Sent to the webhook"><Webhook /></Tip> : null}
                {problem && r.status !== "failed" ? <Tip label={problem}><CircleAlert className="runrow__warn" /></Tip> : null}
              </span>
              <span className="runrow__file">
                {r.hasFile ? (
                  <Tip label={`${r.fileName} · ${bytes(r.fileSize)}`}>
                    <span role="button" tabIndex={0} className="btn btn--sm btn--ghost btn--icon" aria-label="Download the result"
                      onClick={(e) => { e.stopPropagation(); download(`api/schedules/${schedule.id}/runs/${r.id}/file`, r.fileName); }}
                      onKeyDown={(e) => { if (e.key === "Enter") { e.stopPropagation(); download(`api/schedules/${schedule.id}/runs/${r.id}/file`, r.fileName); } }}><Download /></span>
                  </Tip>
                ) : null}
              </span>
            </button>
            {expanded && (
              <div className="runrow__body">
                {r.error && <pre className="runrow__error">{r.error}</pre>}
                <dl className="scheddl">
                  {r.observed && <><dt>Saw</dt><dd className="mono">{r.observed}</dd></>}
                  {r.truncated && <><dt>File</dt><dd>Holds the first {int(schedule.config.output.maxRows)} rows</dd></>}
                  <dt>Email</dt><dd>{d.emailed?.length ? d.emailed.join(", ") : d.emailError ? <span className="danger-text">{d.emailError}</span> : <span className="faint">Not sent</span>}{d.attached ? " · file attached" : ""}</dd>
                  {d.skipped?.length ? <><dt>Skipped</dt><dd>{d.skipped.join(", ")} (outside the allowed recipients)</dd></> : null}
                  {d.webhook && <><dt>Webhook</dt><dd>{d.webhook === "sent" ? "Sent" : <span className="danger-text">{d.webhook}</span>}</dd></>}
                  {d.note && <><dt>Note</dt><dd>{d.note}</dd></>}
                  {r.hasFile && <><dt>Kept until</dt><dd>{new Date(r.fileExpires).toLocaleDateString()}</dd></>}
                </dl>
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}
