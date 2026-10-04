import { useState } from "react";
import { Link, useLocation } from "wouter";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { Users, ScrollText, KeyRound, Settings2, LayoutDashboard, Sparkles, Mail, UserPlus, MoreHorizontal, ShieldOff, Trash2, RotateCcw, Ban, Check, Copy } from "lucide-react";
import { ApiError, del, get, patch, post, put, qs } from "../../lib/api";
import type { AuditEntry, Me, Role, User } from "../../lib/types";
import { ago, int } from "../../lib/format";
import { toast } from "../../lib/store";
import { Alert, Button, Dialog, Empty, Field, Menu, MenuContent, MenuItem, MenuSep, MenuTrigger, Spinner } from "../../components/ui";
import { AISettings } from "./AISettings";
import { EmailSettings } from "./EmailSettings";
import "./admin.css";

const SECTIONS = [
  { id: "", label: "Overview", icon: LayoutDashboard },
  { id: "users", label: "Team", icon: Users },
  { id: "audit", label: "Audit log", icon: ScrollText },
  { id: "ssh", label: "SSH hosts", icon: KeyRound },
  { id: "settings", label: "Security", icon: Settings2 },
  { id: "email", label: "Email & schedules", icon: Mail },
  { id: "ai", label: "AI assistant", icon: Sparkles },
];

export function Admin({ me }: { me: Me }) {
  const [location] = useLocation();
  const section = location.replace(/^\/admin\/?/, "").split("/")[0];
  if (me.user.role !== "owner" && me.user.role !== "admin") {
    return <div className="page"><Empty title="Administrators only">Ask an owner or administrator for access.</Empty></div>;
  }
  return (
    <div className="page">
      <header className="page__head">
        <div>
          <h1 className="page__title display">Administration</h1>
          <p className="page__sub">Team members, security, email, the AI assistant and the record of everything that happened.</p>
        </div>
      </header>
      <nav className="page__tabs" aria-label="Administration sections">
        {SECTIONS.map((s) => (
          <Link key={s.id} href={`/admin${s.id ? "/" + s.id : ""}`} className="page__tab" aria-selected={section === s.id}>
            <s.icon /> {s.label}
          </Link>
        ))}
      </nav>
      {section === "" && <Overview />}
      {section === "users" && <Team me={me} />}
      {section === "audit" && <Audit />}
      {section === "ssh" && <SSHHosts />}
      {section === "settings" && <SecuritySettings />}
      {section === "email" && <EmailSettings me={me} />}
      {section === "ai" && <AISettings />}
    </div>
  );
}

function Overview() {
  const q = useQuery({ queryKey: ["admin-overview"], queryFn: () => get<{ users: number; usersWithMfa: number; connections: number; live: { pools: number; consoles: number; tunnels: number }; version: string; keyId: string }>("admin/overview") });
  if (!q.data) return <Spinner large />;
  const d = q.data;
  const mfaPct = d.users ? Math.round((d.usersWithMfa / d.users) * 100) : 0;
  return (
    <div className="pgrid">
      <section className="card pcard">
        <h2 className="pcard__title">Team</h2>
        <div className="stats">
          <div className="stat"><span className="stat__value">{d.users}</span><span className="stat__label">people</span></div>
          <div className="stat"><span className="stat__value">{mfaPct}%</span><span className="stat__label">use two-step sign-in</span></div>
          <div className="stat"><span className="stat__value">{d.connections}</span><span className="stat__label">saved connections</span></div>
        </div>
        {mfaPct < 100 && <Alert kind="warn">Not everyone uses two-step sign-in. You can require it under Security.</Alert>}
      </section>
      <section className="card pcard">
        <h2 className="pcard__title">Right now</h2>
        <div className="stats">
          <div className="stat"><span className="stat__value">{d.live.pools}</span><span className="stat__label">open connection pools</span></div>
          <div className="stat"><span className="stat__value">{d.live.consoles}</span><span className="stat__label">query sessions</span></div>
          <div className="stat"><span className="stat__value">{d.live.tunnels}</span><span className="stat__label">SSH tunnels</span></div>
        </div>
      </section>
      <section className="card pcard">
        <h2 className="pcard__title">Encryption</h2>
        <p className="pcard__desc">Connection secrets, two-step secrets and API keys are sealed with the master key. Keep a copy of it apart from the data directory.</p>
        <div className="kv"><span>Master key fingerprint</span><code className="mono">{d.keyId}</code></div>
        <div className="kv"><span>Rotate</span><code className="mono">rowsmith rotate-key</code></div>
        <div className="kv"><span>Version</span><code className="mono">{d.version}</code></div>
        <p className="pcard__desc pcard__foot">Rowsmith™ and the Rowsmith logo are trademarks of Syed Abdul Waheed. Rowsmith is open source under the Apache License 2.0.</p>
      </section>
    </div>
  );
}

const ROLE_HELP: Record<Role, string> = {
  owner: "Everything, including managing other owners",
  admin: "Manage people, security settings and the audit log",
  member: "Create connections and use the ones shared with them",
  viewer: "Read-only access to connections shared with them",
};

function Team({ me }: { me: Me }) {
  const qc = useQueryClient();
  const users = useQuery({ queryKey: ["users"], queryFn: () => get<User[]>("users") });
  const [invite, setInvite] = useState(false);
  const [created, setCreated] = useState<{ email: string; password: string } | null>(null);
  const [confirm, setConfirm] = useState<{ user: User; action: "delete" | "mfa" | "reset" | "disable" } | null>(null);
  const refresh = () => qc.invalidateQueries({ queryKey: ["users"] });

  const act = async () => {
    if (!confirm) return;
    const { user, action } = confirm;
    try {
      if (action === "delete") await del(`users/${user.id}`);
      if (action === "mfa") await post(`users/${user.id}/reset-mfa`);
      if (action === "disable") await patch(`users/${user.id}`, { disabled: !user.disabled });
      if (action === "reset") {
        const pw = generatePassword();
        await patch(`users/${user.id}`, { password: pw });
        setCreated({ email: user.email, password: pw });
      }
      toast.success("Done", { delete: "User removed; their connections moved to you", mfa: "Two-step sign-in reset", disable: user.disabled ? "Account enabled" : "Account disabled", reset: "Temporary password set" }[action]);
      refresh();
    } catch (e) {
      toast.error("Could not update user", (e as Error).message);
    }
    setConfirm(null);
  };

  return (
    <>
      <div className="row gap-3" style={{ marginBottom: 14 }}>
        <span className="muted">{users.data?.length ?? 0} people</span>
        <span className="spacer" />
        <Button variant="primary" onClick={() => setInvite(true)}><UserPlus /> Add person</Button>
      </div>
      <div className="stable-wrap">
        <table className="table">
          <thead><tr><th>Name</th><th>Role</th><th>Two-step</th><th>Last sign-in</th><th>Status</th><th /></tr></thead>
          <tbody>
            {users.data?.map((u) => (
              <tr key={u.id}>
                <td>
                  <div className="usercell">
                    <span className="avatar avatar--sm">{u.name.split(/\s+/).map((w) => w[0]).slice(0, 2).join("").toUpperCase()}</span>
                    <div><div className="usercell__name">{u.name}{u.id === me.user.id && <span className="faint"> (you)</span>}</div><div className="usercell__mail">{u.email}</div></div>
                  </div>
                </td>
                <td>
                  <select className="select role-select" value={u.role} disabled={u.id === me.user.id || (u.role === "owner" && me.user.role !== "owner")} onChange={async (e) => {
                    try {
                      await patch(`users/${u.id}`, { role: e.target.value });
                      toast.success("Role updated", `${u.name} is now ${e.target.value}`);
                      refresh();
                    } catch (err) {
                      toast.error("Could not change role", (err as Error).message);
                    }
                  }} aria-label={`Role of ${u.name}`}>
                    {(["owner", "admin", "member", "viewer"] as Role[]).filter((r) => r !== "owner" || me.user.role === "owner" || u.role === "owner").map((r) => <option key={r} value={r}>{r}</option>)}
                  </select>
                </td>
                <td>{u.mfaEnabled ? <span className="badge badge--success"><Check /> on</span> : <span className="badge">off</span>}</td>
                <td className="muted">{u.lastLoginAt ? ago(u.lastLoginAt) : "never"}</td>
                <td>{u.disabled ? <span className="badge badge--danger">disabled</span> : u.mustChangePassword ? <span className="badge badge--warn">must set password</span> : <span className="faint">active</span>}</td>
                <td className="num">
                  {u.id !== me.user.id && (
                    <Menu>
                      <MenuTrigger asChild><Button size="sm" variant="ghost" icon aria-label={`Actions for ${u.name}`}><MoreHorizontal /></Button></MenuTrigger>
                      <MenuContent align="end">
                        <MenuItem icon={<RotateCcw />} onSelect={() => setConfirm({ user: u, action: "reset" })}>Set temporary password</MenuItem>
                        {u.mfaEnabled && <MenuItem icon={<ShieldOff />} onSelect={() => setConfirm({ user: u, action: "mfa" })}>Reset two-step sign-in</MenuItem>}
                        <MenuItem icon={<Ban />} onSelect={() => setConfirm({ user: u, action: "disable" })}>{u.disabled ? "Enable account" : "Disable account"}</MenuItem>
                        <MenuSep />
                        <MenuItem icon={<Trash2 />} danger onSelect={() => setConfirm({ user: u, action: "delete" })}>Remove from team</MenuItem>
                      </MenuContent>
                    </Menu>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {invite && <InviteDialog me={me} onClose={() => setInvite(false)} onCreated={(email, password) => { setInvite(false); refresh(); if (password) setCreated({ email, password }); }} />}
      <Dialog open={!!created} onOpenChange={(o) => !o && setCreated(null)} title="Temporary password"
        description="Share it privately. They will be asked to choose their own password when they sign in. It will not be shown again."
        footer={<Button variant="primary" onClick={() => setCreated(null)}>Done</Button>}>
        {created && <CopyBox label={created.email} value={created.password} />}
      </Dialog>
      <Dialog open={!!confirm} onOpenChange={(o) => !o && setConfirm(null)}
        title={confirm ? { delete: `Remove ${confirm.user.name}?`, mfa: `Reset two-step sign-in for ${confirm.user.name}?`, reset: `Set a temporary password for ${confirm.user.name}?`, disable: confirm.user.disabled ? `Enable ${confirm.user.name}?` : `Disable ${confirm.user.name}?` }[confirm.action] : ""}
        description={confirm ? { delete: "They lose access immediately. Connections they own move to you.", mfa: "They will sign in with only their password until they set it up again.", reset: "Their sessions end and they must choose a new password at next sign-in.", disable: confirm.user.disabled ? "They can sign in again." : "Their sessions end immediately and they cannot sign in." }[confirm.action] : ""}
        footer={<><Button onClick={() => setConfirm(null)}>Cancel</Button><Button variant={confirm?.action === "delete" || confirm?.action === "disable" ? "danger" : "primary"} onClick={act}>Confirm</Button></>} />
    </>
  );
}

function InviteDialog({ me, onClose, onCreated }: { me: Me; onClose(): void; onCreated(email: string, password?: string): void }) {
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<Role>("member");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async () => {
    setBusy(true);
    setError("");
    try {
      const r = await post<{ id: string; temporaryPassword?: string }>("users", { name, email, role });
      onCreated(email, r.temporaryPassword);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} title="Add a person" description="Rowsmith generates a temporary password for you to share."
      footer={<><Button onClick={onClose}>Cancel</Button><Button variant="primary" loading={busy} onClick={submit} disabled={!name || !email}>Add person</Button></>}>
      <div className="col gap-4">
        <Field label="Name" required><input className="input" value={name} onChange={(e) => setName(e.target.value)} autoFocus /></Field>
        <Field label="Email" required><input className="input" type="email" value={email} onChange={(e) => setEmail(e.target.value)} /></Field>
        <Field label="Role" group>
          <div className="roles" role="radiogroup">
            {(["admin", "member", "viewer", ...(me.user.role === "owner" ? ["owner"] : [])] as Role[]).map((r) => (
              <label key={r} className={`rolepick ${role === r ? "is-active" : ""}`}>
                <input type="radio" name="role" checked={role === r} onChange={() => setRole(r)} />
                <span className="rolepick__name">{r}</span>
                <span className="rolepick__help">{ROLE_HELP[r]}</span>
              </label>
            ))}
          </div>
        </Field>
        {error && <Alert kind="danger">{error}</Alert>}
      </div>
    </Dialog>
  );
}

function CopyBox({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="copybox">
      <span className="muted">{label}</span>
      <code className="copybox__value mono">{value}</code>
      <Button size="sm" onClick={() => { navigator.clipboard?.writeText(value); setCopied(true); }}>{copied ? <Check /> : <Copy />} {copied ? "Copied" : "Copy"}</Button>
    </div>
  );
}

function Audit() {
  const [action, setAction] = useState("");
  const q = useInfiniteQuery({
    queryKey: ["audit", action],
    initialPageParam: 0,
    queryFn: ({ pageParam }) => get<AuditEntry[]>(`audit${qs({ action, before: pageParam || undefined, limit: 100 })}`),
    getNextPageParam: (last) => (last.length === 100 ? last[last.length - 1].id : undefined),
  });
  const rows = (q.data?.pages ?? []).flat();
  const filters = [["", "Everything"], ["auth.", "Sign-ins"], ["query.", "Data-changing queries"], ["data.", "Grid edits"], ["connection.", "Connections"], ["user.", "People"], ["ssh.", "SSH"], ["settings.", "Settings"]];
  return (
    <>
      <div className="segmented audit__filters" role="group" aria-label="Filter audit log">
        {filters.map(([v, l]) => <button key={v} aria-pressed={action === v} onClick={() => setAction(v)}>{l}</button>)}
      </div>
      <div className="stable-wrap">
        <table className="table audit">
          <thead><tr><th>When</th><th>Who</th><th>What</th><th>Target</th><th>Details</th><th>IP</th></tr></thead>
          <tbody>
            {rows.map((a) => (
              <tr key={a.id}>
                <td className="muted nowrap" title={new Date(a.at).toLocaleString()}>{ago(a.at)}</td>
                <td className="nowrap">{a.userName || <span className="faint">—</span>}</td>
                <td><span className={`auditaction auditaction--${a.action.split(".")[0]} ${a.action.includes("failed") ? "is-fail" : ""}`}>{a.action}</span></td>
                <td className="mono faint truncate audit__target">{a.target}</td>
                <td className="audit__detail mono">{detailText(a.detail)}</td>
                <td className="mono faint nowrap">{a.ip}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {q.hasNextPage && <Button onClick={() => q.fetchNextPage()} loading={q.isFetchingNextPage} className="audit__more">Load older entries</Button>}
      {!q.isLoading && rows.length === 0 && <Empty title="Nothing recorded">No entries match this filter.</Empty>}
      <p className="faint audit__count">{int(rows.length)} entries shown</p>
    </>
  );
}

function detailText(d: Record<string, unknown>) {
  if (!d || !Object.keys(d).length) return "";
  if (typeof d.sql === "string") return d.sql.replace(/\s+/g, " ").slice(0, 200);
  return Object.entries(d).filter(([, v]) => v !== null && v !== undefined && v !== "").map(([k, v]) => `${k}: ${typeof v === "object" ? JSON.stringify(v) : v}`).join(" · ").slice(0, 240);
}

function SSHHosts() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["known-hosts"], queryFn: () => get<{ host: string; port: number; keyType: string; fingerprint: string; addedBy: string; addedAt: number }[]>("ssh/known-hosts") });
  if (q.isLoading) return <Spinner />;
  if (!q.data?.length) return <Empty icon={<KeyRound />} title="No trusted SSH servers">When someone connects through an SSH tunnel for the first time, they confirm the server's fingerprint and it appears here.</Empty>;
  return (
    <div className="stable-wrap">
      <table className="table">
        <thead><tr><th>Server</th><th>Key</th><th>Fingerprint</th><th>Trusted by</th><th /></tr></thead>
        <tbody>
          {q.data.map((k) => (
            <tr key={`${k.host}:${k.port}:${k.keyType}`}>
              <td className="mono">{k.host}:{k.port}</td>
              <td className="mono faint">{k.keyType}</td>
              <td className="mono">{k.fingerprint}</td>
              <td className="muted">{k.addedBy} · {ago(k.addedAt)}</td>
              <td className="num">
                <Button size="sm" variant="ghost" onClick={async () => {
                  await del("ssh/known-hosts", { host: k.host, port: k.port, keyType: k.keyType });
                  qc.invalidateQueries({ queryKey: ["known-hosts"] });
                  toast.success("Host key removed", "The next connection will ask to verify it again.");
                }}>Forget</Button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function SecuritySettings() {
  const qc = useQueryClient();
  const s = useQuery({ queryKey: ["settings"], queryFn: () => get<Record<string, unknown>>("settings") });
  const requireMfa = s.data?.["security.require_mfa"] === "true";
  return (
    <div className="pgrid">
      <section className="card pcard">
        <h2 className="pcard__title">Two-step sign-in</h2>
        <p className="pcard__desc">When required, anyone without an authenticator app must set one up the next time they sign in, before they can do anything else.</p>
        <label className="switch">
          <input type="checkbox" checked={requireMfa} onChange={async (e) => {
            await put("settings", { "security.require_mfa": e.target.checked ? "true" : null });
            qc.invalidateQueries({ queryKey: ["settings"] });
            toast.success(e.target.checked ? "Two-step sign-in is now required" : "Two-step sign-in is optional");
          }} />
          Require two-step sign-in for everyone
        </label>
      </section>
      <section className="card pcard">
        <h2 className="pcard__title">Built-in protections</h2>
        <ul className="plist">
          <li>Passwords hashed with Argon2id; at least 12 characters, checked against common passwords.</li>
          <li>Accounts lock for 15 minutes after 8 failed sign-ins; sign-in attempts are rate limited per address.</li>
          <li>Sessions end after 8 hours idle and 72 hours at most; changing a password ends other sessions.</li>
          <li>Connection secrets are encrypted per record and never sent back to the browser.</li>
          <li>Production connections require confirmation for anything that may change data.</li>
        </ul>
      </section>
    </div>
  );
}

function generatePassword() {
  const alpha = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789";
  const b = crypto.getRandomValues(new Uint8Array(18));
  let out = "";
  b.forEach((c, i) => {
    if (i > 0 && i % 6 === 0) out += "-";
    out += alpha[c % alpha.length];
  });
  return out;
}
