import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useQueryClient, type QueryClient } from "@tanstack/react-query";
import { Check, Copy, Eye, EyeOff, KeyRound, Trash2, UserPlus, Users, Wand2 } from "lucide-react";
import { ApiError } from "../../lib/api";
import { toast } from "../../lib/store";
import type { Connection } from "../../lib/types";
import { Alert, Button, Dialog, Field, Spinner } from "../../components/ui";
import { applyAccess, previewAccess, splitName, type AccessModel, type AccessReq, type Principal } from "./api";

const message = (e: unknown) => (e instanceof ApiError || e instanceof Error ? e.message : String(e));

/** A database error with what the server added: which objects, and a hint. */
function ErrorText({ e }: { e: unknown }) {
  const d = e instanceof ApiError && e.detail && typeof e.detail === "object" ? (e.detail as { detail?: string; hint?: string }) : {};
  return (
    <>
      <div>{message(e)}</div>
      {d.detail && <pre className="aerror mono">{d.detail}</pre>}
      {d.hint && <div className="faint">{d.hint}</div>}
    </>
  );
}

export function invalidateAccess(qc: QueryClient, connId: string) {
  for (const key of ["access", "grants", "dbusers"]) qc.invalidateQueries({ queryKey: [key, connId] });
}

/** The statements a request would run, refreshed as it changes. */
export function usePreview(connId: string, req: AccessReq | null) {
  const [state, setState] = useState<{ statements?: string[]; error?: string }>({});
  const body = req ? JSON.stringify(req) : "";
  useEffect(() => {
    if (!body) return setState({});
    let live = true;
    const t = setTimeout(() => {
      previewAccess(connId, JSON.parse(body))
        .then((r) => live && setState({ statements: r.statements }))
        .catch((e) => live && setState({ error: message(e) }));
    }, 200);
    return () => {
      live = false;
      clearTimeout(t);
    };
  }, [connId, body]);
  return state;
}

export function SqlPreview({ statements, error, empty = "Nothing to change." }: { statements?: string[]; error?: string; empty?: string }) {
  if (error) return <Alert kind="danger">{error}</Alert>;
  if (!statements) return <div className="apreview apreview--loading"><Spinner /></div>;
  if (!statements.length) return <p className="muted">{empty}</p>;
  return (
    <pre className="apreview mono" aria-label="Statements to run">
      {statements.map((s, i) => <div key={i} className={/^\s*(REVOKE|DROP)/i.test(s) ? "apreview__revoke" : undefined}>{s};</div>)}
    </pre>
  );
}

/** Shows the statements, then applies them. Drops ask for the name first. */
export function ReviewDialog({ conn, title, description, req, verb, confirmName, danger, done, onClose, onDone, children }: {
  conn: Connection; title: ReactNode; description?: ReactNode; req: AccessReq; verb: string; confirmName?: string; danger?: boolean;
  done: string; onClose(): void; onDone?(): void; children?: ReactNode;
}) {
  const qc = useQueryClient();
  const preview = usePreview(conn.id, req);
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const run = async () => {
    setBusy(true);
    setError(null);
    try {
      await applyAccess(conn.id, { ...req, confirm: true });
      toast.success(done);
      invalidateAccess(qc, conn.id);
      onDone?.();
      onClose();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };
  const ready = !!preview.statements?.length && (!confirmName || typed === confirmName);
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} title={title} description={description} width="wide"
      footer={<>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant={danger ? "danger" : "primary"} loading={busy} disabled={!ready} onClick={run}>
          {danger ? <Trash2 /> : <Check />} {verb}
        </Button>
      </>}>
      <div className="col gap-4">
        {conn.environment === "production" && <Alert kind="danger" title="This is a production connection">{conn.name}</Alert>}
        {children}
        <SqlPreview {...preview} />
        {confirmName && (
          <Field label={<>Type <b className="mono">{confirmName}</b> to confirm</>}>
            <input className="input" value={typed} onChange={(e) => setTyped(e.target.value)} autoFocus />
          </Field>
        )}
        {error !== null && <Alert kind="danger"><ErrorText e={error} /></Alert>}
      </div>
    </Dialog>
  );
}

const ALPHABET = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789-_.";

export function generatePassword(length = 22) {
  const bytes = crypto.getRandomValues(new Uint8Array(length));
  return Array.from(bytes, (b) => ALPHABET[b % ALPHABET.length]).join("");
}

function PasswordInput({ value, onChange, id }: { value: string; onChange(v: string, generated: boolean): void; id?: string }) {
  const [show, setShow] = useState(false);
  return (
    <div className="apassword">
      <input id={id} className="input input--mono" type={show ? "text" : "password"} value={value} autoComplete="new-password" spellCheck={false}
        onChange={(e) => onChange(e.target.value, false)} />
      <Button icon variant="ghost" aria-label={show ? "Hide password" : "Show password"} onClick={() => setShow(!show)}>{show ? <EyeOff /> : <Eye />}</Button>
      <Button variant="ghost" onClick={() => { onChange(generatePassword(), true); setShow(true); }}><Wand2 /> Generate</Button>
      <Button icon variant="ghost" aria-label="Copy password" disabled={!value} onClick={() => { navigator.clipboard?.writeText(value); toast.success("Password copied"); }}><Copy /></Button>
    </div>
  );
}

/** Creates a user or a role, showing the statement as it's typed. */
export function NewPrincipalDialog({ conn, model, kind: initial, onClose, onCreated }: {
  conn: Connection; model: AccessModel; kind: "user" | "role"; onClose(): void; onCreated(name: string): void;
}) {
  const qc = useQueryClient();
  const [kind, setKind] = useState(initial);
  const [name, setName] = useState("");
  const [host, setHost] = useState(model.hostDefault ?? "");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // MySQL roles have no host; MongoDB roles are defined in a database like users.
  const withHost = model.hostPart && (kind === "user" || model.hostDefault === "admin");
  const full = withHost && host.trim() ? `${name.trim()}@${host.trim()}` : name.trim();
  const req = useMemo<AccessReq | null>(() => name.trim()
    ? { account: { op: "create", kind, name: full, ...(kind === "user" && password ? { password } : {}) } }
    : null, [name, kind, full, password]);
  const preview = usePreview(conn.id, req);
  const noun = kind === "user" ? "user" : "role";
  const create = async () => {
    if (!req) return;
    setBusy(true);
    setError("");
    try {
      await applyAccess(conn.id, { ...req, confirm: true });
      toast.success(`Created ${noun} ${full}`);
      invalidateAccess(qc, conn.id);
      onCreated(full);
      onClose();
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} title={`New ${noun}`} width="wide"
      description={kind === "user" ? "An account that signs in. Give it access once it's created." : "A group of privileges that users join."}
      footer={<>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="primary" loading={busy} disabled={!req || !!preview.error} onClick={create}><UserPlus /> Create {noun}</Button>
      </>}>
      <form className="col gap-4" onSubmit={(e) => { e.preventDefault(); create(); }}>
        {conn.environment === "production" && <Alert kind="danger" title="This is a production connection">{conn.name}</Alert>}
        {model.canCreate.length > 1 && (
          <Field label="Kind" group>
            <div className="segmented">
              {model.canCreate.map((k) => <button key={k} type="button" aria-pressed={kind === k} onClick={() => setKind(k as "user" | "role")}>{k === "user" ? "User" : "Role"}</button>)}
            </div>
          </Field>
        )}
        <div className="row gap-4 arow-fields">
          <Field label="Name" required><input className="input input--mono" value={name} onChange={(e) => setName(e.target.value)} autoFocus spellCheck={false} autoComplete="off" /></Field>
          {withHost && (
            <Field label={model.hostLabel ?? "Host"} help={model.hostHelp}>
              <input className="input input--mono" value={host} onChange={(e) => setHost(e.target.value)} spellCheck={false} />
            </Field>
          )}
        </div>
        {model.password && kind === "user" && (
          <Field label="Password" htmlFor="new-principal-password" help="Leave empty to set one later.">
            <PasswordInput id="new-principal-password" value={password} onChange={(v) => setPassword(v)} />
          </Field>
        )}
        <div>
          <div className="eyebrow">Will run</div>
          {req ? <SqlPreview {...preview} /> : <p className="muted">Enter a name.</p>}
        </div>
        {error && <Alert kind="danger">{error}</Alert>}
      </form>
    </Dialog>
  );
}

export function PasswordDialog({ conn, principal, onClose }: { conn: Connection; principal: string; onClose(): void }) {
  const [password, setPassword] = useState("");
  const [generated, setGenerated] = useState(false);
  const [again, setAgain] = useState("");
  const [review, setReview] = useState(false);
  const mismatch = again !== "" && again !== password;
  // A generated password is on screen to copy, so it isn't typed twice.
  const ok = !!password && (generated || password === again);
  if (review) {
    return <ReviewDialog conn={conn} title={`Change the password for ${principal}?`} req={{ account: { op: "password", name: principal, password } }}
      verb="Change password" done={`Password changed for ${principal}`} onClose={onClose}
      description="Sessions already signed in stay signed in. Update anything that signs in as this account." />;
  }
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} title={`Change the password for ${principal}`} width="wide"
      footer={<>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="primary" disabled={!ok} onClick={() => setReview(true)}><KeyRound /> Continue</Button>
      </>}>
      <form className="col gap-4" onSubmit={(e) => { e.preventDefault(); if (ok) setReview(true); }}>
        <Field label="New password" htmlFor="pw-new" help={generated ? "Copy it now: it isn't shown again." : undefined}>
          <PasswordInput id="pw-new" value={password} onChange={(v, g) => { setPassword(v); setGenerated(g); }} />
        </Field>
        {!generated && (
          <Field label="Type it again" error={mismatch ? "The passwords don't match." : undefined}>
            <input className="input input--mono" type="password" value={again} onChange={(e) => setAgain(e.target.value)} autoComplete="new-password" aria-invalid={mismatch} />
          </Field>
        )}
      </form>
    </Dialog>
  );
}

/** Picks the principal whose access to copy. */
export function CopyFromDialog({ principals, current, model, where, onClose, onPick }: {
  principals: Principal[]; current: string; model: AccessModel; where: string; onClose(): void; onPick(name: string): Promise<void>;
}) {
  const options = principals.filter((p) => p.name !== current);
  const [from, setFrom] = useState(options.find((p) => !p.system)?.name ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} title={`Copy access in ${where}`}
      description={`${current} gets exactly the access the other account has here, directly. Nothing changes until you review and apply it.`}
      footer={<>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="primary" loading={busy} disabled={!from} onClick={async () => {
          setBusy(true);
          try {
            await onPick(from);
            onClose();
          } catch (e) {
            setError(message(e));
          } finally {
            setBusy(false);
          }
        }}><Users /> Copy access</Button>
      </>}>
      <div className="col gap-4">
        <Field label="Copy from">
          <select className="select" value={from} onChange={(e) => setFrom(e.target.value)}>
            {options.map((p) => {
              const [u, h] = splitName(p.name, model.hostPart);
              return <option key={p.name} value={p.name}>{u}{h ? `@${h}` : ""}{p.kind === "role" ? " (role)" : ""}</option>;
            })}
          </select>
        </Field>
        {error && <Alert kind="danger">{error}</Alert>}
      </div>
    </Dialog>
  );
}
