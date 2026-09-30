import { useEffect, useMemo, useState } from "react";
import { CheckCircle2, AlertTriangle, MessageSquareText, ListTree, TableProperties, Map as MapIcon, Download, Copy, Ban } from "lucide-react";
import type { Connection } from "../../lib/types";
import { cellText, csvEscape, duration, int, modKey, temperForMs } from "../../lib/format";
import { Alert, Button, Empty, Kbd, Menu, MenuContent, MenuItem, MenuSep, MenuTrigger, Spinner } from "../../components/ui";
import { openExport } from "../transfer/store";
import { DataGrid, type GridColumn } from "../grid/DataGrid";
import { Inspector } from "../grid/Inspector";
import { MapView, hasGeometry } from "../map/MapView";
import { PlanView } from "./PlanView";
import type { ResultSet, RunState } from "./runs";

type Pane = { kind: "set"; stmt: number; set: number } | { kind: "messages" } | { kind: "plan" };

export function Results({ run, conn, database, schema }: { run: RunState; conn: Connection; database?: string; schema?: string }) {
  const sets = useMemo(() => {
    const out: { stmt: number; set: number; rs: ResultSet; label: string }[] = [];
    run.stmts.forEach((s, si) =>
      s.sets.forEach((rs, ri) => {
        if (rs.columns.length) out.push({ stmt: si, set: ri, rs, label: `Result ${out.length + 1}` });
      }),
    );
    return out;
  }, [run.stmts]);
  const [pane, setPane] = useState<Pane>({ kind: "messages" });
  const [view, setView] = useState<"grid" | "map">("grid");
  const [inspect, setInspect] = useState<{ r: number; c: number } | null>(null);

  // Follow the newest output: plan when explaining, first result otherwise.
  useEffect(() => {
    if (run.plan || run.planLoading || run.planError) setPane({ kind: "plan" });
  }, [run.plan, run.planLoading, run.planError]);
  useEffect(() => {
    if (run.status === "running" && !run.stmts.length) setPane({ kind: "messages" });
    if (sets.length && (pane.kind === "messages" || pane.kind === "plan") && !run.plan && !run.planLoading) setPane({ kind: "set", stmt: sets[0].stmt, set: sets[0].set });
    if (!sets.length && run.status !== "running" && pane.kind === "set") setPane({ kind: "messages" });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sets.length, run.status]);
  useEffect(() => {
    const failed = run.stmts.some((s) => s.error);
    if (failed && run.status !== "running") setPane({ kind: "messages" });
  }, [run.status]); // eslint-disable-line react-hooks/exhaustive-deps

  const found = pane.kind === "set" ? sets.find((s) => s.stmt === pane.stmt && s.set === pane.set) : undefined;
  // Document stores add a trailing "…" column for fields outside the inferred
  // set; drop it when no document needed it.
  const current = useMemo(() => {
    if (!found) return undefined;
    const i = found.rs.columns.findIndex((c) => c.name === "…");
    if (i < 0 || found.rs.rows.some((r) => r[i] !== null && r[i] !== undefined)) return found;
    return { ...found, rs: { ...found.rs, columns: found.rs.columns.filter((_, j) => j !== i), rows: found.rs.rows.map((r) => r.filter((_, j) => j !== i)) } };
  }, [found]);

  if (run.status === "idle" && !run.stmts.length && !run.plan && !run.planLoading && !run.planError) {
    return (
      <div className="results results--empty">
        <Empty title="Ready when you are">
          <div className="shortcuts">
            <span><Kbd>{modKey()}</Kbd><Kbd>↵</Kbd> run statement at cursor</span>
            <span><Kbd>⇧</Kbd><Kbd>{modKey()}</Kbd><Kbd>↵</Kbd> run everything</span>
            <span><Kbd>{modKey()}</Kbd><Kbd>E</Kbd> explain plan</span>
            <span><Kbd>{modKey()}</Kbd><Kbd>S</Kbd> save query</span>
          </div>
        </Empty>
      </div>
    );
  }


  const errors = run.stmts.filter((s) => s.error).length;
  const notices = run.stmts.reduce((n, s) => n + s.notices.length, 0);
  const gridCols: GridColumn[] = current ? current.rs.columns.map((c) => ({ name: c.name, type: c.type.toLowerCase(), kind: c.kind, nullable: c.nullable })) : [];
  const geo = current && hasGeometry(gridCols);

  const exportSet = (fmt: "csv" | "json" | "tsv") => {
    if (!current) return;
    const names = current.rs.columns.map((c) => c.name);
    let body = "";
    if (fmt === "json") body = JSON.stringify(current.rs.rows.map((r) => Object.fromEntries(names.map((n, i) => [n, r[i]]))), null, 2);
    else {
      const sep = fmt === "csv" ? "," : "\t";
      const esc = (s: string) => (fmt === "csv" ? csvEscape(s) : s.replace(/[\t\n]/g, " "));
      body = [names.map(esc).join(sep), ...current.rs.rows.map((r) => r.map((v) => esc(cellText(v))).join(sep))].join("\n");
    }
    if (fmt === "tsv") {
      navigator.clipboard?.writeText(body);
      return;
    }
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([body], { type: fmt === "csv" ? "text/csv" : "application/json" }));
    a.download = `${conn.name.replace(/\W+/g, "_")}-result.${fmt}`;
    a.click();
  };

  return (
    <div className="results">
      <div className="results__tabs" role="tablist">
        {sets.map((s) => (
          <button key={`${s.stmt}:${s.set}`} role="tab" aria-selected={pane.kind === "set" && pane.stmt === s.stmt && pane.set === s.set}
            className="rtab" onClick={() => { setPane({ kind: "set", stmt: s.stmt, set: s.set }); setInspect(null); }}>
            <TableProperties />
            {s.label}
            <span className="rtab__meta tnum">{int(s.rs.rows.length)}{s.rs.summary?.truncated ? "+" : ""}</span>
          </button>
        ))}
        <button role="tab" aria-selected={pane.kind === "messages"} className="rtab" onClick={() => setPane({ kind: "messages" })}>
          <MessageSquareText /> Messages
          {errors > 0 ? <span className="rtab__badge rtab__badge--err">{errors}</span> : notices > 0 ? <span className="rtab__badge">{notices}</span> : null}
        </button>
        {(run.plan || run.planLoading || run.planError) && (
          <button role="tab" aria-selected={pane.kind === "plan"} className="rtab" onClick={() => setPane({ kind: "plan" })}>
            <ListTree /> Plan
          </button>
        )}
        <span className="spacer" />
        {current && (
          <>
            {geo && (
              <div className="segmented" role="group" aria-label="Result view">
                <button aria-pressed={view === "grid"} onClick={() => setView("grid")}><TableProperties /> Grid</button>
                <button aria-pressed={view === "map"} onClick={() => setView("map")}><MapIcon /> Map</button>
              </div>
            )}
            <Menu>
              <MenuTrigger asChild>
                <Button size="sm" variant="ghost" icon aria-label="Export result"><Download /></Button>
              </MenuTrigger>
              <MenuContent align="end">
                {run.stmts[current.stmt]?.sql && (
                  <>
                    <MenuItem icon={<Download />} onSelect={() => openExport(conn, { kind: "query", sql: run.stmts[current.stmt].sql, database, schema })}
                      hint={current.rs.summary?.truncated ? "all rows" : undefined}>
                      Export to a file…
                    </MenuItem>
                    <MenuSep />
                  </>
                )}
                <MenuItem icon={<Copy />} onSelect={() => exportSet("tsv")}>Copy all as TSV</MenuItem>
                <MenuItem icon={<Download />} onSelect={() => exportSet("csv")} hint={`${int(current.rs.rows.length)} rows`}>Shown rows as CSV</MenuItem>
                <MenuItem icon={<Download />} onSelect={() => exportSet("json")} hint={`${int(current.rs.rows.length)} rows`}>Shown rows as JSON</MenuItem>
              </MenuContent>
            </Menu>
          </>
        )}
      </div>
      <div className="results__body">
        {pane.kind === "set" && current && (
          <>
            {current.rs.summary?.truncated && (
              <div className="results__notice"><Ban size={13} /> {current.rs.summary?.clipped
                ? <>Showing the first {int(current.rs.rows.length)} rows: the values are large, so the result stopped early. Use Export to get every row.</>
                : <>Showing the first {int(current.rs.rows.length)} rows. Raise the limit or add a LIMIT clause to see others.</>}</div>
            )}
            <div className="results__grid">
              {view === "map" && geo ? (
                <MapView columns={gridCols} rows={current.rs.rows} onPick={(r) => { setView("grid"); setInspect({ r, c: 0 }); }} />
              ) : (
                <DataGrid columns={gridCols} rows={current.rs.rows} onInspect={(r, c) => setInspect({ r, c })} onActiveChange={(r, c) => inspect && setInspect({ r, c })}
                  empty={<Empty title="No rows">The statement ran and returned an empty result.</Empty>} />
              )}
              {inspect && view === "grid" && (
                <Inspector column={gridCols[inspect.c]} value={current.rs.rows[inspect.r]?.[inspect.c]} onClose={() => setInspect(null)} />
              )}
            </div>
          </>
        )}
        {pane.kind === "messages" && <Messages run={run} />}
        {pane.kind === "plan" && (
          run.planLoading ? <div className="results__center"><Spinner large /></div> :
          run.planError ? <div className="results__pad"><Alert kind="danger" title="Explain failed">{run.planError}</Alert></div> :
          run.plan ? <PlanView plan={run.plan} /> : null
        )}
      </div>
    </div>
  );
}

function Messages({ run }: { run: RunState }) {
  return (
    <div className="messages">
      {run.error && <Alert kind="danger" title="The run stopped">{run.error}</Alert>}
      {run.status === "running" && run.stmts.length === 0 && <div className="messages__wait"><Spinner /> Waiting for the server…</div>}
      {run.status === "cancelled" && <Alert kind="warn">Cancelled.</Alert>}
      {run.stmts.map((s) => {
        const affected = s.sets.find((x) => x.summary?.rowsAffected !== undefined)?.summary?.rowsAffected;
        const returned = s.sets.filter((x) => x.columns.length).reduce((n, x) => n + x.rows.length, 0);
        return (
          <div key={s.index} className={`msg ${s.error ? "msg--error" : ""}`}>
            <div className="msg__head">
              {s.error ? <AlertTriangle className="msg__icon msg__icon--err" /> : s.done ? <CheckCircle2 className="msg__icon msg__icon--ok" /> : <Spinner />}
              <code className="msg__sql truncate">{s.sql.replace(/\s+/g, " ")}</code>
              <span className="spacer" />
              {!s.error && s.done && (
                <span className="msg__stat">
                  {s.sets.some((x) => x.columns.length) ? `${int(returned)} row${returned === 1 ? "" : "s"}` : affected !== undefined ? `${int(affected)} affected` : "done"}
                </span>
              )}
              {s.ms !== undefined && <span className="heat" style={{ ["--heat" as string]: temperForMs(s.ms) }}>{duration(s.ms)}</span>}
              <span className="faint mono msg__line">L{s.line}</span>
            </div>
            {s.notices.map((n, i) => (
              <div key={i} className="msg__notice"><span className="msg__level">{n.level}</span>{n.text}</div>
            ))}
            {s.error && (
              <div className="msg__err">
                <div className="msg__err-text">{s.error.message}</div>
                {s.error.detail && <div className="msg__err-extra">Detail: {s.error.detail}</div>}
                {s.error.hint && <div className="msg__err-extra">Hint: {s.error.hint}</div>}
                {s.error.code && <div className="msg__err-code mono">code {s.error.code}{s.error.line ? ` · line ${s.error.line}` : ""}</div>}
              </div>
            )}
          </div>
        );
      })}
      {run.status !== "running" && run.ms !== undefined && run.stmts.length > 0 && (
        <div className="messages__total faint">
          {run.stmts.length} statement{run.stmts.length === 1 ? "" : "s"} in {duration(run.ms)}
          {run.inTx ? " · transaction still open" : ""}
        </div>
      )}
    </div>
  );
}
