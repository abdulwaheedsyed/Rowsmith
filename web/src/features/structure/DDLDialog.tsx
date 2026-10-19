import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Trash2, Scissors } from "lucide-react";
import { ApiError, post, stream } from "../../lib/api";
import { toast, useWorkspace } from "../../lib/store";
import type { Connection, ObjectRef } from "../../lib/types";
import { Alert, Button, Dialog, Field, Spinner } from "../../components/ui";
import { useDriver } from "../../lib/queries";

export type DDLAction = "drop" | "truncate" | "drop_database" | "drop_schema";

/** The database to run a DROP DATABASE from: PostgreSQL and SQL Server
 *  refuse to drop the database a session is using. */
function runFrom(conn: Connection, target: string): string | undefined {
  const configured = String((conn.params as Record<string, unknown>).database ?? "");
  switch (conn.driver) {
    case "postgres":
      return [configured, "postgres", "template1"].find((d) => d && d !== target);
    case "yugabytedb":
      return [configured, "yugabyte", "postgres"].find((d) => d && d !== target);
    case "cockroachdb":
      return [configured, "defaultdb", "postgres"].find((d) => d && d !== target);
    case "mssql":
      return "master";
    case "mysql":
    case "mariadb":
    case "tidb":
      return configured && configured !== target ? configured : undefined;
  }
  return undefined;
}

/** Previews generated DDL, asks for the name, then runs it through the
 *  console so read-only rules, confirmations and the audit log apply. */
export function DDLDialog({ conn, action, target, onClose, onDone }: { conn: Connection; action: DDLAction; target: ObjectRef; onClose(): void; onDone?(): void }) {
  const qc = useQueryClient();
  const [script, setScript] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [typed, setTyped] = useState("");
  const [cascade, setCascade] = useState(false);
  const [force, setForce] = useState(false);
  const [busy, setBusy] = useState(false);

  const keys = !!useDriver(conn.driver)?.caps.keys;
  const isDb = action === "drop_database";
  const isSchema = action === "drop_schema";
  const name = isDb ? target.database! : isSchema ? target.schema! : target.name;
  const what = isDb ? "database" : isSchema ? (conn.driver === "bigquery" ? "dataset" : "schema") : (target.kind ?? "object").replace("_", " ");
  const startsHere = isDb && String((conn.params as Record<string, unknown>).database ?? "") === name;
  const pgFamily = ["postgres", "cockroachdb", "yugabytedb"].includes(conn.driver);
  const canCascade = (action === "drop" && pgFamily) || (isSchema && (pgFamily || conn.driver === "bigquery" || conn.driver === "snowflake"));
  const canForce = isDb && ["postgres", "yugabytedb", "mssql"].includes(conn.driver);

  useEffect(() => {
    setScript(null);
    setError("");
    post<{ script: string }>(`c/${conn.id}/ddl`, { action, ref: target, cascade, force })
      .then((r) => setScript(r.script))
      .catch((e) => setError(e instanceof ApiError ? e.message : String(e)));
  }, [conn.id, action, target, cascade, force]);

  // Tabs showing what was dropped have nothing left to show.
  const closeTabs = () => {
    const ws = useWorkspace.getState();
    for (const t of ws.tabs) {
      if (t.connId !== conn.id) continue;
      const db = t.ref?.database ?? t.database;
      const schema = t.ref?.schema ?? t.schema;
      const hit = isDb ? db === name
        : isSchema ? db === target.database && schema === name
        : action === "drop" && t.ref?.name === target.name && (t.ref.database ?? "") === (target.database ?? "") && (t.ref.schema ?? "") === (target.schema ?? "");
      if (hit) ws.closeTab(t.id);
    }
  };

  const run = async () => {
    if (!script) return;
    setBusy(true);
    setError("");
    try {
      let failed = "";
      const database = isDb ? runFrom(conn, name) : target.database;
      for await (const ev of stream(`c/${conn.id}/query`, { tab: `ddl-${what}-${name}`, database, schema: isDb || isSchema ? undefined : target.schema, sql: script, confirm: true })) {
        if (ev.t === "stmtEnd" && ev.error) failed = ev.error.message;
      }
      if (failed) {
        if (canForce && !force && /being accessed by other users|currently in use/i.test(failed)) {
          failed += " Tick “Disconnect everyone using it first” to end those sessions.";
        }
        throw new Error(failed);
      }
      toast.success(action === "truncate" ? `Emptied ${name}` : keys ? `Deleted ${name}` : `Dropped ${what} ${name}`);
      closeTabs();
      for (const key of ["browse", "count", "objects", "dbs", "schemas", "describe", "catalog"]) qc.invalidateQueries({ queryKey: [key, conn.id] });
      onDone?.();
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const verb = action === "truncate" ? "Empty" : keys && action === "drop" ? "Delete" : "Drop";
  const description = action === "truncate" ? "Every row is permanently deleted. The structure stays."
    : keys && action === "drop" ? "The key and its value are permanently deleted."
    : isDb ? "The database and everything in it, including all its tables and data, is permanently removed."
    : isSchema ? `The ${what} and everything in it is permanently removed.`
    : "The object and everything in it is permanently removed.";
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} title={`${verb} ${action === "drop" || action === "truncate" ? "" : what + " "}${name}?`}
      description={description}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="danger" loading={busy} disabled={!script || typed !== name} onClick={run}>
            {action === "truncate" ? <Scissors /> : <Trash2 />} {verb} {name}
          </Button>
        </>
      }>
      <div className="col gap-4">
        {conn.environment === "production" && <Alert kind="danger" title="This is a production connection">{conn.name}</Alert>}
        {startsHere && <Alert kind="warn">This connection starts in {name}. Once it's gone, edit the connection to start in another database.</Alert>}
        {script ? <pre className="ddlpreview mono">{script.trim()}</pre> : !error && <Spinner />}
        {canCascade && (
          <label className="check"><input type="checkbox" checked={cascade} onChange={(e) => setCascade(e.target.checked)} /> {isSchema ? "Also drop everything in it (CASCADE)" : "Also drop dependent objects (CASCADE)"}</label>
        )}
        {canForce && (
          <label className="check"><input type="checkbox" checked={force} onChange={(e) => setForce(e.target.checked)} /> Disconnect everyone using it first <span className="faint">(their open transactions are rolled back)</span></label>
        )}
        <Field label={<>Type <b className="mono">{name}</b> to confirm</>}>
          <input className="input" value={typed} onChange={(e) => setTyped(e.target.value)} autoFocus />
        </Field>
        {error && <Alert kind="danger">{error}</Alert>}
      </div>
    </Dialog>
  );
}
