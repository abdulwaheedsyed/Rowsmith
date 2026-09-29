import { useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { KeyRound, ShieldCheck, Terminal, Copy, Check } from "lucide-react";
import qrcode from "qrcode-generator";
import { ApiError, post, setCsrf, get } from "../../lib/api";
import type { Me } from "../../lib/types";
import { AnvilMark, Alert, Button, Field, Wordmark } from "../../components/ui";
import "./auth.css";

const ENGINES = ["MySQL", "MariaDB", "PostgreSQL", "PostGIS", "SQL Server", "Oracle", "MongoDB", "BigQuery", "SQLite"];

function Frame({ children, wide }: { children: ReactNode; wide?: boolean }) {
  return (
    <div className="auth">
      <div className="auth__forge" aria-hidden="true">
        {Array.from({ length: 22 }).map((_, i) => (
          <div key={i} className="auth__row" style={{ animationDelay: `${i * 28}ms`, width: `${58 + ((i * 37) % 38)}%` }} />
        ))}
        <div className="auth__heat" />
      </div>
      <main className={`auth__panel ${wide ? "auth__panel--wide" : ""}`}>
        <div className="auth__brand">
          <AnvilMark size={34} />
          <Wordmark size={22} />
        </div>
        {children}
      </main>
      <footer className="auth__engines mono" aria-label="Supported databases">
        {ENGINES.map((e) => (
          <span key={e}>{e}</span>
        ))}
      </footer>
    </div>
  );
}

function useFinishAuth() {
  const qc = useQueryClient();
  return async () => {
    const me = await get<Me>("auth/me");
    setCsrf(me.csrf);
    qc.setQueryData(["me"], me);
  };
}

function errText(e: unknown) {
  return e instanceof ApiError ? e.message : "Something went wrong. Check your connection and try again.";
}

// ---- Password strength -------------------------------------------------------------

function strength(pw: string): { score: number; label: string } {
  if (!pw) return { score: 0, label: "" };
  let s = 0;
  if (pw.length >= 12) s++;
  if (pw.length >= 16) s++;
  if (/[a-z]/.test(pw) && /[A-Z]/.test(pw)) s++;
  if (/\d/.test(pw)) s++;
  if (/[^A-Za-z0-9]/.test(pw)) s++;
  if (pw.length < 12) s = Math.min(s, 1);
  const labels = ["Too short", "Weak", "Fair", "Good", "Strong", "Excellent"];
  return { score: s, label: labels[s] };
}

function StrengthMeter({ pw }: { pw: string }) {
  const { score, label } = strength(pw);
  return (
    <div className="strength" aria-live="polite">
      <div className="strength__bar">
        {[0, 1, 2, 3, 4].map((i) => (
          <span key={i} className={i < score ? "on" : ""} data-level={score} />
        ))}
      </div>
      <span className="strength__label">{pw ? label : "At least 12 characters"}</span>
    </div>
  );
}

// ---- Setup ----------------------------------------------------------------------------

export function Setup() {
  const finish = useFinishAuth();
  const [form, setForm] = useState({ token: "", name: "", email: "", password: "" });
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) => setForm({ ...form, [k]: e.target.value });
  const cmd = `docker logs rowsmith 2>&1 | grep "setup code"`;

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await post("setup", form);
      await finish();
    } catch (err) {
      setError(errText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Frame>
      <h1 className="auth__title display">Set up Rowsmith</h1>
      <p className="auth__lede">Create the owner account. You can invite your team afterwards.</p>
      <form className="auth__form" onSubmit={submit}>
        <Field label="Setup code" required help={<>Printed in the server log when Rowsmith first starts.</>}>
          <input className="input input--lg mono setup-code" value={form.token} onChange={set("token")} placeholder="XXXX-XXXX-XXXX" autoComplete="off" spellCheck={false} autoFocus required />
        </Field>
        <div className="auth__hint">
          <Terminal size={14} />
          <code className="grow truncate">{cmd}</code>
          <button type="button" className="btn btn--ghost btn--icon btn--sm" aria-label="Copy command" onClick={() => { navigator.clipboard?.writeText(cmd); setCopied(true); setTimeout(() => setCopied(false), 1500); }}>
            {copied ? <Check /> : <Copy />}
          </button>
        </div>
        <Field label="Your name" required>
          <input className="input input--lg" value={form.name} onChange={set("name")} autoComplete="name" required />
        </Field>
        <Field label="Email" required>
          <input className="input input--lg" type="email" value={form.email} onChange={set("email")} autoComplete="email" required />
        </Field>
        <Field label="Password" required>
          <input className="input input--lg" type="password" value={form.password} onChange={set("password")} autoComplete="new-password" minLength={12} required />
          <StrengthMeter pw={form.password} />
        </Field>
        {error && <Alert kind="danger">{error}</Alert>}
        <Button type="submit" variant="primary" size="lg" block loading={busy}>
          Create owner account
        </Button>
      </form>
    </Frame>
  );
}

// ---- Login --------------------------------------------------------------------------------

export function Login() {
  const finish = useFinishAuth();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await post("auth/login", { email, password });
      await finish();
    } catch (err) {
      setError(errText(err));
      setPassword("");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Frame>
      <h1 className="auth__title display">Sign in</h1>
      <form className="auth__form" onSubmit={submit}>
        <Field label="Email">
          <input className="input input--lg" type="email" value={email} onChange={(e) => setEmail(e.target.value)} autoComplete="username" autoFocus required />
        </Field>
        <Field label="Password">
          <input className="input input--lg" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />
        </Field>
        {error && <Alert kind="danger">{error}</Alert>}
        <Button type="submit" variant="primary" size="lg" block loading={busy}>
          Sign in
        </Button>
      </form>
      <p className="auth__foot muted">Forgot your password? An administrator can reset it for you.</p>
    </Frame>
  );
}

// ---- MFA challenge ----------------------------------------------------------------------------

export function MfaChallenge() {
  const finish = useFinishAuth();
  const qc = useQueryClient();
  const [code, setCode] = useState("");
  const [recovery, setRecovery] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e?: FormEvent) {
    e?.preventDefault();
    setBusy(true);
    setError("");
    try {
      const r = await post<{ recoveryCodesLeft: number }>("auth/mfa", { code });
      await finish();
      if (r.recoveryCodesLeft >= 0 && r.recoveryCodesLeft <= 3) {
        sessionStorage.setItem("rowsmith.lowRecovery", String(r.recoveryCodesLeft));
      }
    } catch (err) {
      setError(errText(err));
      setCode("");
      if (err instanceof ApiError && err.status === 401 && err.message.includes("sign in")) qc.invalidateQueries({ queryKey: ["me"] });
    } finally {
      setBusy(false);
    }
  }

  useEffect(() => {
    if (!recovery && code.replace(/\s/g, "").length === 6) submit();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [code]);

  return (
    <Frame>
      <h1 className="auth__title display">Two-step check</h1>
      <p className="auth__lede">
        {recovery ? "Enter one of the recovery codes you saved when you turned on two-step sign-in." : "Enter the 6-digit code from your authenticator app."}
      </p>
      <form className="auth__form" onSubmit={submit}>
        <input
          className="input input--lg mono otp"
          value={code}
          onChange={(e) => setCode(recovery ? e.target.value : e.target.value.replace(/[^\d]/g, "").slice(0, 6))}
          inputMode={recovery ? "text" : "numeric"}
          autoComplete="one-time-code"
          placeholder={recovery ? "xxxxx-xxxxx" : "000000"}
          aria-label={recovery ? "Recovery code" : "Authentication code"}
          autoFocus
        />
        {error && <Alert kind="danger">{error}</Alert>}
        <Button type="submit" variant="primary" size="lg" block loading={busy}>
          Verify
        </Button>
        <button type="button" className="btn btn--ghost btn--block" onClick={() => { setRecovery(!recovery); setCode(""); setError(""); }}>
          <KeyRound /> {recovery ? "Use authenticator code" : "Use a recovery code"}
        </button>
      </form>
      <button className="auth__foot link-button" onClick={async () => { await post("auth/logout"); qc.setQueryData(["me"], null); qc.invalidateQueries({ queryKey: ["me"] }); }}>
        Sign in as someone else
      </button>
    </Frame>
  );
}

// ---- MFA enrollment (used standalone when required, and from Account) --------------------------

export function QRCode({ text, size = 176 }: { text: string; size?: number }) {
  const svg = useMemo(() => {
    const q = qrcode(0, "M");
    q.addData(text);
    q.make();
    const n = q.getModuleCount();
    let path = "";
    for (let r = 0; r < n; r++) for (let c = 0; c < n; c++) if (q.isDark(r, c)) path += `M${c + 2} ${r + 2}h1v1h-1z`;
    return { n: n + 4, path };
  }, [text]);
  return (
    <svg className="qr" width={size} height={size} viewBox={`0 0 ${svg.n} ${svg.n}`} role="img" aria-label="QR code for your authenticator app" shapeRendering="crispEdges">
      <rect width={svg.n} height={svg.n} fill="#fff" />
      <path d={svg.path} fill="#0b0f17" />
    </svg>
  );
}

export function MfaEnroll({ onDone, standalone }: { onDone(codes: string[]): void; standalone?: boolean }) {
  const [setup, setSetup] = useState<{ secret: string; uri: string } | null>(null);
  const [code, setCode] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    post<{ secret: string; uri: string }>("me/mfa/setup").then(setSetup).catch((e) => setError(errText(e)));
  }, []);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const r = await post<{ recoveryCodes: string[] }>("me/mfa/enable", { code });
      onDone(r.recoveryCodes);
    } catch (err) {
      setError(errText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="enroll">
      {standalone && <Alert kind="info" title="Two-step sign-in is required">Your administrator requires an authenticator app for every account.</Alert>}
      <div className="enroll__grid">
        <div className="enroll__qr">{setup ? <QRCode text={setup.uri} /> : <div className="skeleton" style={{ width: 176, height: 176 }} />}</div>
        <ol className="enroll__steps">
          <li>Open an authenticator app (1Password, Google Authenticator, Authy, Microsoft Authenticator…).</li>
          <li>Scan the code, or enter this key manually:
            <code className="enroll__secret">{setup?.secret.replace(/(.{4})/g, "$1 ").trim() ?? "…"}</code>
          </li>
          <li>Enter the 6-digit code it shows.</li>
        </ol>
      </div>
      <form className="row gap-3" onSubmit={submit}>
        <input className="input input--lg mono otp otp--inline" value={code} onChange={(e) => setCode(e.target.value.replace(/[^\d]/g, "").slice(0, 6))} inputMode="numeric" placeholder="000000" aria-label="Authentication code" autoComplete="one-time-code" />
        <Button type="submit" variant="primary" size="lg" loading={busy} disabled={code.length !== 6}>
          <ShieldCheck /> Turn on
        </Button>
      </form>
      {error && <Alert kind="danger">{error}</Alert>}
    </div>
  );
}

export function RecoveryCodes({ codes, onDone }: { codes: string[]; onDone(): void }) {
  const [copied, setCopied] = useState(false);
  const text = codes.join("\n");
  return (
    <div className="col gap-5">
      <Alert kind="warn" title="Save these recovery codes now">
        Each code works once if you lose your authenticator. They will not be shown again.
      </Alert>
      <div className="recovery mono">{codes.map((c) => <span key={c}>{c}</span>)}</div>
      <div className="row gap-3">
        <Button onClick={() => { navigator.clipboard?.writeText(text); setCopied(true); }}>
          {copied ? <Check /> : <Copy />} {copied ? "Copied" : "Copy codes"}
        </Button>
        <Button onClick={() => { const a = document.createElement("a"); a.href = URL.createObjectURL(new Blob([text + "\n"], { type: "text/plain" })); a.download = "rowsmith-recovery-codes.txt"; a.click(); }}>
          Download .txt
        </Button>
        <span className="spacer" />
        <Button variant="primary" onClick={onDone}>I saved them</Button>
      </div>
    </div>
  );
}

export function EnrollScreen() {
  const finish = useFinishAuth();
  const [codes, setCodes] = useState<string[] | null>(null);
  return (
    <Frame wide>
      <h1 className="auth__title display">Secure your account</h1>
      {codes ? <RecoveryCodes codes={codes} onDone={finish} /> : <MfaEnroll standalone onDone={setCodes} />}
    </Frame>
  );
}

// ---- Forced password change ---------------------------------------------------------------------

export function ChangePasswordScreen() {
  const finish = useFinishAuth();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await post("me/password", { current, next });
      await finish();
    } catch (err) {
      setError(errText(err));
    } finally {
      setBusy(false);
    }
  }
  return (
    <Frame>
      <h1 className="auth__title display">Choose a new password</h1>
      <p className="auth__lede">Your account was created with a temporary password.</p>
      <form className="auth__form" onSubmit={submit}>
        <Field label="Temporary password">
          <input className="input input--lg" type="password" value={current} onChange={(e) => setCurrent(e.target.value)} autoComplete="current-password" autoFocus required />
        </Field>
        <Field label="New password">
          <input className="input input--lg" type="password" value={next} onChange={(e) => setNext(e.target.value)} autoComplete="new-password" minLength={12} required />
          <StrengthMeter pw={next} />
        </Field>
        {error && <Alert kind="danger">{error}</Alert>}
        <Button type="submit" variant="primary" size="lg" block loading={busy}>
          Save password
        </Button>
      </form>
    </Frame>
  );
}
