import { useCallback, useEffect, useMemo, useState } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import * as RPopover from "@radix-ui/react-popover";
import {
  Plus, RefreshCw, Search, X, Filter as FilterIcon, Columns3, Download, PanelRight, Save, Undo2, Code2, Map as MapIcon, TableProperties, ChevronRight,
} from "lucide-react";
import { ApiError, post } from "../../lib/api";
import { useDescribe, useDriver, qualified } from "../../lib/queries";
import { useWorkspace, toast, type Tab } from "../../lib/store";
import type { BrowseRequest, Cell, Connection, Filter, Result, RowEdit, Sort, Table, Column } from "../../lib/types";
import { cellText, csvEscape, int } from "../../lib/format";
import { Alert, Button, Dialog, Empty, Menu, MenuContent, MenuItem, MenuTrigger, Spinner, Tip } from "../../components/ui";
import { DataGrid, type GridColumn } from "../grid/DataGrid";
import { Inspector } from "../grid/Inspector";
import { openObject } from "../workspace/actions";
import { useStatus } from "../shell/status";
import { MapView, hasGeometry } from "../map/MapView";
import { useMediaQuery } from "../../lib/hooks";
import "./browse.css";

const PAGE = 200;
const HIDDEN = "__rowsmith_rowid";

interface BrowseState {
  filters: Filter[];
  sort: Sort[];
  search: string;
  where: string;
  hidden: string[];
  inspector: boolean;
  view: "grid" | "map";
}

const OPS: { op: string; label: string; needs: "one" | "none" | "many" | "two"; kinds?: string[] }[] = [
  { op: "=", label: "=", needs: "one" },
  { op: "!=", label: "≠", needs: "one" },
  { op: "contains", label: "contains", needs: "one" },
  { op: "startswith", label: "starts with", needs: "one" },
  { op: "endswith", label: "ends with", needs: "one" },
  { op: ">", label: ">", needs: "one" },
  { op: ">=", label: "≥", needs: "one" },
  { op: "<", label: "<", needs: "one" },
  { op: "<=", label: "≤", needs: "one" },
  { op: "between", label: "between", needs: "two" },
  { op: "in", label: "is one of", needs: "many" },
  { op: "notin", label: "is not one of", needs: "many" },
  { op: "like", label: "LIKE", needs: "one" },
  { op: "notlike", label: "NOT LIKE", needs: "one" },
  { op: "regexp", label: "matches regex", needs: "one" },
  { op: "null", label: "is NULL", needs: "none" },
  { op: "notnull", label: "is not NULL", needs: "none" },
  { op: "empty", label: "is empty", needs: "none" },
];

export function filterLabel(f: Filter) {
  const op = OPS.find((o) => o.op === f.op);
  if (!op) return `${f.column} ${f.op}`;
  if (op.needs === "none") return `${f.column} ${op.label}`;
  if (op.needs === "many") return `${f.column} ${op.label} (${(f.values ?? []).join(", ")})`;
  if (op.needs === "two") return `${f.column} between ${f.values?.[0]} and ${f.values?.[1]}`;
  return `${f.column} ${op.label} ${String(f.value)}`;
}

function toGridColumns(res: Result | undefined, table: Table | undefined): { cols: GridColumn[]; map: number[] } {
  if (!res) return { cols: [], map: [] };
  const byName = new Map<string, Column>((table?.columns ?? []).map((c) => [c.name, c]));
  const fks = new Map<string, NonNullable<Table["foreignKeys"]>[number]>();
  for (const fk of table?.foreignKeys ?? []) if (fk.columns.length === 1) fks.set(fk.columns[0], fk);
  const cols: GridColumn[] = [];
  const map: number[] = [];
  res.columns.forEach((rc, i) => {
    if (rc.name === HIDDEN) return;
    const c = byName.get(rc.name);
    cols.push({
      name: rc.name,
      type: c?.type ?? rc.type.toLowerCase(),
      kind: c?.kind ?? rc.kind,
      pk: c?.primaryKey,
      fk: fks.get(rc.name),
      nullable: c?.nullable,
      enumValues: c?.enum,
      readOnly: !!c?.generated,
      comment: c?.comment,
    });
    map.push(i);
  });
  return { cols, map };
}

export function BrowseTab({ tab, conn, active }: { tab: Tab; conn: Connection; active: boolean }) {
  const ref = tab.ref!;
  const drv = useDriver(conn.driver);
  const qc = useQueryClient();
  const updateTab = useWorkspace((s) => s.updateTab);
  const pinTab = useWorkspace((s) => s.pinTab);
  const setStatus = useStatus((s) => s.set);
  const narrow = useMediaQuery("(max-width: 640px)");
  const saved = (tab.state as Partial<BrowseState> | undefined) ?? {};
  const [st, setSt] = useState<BrowseState>({ filters: [], sort: [], search: "", where: "", hidden: [], inspector: false, view: "grid", ...saved });
  const [searchDraft, setSearchDraft] = useState(st.search);
  const [whereDraft, setWhereDraft] = useState(st.where);
  const [showWhere, setShowWhere] = useState(!!st.where);
  const [activeCell, setActiveCell] = useState<{ r: number; c: number } | null>(null);
  const [edits, setEdits] = useState<{ updates: Map<number, Record<string, unknown>>; inserts: Record<string, unknown>[]; deletes: Set<number> }>(() => ({ updates: new Map(), inserts: [], deletes: new Set() }));
  const [confirm, setConfirm] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [sqlShown, setSqlShown] = useState(false);

  const desc = useDescribe(conn.id, ref);
  const table = desc.data;

  const patchState = useCallback((p: Partial<BrowseState>) => {
    setSt((s) => {
      const next = { ...s, ...p };
      updateTab(tab.id, { state: next as unknown as Record<string, unknown> });
      return next;
    });
  }, [tab.id, updateTab]);

  // Debounced quick search.
  useEffect(() => {
    const t = setTimeout(() => searchDraft !== st.search && patchState({ search: searchDraft }), 350);
    return () => clearTimeout(t);
  }, [searchDraft, st.search, patchState]);

  const req: Omit<BrowseRequest, "offset" | "limit"> = { ref, filters: st.filters, sort: st.sort, search: st.search || undefined, where: st.where || undefined };
  const key = ["browse", conn.id, ref.database ?? "", ref.schema ?? "", ref.name, JSON.stringify(req)];

  const data = useInfiniteQuery({
    queryKey: key,
    initialPageParam: 0,
    queryFn: ({ pageParam, signal }) => post<Result>(`c/${conn.id}/browse`, { ...req, offset: pageParam, limit: PAGE }, signal),
    getNextPageParam: (last, pages) => (last.truncated ? pages.length * PAGE : undefined),
    staleTime: 30_000,
    retry: false,
  });
  const count = useQuery({
    queryKey: ["count", ...key.slice(1)],
    queryFn: ({ signal }) => post<{ rows: number; exact: boolean }>(`c/${conn.id}/count`, { ...req, offset: 0, limit: 0 }, signal),
    staleTime: 60_000,
    retry: false,
  });

  const first = data.data?.pages[0];
  const { cols, map } = useMemo(() => toGridColumns(first, table), [first, table]);
  const loaded = useMemo(() => (data.data?.pages ?? []).flatMap((p) => p.rows), [data.data]);
  const visibleCols = cols.filter((c) => !st.hidden.includes(c.name));
  const visIdx = visibleCols.map((c) => cols.indexOf(c));

  // Rows as displayed: pending inserts first, then loaded rows with edits applied.
  const nIns = edits.inserts.length;
  const displayRows = useMemo(() => {
    const out: Cell[][] = [];
    for (const ins of edits.inserts) out.push(visIdx.map((i) => (cols[i].name in ins ? (ins[cols[i].name] as Cell) : null)));
    loaded.forEach((row, r) => {
      const up = edits.updates.get(r);
      out.push(visIdx.map((i) => (up && cols[i].name in up ? (up[cols[i].name] as Cell) : row[map[i]])));
    });
    return out;
  }, [loaded, edits, visIdx.join(","), cols, map]); // eslint-disable-line react-hooks/exhaustive-deps

  const editable = !!table?.editable && !!drv?.caps.editRows && conn.access !== "read" && !conn.readOnly;
  const pendingCount = edits.updates.size + edits.inserts.length + edits.deletes.size;

  useEffect(() => {
    if (!active) return;
    const total = count.data ? `${count.data.exact ? "" : "≈ "}${int(count.data.rows)} rows` : "";
    setStatus(tab.id, { rows: loaded.length ? `${int(loaded.length)} loaded${total ? " of " + total : ""}` : total, ms: first?.durationMs, note: pendingCount ? `${pendingCount} unsaved change${pendingCount > 1 ? "s" : ""}` : undefined });
  }, [active, loaded.length, count.data, first?.durationMs, pendingCount, tab.id, setStatus]);

  // Editing -------------------------------------------------------------------------
  const rowKeyOf = (loadedIdx: number): Record<string, Cell> => {
    const row = loaded[loadedIdx];
    const names = first!.columns.map((c) => c.name);
    const val = (n: string) => row[names.indexOf(n)];
    if (table?.rowKeyKind === "rowid") return { [HIDDEN]: val(HIDDEN) };
    if (table?.rowKeyKind === "all") return Object.fromEntries(names.filter((n) => n !== HIDDEN).map((n) => [n, val(n)]));
    return Object.fromEntries((table?.rowKey ?? []).map((n) => [n, val(n)]));
  };

  const commitEdit = (r: number, c: number, value: unknown) => {
    pinTab(tab.id);
    const name = visibleCols[c].name;
    setEdits((e) => {
      if (r < e.inserts.length) {
        const inserts = e.inserts.map((row, i) => (i === r ? { ...row, [name]: value } : row));
        return { ...e, inserts };
      }
      const li = r - e.inserts.length;
      const updates = new Map(e.updates);
      const orig = loaded[li]?.[map[cols.indexOf(visibleCols[c])]];
      const cur = { ...(updates.get(li) ?? {}) };
      if (cellText(orig as Cell) === (value === null ? "" : String(value)) && (orig === null) === (value === null)) delete cur[name];
      else cur[name] = value;
      if (Object.keys(cur).length) updates.set(li, cur);
      else updates.delete(li);
      return { ...e, updates };
    });
  };

  const deleteRows = (rs: number[]) => {
    pinTab(tab.id);
    setEdits((e) => {
      const deletes = new Set(e.deletes);
      const insertsToDrop = new Set(rs.filter((r) => r < e.inserts.length));
      for (const r of rs) if (r >= e.inserts.length) {
        const li = r - e.inserts.length;
        if (deletes.has(li)) deletes.delete(li);
        else deletes.add(li);
      }
      return { ...e, deletes, inserts: e.inserts.filter((_, i) => !insertsToDrop.has(i)) };
    });
  };

  const addRow = () => {
    pinTab(tab.id);
    setEdits((e) => ({ ...e, inserts: [{}, ...e.inserts] }));
  };

  const discard = () => setEdits({ updates: new Map(), inserts: [], deletes: new Set() });

  const buildEdits = (): RowEdit[] => {
    const out: RowEdit[] = [];
    for (const ins of edits.inserts) out.push({ op: "insert", values: ins });
    for (const [li, values] of edits.updates) if (!edits.deletes.has(li)) out.push({ op: "update", key: rowKeyOf(li), values });
    for (const li of edits.deletes) out.push({ op: "delete", key: rowKeyOf(li) });
    return out;
  };

  const save = async (confirmed = false) => {
    setSaving(true);
    setSaveError("");
    try {
      const res = await post<{ applied: number }>(`c/${conn.id}/edit`, { ref, edits: buildEdits(), confirm: confirmed });
      toast.success(`Saved ${res.applied} change${res.applied === 1 ? "" : "s"}`, qualified(ref, drv?.quoteChar ?? '"'));
      discard();
      setConfirm(false);
      await qc.invalidateQueries({ queryKey: ["browse", conn.id, ref.database ?? "", ref.schema ?? "", ref.name] });
      qc.invalidateQueries({ queryKey: ["count", conn.id] });
    } catch (e) {
      if (e instanceof ApiError && e.code === "confirm_required") setConfirm(true);
      else setSaveError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  const rowState = (r: number) => (r < nIns ? "new" : edits.deletes.has(r - nIns) ? "deleted" : edits.updates.has(r - nIns) ? "dirty" : "clean");
  const cellDirty = (r: number, c: number) => {
    const name = visibleCols[c]?.name;
    if (r < nIns) return name in edits.inserts[r];
    return !!edits.updates.get(r - nIns) && name in edits.updates.get(r - nIns)!;
  };

  const openFK = (col: GridColumn, value: Cell) => {
    const fk = col.fk!;
    const target = { database: fk.refTable.database ?? ref.database, schema: fk.refTable.schema ?? ref.schema, name: fk.refTable.name, kind: "table" };
    const id = openObject(conn, target, "browse", false);
    useWorkspace.getState().updateTab(id, { state: { filters: [{ column: fk.refColumns[fk.columns.indexOf(col.name)] ?? fk.refColumns[0], op: "=", value: cellText(value) }], sort: [], search: "", where: "", hidden: [], inspector: false, view: "grid" } });
  };

  const filterBy = (col: GridColumn, value: Cell, exclude: boolean) => {
    const f: Filter = value === null ? { column: col.name, op: exclude ? "notnull" : "null" } : { column: col.name, op: exclude ? "!=" : "=", value: cellText(value) };
    patchState({ filters: [...st.filters, f] });
  };

  const onSort = (column: string, additive: boolean) => {
    const cur = st.sort.find((s) => s.column === column);
    let next: Sort[];
    if (!cur) next = additive ? [...st.sort, { column, desc: false }] : [{ column, desc: false }];
    else if (!cur.desc) next = st.sort.map((s) => (s.column === column ? { ...s, desc: true } : s));
    else next = st.sort.filter((s) => s.column !== column);
    patchState({ sort: next });
  };

  const exportLoaded = (fmt: "csv" | "json") => {
    const names = visibleCols.map((c) => c.name);
    let body: string;
    if (fmt === "csv") body = [names.map((n) => csvEscape(n)).join(","), ...displayRows.map((r) => r.map((v) => csvEscape(cellText(v))).join(","))].join("\n");
    else body = JSON.stringify(displayRows.map((r) => Object.fromEntries(names.map((n, i) => [n, r[i]]))), null, 2);
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([body], { type: fmt === "csv" ? "text/csv" : "application/json" }));
    a.download = `${ref.name}.${fmt}`;
    a.click();
  };

  const err = (data.error ?? desc.error) as Error | null;
  const geo = hasGeometry(visibleCols.map((c) => ({ name: c.name, kind: c.kind })));

  return (
    <div className="browse">
      <div className="browse__bar">
        <FilterBar cols={cols} filters={st.filters} onChange={(filters) => patchState({ filters })} />
        <span className="spacer" />
        <label className="browse__search">
          <Search />
          <input className="input" placeholder="Search rows" value={searchDraft} onChange={(e) => setSearchDraft(e.target.value)} aria-label="Search rows" />
          {searchDraft && <button className="browse__clear" aria-label="Clear search" onClick={() => { setSearchDraft(""); patchState({ search: "" }); }}><X /></button>}
        </label>
        {(drv?.caps.sql || drv?.caps.documents) && (
          <Tip label={drv?.caps.documents ? "Query filter (shell syntax)" : "Raw WHERE condition"}>
            <Button size="sm" variant={showWhere ? "default" : "ghost"} icon onClick={() => setShowWhere(!showWhere)} aria-label="Raw condition"><Code2 /></Button>
          </Tip>
        )}
        <ColumnsMenu cols={cols} hidden={st.hidden} onChange={(hidden) => patchState({ hidden })} />
        {geo && (
          <div className="segmented" role="group" aria-label="View">
            <button aria-pressed={st.view === "grid"} onClick={() => patchState({ view: "grid" })}><TableProperties /> Grid</button>
            <button aria-pressed={st.view === "map"} onClick={() => patchState({ view: "map" })}><MapIcon /> Map</button>
          </div>
        )}
        <Tip label="Refresh">
          <Button size="sm" variant="ghost" icon onClick={() => { data.refetch(); count.refetch(); }} aria-label="Refresh"><RefreshCw className={data.isFetching ? "spin" : ""} /></Button>
        </Tip>
        <Menu>
          <MenuTrigger asChild>
            <Button size="sm" variant="ghost" icon aria-label="Export"><Download /></Button>
          </MenuTrigger>
          <MenuContent align="end">
            <MenuItem onSelect={() => exportLoaded("csv")} hint={`${int(displayRows.length)} rows`}>Loaded rows as CSV</MenuItem>
            <MenuItem onSelect={() => exportLoaded("json")} hint={`${int(displayRows.length)} rows`}>Loaded rows as JSON</MenuItem>
          </MenuContent>
        </Menu>
        {!narrow && (
          <Tip label="Value inspector">
            <Button size="sm" variant={st.inspector ? "default" : "ghost"} icon onClick={() => patchState({ inspector: !st.inspector })} aria-label="Toggle inspector"><PanelRight /></Button>
          </Tip>
        )}
        {editable && (
          <Button size="sm" onClick={addRow}><Plus /> Row</Button>
        )}
      </div>
      {showWhere && (
        <form className="browse__where" onSubmit={(e) => { e.preventDefault(); patchState({ where: whereDraft }); }}>
          <span className="mono browse__where-kw">{drv?.caps.documents ? "FILTER" : "WHERE"}</span>
          <input className="input input--mono" value={whereDraft} onChange={(e) => setWhereDraft(e.target.value)}
            placeholder={drv?.caps.documents ? "{ status: 'paid', total: { $gt: 100 } }" : "total > 100 AND status = 'paid'"} spellCheck={false} />
          <Button size="sm" type="submit">Apply</Button>
          {st.where && <Button size="sm" variant="ghost" onClick={() => { setWhereDraft(""); patchState({ where: "" }); }}>Clear</Button>}
        </form>
      )}

      <div className="browse__body">
        {err ? (
          <div className="browse__error"><Alert kind="danger" title="Could not load rows">{err.message}</Alert></div>
        ) : !first ? (
          <div className="browse__loading"><Spinner large /></div>
        ) : st.view === "map" && geo ? (
          <MapView columns={visibleCols.map((c) => ({ name: c.name, kind: c.kind }))} rows={displayRows} onPick={(r) => { setActiveCell({ r, c: 0 }); patchState({ inspector: true, view: "grid" }); }} />
        ) : narrow ? (
          <CardList cols={visibleCols} rows={displayRows} onOpen={(r) => setActiveCell({ r, c: 0 })} hasMore={!!data.hasNextPage} onMore={() => data.fetchNextPage()} loading={data.isFetchingNextPage} rowState={rowState} />
        ) : (
          <DataGrid
            columns={visibleCols}
            rows={displayRows}
            rowState={rowState}
            cellDirty={cellDirty}
            hasMore={!!data.hasNextPage}
            loadingMore={data.isFetchingNextPage}
            onLoadMore={() => data.fetchNextPage()}
            sort={st.sort}
            onSort={onSort}
            editable={editable}
            onCommitEdit={commitEdit}
            onDeleteRows={editable ? deleteRows : undefined}
            onOpenFK={openFK}
            onFilterBy={filterBy}
            onInspect={(r, c) => { setActiveCell({ r, c }); patchState({ inspector: true }); }}
            onActiveChange={(r, c) => setActiveCell({ r, c })}
            tableName={qualified(ref, drv?.quoteChar ?? '"')}
            quote={drv?.quoteChar}
            empty={
              <Empty title={st.filters.length || st.search || st.where ? "No rows match" : "This table is empty"}>
                {st.filters.length || st.search || st.where ? "Loosen the filters to see more." : editable ? "Add the first row with the + Row button." : null}
              </Empty>
            }
          />
        )}
        {st.inspector && !narrow && st.view === "grid" && (
          <Inspector
            column={activeCell ? visibleCols[activeCell.c] : undefined}
            value={activeCell ? displayRows[activeCell.r]?.[activeCell.c] : undefined}
            editable={editable && !!activeCell && !visibleCols[activeCell.c]?.readOnly}
            onChange={(v) => activeCell && commitEdit(activeCell.r, activeCell.c, v)}
            onClose={() => patchState({ inspector: false })}
          />
        )}
        {narrow && activeCell && (
          <RowSheet cols={visibleCols} row={displayRows[activeCell.r]} editable={editable} onClose={() => setActiveCell(null)}
            onChange={(c, v) => commitEdit(activeCell.r, c, v)} onDelete={editable ? () => { deleteRows([activeCell.r]); setActiveCell(null); } : undefined} />
        )}
      </div>

      <div className="browse__foot">
        <span className="tnum muted">
          {count.data ? <>{count.data.exact ? "" : "≈ "}{int(count.data.rows)} rows</> : count.isLoading ? "Counting…" : ""}
          {loaded.length > 0 && <span className="faint"> · {int(loaded.length)} loaded</span>}
        </span>
        {first?.sql && (
          <button className="browse__sql mono truncate" onClick={() => setSqlShown(!sqlShown)} title="Generated SQL">
            <ChevronRight className={sqlShown ? "rot" : ""} />{sqlShown ? first.sql : first.sql.slice(0, 120)}
          </button>
        )}
        <span className="spacer" />
        {pendingCount > 0 && (
          <div className="savebar">
            <span className="savebar__summary">
              {edits.updates.size > 0 && <span className="pill pill--dirty">{edits.updates.size} edited</span>}
              {edits.inserts.length > 0 && <span className="pill pill--new">{edits.inserts.length} new</span>}
              {edits.deletes.size > 0 && <span className="pill pill--del">{edits.deletes.size} deleted</span>}
            </span>
            {saveError && <span className="savebar__err truncate" title={saveError}>{saveError}</span>}
            <Button size="sm" variant="ghost" onClick={discard}><Undo2 /> Discard</Button>
            <Button size="sm" variant="primary" onClick={() => save(false)} loading={saving}><Save /> Save changes</Button>
          </div>
        )}
      </div>

      <Dialog open={confirm} onOpenChange={setConfirm} title="Save changes to production?"
        description={`${conn.name} is marked as production. ${pendingCount} change${pendingCount === 1 ? "" : "s"} will be written in a single transaction.`}
        footer={<><Button onClick={() => setConfirm(false)}>Cancel</Button><Button variant="danger" loading={saving} onClick={() => save(true)}>Write {pendingCount} change{pendingCount === 1 ? "" : "s"} to production</Button></>}
      />
    </div>
  );
}

// ---- Filters ---------------------------------------------------------------------

function FilterBar({ cols, filters, onChange }: { cols: GridColumn[]; filters: Filter[]; onChange(f: Filter[]): void }) {
  const [open, setOpen] = useState(false);
  const [editIdx, setEditIdx] = useState<number | null>(null);
  return (
    <div className="filters">
      {filters.map((f, i) => (
        <span key={i} className="chip">
          <button className="chip__body mono" onClick={() => { setEditIdx(i); setOpen(true); }}>{filterLabel(f)}</button>
          <button className="chip__x" aria-label="Remove filter" onClick={() => onChange(filters.filter((_, j) => j !== i))}><X /></button>
        </span>
      ))}
      <RPopover.Root open={open} onOpenChange={(o) => { setOpen(o); if (!o) setEditIdx(null); }}>
        <RPopover.Trigger asChild>
          <Button size="sm" variant="ghost"><FilterIcon /> {filters.length ? "Add" : "Filter"}</Button>
        </RPopover.Trigger>
        <RPopover.Portal>
          <RPopover.Content className="popover filterpop" align="start" sideOffset={6}>
            <FilterEditor cols={cols} initial={editIdx !== null ? filters[editIdx] : undefined}
              onDone={(f) => {
                if (editIdx !== null) onChange(filters.map((x, j) => (j === editIdx ? f : x)));
                else onChange([...filters, f]);
                setOpen(false);
                setEditIdx(null);
              }} />
          </RPopover.Content>
        </RPopover.Portal>
      </RPopover.Root>
      {filters.length > 1 && <Button size="sm" variant="ghost" onClick={() => onChange([])}>Clear all</Button>}
    </div>
  );
}

function FilterEditor({ cols, initial, onDone }: { cols: GridColumn[]; initial?: Filter; onDone(f: Filter): void }) {
  const [column, setColumn] = useState(initial?.column ?? cols[0]?.name ?? "");
  const [op, setOp] = useState(initial?.op ?? "=");
  const [value, setValue] = useState(initial?.value !== undefined ? String(initial.value) : initial?.values?.join(", ") ?? "");
  const [value2, setValue2] = useState(initial?.values?.[1] !== undefined ? String(initial.values[1]) : "");
  const meta = OPS.find((o) => o.op === op)!;
  const col = cols.find((c) => c.name === column);
  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!column) return;
    if (meta.needs === "none") onDone({ column, op });
    else if (meta.needs === "many") onDone({ column, op, values: value.split(",").map((s) => s.trim()).filter(Boolean) });
    else if (meta.needs === "two") onDone({ column, op, values: [value.split(",")[0]?.trim() ?? value, value2] });
    else onDone({ column, op, value });
  };
  return (
    <form className="col gap-3" onSubmit={submit}>
      <div className="eyebrow">Filter rows</div>
      <select className="select" value={column} onChange={(e) => setColumn(e.target.value)} aria-label="Column">
        {cols.map((c) => <option key={c.name} value={c.name}>{c.name} · {c.type}</option>)}
      </select>
      <select className="select" value={op} onChange={(e) => setOp(e.target.value)} aria-label="Operator">
        {OPS.map((o) => <option key={o.op} value={o.op}>{o.label}</option>)}
      </select>
      {meta.needs !== "none" && (
        col?.enumValues?.length && (meta.needs === "one") ? (
          <select className="select" value={value} onChange={(e) => setValue(e.target.value)} aria-label="Value">
            <option value="">Choose…</option>
            {col.enumValues.map((v) => <option key={v}>{v}</option>)}
          </select>
        ) : (
          <input className="input" autoFocus value={value} onChange={(e) => setValue(e.target.value)} placeholder={meta.needs === "many" ? "comma, separated, values" : meta.needs === "two" ? "from" : "value"} aria-label="Value" />
        )
      )}
      {meta.needs === "two" && <input className="input" value={value2} onChange={(e) => setValue2(e.target.value)} placeholder="to" aria-label="Second value" />}
      <Button type="submit" variant="primary" size="sm">{initial ? "Update filter" : "Apply filter"}</Button>
    </form>
  );
}

function ColumnsMenu({ cols, hidden, onChange }: { cols: GridColumn[]; hidden: string[]; onChange(h: string[]): void }) {
  const [q, setQ] = useState("");
  return (
    <RPopover.Root>
      <Tip label="Choose columns">
        <RPopover.Trigger asChild>
          <Button size="sm" variant={hidden.length ? "default" : "ghost"} icon aria-label="Columns"><Columns3 /></Button>
        </RPopover.Trigger>
      </Tip>
      <RPopover.Portal>
        <RPopover.Content className="popover colsmenu" align="end" sideOffset={6}>
          <div className="row gap-3">
            <input className="input" placeholder="Find column" value={q} onChange={(e) => setQ(e.target.value)} />
          </div>
          <div className="row gap-3 colsmenu__actions">
            <button className="link-button" onClick={() => onChange([])}>Show all</button>
            <button className="link-button" onClick={() => onChange(cols.map((c) => c.name))}>Hide all</button>
            <span className="spacer" />
            <span className="faint">{cols.length - hidden.length}/{cols.length}</span>
          </div>
          <div className="colsmenu__list">
            {cols.filter((c) => c.name.toLowerCase().includes(q.toLowerCase())).map((c) => (
              <label key={c.name} className="check colsmenu__item">
                <input type="checkbox" checked={!hidden.includes(c.name)} onChange={(e) => onChange(e.target.checked ? hidden.filter((h) => h !== c.name) : [...hidden, c.name])} />
                <span className="truncate grow">{c.name}</span>
                <span className="mono faint">{c.type}</span>
              </label>
            ))}
          </div>
        </RPopover.Content>
      </RPopover.Portal>
    </RPopover.Root>
  );
}

// ---- Mobile card list ------------------------------------------------------------

function CardList({ cols, rows, onOpen, hasMore, onMore, loading, rowState }: { cols: GridColumn[]; rows: Cell[][]; onOpen(r: number): void; hasMore: boolean; onMore(): void; loading: boolean; rowState(r: number): string }) {
  const titleIdx = Math.max(0, cols.findIndex((c) => !c.pk && (c.kind === "string" || c.kind === "text")));
  const keyIdx = cols.findIndex((c) => c.pk);
  const detail = cols.map((_, i) => i).filter((i) => i !== titleIdx && i !== keyIdx).slice(0, 4);
  return (
    <div className="cards" onScroll={(e) => {
      const el = e.currentTarget;
      if (hasMore && !loading && el.scrollTop + el.clientHeight > el.scrollHeight - 400) onMore();
    }}>
      {rows.map((row, r) => (
        <button key={r} className={`rowcard rowcard--${rowState(r)}`} onClick={() => onOpen(r)}>
          <div className="rowcard__head">
            <span className="rowcard__title truncate">{cellText(row[titleIdx]) || "—"}</span>
            {keyIdx >= 0 && <span className="rowcard__key mono">#{cellText(row[keyIdx])}</span>}
          </div>
          <dl className="rowcard__fields">
            {detail.map((i) => (
              <div key={i}>
                <dt>{cols[i].name}</dt>
                <dd className="truncate">{row[i] === null ? <span className="c-null">NULL</span> : cellText(row[i]).slice(0, 80)}</dd>
              </div>
            ))}
          </dl>
        </button>
      ))}
      {loading && <div className="cards__more"><Spinner /></div>}
    </div>
  );
}

function RowSheet({ cols, row, editable, onClose, onChange, onDelete }: { cols: GridColumn[]; row?: Cell[]; editable: boolean; onClose(): void; onChange(c: number, v: unknown): void; onDelete?(): void }) {
  const [draft, setDraft] = useState<Record<number, string | null>>({});
  if (!row) return null;
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} title="Row details"
      footer={
        <>
          {onDelete && <Button variant="ghost" onClick={onDelete}>Delete row</Button>}
          <span className="spacer" />
          <Button onClick={onClose}>Close</Button>
          {editable && Object.keys(draft).length > 0 && (
            <Button variant="primary" onClick={() => { for (const [c, v] of Object.entries(draft)) onChange(Number(c), v); onClose(); }}>Keep changes</Button>
          )}
        </>
      }>
      <div className="rowsheet">
        {cols.map((c, i) => (
          <div key={c.name} className="rowsheet__field">
            <div className="rowsheet__label"><span>{c.name}</span><span className="mono faint">{c.type}</span></div>
            {editable && !c.readOnly ? (
              <textarea className="textarea" rows={1} style={{ minHeight: 38 }} value={draft[i] !== undefined ? draft[i] ?? "" : row[i] === null ? "" : cellText(row[i])}
                placeholder={row[i] === null ? "NULL" : ""} onChange={(e) => setDraft({ ...draft, [i]: e.target.value })} />
            ) : (
              <div className="rowsheet__value">{row[i] === null ? <span className="c-null">NULL</span> : cellText(row[i])}</div>
            )}
          </div>
        ))}
      </div>
    </Dialog>
  );
}


