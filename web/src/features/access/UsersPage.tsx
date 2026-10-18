import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { KeyRound, Lock, LockOpen, MoreHorizontal, Plus, RefreshCw, Search, ShieldAlert, Trash2, UserRound, Users, X, Copy, FileCode2, UserPlus } from "lucide-react";
import { get, qs } from "../../lib/api";
import { useDatabases, useSchemas, useServer } from "../../lib/queries";
import { toast, useWorkspace } from "../../lib/store";
import type { Connection } from "../../lib/types";
import { pluralize } from "../../lib/format";
import { Alert, Button, Dialog, Empty, Field, Menu, MenuContent, MenuItem, MenuSep, MenuTrigger, Spinner } from "../../components/ui";
import { newQueryTab } from "../workspace/actions";
import { LEVEL, splitName, useAccessModel, usePrincipals, type AccessModel, type AccountOp, type Principal } from "./api";
import { hasLevel } from "./model";
import { AccessEditor, type Scope } from "./AccessEditor";
import { NewPrincipalDialog, PasswordDialog, ReviewDialog } from "./dialogs";
import "./access.css";

/** Users and roles, and what each may do. Engines Rowsmith can't manage
 *  access on yet show the read-only list (fallback). */
export function UsersPage({ conn, fallback }: { conn: Connection; fallback: ReactNode }) {
  const m = useAccessModel(conn.id);
  if (m.isLoading) return <div className="panel__center"><Spinner large /></div>;
  if (m.error) return <div className="panel__pad"><Alert kind="danger" title="Could not read users">{(m.error as Error).message}</Alert></div>;
  if (!m.data?.supported || !m.data.model) return <>{fallback}</>;
  return <Access conn={conn} model={m.data.model} canChange={!!m.data.canChange && conn.access !== "read"} />;
}

type View = "access" | "account" | "sql";

function Access({ conn, model, canChange }: { conn: Connection; model: AccessModel; canChange: boolean }) {
  const ps = usePrincipals(conn.id);
  const me = useServer(conn.id).data?.server.user;
  const [selected, setSelected] = useState<string>();
  const [search, setSearch] = useState("");
  const [creating, setCreating] = useState<"user" | "role" | null>(null);
  const [dirty, setDirty] = useState(0);
  const [guard, setGuard] = useState<(() => void) | null>(null);
  const [view, setView] = useState<View>("access");
  const scope = useScope(conn, model);

  const list = useMemo(() => ps.data ?? [], [ps.data]);
  // Oracle upper-cases a new plain name, so a just-created account may differ in case.
  const current = list.find((p) => p.name === selected) ?? list.find((p) => p.name.toLowerCase() === selected?.toLowerCase())
    ?? list.find((p) => !p.system) ?? list[0];
  // Leaving unapplied changes asks first.
  const leave = (go: () => void) => (dirty ? setGuard(() => go) : go());
  const choose = (name: string) => leave(() => setSelected(name));

  const q = search.trim().toLowerCase();
  const shown = q ? list.filter((p) => p.name.toLowerCase().includes(q)) : list;
  const groups: [string, Principal[]][] = [
    ["Users", shown.filter((p) => !p.system && p.kind === "user")],
    ["Roles", shown.filter((p) => !p.system && p.kind === "role")],
    ["Built in", shown.filter((p) => p.system)],
  ];

  return (
    <div className="access">
      <aside className="access__list" aria-label="Users and roles">
        <div className="access__listhead">
          <h2 className="panel__title grow">Users &amp; roles</h2>
          <Button size="sm" variant="ghost" icon aria-label="Refresh" onClick={() => ps.refetch()}><RefreshCw className={ps.isFetching ? "spin" : ""} /></Button>
          {canChange && model.canCreate.length > 0 && (
            model.canCreate.length === 1
              ? <Button size="sm" onClick={() => setCreating("user")}><Plus /> New user</Button>
              : (
                <Menu>
                  <MenuTrigger asChild><Button size="sm"><Plus /> New</Button></MenuTrigger>
                  <MenuContent align="end">
                    <MenuItem icon={<UserRound />} onSelect={() => setCreating("user")}>New user…</MenuItem>
                    <MenuItem icon={<Users />} onSelect={() => setCreating("role")}>New role…</MenuItem>
                  </MenuContent>
                </Menu>
              )
          )}
        </div>
        <label className="access__search">
          <Search aria-hidden="true" />
          <input className="input" placeholder="Find a user or role" value={search} onChange={(e) => setSearch(e.target.value)} aria-label="Find a user or role" />
        </label>
        <div className="access__groups">
          {ps.isLoading && <div className="panel__center"><Spinner /></div>}
          {ps.error && <Alert kind="danger">{(ps.error as Error).message}</Alert>}
          {groups.map(([label, items]) => items.length > 0 && (
            <div key={label} className="access__group">
              <h3 className="eyebrow">{label} <span className="faint tnum">{items.length}</span></h3>
              {items.map((p) => <PrincipalRow key={p.name} p={p} model={model} me={p.name === me} active={p.name === current?.name} onClick={() => choose(p.name)} />)}
            </div>
          ))}
          {ps.data && !shown.length && <p className="muted access__none">No user or role matches “{search}”.</p>}
        </div>
      </aside>

      <section className="access__main">
        {current ? (
          <PrincipalView key={current.name} conn={conn} model={model} canChange={canChange && !current.system} principal={current} principals={list} me={me}
            view={view} onView={(v) => leave(() => { setDirty(0); setView(v); })} scope={scope} onDirty={setDirty} onOpen={choose} onLeave={leave} />
        ) : !ps.isLoading && (
          <Empty icon={<Users />} title="No users or roles">{canChange ? "Create one to give someone access." : null}</Empty>
        )}
      </section>

      {creating && <NewPrincipalDialog conn={conn} model={model} kind={creating} onClose={() => setCreating(null)} onCreated={(name) => { setSelected(name); setView("access"); }} />}
      <Dialog open={!!guard} onOpenChange={(o) => !o && setGuard(null)} title={`Discard ${pluralize(dirty, "change")}?`}
        description="They haven't been applied yet."
        footer={<>
          <Button onClick={() => setGuard(null)}>Keep editing</Button>
          <Button variant="danger" onClick={() => { const go = guard; setGuard(null); setDirty(0); go?.(); }}>Discard</Button>
        </>} />
    </div>
  );
}

function PrincipalRow({ p, model, me, active, onClick }: { p: Principal; model: AccessModel; me: boolean; active: boolean; onClick(): void }) {
  const [user, host] = splitName(p.name, model.hostPart);
  const a = p.attributes ?? {};
  return (
    <button type="button" className="aprin" aria-current={active || undefined} onClick={onClick}>
      <span className={`aprin__icon aprin__icon--${p.kind}`} aria-hidden="true">{p.kind === "role" ? <Users /> : <UserRound />}</span>
      <span className="aprin__name truncate">{user}{host && <span className="aprin__host">@{host}</span>}</span>
      {me && <span className="badge">you</span>}
      {a.superuser === true && <span className="badge badge--warn" title="Superuser">super</span>}
      {a.locked === true && p.kind === "user" && <Lock className="aprin__flag" aria-label="Locked" />}
    </button>
  );
}

/** The database (and schema) the access editor shows, kept while switching principals. */
function useScope(conn: Connection, model: AccessModel) {
  // Tables live in a schema where there are schemas (Oracle has no database level), else in a database.
  const needsSchema = hasLevel(model, LEVEL.schema);
  const needsDb = hasLevel(model, LEVEL.database) || (hasLevel(model, LEVEL.table) && !needsSchema);
  const server = useServer(conn.id).data?.server;
  const dbs = useDatabases(conn.id, needsDb);
  // Start where the navigator is, else where the connection starts.
  const nav = useWorkspace((s) => s.scope[conn.id]);
  const [database, setDatabase] = useState<string>();
  const known = (d?: string) => !!d && !!dbs.data?.some((x) => x.name === d);
  const configured = String((conn.params as Record<string, unknown>).database ?? "") || server?.database || "";
  const db = database ?? [nav?.database, configured].find(known) ?? dbs.data?.find((d) => !d.system)?.name ?? configured;
  const schemas = useSchemas(conn.id, needsDb ? db : undefined, needsSchema && (!needsDb || !!db));
  const [schema, setSchema] = useState<string>();
  const has = (x?: string) => !!x && !!schemas.data?.some((s) => s.name === x);
  const sc = [schema, db === nav?.database ? nav?.schema : undefined, "public"].find(has) ?? schemas.data?.find((s) => !s.system)?.name;
  return {
    needsDb, needsSchema, dbs: dbs.data ?? [], schemas: schemas.data ?? [],
    value: { database: needsDb ? db || undefined : undefined, schema: needsSchema ? sc : undefined } as Scope,
    setDatabase: (d: string) => { setDatabase(d); setSchema(undefined); },
    setSchema,
    loading: (needsDb && dbs.isLoading) || (needsSchema && schemas.isLoading),
  };
}

function PrincipalView({ conn, model, canChange, principal: p, principals, me, view, onView, scope, onDirty, onOpen, onLeave }: {
  conn: Connection; model: AccessModel; canChange: boolean; principal: Principal; principals: Principal[]; me?: string;
  view: View; onView(v: View): void; scope: ReturnType<typeof useScope>; onDirty(n: number): void; onOpen(name: string): void; onLeave(go: () => void): void;
}) {
  const [user, host] = splitName(p.name, model.hostPart);
  const [op, setOp] = useState<OpRequest | null>(null);
  const [password, setPassword] = useState(false);
  const a = p.attributes ?? {};
  const isMe = p.name === me;
  const locked = a.locked === true;
  // MySQL creates roles locked; that's how roles are, not a state to point out.
  const flags = model.attributes.filter((x) => x.type === "bool" && x.key !== "login" && a[x.key] === true && !(x.key === "locked" && p.kind === "role"));
  const where = [scope.value.database, scope.value.schema].filter(Boolean).join(".");
  const canLock = model.attributes.some((x) => x.key === "locked");

  return (
    <div className="aview">
      <header className="aview__head">
        <div className="grow" style={{ minWidth: 0 }}>
          <div className="eyebrow">{p.kind === "role" ? (p.login ? "role that can log in" : "role") : "user"}{p.system ? " · built in" : ""}{isMe ? " · this connection signs in as it" : ""}</div>
          <h1 className="aview__name display truncate">{user}{host && <span className="aview__host">@{host}</span>}</h1>
          {(flags.length > 0 || p.memberOf.length > 0 || !!a.valid_until) && (
            <div className="aview__facts">
              {flags.map((f) => <span key={f.key} className={`badge ${f.key === "locked" || f.key === "superuser" ? "badge--warn" : ""}`}>{f.label}</span>)}
              {!!a.valid_until && <span className="badge">Password valid until {String(a.valid_until)}</span>}
              {p.memberOf.length > 0 && <span className="faint">Member of {p.memberOf.map((r, i) => <span key={r}>{i > 0 && ", "}<button type="button" className="alink" onClick={() => onOpen(r)}>{r}</button></span>)}</span>}
            </div>
          )}
        </div>
        {canChange && (
          <div className="row gap-3">
            {model.password && (p.kind === "user" || p.login) && <Button onClick={() => setPassword(true)}><KeyRound /> Change password</Button>}
            <Menu>
              <MenuTrigger asChild><Button icon aria-label="More actions"><MoreHorizontal /></Button></MenuTrigger>
              <MenuContent align="end">
                {canLock && p.kind === "user" && (
                  <MenuItem icon={locked ? <LockOpen /> : <Lock />} disabled={isMe && !locked}
                    onSelect={() => setOp({
                      title: `${locked ? "Unlock" : "Lock"} ${p.name}?`, verb: locked ? "Unlock" : "Lock", done: `${locked ? "Unlocked" : "Locked"} ${p.name}`,
                      description: locked ? "It can sign in again." : "It can't sign in until it's unlocked. Sessions already signed in stay open.",
                      req: { op: "alter", name: p.name, attrs: { locked: String(!locked) } },
                    })}>
                    {locked ? "Unlock" : "Lock"}…
                  </MenuItem>
                )}
                <MenuItem icon={<FileCode2 />} onSelect={() => onView("sql")}>Show grants as SQL</MenuItem>
                <MenuSep />
                <MenuItem icon={<Trash2 />} danger disabled={isMe} hint={isMe ? "signed in as it" : undefined}
                  onSelect={() => setOp({
                    title: `Drop ${p.kind} ${p.name}?`, verb: `Drop ${p.kind}`, done: `Dropped ${p.name}`, confirm: user, danger: true,
                    description: p.kind === "role" ? "Members lose the access they had through it." : "The account is removed and can't sign in again.",
                    req: { op: "drop", name: p.name, kind: p.kind }, database: scope.value.database,
                  })}>
                  Drop {p.kind}…
                </MenuItem>
              </MenuContent>
            </Menu>
          </div>
        )}
      </header>

      <div className="page__tabs aview__tabs" role="tablist" aria-label="Sections">
        {([["access", "Access"], ["account", "Account"], ["sql", "As SQL"]] as [View, string][]).map(([v, label]) => (
          <button key={v} role="tab" type="button" className="page__tab" aria-selected={view === v} onClick={() => view !== v && onView(v)}>{label}</button>
        ))}
        {view === "access" && !p.serverOnly && (scope.needsDb || scope.needsSchema) && (
          <div className="aview__scope">
            {scope.needsDb && (
              <Field label="Database">
                <select className="select" value={scope.value.database ?? ""} onChange={(e) => { const d = e.target.value; onLeave(() => { onDirty(0); scope.setDatabase(d); }); }}>
                  {scope.dbs.map((d) => <option key={d.name} value={d.name}>{d.name}</option>)}
                </select>
              </Field>
            )}
            {scope.needsSchema && (
              <Field label="Schema">
                <select className="select" value={scope.value.schema ?? ""} onChange={(e) => { const s = e.target.value; onLeave(() => { onDirty(0); scope.setSchema(s); }); }}>
                  {scope.schemas.map((s) => <option key={s.name} value={s.name}>{s.name}</option>)}
                </select>
              </Field>
            )}
          </div>
        )}
      </div>

      <div className="aview__body">
        {p.system && <Alert kind="info">A built-in account. Rowsmith shows its access but doesn't change it.</Alert>}
        {view === "access" && (scope.loading ? <Spinner large /> : (
          <AccessEditor key={`${scope.value.database}|${scope.value.schema}`} conn={conn} model={model} editable={canChange} principal={p} principals={principals}
            scope={scope.value} scopeLabel={where || "the server"} onPending={onDirty} onOpenPrincipal={onOpen} />
        ))}
        {view === "account" && <AccountView model={model} canChange={canChange} principal={p} principals={principals} onDirty={onDirty} onOp={setOp} onOpen={onOpen} />}
        {view === "sql" && <GrantsSql conn={conn} principal={p.name} />}
      </div>

      {op && (op.req.op === "drop"
        ? <DropDialog conn={conn} model={model} op={op} onClose={() => setOp(null)} />
        : <ReviewDialog conn={conn} title={op.title} description={op.description} req={{ account: op.req, database: op.database }} verb={op.verb} done={op.done}
          confirmName={op.confirm} danger={op.danger} onClose={() => setOp(null)} />)}
      {password && <PasswordDialog conn={conn} principal={p.name} onClose={() => setPassword(false)} />}
    </div>
  );
}

type OpRequest = { title: string; req: AccountOp; verb: string; done: string; confirm?: string; danger?: boolean; description?: string; database?: string };

/** Dropping an account, with the engine's clean-up choices (ticked to start). */
function DropDialog({ conn, model, op, onClose }: { conn: Connection; model: AccessModel; op: OpRequest; onClose(): void }) {
  const options = model.dropOptions ?? [];
  const [on, setOn] = useState<Record<string, boolean>>(() => Object.fromEntries(options.map((o) => [o.key, o.default === "true"])));
  const attrs = Object.fromEntries(options.filter((o) => on[o.key]).map((o) => [o.key, "true"]));
  const req = { account: { ...op.req, ...(Object.keys(attrs).length ? { attrs } : {}) }, database: op.database };
  return (
    <ReviewDialog conn={conn} title={op.title} description={op.description} req={req} verb={op.verb} done={op.done} confirmName={op.confirm} danger onClose={onClose}>
      {options.map((o) => (
        <div key={o.key}>
          <label className="check"><input type="checkbox" checked={!!on[o.key]} onChange={(e) => setOn({ ...on, [o.key]: e.target.checked })} /> {o.label}{op.database ? ` (${op.database})` : ""}</label>
          {o.help && <p className="field__help adrop__help">{o.help}</p>}
        </div>
      ))}
    </ReviewDialog>
  );
}

function AccountView({ model, canChange, principal: p, principals, onDirty, onOp, onOpen }: {
  model: AccessModel; canChange: boolean; principal: Principal; principals: Principal[]; onDirty(n: number): void; onOp(op: OpRequest): void; onOpen(name: string): void;
}) {
  // Server roles (SQL Server) have no login settings.
  const attributes = useMemo(() => (p.serverOnly ? [] : model.attributes), [p.serverOnly, model.attributes]);
  const initial = useMemo(() => Object.fromEntries(attributes.map((x) => [x.key, attrText(p.attributes?.[x.key])])), [attributes, p]);
  const [values, setValues] = useState(initial);
  useEffect(() => setValues(initial), [initial]);
  const changed = Object.fromEntries(Object.entries(values).filter(([k, v]) => v !== initial[k]));
  const n = Object.keys(changed).length;
  useEffect(() => onDirty(n), [n, onDirty]);
  // Built-in roles (Oracle's CONNECT, SQL Server's sysadmin) are the ones most often joined.
  const roles = principals.filter((r) => r.name !== p.name && (r.kind === "role" || !model.hostPart) && !p.memberOf.includes(r.name) && r.name !== "public");
  const members = principals.filter((m) => m.memberOf.includes(p.name));
  const candidates = principals.filter((m) => m.name !== p.name && !m.system && !m.serverOnly && !m.memberOf.includes(p.name));
  const [addRole, setAddRole] = useState("");
  const [addMember, setAddMember] = useState("");

  return (
    <div className="aaccount">
      {attributes.length > 0 && (
        <section className="aaccount__section">
          <h3 className="alevel__title">Settings</h3>
          <form className="aaccount__attrs" onSubmit={(e) => {
            e.preventDefault();
            if (n) onOp({ title: `Change ${pluralize(n, "setting")} for ${p.name}?`, verb: "Save changes", done: `Saved settings for ${p.name}`, req: { op: "alter", name: p.name, attrs: changed } });
          }}>
            {attributes.map((x) => (
              <div key={x.key} className="aaccount__attr">
                {x.type === "bool" ? (
                  <label className="switch">
                    <input type="checkbox" disabled={!canChange} checked={values[x.key] === "true"} onChange={(e) => setValues({ ...values, [x.key]: String(e.target.checked) })} />
                    {x.label}
                  </label>
                ) : (
                  <Field label={x.label}>
                    <input className="input" type={x.type === "int" ? "number" : x.type === "date" ? "date" : "text"} disabled={!canChange} value={values[x.key] ?? ""}
                      onChange={(e) => setValues({ ...values, [x.key]: e.target.value })} />
                  </Field>
                )}
                {x.help && <p className="field__help">{x.help}</p>}
              </div>
            ))}
            {canChange && (
              <div className="row gap-3 aaccount__save">
                <Button type="submit" variant="primary" disabled={!n}>Save changes</Button>
                {n > 0 && <Button variant="ghost" onClick={() => setValues(initial)}>Discard</Button>}
              </div>
            )}
          </form>
        </section>
      )}

      {model.roles && (
        <section className="aaccount__section">
          <h3 className="alevel__title">Member of</h3>
          <p className="alevel__help">{p.name} has everything granted to these roles.</p>
          <div className="achips">
            {p.memberOf.length === 0 && <span className="muted">No roles.</span>}
            {p.memberOf.map((r) => (
              <span key={r} className="arole">
                <button type="button" className="alink" onClick={() => onOpen(r)}><Users /> {r}</button>
                {canChange && (
                  <button type="button" className="arole__x" aria-label={`Remove from ${r}`}
                    onClick={() => onOp({ title: `Remove ${p.name} from ${r}?`, verb: "Remove", done: `Removed ${p.name} from ${r}`, req: { op: "remove_role", name: p.name, kind: p.kind, role: r } })}><X /></button>
                )}
              </span>
            ))}
          </div>
          {canChange && roles.length > 0 && (
            <div className="row gap-3 aaccount__add">
              <select className="select" value={addRole} onChange={(e) => setAddRole(e.target.value)} aria-label="Role to join">
                <option value="">Choose a role…</option>
                {roles.map((r) => <option key={r.name} value={r.name}>{r.name}</option>)}
              </select>
              <Button disabled={!addRole} onClick={() => { onOp({ title: `Add ${p.name} to ${addRole}?`, verb: "Add", done: `Added ${p.name} to ${addRole}`, req: { op: "add_role", name: p.name, kind: p.kind, role: addRole } }); setAddRole(""); }}>
                <Plus /> Add to role
              </Button>
            </div>
          )}
        </section>
      )}

      {model.roles && (p.kind === "role" || members.length > 0) && (
        <section className="aaccount__section">
          <h3 className="alevel__title">Members <span className="faint tnum">{members.length}</span></h3>
          <div className="achips">
            {members.length === 0 && <span className="muted">Nobody yet.</span>}
            {members.map((m) => (
              <span key={m.name} className="arole">
                <button type="button" className="alink" onClick={() => onOpen(m.name)}>{m.kind === "role" ? <Users /> : <UserRound />} {m.name}</button>
                {canChange && (
                  <button type="button" className="arole__x" aria-label={`Remove ${m.name}`}
                    onClick={() => onOp({ title: `Remove ${m.name} from ${p.name}?`, verb: "Remove", done: `Removed ${m.name} from ${p.name}`, req: { op: "remove_role", name: m.name, kind: m.kind, role: p.name } })}><X /></button>
                )}
              </span>
            ))}
          </div>
          {canChange && candidates.length > 0 && (
            <div className="row gap-3 aaccount__add">
              <select className="select" value={addMember} onChange={(e) => setAddMember(e.target.value)} aria-label="Member to add">
                <option value="">Choose a user or role…</option>
                {candidates.map((m) => <option key={m.name} value={m.name}>{m.name}</option>)}
              </select>
              <Button disabled={!addMember} onClick={() => { onOp({ title: `Add ${addMember} to ${p.name}?`, verb: "Add", done: `Added ${addMember} to ${p.name}`, req: { op: "add_role", name: addMember, kind: principals.find((m) => m.name === addMember)?.kind, role: p.name } }); setAddMember(""); }}>
                <UserPlus /> Add member
              </Button>
            </div>
          )}
        </section>
      )}
      {!attributes.length && !model.roles && <p className="muted">Nothing to set for this account.</p>}
      {!canChange && !p.system && <p className="anote"><ShieldAlert /> This connection can't change accounts.</p>}
    </div>
  );
}

const attrText = (v: unknown) => (v === undefined || v === null ? "" : String(v));

function GrantsSql({ conn, principal }: { conn: Connection; principal: string }) {
  const q = useQuery({ queryKey: ["grants", conn.id, principal], queryFn: () => get<{ grants: string[] }>(`c/${conn.id}/db-users/grants${qs({ user: principal })}`) });
  const text = (q.data?.grants ?? []).map((g) => g.replace(/;?\s*$/, ";")).join("\n");
  return (
    <div className="asql">
      <div className="row gap-3">
        <p className="muted grow">Everything granted to {principal}, as the server reports it.</p>
        <Button size="sm" variant="ghost" disabled={!text} onClick={() => { navigator.clipboard?.writeText(text); toast.success("Grants copied"); }}><Copy /> Copy</Button>
        <Button size="sm" variant="ghost" disabled={!text} onClick={() => newQueryTab(conn, { sql: text, title: `Grants ${principal}` })}><FileCode2 /> Open in editor</Button>
      </div>
      {q.isLoading && <Spinner />}
      {q.error && <Alert kind="danger">{(q.error as Error).message}</Alert>}
      {q.data?.grants.length === 0 && <p className="muted">No grants found.</p>}
      {text && <pre className="apreview mono">{text}</pre>}
    </div>
  );
}
