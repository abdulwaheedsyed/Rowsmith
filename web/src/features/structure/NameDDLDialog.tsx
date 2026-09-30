import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ApiError, post, stream } from "../../lib/api";
import { toast } from "../../lib/store";
import type { Connection, ObjectRef } from "../../lib/types";
import { Alert, Button, Dialog, Field, Spinner } from "../../components/ui";

type Action = "create_database" | "create_schema" | "rename";

const titles: Record<Action, string> = { create_database: "New database", create_schema: "New schema", rename: "Rename" };

/** Asks for one name, previews the engine's statement and runs it through the console. */
export function NameDDLDialog({ conn, action, target, onClose, onDone }: {
  conn: Connection; action: Action; target?: ObjectRef; onClose(): void; onDone?(name: string): void;
}) {
  const qc = useQueryClient();
  const [name, setName] = useState(action === "rename" ? target?.name ?? "" : "");
  const [collation, setCollation] = useState("");
  const [script, setScript] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const withCollation = action === "create_database" && (conn.driver === "mysql" || conn.driver === "mariadb" || conn.driver === "mssql");
  const unchanged = action === "rename" && name === target?.name;

  useEffect(() => {
    setScript("");
    setError("");
    if (!name.trim() || unchanged) return;
    const ac = new AbortController();
    const t = setTimeout(() => {
      const body = action === "rename"
        ? { action, ref: target, newName: name.trim() }
        : { action, ref: { database: target?.database ?? "", name: "" }, newName: name.trim(), options: collation ? { collation } : undefined };
      post<{ script: string }>(`c/${conn.id}/ddl`, body, ac.signal)
        .then((r) => setScript(r.script))
        .catch((e) => (e as Error).name !== "AbortError" && setError(e instanceof ApiError ? e.message : String(e)));
    }, 250);
    return () => {
      clearTimeout(t);
      ac.abort();
    };
  }, [name, collation, action, target, conn.id, unchanged]);

  const run = async () => {
    setBusy(true);
    setError("");
    let consoleId = "";
    let failed = "";
    try {
      const scope = action === "rename" ? { database: target?.database, schema: target?.schema } : { database: action === "create_schema" ? target?.database : undefined };
      for await (const ev of stream(`c/${conn.id}/query`, { tab: `ddl-name-${action}`, ...scope, sql: script, confirm: true })) {
        if (ev.t === "start") consoleId = ev.console;
        if (ev.t === "stmtEnd" && ev.error && !failed) failed = ev.error.message;
      }
    } catch (e) {
      failed = e instanceof ApiError ? e.message : String(e);
    } finally {
      if (consoleId) post(`c/${conn.id}/console/close`, { console: consoleId }).catch(() => {});
      setBusy(false);
    }
    if (failed) return setError(failed);
    for (const k of ["dbs", "schemas", "objects", "catalog", "describe"]) qc.invalidateQueries({ queryKey: [k, conn.id] });
    toast.success(action === "rename" ? `Renamed ${target?.name} to ${name}` : `Created ${name.trim()}`);
    onDone?.(name.trim());
    onClose();
  };

  const what = action === "rename" ? `${(target?.kind ?? "object").replace("_", " ")} ${target?.name}` : "";
  return (
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()} title={action === "rename" ? `Rename ${what}` : titles[action]}
      description={action === "create_schema" && target?.database ? `In ${target.database}.` : undefined}
      footer={<><Button onClick={onClose} disabled={busy}>Cancel</Button><Button variant={conn.environment === "production" ? "danger" : "primary"} disabled={!script || busy} loading={busy} onClick={run}>{action === "rename" ? "Rename" : "Create"}</Button></>}>
      <form className="col gap-4" onSubmit={(e) => { e.preventDefault(); if (script && !busy) run(); }}>
        {conn.environment === "production" && <Alert kind="danger" title="This is a production connection">{conn.name}</Alert>}
        <Field label={action === "rename" ? "New name" : "Name"}>
          <input className="input input--mono" value={name} onChange={(e) => setName(e.target.value)} autoFocus spellCheck={false} />
        </Field>
        {withCollation && (
          <Field label="Collation" help="Leave empty for the server default.">
            <input className="input input--mono" value={collation} onChange={(e) => setCollation(e.target.value)} placeholder={conn.driver === "mssql" ? "e.g. Latin1_General_100_CI_AS_SC_UTF8" : "e.g. utf8mb4_0900_ai_ci"} spellCheck={false} />
          </Field>
        )}
        {script ? <pre className="ddlpreview mono">{script.trim()}</pre> : name.trim() && !unchanged && !error ? <Spinner /> : null}
        {error && <Alert kind="danger">{error}</Alert>}
      </form>
    </Dialog>
  );
}
