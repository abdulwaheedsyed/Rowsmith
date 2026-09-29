import type { Cell, ValueKind } from "./types";

export function bytes(n?: number | null): string {
  if (n === undefined || n === null) return "";
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB", "PB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

const compactFmt = new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 });
const intFmt = new Intl.NumberFormat();

export function compact(n?: number | null): string {
  if (n === undefined || n === null) return "";
  return n < 10000 ? intFmt.format(n) : compactFmt.format(n);
}

export function int(n?: number | null): string {
  if (n === undefined || n === null) return "";
  return intFmt.format(n);
}

export function duration(ms?: number | null): string {
  if (ms === undefined || ms === null) return "";
  if (ms < 1) return `${(ms * 1000).toFixed(0)} µs`;
  if (ms < 1000) return `${ms < 10 ? ms.toFixed(1) : Math.round(ms)} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(s < 10 ? 2 : 1)} s`;
  const m = Math.floor(s / 60);
  return `${m}m ${Math.round(s % 60)}s`;
}

// The temper scale encodes heat: a duration (or relative cost) maps onto the
// oxide colors of heated steel, from pale straw to blue.
const temperStops = ["--temper-0", "--temper-1", "--temper-2", "--temper-3", "--temper-4", "--temper-5"];

export function temperForMs(ms: number): string {
  // 0–50ms straw · ~200ms dark straw · ~1s bronze · ~5s plum · ~20s peacock · 60s+ blue
  const thresholds = [50, 200, 1000, 5000, 20000];
  let i = 0;
  while (i < thresholds.length && ms > thresholds[i]) i++;
  return `var(${temperStops[i]})`;
}

export function temperForShare(share: number): string {
  const i = Math.min(temperStops.length - 1, Math.max(0, Math.floor(share * temperStops.length)));
  return `var(${temperStops[i]})`;
}

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
export function ago(ms?: number): string {
  if (!ms) return "never";
  const diff = (ms - Date.now()) / 1000;
  const abs = Math.abs(diff);
  if (abs < 45) return "just now";
  if (abs < 3600) return rtf.format(Math.round(diff / 60), "minute");
  if (abs < 86400) return rtf.format(Math.round(diff / 3600), "hour");
  if (abs < 86400 * 30) return rtf.format(Math.round(diff / 86400), "day");
  return new Date(ms).toLocaleDateString();
}

export function isObjectCell(v: Cell): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

// Plain-text rendering of a cell (copy, CSV, search).
export function cellText(v: Cell): string {
  if (v === null || v === undefined) return "";
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  if (Array.isArray(v)) return JSON.stringify(v);
  const o = v as Record<string, any>;
  if ("$text" in o) return o.$text;
  if ("$bin" in o) return "0x" + b64ToHex(o.$bin);
  if ("$geo" in o) return o.wkt ?? JSON.stringify(o.$geo);
  if ("$oid" in o) return o.$oid;
  if ("$date" in o) return o.$date;
  if ("$numberDecimal" in o) return o.$numberDecimal;
  if ("$numberLong" in o) return o.$numberLong;
  if ("$uuid" in o) return o.$uuid;
  return JSON.stringify(o);
}

export function b64ToHex(b64: string, max = 4096): string {
  try {
    const raw = atob(b64);
    let out = "";
    for (let i = 0; i < raw.length && i < max; i++) out += raw.charCodeAt(i).toString(16).padStart(2, "0");
    return out.toUpperCase();
  } catch {
    return "";
  }
}

export function isNumericKind(k: ValueKind) {
  return k === "int" || k === "float" || k === "decimal";
}

export function pluralize(n: number, one: string, many = one + "s") {
  return `${int(n)} ${n === 1 ? one : many}`;
}

export function csvEscape(s: string, sep = ","): string {
  if (s.includes(sep) || s.includes('"') || s.includes("\n") || s.includes("\r")) return '"' + s.replace(/"/g, '""') + '"';
  return s;
}

export function isMac() {
  return typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
}

export const modKey = () => (isMac() ? "⌘" : "Ctrl");
