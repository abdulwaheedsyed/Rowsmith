import { useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Plus, Timer, X } from "lucide-react";
import { ApiError, get, qs, stream } from "../../lib/api";
import { describeKey } from "../../lib/queries";
import { toast } from "../../lib/store";
import type { Connection, ObjectRef, Table } from "../../lib/types";
import { Alert, Button, Dialog, Field } from "../../components/ui";
import { openObject } from "../workspace/actions";
import "./keys.css";

// Key-value stores (Redis, Valkey): new keys and expiry, written as console
// commands so read-only rules, confirmations and the audit log apply.

export const KEY_TYPES: { kind: string; label: string }[] = [
  { kind: "string", label: "String" },
  { kind: "hash", label: "Hash" },
  { kind: "list", label: "List" },
  { kind: "set", label: "Set" },
  { kind: "zset", label: "Sorted set" },
  { kind: "stream", label: "Stream" },
];

/** Quotes an argument the way the console reads it back (redis-cli syntax). */
export function redisQuote(s: string): string {
  if (s !== "" && /^[\x21-\x7e]+$/.test(s) && !/["'\\]/.test(s) && !s.startsWith("#") && !s.startsWith("//")) return s;
  let out = '"';
  for (const ch of s) {
    const c = ch.codePointAt(0)!;
    if (ch === '"' || ch === "\\") out += "\\" + ch;
    else if (ch === "\n") out += "\\n";
    else if (ch === "\r") out += "\\r";
    else if (ch === "\t") out += "\\t";
    else if (c < 0x20 || c === 0x7f) out += "\\x" + c.toString(16).padStart(2, "0");
    else out += ch;
  }
  return out + '"';
}

/** Turns the navigator's filter into a SCAN pattern: text without glob
 *  characters matches anywhere in the key name. */
export function keyPattern(filter: string): string | undefined {
  const f = filter.trim();
  if (!f) return undefined;
  if (/[*?[]/.test(f)) return f;
  return `*${f.replace(/([\\\]])/g, "\\$1")}*`;
}

const UNITS = [
  { value: 1, label: "seconds" },
  { value: 60, label: "minutes" },
  { value: 3600, label: "hours" },
  { value: 86400, label: "days" },
];

/** Runs commands through the console and throws the first failure. */
async function runCommands(conn: Connection, database: string | undefined, script: string, tab: string) {
  let failed = "";
  for await (const ev of stream(`c/${conn.id}/query`, { tab, database, sql: script, confirm: true })) {
    if (ev.t === "stmtEnd" && ev.error && !failed) failed = ev.error.message;
  }
  if (failed) throw new Error(failed);
}

function ExpiryInput({ on, setOn, amount, setAmount, unit, setUnit }: {
  on: boolean; setOn(v: boolean): void; amount: string; setAmount(v: string): void; unit: number; setUnit(v: number): void;
}) {
  return (
    <div className="keyexp">
      <div className="segmented" role="group" aria-label="Expiry">
        <button type="button" aria-pressed={!on} onClick={() => setOn(false)}>Never expires</button>
        <button type="button" aria-pressed={on} onClick={() => setOn(true)}>Expires in</button>
      </div>
      {on && (
        <>
          <input className="input keyexp__amount" type="number" min={1} value={amount} onChange={(e) => setAmount(e.target.value)} aria-label="Expires in" />
          <select className="select keyexp__unit" value={unit} onChange={(e) => setUnit(Number(e.target.value))} aria-label="Unit">
            {UNITS.map((u) => <option key={u.value} value={u.value}>{u.label}</option>)}
          </select>
        </>
      )}
    </div>
  );
}

type Item = { a: string; b: string };

/** Creates a key of any type with its first values and an optional expiry. */
export function NewKeyDialog({ conn, database, initialType = "string", onClose }: { conn: Connection; database?: string; initialType?: string; onClose(): void }) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [type, setType] = useState(KEY_TYPES.some((t) => t.kind === initialType) ? initialType : "string");
  const [value, setValue] = useState("");
  const [items, setItems] = useState<Item[]>([{ a: "", b: "" }]);
  const [expires, setExpires] = useState(false);
  const [amount, setAmount] = useState("1");
  const [unit, setUnit] = useState(3600);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const pairs = type === "hash" || type === "zset" || type === "stream";
  const [first, second] = type === "zset" ? ["Member", "Score"] : type === "list" ? ["Value", ""] : type === "set" ? ["Member", ""] : ["Field", "Value"];
  const seconds = Math.round(Number(amount) * unit);

  const script = useMemo(() => {
    if (!name) return "";
    const k = redisQuote(name);
    const filled = items.filter((it) => it.a !== "" || (pairs && it.b !== ""));
    const lines: string[] = [];
    switch (type) {
      case "string":
        lines.push(`SET ${k} ${redisQuote(value)}${expires && seconds > 0 ? ` EX ${seconds}` : ""}`);
        break;
      case "hash":
        if (filled.length) lines.push(`HSET ${k} ${filled.map((it) => `${redisQuote(it.a)} ${redisQuote(it.b)}`).join(" ")}`);
        break;
      case "list":
        if (filled.length) lines.push(`RPUSH ${k} ${filled.map((it) => redisQuote(it.a)).join(" ")}`);
        break;
      case "set":
        if (filled.length) lines.push(`SADD ${k} ${filled.map((it) => redisQuote(it.a)).join(" ")}`);
        break;
      case "zset":
        if (filled.length) lines.push(`ZADD ${k} ${filled.map((it) => `${redisQuote(it.b.trim() || "0")} ${redisQuote(it.a)}`).join(" ")}`);
        break;
      case "stream":
        if (filled.length) lines.push(`XADD ${k} * ${filled.map((it) => `${redisQuote(it.a)} ${redisQuote(it.b)}`).join(" ")}`);
        break;
    }
    if (type !== "string" && lines.length && expires && seconds > 0) lines.push(`EXPIRE ${k} ${seconds}`);
    return lines.join("\n");
  }, [name, type, value, items, expires, seconds, pairs]);

  const missing = !name ? "Enter a key name."
    : type !== "string" && !script ? `Add at least one ${first.toLowerCase()}: Redis has no empty ${KEY_TYPES.find((t) => t.kind === type)!.label.toLowerCase()}s.`
    : type === "zset" && items.some((it) => it.a !== "" && it.b.trim() !== "" && !Number.isFinite(Number(it.b))) ? "Scores are numbers."
    : expires && !(seconds > 0) ? "Enter how long until the key expires."
    : "";

  const create = async () => {
    setBusy(true);
    setError("");
    try {
      // The commands would add to an existing key, or replace a string: refuse names in use.
      const exists = await get<Table>(`c/${conn.id}/describe${qs({ db: database, name })}`).then(() => true, (e) => (e instanceof ApiError && e.status < 500 ? false : Promise.reject(e)));
      if (exists) throw new Error(`A key named “${name}” already exists.`);
      await runCommands(conn, database, script, `key-new-${name}`);
      toast.success(`Created ${name}`);
      for (const key of ["objects", "dbs"]) qc.invalidateQueries({ queryKey: [key, conn.id] });
      openObject(conn, { database, name, kind: type }, "browse");
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const setItem = (i: number, p: Partial<Item>) => setItems((list) => list.map((it, j) => (j === i ? { ...it, ...p } : it)));
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} title="New key" description={database ? `In ${database}.` : undefined} width="wide"
      footer={<><Button onClick={onClose}>Cancel</Button><Button variant="primary" loading={busy} disabled={!!missing} onClick={create}><Plus /> Create key</Button></>}>
      <div className="col gap-4">
        <div className="keynew__head">
          <Field label="Key name" required>
            <input className="input input--mono" value={name} onChange={(e) => setName(e.target.value)} placeholder="user:42" autoFocus />
          </Field>
          <Field label="Type">
            <select className="select" value={type} onChange={(e) => setType(e.target.value)}>
              {KEY_TYPES.map((t) => <option key={t.kind} value={t.kind}>{t.label}</option>)}
            </select>
          </Field>
        </div>
        {type === "string" ? (
          <Field label="Value">
            <textarea className="input input--mono keynew__value" value={value} onChange={(e) => setValue(e.target.value)} rows={4} />
          </Field>
        ) : (
          <Field label={type === "stream" ? "First entry" : pairs ? `${first}s and ${second.toLowerCase()}s` : `${first}s`} group>
            <div className="keynew__items">
              {items.map((it, i) => (
                <div key={i} className={`keynew__item ${pairs ? "keynew__item--pair" : ""}`}>
                  <input className="input input--mono" value={it.a} placeholder={first.toLowerCase()} aria-label={`${first} ${i + 1}`} onChange={(e) => setItem(i, { a: e.target.value })} />
                  {pairs && <input className="input input--mono" value={it.b} placeholder={type === "zset" ? "0" : second.toLowerCase()} aria-label={`${second} ${i + 1}`} inputMode={type === "zset" ? "decimal" : undefined} onChange={(e) => setItem(i, { b: e.target.value })} />}
                  <Button variant="ghost" size="sm" icon aria-label="Remove" disabled={items.length === 1} onClick={() => setItems((l) => l.filter((_, j) => j !== i))}><X /></Button>
                </div>
              ))}
              <Button size="sm" variant="ghost" onClick={() => setItems((l) => [...l, { a: "", b: "" }])}><Plus /> Add {first.toLowerCase()}</Button>
            </div>
          </Field>
        )}
        <Field label="Expiry" group>
          <ExpiryInput on={expires} setOn={setExpires} amount={amount} setAmount={setAmount} unit={unit} setUnit={setUnit} />
        </Field>
        {script && <pre className="ddlpreview mono">{script}</pre>}
        {missing && name && <p className="faint">{missing}</p>}
        {error && <Alert kind="danger">{error}</Alert>}
      </div>
    </Dialog>
  );
}

/** Sets or removes a key's expiry. */
export function ExpiryDialog({ conn, target, current, onClose }: { conn: Connection; target: ObjectRef; current?: string; onClose(): void }) {
  const qc = useQueryClient();
  const [on, setOn] = useState(true);
  const [amount, setAmount] = useState("1");
  const [unit, setUnit] = useState(3600);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const seconds = Math.round(Number(amount) * unit);
  const k = redisQuote(target.name);
  const script = on ? `EXPIRE ${k} ${seconds > 0 ? seconds : "…"}` : `PERSIST ${k}`;
  const save = async () => {
    setBusy(true);
    setError("");
    try {
      await runCommands(conn, target.database, script, `key-expiry-${target.name}`);
      toast.success(on ? `${target.name} expires in ${amount} ${UNITS.find((u) => u.value === unit)!.label}` : `${target.name} no longer expires`);
      qc.invalidateQueries({ queryKey: describeKey(conn.id, target) });
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} title={`Expiry of ${target.name}`} description={current ? `Now: ${current === "never" ? "never expires" : `expires ${current}`}.` : undefined}
      footer={<><Button onClick={onClose}>Cancel</Button><Button variant="primary" loading={busy} disabled={on && !(seconds > 0)} onClick={save}><Timer /> Save expiry</Button></>}>
      <div className="col gap-4">
        <ExpiryInput on={on} setOn={setOn} amount={amount} setAmount={setAmount} unit={unit} setUnit={setUnit} />
        <pre className="ddlpreview mono">{script}</pre>
        {error && <Alert kind="danger">{error}</Alert>}
      </div>
    </Dialog>
  );
}
