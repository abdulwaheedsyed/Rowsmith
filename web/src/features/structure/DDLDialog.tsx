import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Trash2, Scissors } from "lucide-react";
import { ApiError, post, stream } from "../../lib/api";
import { toast } from "../../lib/store";
import type { Connection, ObjectRef } from "../../lib/types";
import { Alert, Button, Dialog, Field, Spinner } from "../../components/ui";

/** Previews generated DDL, asks for the object name, then runs it through
 *  the console so read-only rules, confirmations and the audit log apply. */
export function DDLDialog({ conn, action, target, onClose, onDone }: { conn: Connection; action: "drop" | "truncate"; target: ObjectRef; onClose(): void; onDone?(): void }) {
  const qc = useQueryClient();
  const [stmts, setStmts] = useState<string[] | null>(null);
  const [error, setError] = useState("");
  const [typed, setTyped] = useState("");
  const [cascade, setCascade] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    setStmts(null);
    post<{ statements: string[] }>(`c/${conn.id}/ddl`, { action, ref: target, cascade })
      .then((r) => setStmts(r.statements))
      .catch((e) => setError(e instanceof ApiError ? e.message : String(e)));
  }, [conn.id, action, target, cascade]);

  const run = async () => {
    if (!stmts) return;
    setBusy(true);
    setError("");
    try {
      let failed = "";
      for await (const ev of stream(`c/${conn.id}/query`, { tab: `ddl-${target.name}`, database: target.database, schema: target.schema, sql: stmts.join(";\n"), confirm: true })) {
        if (ev.t === "stmtEnd" && ev.error) failed = ev.error.message;
      }
      if (failed) throw new Error(failed);
      toast.success(action === "drop" ? `Dropped ${target.name}` : `Emptied ${target.name}`);
      qc.invalidateQueries({ queryKey: ["browse", conn.id] });
      qc.invalidateQueries({ queryKey: ["count", conn.id] });
      onDone?.();
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const verb = action === "drop" ? "Drop" : "Empty";
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} title={`${verb} ${target.name}?`}
      description={action === "drop" ? "The object and everything in it is permanently removed." : "Every row is permanently deleted. The structure stays."}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="danger" loading={busy} disabled={!stmts || typed !== target.name} onClick={run}>
            {action === "drop" ? <Trash2 /> : <Scissors />} {verb} {target.name}
          </Button>
        </>
      }>
      <div className="col gap-4">
        {conn.environment === "production" && <Alert kind="danger" title="This is a production connection">{conn.name}</Alert>}
        {stmts ? <pre className="ddlpreview mono">{stmts.join(";\n") + ";"}</pre> : !error && <Spinner />}
        {action === "drop" && conn.driver === "postgres" && (
          <label className="check"><input type="checkbox" checked={cascade} onChange={(e) => setCascade(e.target.checked)} /> Also drop dependent objects (CASCADE)</label>
        )}
        <Field label={<>Type <b className="mono">{target.name}</b> to confirm</>}>
          <input className="input" value={typed} onChange={(e) => setTyped(e.target.value)} autoFocus />
        </Field>
        {error && <Alert kind="danger">{error}</Alert>}
      </div>
    </Dialog>
  );
}
