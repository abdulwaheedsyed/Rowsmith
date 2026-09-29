import { useEffect, useState } from "react";
import { duration, temperForMs } from "../../lib/format";

/**
 * The query heat line. While a query runs, a thin line grows across the top
 * of the results and its color moves through the temper scale — straw for a
 * quick query, bronze after a second, plum and peacock as it keeps going,
 * blue for anything long. When the query ends it settles at its final heat.
 */
export function HeatBar({ running, startedAt, finalMs, status }: { running: boolean; startedAt?: number; finalMs?: number; status: string }) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!running) return;
    const t = setInterval(() => setNow(Date.now()), 100);
    return () => clearInterval(t);
  }, [running]);

  if (!running && (status === "idle" || finalMs === undefined)) return <div className="heatbar" aria-hidden="true" />;
  const elapsed = running ? Math.max(0, now - (startedAt ?? now)) : finalMs ?? 0;
  // Asymptotic progress: never claims to know when the query will finish.
  const progress = running ? 1 - Math.exp(-elapsed / 4000) : 1;
  const color = temperForMs(elapsed);
  return (
    <div className={`heatbar ${running ? "is-running" : "is-done"} ${status === "error" ? "is-error" : ""}`} role="progressbar" aria-label={running ? "Query running" : "Query finished"} aria-valuetext={duration(elapsed)}>
      <div className="heatbar__fill" style={{ width: `${Math.max(2, progress * 100)}%`, background: status === "error" ? "var(--danger)" : color }} />
      {running && <span className="heatbar__time mono" style={{ color }}>{duration(elapsed)}</span>}
    </div>
  );
}
