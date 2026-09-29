import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ShieldCheck, ShieldOff, KeyRound, Monitor, Moon, Sun, LogOut } from "lucide-react";
import { ApiError, del, get, post } from "../../lib/api";
import type { Me } from "../../lib/types";
import { ago } from "../../lib/format";
import { toast, useUI } from "../../lib/store";
import { Alert, Button, Dialog, Field, Spinner } from "../../components/ui";
import { MfaEnroll, RecoveryCodes } from "../auth/Auth";

export function Account({ me }: { me: Me }) {
  const qc = useQueryClient();
  const { theme, setTheme } = useUI();
  const [pw, setPw] = useState({ current: "", next: "" });
  const [pwError, setPwError] = useState("");
  const [enroll, setEnroll] = useState(false);
  const [codes, setCodes] = useState<string[] | null>(null);
  const [disable, setDisable] = useState(false);
  const [regen, setRegen] = useState(false);
  const [confirmPw, setConfirmPw] = useState("");
  const sessions = useQuery({ queryKey: ["my-sessions"], queryFn: () => get<{ id: string; createdAt: number; lastSeenAt: number; ip: string; userAgent: string; current: boolean }[]>("me/sessions") });
  const refreshMe = () => qc.invalidateQueries({ queryKey: ["me"] });

  return (
    <div className="page">
      <header className="page__head">
        <div>
          <h1 className="page__title display">Account & security</h1>
          <p className="page__sub">{me.user.name} · {me.user.email} · {me.user.role}</p>
        </div>
      </header>
      <div className="pgrid">
        <section className="card pcard">
          <h2 className="pcard__title">Two-step sign-in</h2>
          <p className="pcard__desc">An authenticator app code is needed after your password, so a leaked password alone cannot open your connections.</p>
          {me.user.mfaEnabled ? (
            <div className="col gap-4">
              <Alert kind="success" title="On">{me.recoveryCodesLeft} recovery code{me.recoveryCodesLeft === 1 ? "" : "s"} left.</Alert>
              <div className="row gap-3">
                <Button onClick={() => { setConfirmPw(""); setRegen(true); }}><KeyRound /> New recovery codes</Button>
                <Button variant="ghost" onClick={() => { setConfirmPw(""); setDisable(true); }}><ShieldOff /> Turn off</Button>
              </div>
            </div>
          ) : enroll ? (
            codes ? <RecoveryCodes codes={codes} onDone={() => { setCodes(null); setEnroll(false); refreshMe(); }} /> : <MfaEnroll onDone={(c) => { setCodes(c); toast.success("Two-step sign-in is on"); }} />
          ) : (
            <Button variant="primary" onClick={() => setEnroll(true)}><ShieldCheck /> Set up two-step sign-in</Button>
          )}
        </section>

        <section className="card pcard">
          <h2 className="pcard__title">Password</h2>
          <p className="pcard__desc">Changing it signs you out everywhere else.</p>
          <form className="col gap-4" onSubmit={async (e) => {
            e.preventDefault();
            setPwError("");
            try {
              await post("me/password", pw);
              setPw({ current: "", next: "" });
              toast.success("Password changed", "Other sessions were signed out.");
              qc.invalidateQueries({ queryKey: ["my-sessions"] });
            } catch (err) {
              setPwError(err instanceof ApiError ? err.message : String(err));
            }
          }}>
            <Field label="Current password"><input className="input" type="password" autoComplete="current-password" value={pw.current} onChange={(e) => setPw({ ...pw, current: e.target.value })} required /></Field>
            <Field label="New password" help="At least 12 characters."><input className="input" type="password" autoComplete="new-password" minLength={12} value={pw.next} onChange={(e) => setPw({ ...pw, next: e.target.value })} required /></Field>
            {pwError && <Alert kind="danger">{pwError}</Alert>}
            <div><Button type="submit" variant="primary">Change password</Button></div>
          </form>
        </section>

        <section className="card pcard">
          <h2 className="pcard__title">Where you're signed in</h2>
          {sessions.isLoading && <Spinner />}
          <div className="sessions">
            {sessions.data?.map((s) => (
              <div key={s.id} className="session">
                <Monitor />
                <div className="grow" style={{ minWidth: 0 }}>
                  <div className="truncate session__ua">{browserName(s.userAgent)}{s.current && <span className="badge badge--accent" style={{ marginLeft: 8 }}>this device</span>}</div>
                  <div className="faint session__meta">{s.ip} · active {ago(s.lastSeenAt)} · signed in {ago(s.createdAt)}</div>
                </div>
                {!s.current && (
                  <Button size="sm" variant="ghost" onClick={async () => {
                    await del(`me/sessions/${s.id}`);
                    qc.invalidateQueries({ queryKey: ["my-sessions"] });
                    toast.success("Signed out that session");
                  }}><LogOut /> Sign out</Button>
                )}
              </div>
            ))}
          </div>
        </section>

        <section className="card pcard">
          <h2 className="pcard__title">Appearance</h2>
          <div className="segmented" role="group" aria-label="Theme">
            <button aria-pressed={theme === "system"} onClick={() => setTheme("system")}><Monitor /> System</button>
            <button aria-pressed={theme === "dark"} onClick={() => setTheme("dark")}><Moon /> Dark</button>
            <button aria-pressed={theme === "light"} onClick={() => setTheme("light")}><Sun /> Light</button>
          </div>
        </section>
      </div>

      <Dialog open={disable || regen} onOpenChange={(o) => { if (!o) { setDisable(false); setRegen(false); } }}
        title={disable ? "Turn off two-step sign-in?" : "Create new recovery codes?"}
        description={disable ? "Your account will be protected by your password only." : "Your old recovery codes stop working."}
        footer={<>
          <Button onClick={() => { setDisable(false); setRegen(false); }}>Cancel</Button>
          <Button variant={disable ? "danger" : "primary"} onClick={async () => {
            try {
              if (disable) {
                await post("me/mfa/disable", { current: confirmPw });
                toast.success("Two-step sign-in is off");
              } else {
                const r = await post<{ recoveryCodes: string[] }>("me/recovery-codes", { current: confirmPw });
                setCodes(r.recoveryCodes);
                setEnroll(true);
              }
              setDisable(false);
              setRegen(false);
              refreshMe();
            } catch (e) {
              toast.error("Could not update", (e as Error).message);
            }
          }}>{disable ? "Turn off" : "Create codes"}</Button>
        </>}>
        <Field label="Confirm with your password"><input className="input" type="password" autoComplete="current-password" value={confirmPw} onChange={(e) => setConfirmPw(e.target.value)} autoFocus /></Field>
      </Dialog>
      {codes && me.user.mfaEnabled && (
        <Dialog open onOpenChange={() => setCodes(null)} title="Your new recovery codes">
          <RecoveryCodes codes={codes} onDone={() => { setCodes(null); setEnroll(false); }} />
        </Dialog>
      )}
    </div>
  );
}

function browserName(ua: string) {
  const b = /Edg\//.test(ua) ? "Edge" : /Chrome\//.test(ua) ? "Chrome" : /Firefox\//.test(ua) ? "Firefox" : /Safari\//.test(ua) ? "Safari" : "Browser";
  const os = /Windows/.test(ua) ? "Windows" : /Mac OS X/.test(ua) ? "macOS" : /Android/.test(ua) ? "Android" : /iPhone|iPad/.test(ua) ? "iOS" : /Linux/.test(ua) ? "Linux" : "";
  return os ? `${b} on ${os}` : b;
}
