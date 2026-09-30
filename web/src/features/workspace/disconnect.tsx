import { useEffect } from "react";
import { create } from "zustand";
import { useQueryClient, type Query, type QueryClient } from "@tanstack/react-query";
import { post } from "../../lib/api";
import { go } from "../../lib/nav";
import { toast, useWorkspace } from "../../lib/store";
import type { Connection } from "../../lib/types";
import { Button, Dialog } from "../../components/ui";
import { useRuns } from "../query/runs";

const usePending = create<{ conn: Connection | null; set(conn: Connection | null): void }>((set) => ({
  conn: null,
  set: (conn) => set({ conn }),
}));

// Disconnects right away, or asks first when a tab has an open transaction
// or a running query.
export function requestDisconnect(conn: Connection) {
  usePending.getState().set(conn);
}

function busy(connId: string) {
  const runs = useRuns.getState().runs;
  const tabs = useWorkspace.getState().tabs.filter((t) => t.connId === connId);
  return {
    inTx: tabs.filter((t) => runs[t.id]?.inTx).length,
    running: tabs.filter((t) => runs[t.id]?.status === "running").length,
  };
}

// Ends the server sessions and forgets cached schema data. Tabs stay, so
// reopening the connection picks up where the user left off.
async function disconnect(conn: Connection, qc: QueryClient) {
  if (window.location.pathname.includes(`/c/${conn.id}`)) go("/");
  const ours = (q: Query) => q.queryKey[1] === conn.id && q.queryKey[0] !== "history";
  await qc.cancelQueries({ predicate: ours });
  const tabs = useWorkspace.getState().tabs.filter((t) => t.connId === conn.id);
  for (const t of tabs) useRuns.getState().clear(t.id);
  try {
    await post(`c/${conn.id}/disconnect`, {});
  } catch {
    // The server closes idle sessions by itself; nothing more to do.
  }
  qc.removeQueries({ predicate: ours });
  toast.success(`Disconnected from ${conn.name}`, tabs.length ? "Your tabs are kept for next time." : undefined);
}

export function DisconnectDialog() {
  const qc = useQueryClient();
  const { conn, set } = usePending();
  const b = conn ? busy(conn.id) : { inTx: 0, running: 0 };
  const ask = b.inTx + b.running > 0;

  useEffect(() => {
    if (conn && !ask) {
      set(null);
      void disconnect(conn, qc);
    }
  }, [conn, ask, qc, set]);

  if (!conn || !ask) return null;
  const tabs = (n: number) => (n === 1 ? "1 tab" : `${n} tabs`);
  const lines = [
    b.inTx > 0 && `${tabs(b.inTx)} ${b.inTx === 1 ? "has" : "have"} an open transaction. Uncommitted changes will be rolled back.`,
    b.running > 0 && `A query is still running in ${tabs(b.running)}. It will be stopped.`,
  ].filter(Boolean);
  return (
    <Dialog open onOpenChange={(o) => !o && set(null)} title={`Disconnect from ${conn.name}?`} description={lines.join(" ")}
      footer={<>
        <Button onClick={() => set(null)}>Stay connected</Button>
        <Button variant="danger" onClick={() => { set(null); void disconnect(conn, qc); }}>
          {b.inTx > 0 ? "Roll back and disconnect" : "Stop and disconnect"}
        </Button>
      </>} />
  );
}
