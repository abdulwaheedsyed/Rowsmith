import type { Cell, ValueKind, GeoJSON } from "../../lib/types";
import { bytes, isNumericKind } from "../../lib/format";

// Rendering of a single value inside the grid. Each kind gets a distinct,
// quiet treatment so a column can be scanned without reading every cell.

export function geoSummary(g: GeoJSON): string {
  const t = g.type;
  const c = g.coordinates as any;
  if (t === "Point" && Array.isArray(c)) return `${fmtCoord(c[0])}, ${fmtCoord(c[1])}`;
  if (t === "LineString" && Array.isArray(c)) return `${c.length} points`;
  if (t === "Polygon" && Array.isArray(c)) return `${c[0]?.length ?? 0} vertices`;
  if (t?.startsWith("Multi") && Array.isArray(c)) return `${c.length} parts`;
  if (t === "GeometryCollection") return `${g.geometries?.length ?? 0} geometries`;
  return "";
}

function fmtCoord(n: unknown) {
  return typeof n === "number" ? n.toFixed(5).replace(/0+$/, "").replace(/\.$/, "") : String(n);
}

// Compact preview that also collapses Extended JSON wrappers inside documents.
function jsonPreview(v: unknown, max = 160): string {
  try {
    const s = typeof v === "string" ? v : JSON.stringify(v, (_k, x) => {
      if (x && typeof x === "object" && !Array.isArray(x)) {
        if ("$oid" in x) return `ObjectId(${x.$oid})`;
        if ("$date" in x) return x.$date;
        if ("$numberDecimal" in x) return Number(x.$numberDecimal);
        if ("$numberLong" in x) return x.$numberLong;
        if ("$geo" in x) return x.$geo;
      }
      return x;
    });
    return s.length > max ? s.slice(0, max) + "…" : s;
  } catch {
    return String(v);
  }
}

export function CellView({ value, kind }: { value: Cell; kind: ValueKind }) {
  if (value === null || value === undefined) return <span className="c-null">NULL</span>;
  if (kind === "bool" && (value === 0 || value === 1 || value === "0" || value === "1" || value === "t" || value === "f")) value = value === 1 || value === "1" || value === "t";
  if (typeof value === "boolean") return <span className={value ? "c-true" : "c-false"}>{value ? "true" : "false"}</span>;
  if (typeof value === "number") return <span className="c-num">{String(value)}</span>;
  if (typeof value === "string") {
    if (isNumericKind(kind)) return <span className="c-num">{value}</span>;
    if (kind === "date" || kind === "datetime" || kind === "timestamp" || kind === "time") return <span className="c-date">{value}</span>;
    if (kind === "json" || kind === "array") return <span className="c-json">{jsonPreview(value)}</span>;
    if (kind === "uuid") return <span className="c-uuid">{value}</span>;
    if (kind === "enum") return <span className="c-enum">{value}</span>;
    if (value === "") return <span className="c-empty">empty</span>;
    return <span className="c-text">{value.length > 400 ? value.slice(0, 400) : value.replace(/\n/g, " ↵ ")}</span>;
  }
  if (Array.isArray(value)) return <span className="c-json">{jsonPreview(value)}</span>;
  const o = value as Record<string, any>;
  if ("$oid" in o) return <span className="c-oid">{o.$oid}</span>;
  if ("$date" in o) return <span className="c-date">{String(o.$date).replace("T", " ").replace(/\.000Z$|Z$/, "")}</span>;
  if ("$numberDecimal" in o) return <span className="c-num">{o.$numberDecimal}</span>;
  if ("$numberLong" in o) return <span className="c-num">{o.$numberLong}</span>;
  if ("$uuid" in o) return <span className="c-uuid">{o.$uuid}</span>;
  if ("$bin" in o) {
    const mime: string | undefined = o.mime;
    const label = mime ? mime.split("/")[1]?.toUpperCase() : "binary";
    return (
      <span className="c-bin">
        {mime?.startsWith("image/") && <img className="c-thumb" src={`data:${mime};base64,${o.$bin}`} alt="" loading="lazy" />}
        <span className="c-tag">{label}</span>
        <span className="c-size">{bytes(o.size)}</span>
      </span>
    );
  }
  if ("$geo" in o) {
    const g = o.$geo as GeoJSON;
    return (
      <span className="c-geo">
        <span className="c-geo__glyph" data-type={g.type} />
        <span className="c-tag">{g.type}</span>
        <span className="c-geo__sum">{geoSummary(g)}</span>
      </span>
    );
  }
  if ("$text" in o) {
    return (
      <span className="c-text">
        {String(o.$text).slice(0, 400).replace(/\n/g, " ↵ ")}
        <span className="c-size"> · {bytes(o.size)}</span>
      </span>
    );
  }
  return <span className="c-json">{jsonPreview(o)}</span>;
}

export function alignFor(kind: ValueKind) {
  return isNumericKind(kind) ? "right" : "left";
}

/** Estimated display width in characters, for initial column sizing. */
export function cellChars(value: Cell): number {
  if (value === null || value === undefined) return 4;
  if (typeof value === "string") return Math.min(value.length, 60);
  if (typeof value === "number") return String(value).length;
  if (typeof value === "boolean") return 5;
  if (Array.isArray(value)) return 30;
  const o = value as Record<string, any>;
  if ("$oid" in o) return 24;
  if ("$date" in o) return 20;
  if ("$bin" in o) return 16;
  if ("$geo" in o) return 26;
  if ("$text" in o) return 60;
  return 30;
}
