import { Lock, Circle } from "lucide-react";
import { useServer } from "../../lib/queries";
import { useWorkspace } from "../../lib/store";
import type { Connection } from "../../lib/types";
import { duration, temperForMs } from "../../lib/format";
import { Tip } from "../../components/ui";
import { useStatus } from "./status";

export function StatusBar({ conn }: { conn?: Connection }) {
  const server = useServer(conn?.id);
  const activeId = useWorkspace((s) => (conn ? s.active[conn.id] : undefined));
  const scope = useWorkspace((s) => (conn ? s.scope[conn.id] : undefined));
  const st = useStatus((s) => (activeId ? s.byTab[activeId] : undefined));
  if (!conn) {
    return (
      <footer className="statusbar">
        <span className="statusbar__item faint">Rowsmith™</span>
      </footer>
    );
  }
  const ro = server.data?.readOnly || conn.readOnly;
  return (
    <footer className={`statusbar statusbar--${conn.environment}`}>
      <span className="statusbar__item">
        <Circle className={`statusbar__dot ${server.isError ? "is-error" : server.data ? "is-ok" : ""}`} />
        {server.isError ? "Not connected" : server.data ? `${conn.name} · ${server.data.server.user}` : "Connecting…"}
      </span>
      {scope?.database && (
        <span className="statusbar__item mono">
          {scope.database}
          {scope.schema ? `.${scope.schema}` : ""}
        </span>
      )}
      <span className="spacer" />
      {st?.inTx && (
        <Tip label="A transaction is open in this query tab. Commit or roll back before closing it.">
          <span className="statusbar__item statusbar__tx">● In transaction</span>
        </Tip>
      )}
      {st?.note && <span className="statusbar__item faint">{st.note}</span>}
      {st?.rows && <span className="statusbar__item tnum">{st.rows}</span>}
      {st?.ms !== undefined && (
        <span className="statusbar__item">
          <span className="heat" style={{ ["--heat" as string]: temperForMs(st.ms) }}>{duration(st.ms)}</span>
        </span>
      )}
      {ro && (
        <span className="statusbar__item statusbar__ro">
          <Lock /> read-only
        </span>
      )}
    </footer>
  );
}
