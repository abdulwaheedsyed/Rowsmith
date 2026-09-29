import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import * as RContext from "@radix-ui/react-context-menu";
import { ArrowDown, ArrowUp, KeyRound, Link2, Copy, Filter, Trash2, Ban, Eye, CornerDownRight } from "lucide-react";
import type { Cell, ForeignKey, Sort, ValueKind } from "../../lib/types";
import { cellText, csvEscape } from "../../lib/format";
import { CellView, alignFor, cellChars } from "./cells";
import "./grid.css";

export interface GridColumn {
  name: string;
  type: string;
  kind: ValueKind;
  pk?: boolean;
  fk?: ForeignKey;
  nullable?: boolean;
  enumValues?: string[];
  readOnly?: boolean;
  comment?: string;
}

export type RowState = "clean" | "dirty" | "new" | "deleted";

export interface GridSelection {
  r0: number;
  r1: number;
  c0: number;
  c1: number;
  active: { r: number; c: number };
  rowsMode: boolean;
  anchorR?: number;
  anchorC?: number;
}

export interface DataGridProps {
  columns: GridColumn[];
  rows: Cell[][];
  rowState?(i: number): RowState;
  cellDirty?(i: number, c: number): boolean;
  hasMore?: boolean;
  loadingMore?: boolean;
  onLoadMore?(): void;
  sort?: Sort[];
  onSort?(col: string, additive: boolean): void;
  editable?: boolean;
  onCommitEdit?(row: number, col: number, value: unknown): void;
  onDeleteRows?(rows: number[]): void;
  onOpenFK?(col: GridColumn, value: Cell): void;
  onFilterBy?(col: GridColumn, value: Cell, exclude: boolean): void;
  onInspect?(row: number, col: number): void;
  onActiveChange?(row: number, col: number): void;
  tableName?: string;
  quote?: string;
  empty?: ReactNode;
  footer?: ReactNode;
}

const HEADER_H = 36;
const coarse = typeof window !== "undefined" && window.matchMedia?.("(pointer: coarse)").matches;
const ROW_H = coarse ? 38 : 28;
const OVERSCAN_ROWS = 10;
const OVERSCAN_PX = 400;

function computeWidths(cols: GridColumn[], rows: Cell[][]): number[] {
  const sample = rows.slice(0, 80);
  return cols.map((c, i) => {
    let chars = Math.max(c.name.length + 3, Math.min(c.type.length, 18) * 0.85 + 2);
    for (const r of sample) chars = Math.max(chars, cellChars(r[i]));
    return Math.round(Math.max(72, Math.min(420, chars * 7.4 + 26)));
  });
}

function lowerBound(arr: number[], x: number) {
  let lo = 0, hi = arr.length;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (arr[mid] < x) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

export function DataGrid(props: DataGridProps) {
  const { columns, rows, editable } = props;
  const scroller = useRef<HTMLDivElement>(null);
  const root = useRef<HTMLDivElement>(null);
  const [view, setView] = useState({ top: 0, left: 0, w: 800, h: 400 });
  const [widths, setWidths] = useState<number[]>(() => computeWidths(columns, rows));
  const [sel, setSel] = useState<GridSelection | null>(null);
  const [editing, setEditing] = useState<{ r: number; c: number; value: string; initial?: string } | null>(null);
  const colKey = columns.map((c) => c.name).join("\u0000");

  // Reset widths when the column set changes, sizing from the first rows.
  const lastKey = useRef(colKey);
  useEffect(() => {
    if (lastKey.current !== colKey || widths.length !== columns.length) {
      lastKey.current = colKey;
      setWidths(computeWidths(columns, rows));
      setSel(null);
      setEditing(null);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [colKey]);
  useEffect(() => {
    // First rows arrived after columns: resize once.
    if (rows.length > 0 && widths.every((w) => w <= 90) && columns.length > 0) setWidths(computeWidths(columns, rows));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rows.length > 0]);

  const rnW = Math.max(44, String(rows.length + 1).length * 8 + 22);
  const lefts = useMemo(() => {
    const out: number[] = [];
    let x = 0;
    for (const w of widths) {
      out.push(x);
      x += w;
    }
    out.push(x);
    return out;
  }, [widths]);
  const totalW = lefts[lefts.length - 1] ?? 0;
  const totalH = HEADER_H + rows.length * ROW_H + (props.hasMore ? ROW_H * 2 : 0);

  useLayoutEffect(() => {
    const el = scroller.current;
    if (!el) return;
    const ro = new ResizeObserver(() => setView((v) => ({ ...v, w: el.clientWidth, h: el.clientHeight })));
    ro.observe(el);
    setView((v) => ({ ...v, w: el.clientWidth, h: el.clientHeight }));
    return () => ro.disconnect();
  }, []);

  const raf = useRef(0);
  const onScroll = () => {
    cancelAnimationFrame(raf.current);
    raf.current = requestAnimationFrame(() => {
      const el = scroller.current;
      if (el) setView((v) => ({ ...v, top: el.scrollTop, left: el.scrollLeft }));
    });
  };

  const r0 = Math.max(0, Math.floor((view.top - HEADER_H) / ROW_H) - OVERSCAN_ROWS);
  const r1 = Math.min(rows.length, Math.ceil((view.top + view.h) / ROW_H) + OVERSCAN_ROWS);
  const cStart = Math.max(0, lowerBound(lefts, view.left - OVERSCAN_PX) - 1);
  const cEnd = Math.min(columns.length, lowerBound(lefts, view.left + view.w - rnW + OVERSCAN_PX) + 1);

  // Infinite loading.
  useEffect(() => {
    if (props.hasMore && !props.loadingMore && r1 >= rows.length - 40) props.onLoadMore?.();
  }, [r1, rows.length, props.hasMore, props.loadingMore, props.onLoadMore]);

  // ---- selection helpers -------------------------------------------------------
  const inSel = (r: number, c: number) => !!sel && r >= sel.r0 && r <= sel.r1 && c >= sel.c0 && c <= sel.c1;

  const select = useCallback(
    (r: number, c: number, extend: boolean, rowsMode = false) => {
      r = Math.max(0, Math.min(rows.length - 1, r));
      c = Math.max(0, Math.min(columns.length - 1, c));
      setSel((cur) => {
        if (extend && cur) {
          const a = cur.anchorR ?? cur.active.r;
          const b = cur.anchorC ?? cur.active.c;
          const next = {
            r0: Math.min(a, r), r1: Math.max(a, r),
            c0: rowsMode || cur.rowsMode ? 0 : Math.min(b, c), c1: rowsMode || cur.rowsMode ? columns.length - 1 : Math.max(b, c),
            active: { r, c }, rowsMode: rowsMode || cur.rowsMode, anchorR: a, anchorC: b,
          } as GridSelection;
          return next;
        }
        return {
          r0: r, r1: r, c0: rowsMode ? 0 : c, c1: rowsMode ? columns.length - 1 : c,
          active: { r, c }, rowsMode, anchorR: r, anchorC: c,
        } as GridSelection;
      });
      props.onActiveChange?.(r, c);
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [rows.length, columns.length],
  );

  // Keep the active cell in view.
  useEffect(() => {
    const el = scroller.current;
    if (!el || !sel) return;
    const { r, c } = sel.active;
    const top = HEADER_H + r * ROW_H;
    if (top - HEADER_H < el.scrollTop) el.scrollTop = top - HEADER_H;
    else if (top + ROW_H > el.scrollTop + el.clientHeight) el.scrollTop = top + ROW_H - el.clientHeight;
    const left = lefts[c], right = lefts[c + 1];
    if (!sel.rowsMode) {
      if (left < el.scrollLeft) el.scrollLeft = left;
      else if (right + rnW > el.scrollLeft + el.clientWidth) el.scrollLeft = right + rnW - el.clientWidth;
    }
  }, [sel?.active.r, sel?.active.c]); // eslint-disable-line react-hooks/exhaustive-deps

  // ---- copy --------------------------------------------------------------------------
  const selectionMatrix = useCallback(
    (s: GridSelection | null = sel) => {
      if (!s) return { cols: [] as GridColumn[], data: [] as Cell[][], indexes: [] as number[] };
      const cols = columns.slice(s.c0, s.c1 + 1);
      const data: Cell[][] = [];
      const indexes: number[] = [];
      for (let r = s.r0; r <= s.r1; r++) {
        if (!rows[r]) continue;
        data.push(rows[r].slice(s.c0, s.c1 + 1));
        indexes.push(r);
      }
      return { cols, data, indexes };
    },
    [sel, columns, rows],
  );

  const copyAs = useCallback(
    (format: "tsv" | "csv" | "json" | "markdown" | "insert", headers = false) => {
      const { cols, data } = selectionMatrix();
      if (!cols.length) return;
      let out = "";
      if (format === "tsv" || format === "csv") {
        const sep = format === "tsv" ? "\t" : ",";
        const esc = (s: string) => (format === "csv" ? csvEscape(s) : s.replace(/[\t\n\r]/g, " "));
        const lines = data.map((r) => r.map((v) => esc(cellText(v))).join(sep));
        if (headers || format === "csv") lines.unshift(cols.map((c) => esc(c.name)).join(sep));
        out = lines.join("\n");
      } else if (format === "json") {
        out = JSON.stringify(data.map((r) => Object.fromEntries(cols.map((c, i) => [c.name, jsonValue(r[i])]))), null, 2);
      } else if (format === "markdown") {
        const lines = [
          "| " + cols.map((c) => c.name).join(" | ") + " |",
          "| " + cols.map((c) => (alignFor(c.kind) === "right" ? "---:" : "---")).join(" | ") + " |",
          ...data.map((r) => "| " + r.map((v) => cellText(v).replace(/\|/g, "\\|").replace(/\n/g, " ")).join(" | ") + " |"),
        ];
        out = lines.join("\n");
      } else {
        const q = props.quote ?? '"';
        const qi = (n: string) => q + n.split(q === "[" ? "]" : q).join(q === "[" ? "]]" : q + q) + (q === "[" ? "]" : q);
        const table = props.tableName ?? "table_name";
        out = data
          .map((r) => `INSERT INTO ${table} (${cols.map((c) => qi(c.name)).join(", ")}) VALUES (${r.map((v, i) => sqlLiteral(v, cols[i].kind)).join(", ")});`)
          .join("\n");
      }
      navigator.clipboard?.writeText(out);
    },
    [selectionMatrix, props.quote, props.tableName],
  );

  // ---- editing ------------------------------------------------------------------------
  const canEdit = (c: number) => !!editable && !columns[c]?.readOnly;

  const startEdit = (r: number, c: number, seed?: string) => {
    if (!canEdit(c) || !rows[r] || props.rowState?.(r) === "deleted") return;
    const v = rows[r][c];
    const initial = v === null || v === undefined ? "" : typeof v === "object" && v && ("$geo" in v) ? ((v as any).wkt ?? JSON.stringify((v as any).$geo)) : cellText(v);
    setEditing({ r, c, value: seed ?? initial, initial });
  };

  const commitEdit = (value: unknown, move?: "down" | "right" | "left") => {
    if (!editing) return;
    const { r, c } = editing;
    setEditing(null);
    const orig = rows[r]?.[c];
    const same = value !== null && typeof value === "string" && editing.initial === value && orig !== null;
    if (!same) props.onCommitEdit?.(r, c, value);
    if (move === "down") select(r + 1, c, false);
    else if (move === "right") select(r, c + 1, false);
    else if (move === "left") select(r, c - 1, false);
    root.current?.focus();
  };

  // ---- keyboard -------------------------------------------------------------------------
  const onKeyDown = (e: React.KeyboardEvent) => {
    if (editing) return;
    const mod = e.metaKey || e.ctrlKey;
    if (!sel) {
      if (rows.length && ["ArrowDown", "ArrowUp", "ArrowLeft", "ArrowRight", "Enter"].includes(e.key)) {
        e.preventDefault();
        select(0, 0, false);
      }
      return;
    }
    const { r, c } = sel.active;
    const pageRows = Math.max(1, Math.floor(view.h / ROW_H) - 2);
    const move = (dr: number, dc: number) => {
      e.preventDefault();
      select(r + dr, c + dc, e.shiftKey);
    };
    switch (e.key) {
      case "ArrowDown": return move(mod ? rows.length : 1, 0);
      case "ArrowUp": return move(mod ? -rows.length : -1, 0);
      case "ArrowRight": return move(0, mod ? columns.length : 1);
      case "ArrowLeft": return move(0, mod ? -columns.length : -1);
      case "PageDown": return move(pageRows, 0);
      case "PageUp": return move(-pageRows, 0);
      case "Home": return move(mod ? -rows.length : 0, -columns.length);
      case "End": return move(mod ? rows.length : 0, columns.length);
      case "Tab":
        e.preventDefault();
        return select(r, c + (e.shiftKey ? -1 : 1), false);
      case "Enter":
      case "F2":
        e.preventDefault();
        if (e.key === "Enter" && !canEdit(c)) return props.onInspect?.(r, c);
        return startEdit(r, c);
      case "Escape":
        return select(r, c, false);
      case " ":
        if (canEdit(c) && columns[c].kind === "bool") {
          e.preventDefault();
          const v = rows[r][c];
          props.onCommitEdit?.(r, c, !(v === true || v === 1 || v === "1" || v === "t"));
        }
        return;
      case "Delete":
      case "Backspace":
        if (sel.rowsMode && editable && props.onDeleteRows) {
          e.preventDefault();
          const rs: number[] = [];
          for (let i = sel.r0; i <= sel.r1; i++) rs.push(i);
          props.onDeleteRows(rs);
        }
        return;
    }
    if (mod && (e.key === "c" || e.key === "C")) {
      e.preventDefault();
      copyAs("tsv", e.shiftKey);
      return;
    }
    if (mod && (e.key === "a" || e.key === "A")) {
      e.preventDefault();
      setSel({ r0: 0, r1: rows.length - 1, c0: 0, c1: columns.length - 1, active: { r, c }, rowsMode: false });
      return;
    }
    if (!mod && !e.altKey && e.key.length === 1 && canEdit(c) && columns[c].kind !== "bool") {
      e.preventDefault();
      startEdit(r, c, e.key);
    }
  };

  // ---- mouse --------------------------------------------------------------------------------
  const dragging = useRef(false);
  const cellFromEvent = (e: React.MouseEvent): { r: number; c: number } | null => {
    const t = (e.target as HTMLElement).closest("[data-r]") as HTMLElement | null;
    if (!t) return null;
    return { r: Number(t.dataset.r), c: Number(t.dataset.c ?? -1) };
  };
  const onMouseDown = (e: React.MouseEvent) => {
    if (e.button !== 0 && e.button !== 2) return;
    const cell = cellFromEvent(e);
    if (!cell) return;
    if (e.button === 2 && sel && inSel(cell.r, Math.max(0, cell.c))) return; // keep selection for context menu
    if (cell.c < 0) {
      select(cell.r, 0, e.shiftKey, true);
    } else {
      select(cell.r, cell.c, e.shiftKey);
    }
    dragging.current = e.button === 0;
    root.current?.focus({ preventScroll: true });
  };
  const onMouseOver = (e: React.MouseEvent) => {
    if (!dragging.current || !(e.buttons & 1)) return;
    const cell = cellFromEvent(e);
    if (cell) select(cell.r, Math.max(0, cell.c), true, cell.c < 0);
  };
  useEffect(() => {
    const up = () => (dragging.current = false);
    window.addEventListener("mouseup", up);
    return () => window.removeEventListener("mouseup", up);
  }, []);

  // ---- column resize -------------------------------------------------------------------------
  const resizing = useRef<{ i: number; x: number; w: number } | null>(null);
  const onResizeDown = (i: number) => (e: React.PointerEvent) => {
    e.stopPropagation();
    e.preventDefault();
    resizing.current = { i, x: e.clientX, w: widths[i] };
    (e.target as HTMLElement).setPointerCapture(e.pointerId);
  };
  const onResizeMove = (e: React.PointerEvent) => {
    const rz = resizing.current;
    if (!rz) return;
    const w = Math.max(48, Math.min(1200, rz.w + e.clientX - rz.x));
    setWidths((ws) => ws.map((x, j) => (j === rz.i ? w : x)));
  };
  const autoFit = (i: number) => {
    let chars = columns[i].name.length + 3;
    for (const r of rows.slice(0, 500)) chars = Math.max(chars, cellChars(r[i]));
    setWidths((ws) => ws.map((x, j) => (j === i ? Math.round(Math.max(60, Math.min(900, chars * 7.4 + 26))) : x)));
  };

  const sortOf = (name: string) => props.sort?.findIndex((s) => s.column === name) ?? -1;
  const ctxCell = sel?.active;

  // ---- render ----------------------------------------------------------------------------------
  const visibleCols: number[] = [];
  for (let c = cStart; c < cEnd; c++) visibleCols.push(c);

  const body: ReactNode[] = [];
  for (let r = r0; r < r1; r++) {
    const row = rows[r];
    if (!row) continue;
    const state = props.rowState?.(r) ?? "clean";
    body.push(
      <div key={r} className={`g-row g-row--${state} ${r % 2 ? "g-row--alt" : ""} ${sel && r >= sel.r0 && r <= sel.r1 && sel.rowsMode ? "g-row--sel" : ""}`} style={{ top: r * ROW_H, height: ROW_H, width: rnW + totalW }}>
        <div className="g-rn" data-r={r} data-c={-1} style={{ width: rnW }}>
          {state === "new" ? "+" : state === "deleted" ? "−" : r + 1}
        </div>
        {visibleCols.map((c) => {
          const col = columns[c];
          const selected = inSel(r, c);
          const active = sel?.active.r === r && sel?.active.c === c;
          const dirty = props.cellDirty?.(r, c);
          return (
            <div
              key={c}
              data-r={r}
              data-c={c}
              className={`g-cell ${alignFor(col.kind) === "right" ? "g-cell--num" : ""} ${selected ? "is-sel" : ""} ${active ? "is-active" : ""} ${dirty ? "is-dirty" : ""}`}
              style={{ left: rnW + lefts[c], width: widths[c], height: ROW_H }}
              onDoubleClick={() => (canEdit(c) ? startEdit(r, c) : props.onInspect?.(r, c))}
            >
              <CellView value={row[c]} kind={col.kind} />
              {col.fk && row[c] !== null && props.onOpenFK && (
                <button className="g-fk" tabIndex={-1} aria-label={`Open referenced ${col.fk.refTable.name}`} onMouseDown={(e) => e.stopPropagation()} onClick={() => props.onOpenFK!(col, row[c])}>
                  <CornerDownRight />
                </button>
              )}
            </div>
          );
        })}
      </div>,
    );
  }

  return (
    <div className="grid" ref={root} tabIndex={0} onKeyDown={onKeyDown} role="grid" aria-rowcount={rows.length} aria-colcount={columns.length}>
      <RContext.Root>
        <RContext.Trigger asChild>
          <div className="grid__scroll" ref={scroller} onScroll={onScroll} onMouseDown={onMouseDown} onMouseOver={onMouseOver}>
            <div className="grid__canvas" style={{ width: rnW + totalW, height: totalH }}>
              <div className="g-head" style={{ height: HEADER_H, width: rnW + totalW }}>
                <div className="g-corner" style={{ width: rnW }} onClick={() => rows.length && setSel({ r0: 0, r1: rows.length - 1, c0: 0, c1: columns.length - 1, active: { r: 0, c: 0 }, rowsMode: true })} title="Select all" />
                {visibleCols.map((c) => {
                  const col = columns[c];
                  const si = sortOf(col.name);
                  const s = si >= 0 ? props.sort![si] : undefined;
                  return (
                    <div
                      key={c}
                      className={`g-hcell ${s ? "is-sorted" : ""} ${sel && c >= sel.c0 && c <= sel.c1 && !sel.rowsMode ? "is-selcol" : ""}`}
                      style={{ left: rnW + lefts[c], width: widths[c] }}
                      onClick={(e) => props.onSort?.(col.name, e.shiftKey)}
                      title={[col.name, col.type, col.comment].filter(Boolean).join(" — ")}
                      role="columnheader"
                      aria-sort={s ? (s.desc ? "descending" : "ascending") : "none"}
                    >
                      <div className="g-hcell__top">
                        {col.pk && <KeyRound className="g-hicon g-hicon--pk" />}
                        {col.fk && <Link2 className="g-hicon g-hicon--fk" />}
                        <span className="g-hcell__name truncate">{col.name}</span>
                        {s && (
                          <span className="g-sort">
                            {s.desc ? <ArrowDown /> : <ArrowUp />}
                            {(props.sort?.length ?? 0) > 1 && <sub>{si + 1}</sub>}
                          </span>
                        )}
                      </div>
                      <div className="g-hcell__type truncate">{col.type}{col.nullable === false ? "" : ""}</div>
                      <div className="g-resize" onPointerDown={onResizeDown(c)} onPointerMove={onResizeMove} onPointerUp={() => (resizing.current = null)} onDoubleClick={(e) => { e.stopPropagation(); autoFit(c); }} onClick={(e) => e.stopPropagation()} />
                    </div>
                  );
                })}
              </div>
              <div className="g-body" style={{ top: HEADER_H }}>
                {body}
                {props.hasMore && (
                  <div className="g-more" style={{ top: rows.length * ROW_H, height: ROW_H * 2, width: Math.min(rnW + totalW, view.w) , left: view.left }}>
                    <span className="spinner" /> Loading more rows…
                  </div>
                )}
              </div>
              {editing && (
                <CellEditor
                  key={`${editing.r}:${editing.c}`}
                  col={columns[editing.c]}
                  value={editing.value}
                  style={{ top: HEADER_H + editing.r * ROW_H, left: rnW + lefts[editing.c], width: Math.max(widths[editing.c], 180), minHeight: ROW_H }}
                  onCommit={commitEdit}
                  onCancel={() => { setEditing(null); root.current?.focus(); }}
                />
              )}
            </div>
            {rows.length === 0 && props.empty && <div className="grid__empty">{props.empty}</div>}
          </div>
        </RContext.Trigger>
        <RContext.Portal>
          <RContext.Content className="menu">
            {ctxCell && rows[ctxCell.r] && (
              <>
                <CtxItem icon={<Copy />} onSelect={() => navigator.clipboard?.writeText(cellText(rows[ctxCell.r][ctxCell.c]))}>Copy value</CtxItem>
                <CtxItem icon={<Copy />} onSelect={() => copyAs("tsv", true)} hint="⇧⌘C">Copy with headers</CtxItem>
                <RContext.Sub>
                  <RContext.SubTrigger className="menu__item"><Copy /><span className="grow">Copy as</span>›</RContext.SubTrigger>
                  <RContext.Portal>
                    <RContext.SubContent className="menu" sideOffset={4}>
                      <CtxItem onSelect={() => copyAs("csv")}>CSV</CtxItem>
                      <CtxItem onSelect={() => copyAs("json")}>JSON</CtxItem>
                      <CtxItem onSelect={() => copyAs("markdown")}>Markdown table</CtxItem>
                      <CtxItem onSelect={() => copyAs("insert")}>SQL INSERT statements</CtxItem>
                    </RContext.SubContent>
                  </RContext.Portal>
                </RContext.Sub>
                <RContext.Separator className="menu__sep" />
                {props.onFilterBy && (
                  <>
                    <CtxItem icon={<Filter />} onSelect={() => props.onFilterBy!(columns[ctxCell.c], rows[ctxCell.r][ctxCell.c], false)}>Filter to this value</CtxItem>
                    <CtxItem icon={<Filter />} onSelect={() => props.onFilterBy!(columns[ctxCell.c], rows[ctxCell.r][ctxCell.c], true)}>Exclude this value</CtxItem>
                  </>
                )}
                {columns[ctxCell.c]?.fk && props.onOpenFK && rows[ctxCell.r][ctxCell.c] !== null && (
                  <CtxItem icon={<CornerDownRight />} onSelect={() => props.onOpenFK!(columns[ctxCell.c], rows[ctxCell.r][ctxCell.c])}>
                    Open referenced {columns[ctxCell.c].fk!.refTable.name}
                  </CtxItem>
                )}
                {props.onInspect && <CtxItem icon={<Eye />} onSelect={() => props.onInspect!(ctxCell.r, ctxCell.c)}>Inspect value</CtxItem>}
                {editable && (
                  <>
                    <RContext.Separator className="menu__sep" />
                    {canEdit(ctxCell.c) && columns[ctxCell.c].nullable !== false && (
                      <CtxItem icon={<Ban />} onSelect={() => props.onCommitEdit?.(ctxCell.r, ctxCell.c, null)}>Set NULL</CtxItem>
                    )}
                    {props.onDeleteRows && (
                      <CtxItem icon={<Trash2 />} danger onSelect={() => {
                        const rs: number[] = [];
                        for (let i = sel!.r0; i <= sel!.r1; i++) rs.push(i);
                        props.onDeleteRows!(rs);
                      }}>
                        {sel && sel.r1 > sel.r0 ? `Delete ${sel.r1 - sel.r0 + 1} rows` : "Delete row"}
                      </CtxItem>
                    )}
                  </>
                )}
              </>
            )}
          </RContext.Content>
        </RContext.Portal>
      </RContext.Root>
      {props.footer}
    </div>
  );
}

function CtxItem({ icon, children, onSelect, danger, hint }: { icon?: ReactNode; children: ReactNode; onSelect(): void; danger?: boolean; hint?: string }) {
  return (
    <RContext.Item className={`menu__item ${danger ? "menu__item--danger" : ""}`} onSelect={onSelect}>
      {icon}
      <span className="grow">{children}</span>
      {hint && <span className="menu__hint">{hint}</span>}
    </RContext.Item>
  );
}

function CellEditor({ col, value, style, onCommit, onCancel }: {
  col: GridColumn; value: string; style: React.CSSProperties;
  onCommit(v: unknown, move?: "down" | "right" | "left"): void; onCancel(): void;
}) {
  const [v, setV] = useState(value);
  const ref = useRef<HTMLTextAreaElement & HTMLSelectElement>(null);
  const multiline = col.kind === "text" || col.kind === "json" || v.includes("\n") || v.length > 60;
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    el.focus();
    if ("setSelectionRange" in el && value.length === 1 && col.kind !== "enum") el.setSelectionRange(1, 1);
    else if ("select" in el && col.kind !== "enum") (el as HTMLTextAreaElement).select();
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const keys = (e: React.KeyboardEvent) => {
    e.stopPropagation();
    if (e.key === "Escape") return onCancel();
    if (e.key === "Enter" && !(multiline && e.shiftKey) && !(e.altKey)) {
      e.preventDefault();
      return onCommit(v, "down");
    }
    if (e.key === "Tab") {
      e.preventDefault();
      return onCommit(v, e.shiftKey ? "left" : "right");
    }
  };

  return (
    <div className="g-editor" style={style} onMouseDown={(e) => e.stopPropagation()}>
      {col.enumValues?.length ? (
        <select ref={ref} className="g-editor__input" value={v} onChange={(e) => { setV(e.target.value); onCommit(e.target.value); }} onKeyDown={keys} onBlur={() => onCommit(v)}>
          {!col.enumValues.includes(v) && <option value={v}>{v}</option>}
          {col.enumValues.map((x) => <option key={x} value={x}>{x}</option>)}
        </select>
      ) : (
        <textarea
          ref={ref}
          className={`g-editor__input ${multiline ? "is-multi" : ""}`}
          value={v}
          rows={multiline ? Math.min(8, Math.max(3, v.split("\n").length)) : 1}
          onChange={(e) => setV(e.target.value)}
          onKeyDown={keys}
          onBlur={() => onCommit(v)}
          spellCheck={false}
        />
      )}
      <div className="g-editor__bar">
        <span className="faint mono">{col.type}</span>
        <span className="spacer" />
        {col.nullable !== false && (
          <button className="g-editor__null" onMouseDown={(e) => { e.preventDefault(); onCommit(null); }}>Set NULL</button>
        )}
        <span className="faint">{multiline ? "⇧↵ newline · ↵ save" : "↵ save · esc cancel"}</span>
      </div>
    </div>
  );
}

function jsonValue(v: Cell): unknown {
  if (v === null || typeof v !== "object" || Array.isArray(v)) return v;
  const o = v as Record<string, any>;
  if ("$geo" in o) return o.$geo;
  if ("$text" in o) return o.$text;
  if ("$bin" in o) return { base64: o.$bin, size: o.size };
  return o;
}

function sqlLiteral(v: Cell, kind: ValueKind): string {
  if (v === null || v === undefined) return "NULL";
  if (typeof v === "number") return String(v);
  if (typeof v === "boolean") return v ? "TRUE" : "FALSE";
  if (typeof v === "string" && (kind === "int" || kind === "float" || kind === "decimal") && /^-?\d+(\.\d+)?([eE][-+]?\d+)?$/.test(v)) return v;
  if (typeof v === "object" && !Array.isArray(v) && "$bin" in (v as any)) return "X'" + cellText(v).slice(2) + "'";
  return "'" + cellText(v).replace(/'/g, "''") + "'";
}
