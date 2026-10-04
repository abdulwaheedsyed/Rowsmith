import { useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlert, CircleCheck, Send } from "lucide-react";
import { get, post, put } from "../../lib/api";
import type { Me } from "../../lib/types";
import { toast } from "../../lib/store";
import { Alert, Button, Field, Spinner } from "../../components/ui";

type Security = "starttls" | "tls" | "none";

interface MailDraft {
  host: string;
  port: string;
  security: Security;
  username: string;
  password: string;
  from: string;
}

// Common providers; each only fills in the server details.
const PRESETS: { label: string; host: string; port: number; security: Security; username?: string; hint?: string }[] = [
  { label: "Google Workspace", host: "smtp.gmail.com", port: 587, security: "starttls", hint: "Use an app password." },
  { label: "Microsoft 365", host: "smtp.office365.com", port: 587, security: "starttls", hint: "SMTP AUTH must be allowed for the mailbox." },
  { label: "Amazon SES", host: "email-smtp.us-east-1.amazonaws.com", port: 587, security: "starttls", hint: "Use your region's endpoint and SMTP credentials." },
  { label: "SendGrid", host: "smtp.sendgrid.net", port: 587, security: "starttls", username: "apikey", hint: "The password is an API key." },
  { label: "Mailgun", host: "smtp.mailgun.org", port: 587, security: "starttls" },
  { label: "Postmark", host: "smtp.postmarkapp.com", port: 587, security: "starttls", hint: "Username and password are both the server token." },
  { label: "Relay on this server", host: "host.docker.internal", port: 25, security: "none", hint: "For a local relay such as Postfix that accepts mail from the container." },
];

type Policy = "team" | "domains" | "anyone";

export function EmailSettings({ me }: { me: Me }) {
  const qc = useQueryClient();
  const settings = useQuery({ queryKey: ["settings"], queryFn: () => get<Record<string, unknown>>("settings") });
  const saved = useMemo(() => {
    const s = settings.data ?? {};
    const str = (k: string) => (typeof s[k] === "string" ? (s[k] as string) : "");
    return {
      host: str("smtp.host"), port: str("smtp.port"), security: (str("smtp.tls") || "starttls") as Security, username: str("smtp.username"),
      passwordSet: (s["smtp.password"] as { set?: boolean } | undefined)?.set === true, from: str("smtp.from"),
      domains: str("schedules.email_domains"), webhooks: str("schedules.webhooks") !== "off", privateWebhooks: str("schedules.private_webhooks") === "true",
      retention: str("schedules.retention_days") || "14",
    };
  }, [settings.data]);

  const [m, setM] = useState<MailDraft | null>(null);
  const [p, setP] = useState<{ policy: Policy; domains: string; webhooks: boolean; privateWebhooks: boolean; retention: string } | null>(null);
  useEffect(() => {
    if (!settings.data) return;
    if (!m) setM({ host: saved.host, port: saved.port, security: saved.security, username: saved.username, password: "", from: saved.from });
    if (!p) {
      const list = saved.domains.split(/[\s,;]+/).filter(Boolean);
      setP({ policy: list.includes("*") ? "anyone" : list.length ? "domains" : "team", domains: list.filter((d) => d !== "*").join(", "),
        webhooks: saved.webhooks, privateWebhooks: saved.privateWebhooks, retention: saved.retention });
    }
  }, [settings.data, saved, m, p]);
  const [hint, setHint] = useState("");
  const [testing, setTesting] = useState<{ state: "idle" | "running" } | { state: "ok"; to: string } | { state: "fail"; message: string }>({ state: "idle" });
  const [savingMail, setSavingMail] = useState(false);
  const [savingPolicy, setSavingPolicy] = useState(false);

  if (settings.isLoading || !m || !p) return <Spinner large />;

  const setMail = (x: Partial<MailDraft>) => {
    setM({ ...m, ...x });
    setTesting({ state: "idle" });
  };
  const samePlace = m.host.trim().toLowerCase() === saved.host.toLowerCase() && m.username.trim() === saved.username;
  const passwordKept = saved.passwordSet && samePlace && !m.password;
  const mailProblem = !m.host.trim() ? "Enter the mail server." : !m.from.trim() ? "Enter the sender address." :
    m.username.trim() && m.security === "none" ? "Signing in needs STARTTLS or TLS." : "";
  const mailDirty = m.host !== saved.host || m.port !== saved.port || m.security !== saved.security || m.username !== saved.username || m.from !== saved.from || !!m.password;
  const configured = !!saved.host && !!saved.from;

  const draft = () => ({ host: m.host.trim(), port: Number(m.port) || 0, security: m.security, username: m.username.trim(), password: m.password, from: m.from.trim() });
  const test = async () => {
    setTesting({ state: "running" });
    try {
      const r = await post<{ to: string }>("admin/mail/test", draft());
      setTesting({ state: "ok", to: r.to });
    } catch (e) {
      setTesting({ state: "fail", message: e instanceof Error ? e.message : String(e) });
    }
  };
  const saveMail = async () => {
    setSavingMail(true);
    try {
      await put("settings", {
        "smtp.host": m.host.trim() || null, "smtp.port": m.port.trim() || null, "smtp.tls": m.security, "smtp.username": m.username.trim() || null,
        "smtp.from": m.from.trim() || null, ...(m.password ? { "smtp.password": m.password } : {}),
      });
      await Promise.all([qc.invalidateQueries({ queryKey: ["settings"] }), qc.invalidateQueries({ queryKey: ["schedule-policy"] })]);
      setM({ ...m, password: "" });
      toast.success("Mail server saved");
    } catch (e) {
      toast.error("Could not save", e instanceof Error ? e.message : undefined);
    } finally {
      setSavingMail(false);
    }
  };
  const removeMail = async () => {
    await put("settings", { "smtp.host": null, "smtp.port": null, "smtp.tls": null, "smtp.username": null, "smtp.from": null, "smtp.password": null });
    await Promise.all([qc.invalidateQueries({ queryKey: ["settings"] }), qc.invalidateQueries({ queryKey: ["schedule-policy"] })]);
    setM({ host: "", port: "", security: "starttls", username: "", password: "", from: "" });
    toast.success("Mail server removed", "Schedules keep running; nothing is emailed.");
  };
  const savePolicy = async () => {
    setSavingPolicy(true);
    try {
      const domains = p.policy === "anyone" ? "*" : p.policy === "domains" ? p.domains : "";
      await put("settings", {
        "schedules.email_domains": domains || null, "schedules.webhooks": p.webhooks ? null : "off",
        "schedules.private_webhooks": p.privateWebhooks ? "true" : null, "schedules.retention_days": p.retention || null,
      });
      await Promise.all([qc.invalidateQueries({ queryKey: ["settings"] }), qc.invalidateQueries({ queryKey: ["schedule-policy"] })]);
      toast.success("Schedule policy saved");
    } catch (e) {
      toast.error("Could not save", e instanceof Error ? e.message : undefined);
    } finally {
      setSavingPolicy(false);
    }
  };

  return (
    <div className="pgrid mailset">
      <section className="card pcard">
        <h2 className="pcard__title">Mail server</h2>
        <p className="pcard__desc">Scheduled reports, alerts and failure notices are sent through this SMTP server.</p>
        <div className="mailset__state">
          {configured ? <CircleCheck className="ok" /> : <CircleAlert className="warn" />}
          <span>{configured ? <>Sending as <b>{saved.from}</b> through {saved.host}</> : "Not set up: schedules run, but nothing is emailed."}</span>
        </div>
        <div className="mailset__presets">
          {PRESETS.map((x) => (
            <button key={x.label} className="aiset__chip" onClick={() => { setMail({ host: x.host, port: String(x.port), security: x.security, ...(x.username ? { username: x.username } : {}) }); setHint(x.hint ?? ""); }}>{x.label}</button>
          ))}
        </div>
        {hint && <p className="faint mailset__hint">{hint}</p>}
        <div className="mailset__grid">
          <div className="mailset__host">
            <Field label="Server" required htmlFor="smtp-host"><input id="smtp-host" className="input input--mono" value={m.host} onChange={(e) => setMail({ host: e.target.value.trim() })} placeholder="smtp.example.com" spellCheck={false} autoComplete="off" /></Field>
          </div>
          <Field label="Port" htmlFor="smtp-port"><input id="smtp-port" className="input" inputMode="numeric" value={m.port} onChange={(e) => setMail({ port: e.target.value.replace(/\D/g, "") })} placeholder={m.security === "tls" ? "465" : m.security === "none" ? "25" : "587"} /></Field>
        </div>
        <Field label="Encryption" group>
          <div className="segmented">
            {([["starttls", "STARTTLS"], ["tls", "TLS"], ["none", "None"]] as const).map(([v, l]) => (
              <button key={v} aria-pressed={m.security === v} onClick={() => setMail({ security: v })}>{l}</button>
            ))}
          </div>
        </Field>
        {m.security === "none" && <Alert kind="warn">Without encryption, messages cross the network in plain text. Use it only for a relay on this machine or a trusted network.</Alert>}
        <div className="mailset__grid mailset__grid--even">
          <Field label="Username" htmlFor="smtp-user"><input id="smtp-user" className="input" value={m.username} onChange={(e) => setMail({ username: e.target.value })} placeholder="Leave empty if the server needs no sign-in" autoComplete="off" /></Field>
          <Field label="Password" htmlFor="smtp-pass" help={saved.passwordSet ? (samePlace ? "Saved; type a new one to replace it." : "The saved password is only used with the server and username it was saved for.") : undefined}>
            <input id="smtp-pass" className="input" type="password" value={m.password} onChange={(e) => setMail({ password: e.target.value })} placeholder={passwordKept ? "••••••••" : ""} autoComplete="new-password" />
          </Field>
        </div>
        <Field label="Send as" required htmlFor="smtp-from" help="The address reports come from. Many providers only accept an address they verified.">
          <input id="smtp-from" className="input" value={m.from} onChange={(e) => setMail({ from: e.target.value })} placeholder="Rowsmith Reports <reports@example.com>" autoComplete="off" />
        </Field>
        {testing.state === "ok" && <Alert kind="success" title="Test email sent">Check the inbox of {testing.to}.</Alert>}
        {testing.state === "fail" && <Alert kind="danger" title="The test failed">{testing.message}</Alert>}
        <div className="aiset__actions">
          <span className="faint aiset__problem">{mailProblem}</span>
          {configured && <Button variant="ghost" onClick={removeMail}>Remove</Button>}
          <Button onClick={test} loading={testing.state === "running"} disabled={!!mailProblem}><Send size={14} /> Send me a test</Button>
          <Button variant="primary" onClick={saveMail} loading={savingMail} disabled={!!mailProblem || !mailDirty}>Save</Button>
        </div>
        <p className="faint mailset__hint">The test goes to {me.user.email}.</p>
      </section>

      <section className="card pcard">
        <h2 className="pcard__title">Schedules</h2>
        <p className="pcard__desc">Where scheduled results may go. Schedules run with their owner's access and only read data.</p>
        <h3 className="aiset__h3">Email recipients</h3>
        <div className="mailset__radios" role="radiogroup" aria-label="Email recipients">
          {([["team", "Team members only"], ["domains", "Team members and these domains"], ["anyone", "Any address"]] as const).map(([v, l]) => (
            <label key={v} className="radio"><input type="radio" name="recipients" checked={p.policy === v} onChange={() => setP({ ...p, policy: v })} /> {l}</label>
          ))}
        </div>
        {p.policy === "domains" && (
          <Field label="Domains" help="Subdomains are included. Separate with commas.">
            <input className="input input--mono" value={p.domains} onChange={(e) => setP({ ...p, domains: e.target.value })} placeholder="example.com, partner.org" spellCheck={false} />
          </Field>
        )}
        <h3 className="aiset__h3">Webhooks</h3>
        <label className="switch"><input type="checkbox" checked={p.webhooks} onChange={(e) => setP({ ...p, webhooks: e.target.checked })} /> Let schedules post to webhooks (Slack, Teams, Google Chat, Discord or any URL)</label>
        {p.webhooks && (
          <label className="switch"><input type="checkbox" checked={p.privateWebhooks} onChange={(e) => setP({ ...p, privateWebhooks: e.target.checked })} /> Allow private network and plain http addresses</label>
        )}
        {p.webhooks && p.privateWebhooks && <Alert kind="warn">Anyone who can schedule could then make Rowsmith call services inside your network. Allow this only if you trust every member.</Alert>}
        <h3 className="aiset__h3">History</h3>
        <Field label="Keep result files for" help="Each run's file is encrypted on the server and deleted after this many days." htmlFor="retention-days">
          <div className="row gap-3"><input id="retention-days" className="input mailset__days" inputMode="numeric" value={p.retention} onChange={(e) => setP({ ...p, retention: e.target.value.replace(/\D/g, "") })} /> <span className="muted">days</span></div>
        </Field>
        <div className="aiset__actions">
          <span className="spacer" />
          <Button variant="primary" onClick={savePolicy} loading={savingPolicy}>Save</Button>
        </div>
      </section>
    </div>
  );
}
