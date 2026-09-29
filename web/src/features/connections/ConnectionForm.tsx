import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ChevronDown, ChevronRight, Link2, Plus, Trash2, Plug, CheckCircle2, AlertTriangle, ShieldAlert, Upload, Route as RouteIcon, KeyRound } from "lucide-react";
import { ApiError, patch, post } from "../../lib/api";
import { useConnections, useDrivers } from "../../lib/queries";
import type { Access, Connection, DriverInfo, Environment, Field as DField, ServerInfo, SSHHop } from "../../lib/types";
import { toast } from "../../lib/store";
import { go } from "../../lib/nav";
import { Alert, Button, Dialog, EngineBadge, Field } from "../../components/ui";
import "./connections.css";

interface HostKey {
  host: string;
  port: number;
  keyType: string;
  fingerprint: string;
  publicKey: string;
}

const ENVS: { value: Environment; label: string }[] = [
  { value: "production", label: "Production" },
  { value: "staging", label: "Staging" },
  { value: "development", label: "Development" },
  { value: "local", label: "Local" },
];

const SWATCHES = ["", "#e8b84a", "#e4572e", "#4c9aff", "#45c08a", "#b07bd6", "#e36fa3", "#43b5c4", "#8793a6"];

type SecretEdits = Record<string, string | null>;

function defaults(d?: DriverInfo) {
  const p: Record<string, unknown> = {};
  for (const f of d?.fields ?? []) if (!f.secret && f.default !== undefined) p[f.key] = f.default;
  return p;
}

// Parses mysql://, postgres://, mongodb://, sqlserver:// style URLs.
function parseURL(raw: string): { driver?: string; params: Record<string, unknown>; secrets: Record<string, string> } | null {
  try {
    const u = new URL(raw.trim());
    const scheme = u.protocol.replace(":", "").toLowerCase();
    const driver = ({ mysql: "mysql", mariadb: "mariadb", postgres: "postgres", postgresql: "postgres", mongodb: "mongodb", "mongodb+srv": "mongodb", sqlserver: "mssql", mssql: "mssql", oracle: "oracle", sqlite: "sqlite" } as Record<string, string>)[scheme];
    const params: Record<string, unknown> = {};
    const secrets: Record<string, string> = {};
    if (u.hostname) params.host = decodeURIComponent(u.hostname);
    if (u.port) params.port = Number(u.port);
    if (u.username) params.user = decodeURIComponent(u.username);
    if (u.password) secrets.password = decodeURIComponent(u.password);
    const db = decodeURIComponent(u.pathname.replace(/^\//, ""));
    if (db) params[driver === "oracle" ? "service" : "database"] = db;
    const ssl = u.searchParams.get("sslmode") || u.searchParams.get("ssl-mode") || u.searchParams.get("ssl");
    if (ssl) {
      const m = ssl.toLowerCase();
      params.tls = m === "disable" || m === "disabled" || m === "false" ? "disable" : m.includes("verify_identity") || m === "verify-full" ? "verify-full" : m.includes("verify") ? "verify-ca" : m === "require" || m === "required" || m === "true" ? "require" : "prefer";
    }
    return { driver, params, secrets };
  } catch {
    return null;
  }
}

export function ConnectionDialog({ conn, initialDriver, onClose }: { conn?: Connection; initialDriver?: string; onClose(): void }) {
  const qc = useQueryClient();
  const drivers = useDrivers();
  const editing = !!conn;
  const [driverId, setDriverId] = useState(conn?.driver ?? initialDriver ?? "");
  const driver = drivers.data?.find((d) => d.id === driverId);
  const [name, setName] = useState(conn?.name ?? "");
  const [env, setEnv] = useState<Environment>(conn?.environment ?? "development");
  const [folder, setFolder] = useState(conn?.folder ?? "");
  const [color, setColor] = useState(conn?.color ?? "");
  const [readOnly, setReadOnly] = useState(conn?.readOnly ?? false);
  const [teamAccess, setTeamAccess] = useState<Access>(conn?.teamAccess ?? "");
  const [notes, setNotes] = useState(conn?.notes ?? "");
  const [params, setParams] = useState<Record<string, unknown>>(conn ? { ...conn.params } : {});
  const [secrets, setSecrets] = useState<SecretEdits>({});
  const [sshOn, setSshOn] = useState(!!conn?.ssh?.enabled);
  const [hops, setHops] = useState<SSHHop[]>(conn?.ssh?.hops?.length ? conn.ssh.hops : [{ host: "", port: 22, user: "", auth: "password" }]);
  const [open, setOpen] = useState<Record<string, boolean>>({ tls: false, advanced: false, url: false });
  const [url, setUrl] = useState("");
  const [test, setTest] = useState<{ state: "idle" | "running" | "ok" | "error"; msg?: string; server?: ServerInfo; ms?: number }>({ state: "idle" });
  const [saving, setSaving] = useState(false);
  const [hostKey, setHostKey] = useState<{ key: HostKey; then: "test" | "save" } | null>(null);
  const [changedKey, setChangedKey] = useState<HostKey | null>(null);
  const [error, setError] = useState("");
  const secretsSet = conn?.secretsSet ?? {};
  const all = useConnections();
  const folders = useMemo(() => [...new Set((all.data ?? []).map((c) => c.folder).filter(Boolean))].sort(), [all.data]);

  useEffect(() => {
    if (!editing && driver) {
      setParams((p) => ({ ...defaults(driver), ...p, port: p.port ?? driver.defaultPort }));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [driverId, drivers.data]);

  useEffect(() => {
    if (!driverId && !editing && drivers.data?.length) setDriverId(drivers.data[0].id);
  }, [drivers.data, driverId, editing]);

  const setParam = (k: string, v: unknown) => setParams((p) => ({ ...p, [k]: v }));

  function visible(f: DField) {
    if (!f.showIf) return true;
    return Object.entries(f.showIf).every(([k, vals]) => vals.includes(String(params[k] ?? defaults(driver)[k] ?? "")));
  }

  function payload() {
    const cleanSecrets: Record<string, string | null> = {};
    for (const [k, v] of Object.entries(secrets)) cleanSecrets[k] = v;
    const ssh = driver?.ssh ? { enabled: sshOn, hops: hops.map((h) => ({ ...h, port: Number(h.port) || 22 })) } : undefined;
    return {
      name: name.trim() || autoName(),
      driver: driverId,
      environment: env,
      folder,
      color,
      readOnly,
      notes,
      params,
      secrets: cleanSecrets,
      ssh,
      ...(conn && conn.ownerId && conn.access !== "manage" ? {} : { teamAccess }),
    };
  }

  function autoName() {
    const host = String(params.host ?? params.project ?? params.file ?? "");
    const db = String(params.database ?? "");
    return [db || host || driver?.name || "Connection", driver?.name ? `(${driver.name})` : ""].filter(Boolean).join(" ");
  }

  function handleErr(e: unknown, then: "test" | "save") {
    if (e instanceof ApiError && e.code === "ssh_unknown_host") {
      setHostKey({ key: e.detail as HostKey, then });
      return true;
    }
    if (e instanceof ApiError && e.code === "ssh_host_changed") {
      setChangedKey(e.detail as HostKey);
      return true;
    }
    return false;
  }

  async function runTest() {
    setTest({ state: "running" });
    try {
      const r = await post<{ server: ServerInfo; latencyMs: number }>("connections/test", { ...payload(), id: conn?.id });
      setTest({ state: "ok", server: r.server, ms: r.latencyMs });
    } catch (e) {
      if (handleErr(e, "test")) return setTest({ state: "idle" });
      setTest({ state: "error", msg: e instanceof ApiError ? e.message : String(e) });
    }
  }

  async function save() {
    setSaving(true);
    setError("");
    try {
      let saved: Connection;
      if (editing) saved = await patch<Connection>(`connections/${conn!.id}`, payload());
      else saved = await post<Connection>("connections", payload());
      await qc.invalidateQueries({ queryKey: ["connections"] });
      qc.removeQueries({ queryKey: ["server", saved.id] });
      toast.success(editing ? "Connection updated" : "Connection saved", saved.name);
      onClose();
      if (!editing) go(`/c/${saved.id}`);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  }

  async function trust() {
    if (!hostKey) return;
    await post("ssh/trust", hostKey.key);
    const then = hostKey.then;
    setHostKey(null);
    toast.success("Host key trusted", `${hostKey.key.host} · ${hostKey.key.fingerprint}`);
    if (then === "test") runTest();
  }

  const mainFields = driver?.fields.filter((f) => !f.section && visible(f)) ?? [];
  const authFields = driver?.fields.filter((f) => f.section === "auth" && visible(f)) ?? [];
  const tlsFields = driver?.fields.filter((f) => f.section === "tls" && visible(f)) ?? [];
  const advFields = driver?.fields.filter((f) => f.section === "advanced" && visible(f)) ?? [];
  const isOwner = !conn || conn.access === "manage";

  const renderField = (f: DField) => (
    <DriverField key={f.key} f={f} value={params[f.key]} onChange={(v) => setParam(f.key, v)}
      secret={f.secret ? { set: !!secretsSet[f.key], edit: secrets[f.key], onChange: (v) => setSecrets((s) => ({ ...s, [f.key]: v })) } : undefined} />
  );

  return (
    <>
      <Dialog
        open
        onOpenChange={(o) => !o && onClose()}
        width="xwide"
        title={editing ? `Edit ${conn!.name}` : "New connection"}
        description={editing ? "Changes take effect for everyone using this connection." : "Credentials are encrypted before they are stored and are never sent back to the browser."}
        footer={
          <>
            <TestResult test={test} />
            <span className="spacer" />
            <Button onClick={runTest} loading={test.state === "running"} disabled={!driver}>
              <Plug /> Test connection
            </Button>
            <Button onClick={onClose}>Cancel</Button>
            <Button variant="primary" onClick={save} loading={saving} disabled={!driver}>
              {editing ? "Save changes" : "Save connection"}
            </Button>
          </>
        }
      >
        <datalist id="rs-folders">{folders.map((f) => <option key={f} value={f} />)}</datalist>
        <div className="connform">
          {!editing && (
            <div className="connform__engines" role="radiogroup" aria-label="Database type">
              {drivers.data?.map((d) => (
                <button key={d.id} role="radio" aria-checked={d.id === driverId} className={`engine-row ${d.id === driverId ? "is-active" : ""}`} onClick={() => { setDriverId(d.id); setTest({ state: "idle" }); }}>
                  <EngineBadge driver={d.id} size={26} />
                  <span className="truncate">{d.name}</span>
                </button>
              ))}
            </div>
          )}
          <div className="connform__body">
            {error && <Alert kind="danger">{error}</Alert>}

            <div className="connform__url">
              <button className="section-toggle" onClick={() => setOpen((o) => ({ ...o, url: !o.url }))}>
                <Link2 size={14} /> Paste a connection URL
                {open.url ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
              </button>
              {open.url && (
                <div className="row gap-3">
                  <input className="input input--mono" placeholder="postgres://user:password@host:5432/dbname?sslmode=require" value={url} onChange={(e) => setUrl(e.target.value)} />
                  <Button onClick={() => {
                    const r = parseURL(url);
                    if (!r) return toast.error("That does not look like a connection URL");
                    if (r.driver && drivers.data?.some((d) => d.id === r.driver) && !editing) setDriverId(r.driver);
                    setParams((p) => ({ ...p, ...r.params }));
                    if (Object.keys(r.secrets).length) setSecrets((s) => ({ ...s, ...r.secrets }));
                    setUrl("");
                    setOpen((o) => ({ ...o, url: false }));
                    toast.info("Fields filled from URL", "The password was moved to the encrypted secrets.");
                  }}>Fill</Button>
                </div>
              )}
            </div>

            <div className="formgrid">
              <div className="span-4"><Field label="Name"><input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder={autoName()} /></Field></div>
              <div className="span-2"><Field label="Folder"><input className="input" value={folder} onChange={(e) => setFolder(e.target.value)} placeholder="optional" list="rs-folders" /></Field></div>
              <div className="span-6">
                <Field label="Environment" help={env === "production" ? "Production connections ask for confirmation before any statement that may change data." : undefined}>
                  <div className="envpick" role="radiogroup" aria-label="Environment">
                    {ENVS.map((e) => (
                      <button key={e.value} role="radio" aria-checked={env === e.value} className={`envpick__opt envpick__opt--${e.value}`} onClick={() => setEnv(e.value)}>
                        <span className={`dot dot--${e.value}`} /> {e.label}
                      </button>
                    ))}
                    <span className="spacer" />
                    <div className="swatches" aria-label="Rail color">
                      {SWATCHES.map((s) => (
                        <button key={s || "none"} className={`swatch ${color === s ? "is-active" : ""}`} style={{ background: s || "transparent" }} onClick={() => setColor(s)} aria-label={s ? `Color ${s}` : "No color"} />
                      ))}
                    </div>
                  </div>
                </Field>
              </div>
            </div>

            {driver && (
              <>
                <Section title="Connection">
                  <div className="formgrid">{mainFields.map(renderField)}</div>
                </Section>
                {authFields.length > 0 && (
                  <Section title="Authentication">
                    <div className="formgrid">{authFields.map(renderField)}</div>
                  </Section>
                )}
                {driver.ssh && (
                  <Section title="SSH tunnel" icon={<RouteIcon size={14} />} aside={
                    <label className="switch"><input type="checkbox" checked={sshOn} onChange={(e) => setSshOn(e.target.checked)} /> {sshOn ? "On" : "Off"}</label>
                  }>
                    {sshOn ? (
                      <SSHHops hops={hops} setHops={setHops} secrets={secrets} setSecrets={setSecrets} secretsSet={secretsSet} />
                    ) : (
                      <p className="muted connform__hint">Reach a database on a private network by connecting through an SSH server. The database host is then resolved from that server.</p>
                    )}
                  </Section>
                )}
                {tlsFields.length > 0 && (
                  <Collapsible title="TLS / SSL" open={open.tls} onToggle={() => setOpen((o) => ({ ...o, tls: !o.tls }))} summary={String(params.tls ?? "")}>
                    <div className="formgrid">{tlsFields.map(renderField)}</div>
                  </Collapsible>
                )}
                {advFields.length > 0 && (
                  <Collapsible title="Advanced" open={open.advanced} onToggle={() => setOpen((o) => ({ ...o, advanced: !o.advanced }))}>
                    <div className="formgrid">{advFields.map(renderField)}</div>
                  </Collapsible>
                )}
                <Section title="Access">
                  <div className="formgrid">
                    <div className="span-6">
                      <label className="check">
                        <input type="checkbox" checked={readOnly} onChange={(e) => setReadOnly(e.target.checked)} />
                        Read-only for everyone: block statements and edits that could change data
                      </label>
                    </div>
                    {isOwner && (
                      <div className="span-3">
                        <Field label="Share with the whole team" help="Specific people can be added from the connection's sharing settings.">
                          <select className="select" value={teamAccess} onChange={(e) => setTeamAccess(e.target.value as Access)}>
                            <option value="">Only me and people I share with</option>
                            <option value="read">Everyone can read</option>
                            <option value="write">Everyone can read and write</option>
                            <option value="manage">Everyone can manage</option>
                          </select>
                        </Field>
                      </div>
                    )}
                    <div className={isOwner ? "span-3" : "span-6"}>
                      <Field label="Notes" help="Visible to everyone with access.">
                        <textarea className="textarea" rows={2} style={{ minHeight: 60 }} value={notes} onChange={(e) => setNotes(e.target.value)} placeholder="Runbook link, owner team, maintenance window…" />
                      </Field>
                    </div>
                  </div>
                </Section>
              </>
            )}
          </div>
        </div>
      </Dialog>

      <Dialog
        open={!!hostKey}
        onOpenChange={(o) => !o && setHostKey(null)}
        title="Trust this SSH server?"
        description="Rowsmith has not connected to this server before. Check the fingerprint with whoever runs the server before trusting it."
        footer={
          <>
            <Button onClick={() => setHostKey(null)}>Cancel</Button>
            <Button variant="primary" onClick={trust}><KeyRound /> Trust and continue</Button>
          </>
        }
      >
        {hostKey && (
          <div className="hostkey">
            <div className="hostkey__row"><span className="muted">Server</span><span className="mono">{hostKey.key.host}:{hostKey.key.port}</span></div>
            <div className="hostkey__row"><span className="muted">Key type</span><span className="mono">{hostKey.key.keyType}</span></div>
            <div className="hostkey__fp mono">{hostKey.key.fingerprint}</div>
            <p className="muted hostkey__how">On the server, <code>ssh-keygen -lf /etc/ssh/ssh_host_*_key.pub</code> prints the fingerprints to compare.</p>
          </div>
        )}
      </Dialog>

      <Dialog open={!!changedKey} onOpenChange={(o) => !o && setChangedKey(null)} title="SSH host key changed" footer={<Button onClick={() => setChangedKey(null)}>Close</Button>}>
        {changedKey && (
          <div className="col gap-4">
            <Alert kind="danger" title="Rowsmith refused to connect">
              {changedKey.host}:{changedKey.port} presented a different key than the one trusted before. The server may have been reinstalled — or someone may be intercepting the connection.
            </Alert>
            <div className="hostkey__fp mono">{changedKey.fingerprint}</div>
            <p className="muted">If the change is expected, an administrator can remove the old key under Administration → SSH hosts, then connect again.</p>
          </div>
        )}
      </Dialog>
    </>
  );
}

function TestResult({ test }: { test: { state: string; msg?: string; server?: ServerInfo; ms?: number } }) {
  if (test.state === "ok" && test.server)
    return (
      <span className="testres testres--ok">
        <CheckCircle2 size={15} /> Connected · {test.server.product} {test.server.version} · {test.ms} ms
      </span>
    );
  if (test.state === "error")
    return (
      <span className="testres testres--error" title={test.msg}>
        <AlertTriangle size={15} /> <span className="truncate">{test.msg}</span>
      </span>
    );
  return null;
}

function Section({ title, icon, aside, children }: { title: string; icon?: ReactNode; aside?: ReactNode; children: ReactNode }) {
  return (
    <section className="fsection">
      <div className="fsection__head">
        <h3 className="eyebrow row gap-2">{icon}{title}</h3>
        <span className="spacer" />
        {aside}
      </div>
      {children}
    </section>
  );
}

function Collapsible({ title, open, onToggle, summary, children }: { title: string; open: boolean; onToggle(): void; summary?: string; children: ReactNode }) {
  return (
    <section className="fsection">
      <button className="section-toggle" onClick={onToggle} aria-expanded={open}>
        {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        <span className="eyebrow">{title}</span>
        {summary && !open && <span className="faint mono section-toggle__summary">{summary}</span>}
      </button>
      {open && <div className="fsection__body">{children}</div>}
    </section>
  );
}

function readFile(onText: (t: string) => void) {
  const input = document.createElement("input");
  input.type = "file";
  input.onchange = async () => {
    const f = input.files?.[0];
    if (f) onText(await f.text());
  };
  input.click();
}

function SecretInput({ set, edit, onChange, multiline, placeholder }: { set: boolean; edit: string | null | undefined; onChange(v: string | null): void; multiline?: boolean; placeholder?: string }) {
  const cleared = edit === null;
  const ph = cleared ? "will be removed" : set && edit === undefined ? "•••••••• saved — type to replace" : placeholder;
  const common = { value: typeof edit === "string" ? edit : "", onChange: (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => onChange(e.target.value), placeholder: ph, autoComplete: "new-password", spellCheck: false };
  return (
    <div className="secret">
      {multiline ? <textarea className="textarea input--mono" rows={3} {...common} /> : <input className="input" type="password" {...common} />}
      {multiline && (
        <Button size="sm" variant="ghost" onClick={() => readFile((t) => onChange(t))}><Upload /> Load file</Button>
      )}
      {set && edit !== null && (
        <Button size="sm" variant="ghost" onClick={() => onChange(null)}>Clear</Button>
      )}
    </div>
  );
}

function DriverField({ f, value, onChange, secret }: { f: DField; value: unknown; onChange(v: unknown): void; secret?: { set: boolean; edit: string | null | undefined; onChange(v: string | null): void } }) {
  const span = `span-${f.span ?? 6}`;
  let control: ReactNode;
  if (secret) {
    control = <SecretInput set={secret.set} edit={secret.edit} onChange={secret.onChange} multiline={f.type === "file" || f.type === "textarea"} placeholder={f.placeholder} />;
  } else {
    switch (f.type) {
      case "select":
        control = (
          <select className="select" value={String(value ?? f.default ?? "")} onChange={(e) => onChange(e.target.value)}>
            {f.options?.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>
        );
        break;
      case "bool":
        return (
          <div className={span} key={f.key}>
            <label className="check"><input type="checkbox" checked={!!value} onChange={(e) => onChange(e.target.checked)} /> {f.label}</label>
            {f.help && <div className="field__help">{f.help}</div>}
          </div>
        );
      case "number":
        control = <input className="input" type="number" value={value === undefined || value === null ? "" : String(value)} placeholder={f.placeholder ?? (f.default !== undefined ? String(f.default) : "")} onChange={(e) => onChange(e.target.value === "" ? undefined : Number(e.target.value))} />;
        break;
      case "textarea":
      case "file":
        control = (
          <div className="secret">
            <textarea className="textarea input--mono" rows={3} value={String(value ?? "")} placeholder={f.placeholder} onChange={(e) => onChange(e.target.value)} spellCheck={false} />
            {f.type === "file" && <Button size="sm" variant="ghost" onClick={() => readFile((t) => onChange(t))}><Upload /> Load file</Button>}
          </div>
        );
        break;
      default:
        control = <input className="input" type="text" value={String(value ?? "")} placeholder={f.placeholder} onChange={(e) => onChange(e.target.value)} spellCheck={false} autoCapitalize="off" autoCorrect="off" />;
    }
  }
  return (
    <div className={span}>
      <Field label={f.label} required={f.required} help={f.help}>{control}</Field>
    </div>
  );
}

function SSHHops({ hops, setHops, secrets, setSecrets, secretsSet }: { hops: SSHHop[]; setHops(h: SSHHop[]): void; secrets: SecretEdits; setSecrets(fn: (s: SecretEdits) => SecretEdits): void; secretsSet: Record<string, boolean> }) {
  const update = (i: number, p: Partial<SSHHop>) => setHops(hops.map((h, j) => (j === i ? { ...h, ...p } : h)));
  const sk = (i: number, f: string) => `ssh.${i}.${f}`;
  return (
    <div className="hops">
      {hops.map((h, i) => (
        <div key={i} className="hop">
          <div className="hop__label">
            <span className="hop__num">{i + 1}</span>
            <span className="eyebrow">{i === 0 ? "SSH server" : "Jump host"}</span>
            {i > 0 && <span className="faint">reached through {i === 1 ? "the SSH server" : `hop ${i}`}</span>}
            <span className="spacer" />
            {hops.length > 1 && <Button size="sm" variant="ghost" icon aria-label="Remove hop" onClick={() => setHops(hops.filter((_, j) => j !== i))}><Trash2 /></Button>}
          </div>
          <div className="formgrid">
            <div className="span-3"><Field label="Host" required><input className="input" value={h.host} onChange={(e) => update(i, { host: e.target.value })} placeholder="bastion.example.com" spellCheck={false} /></Field></div>
            <div className="span-1"><Field label="Port"><input className="input" type="number" value={h.port} onChange={(e) => update(i, { port: Number(e.target.value) })} /></Field></div>
            <div className="span-2"><Field label="User" required><input className="input" value={h.user} onChange={(e) => update(i, { user: e.target.value })} spellCheck={false} autoCapitalize="off" /></Field></div>
            <div className="span-6">
              <div className="segmented" role="group" aria-label="SSH authentication">
                <button aria-pressed={h.auth === "password"} onClick={() => update(i, { auth: "password" })}>Password</button>
                <button aria-pressed={h.auth === "key"} onClick={() => update(i, { auth: "key" })}>Private key</button>
              </div>
            </div>
            {h.auth === "password" ? (
              <div className="span-6"><Field label="SSH password"><SecretInput set={!!secretsSet[sk(i, "password")]} edit={secrets[sk(i, "password")]} onChange={(v) => setSecrets((s) => ({ ...s, [sk(i, "password")]: v }))} /></Field></div>
            ) : (
              <>
                <div className="span-4"><Field label="Private key (OpenSSH or PEM)"><SecretInput multiline set={!!secretsSet[sk(i, "key")]} edit={secrets[sk(i, "key")]} onChange={(v) => setSecrets((s) => ({ ...s, [sk(i, "key")]: v }))} placeholder="-----BEGIN OPENSSH PRIVATE KEY-----" /></Field></div>
                <div className="span-2"><Field label="Key passphrase"><SecretInput set={!!secretsSet[sk(i, "passphrase")]} edit={secrets[sk(i, "passphrase")]} onChange={(v) => setSecrets((s) => ({ ...s, [sk(i, "passphrase")]: v }))} placeholder="if encrypted" /></Field></div>
              </>
            )}
          </div>
        </div>
      ))}
      {hops.length < 4 && (
        <Button size="sm" onClick={() => setHops([...hops, { host: "", port: 22, user: "", auth: "password" }])}>
          <Plus /> Add jump host
        </Button>
      )}
      <p className="connform__hint faint"><ShieldAlert size={12} /> The first time Rowsmith connects to an SSH server you will be asked to confirm its fingerprint.</p>
    </div>
  );
}
