import { useEffect, useMemo, useState } from "react";
import { X, Copy, Check, Download } from "lucide-react";
import type { Cell, GeoJSON } from "../../lib/types";
import { bytes, cellText, partialCell } from "../../lib/format";
import { Button } from "../../components/ui";
import type { GridColumn } from "./DataGrid";
import { MapView } from "../map/MapView";

function hexDump(b64: string, max = 4096) {
  const raw = atob(b64);
  const lines: string[] = [];
  for (let off = 0; off < Math.min(raw.length, max); off += 16) {
    const chunk = raw.slice(off, off + 16);
    const hex = [...chunk].map((c) => c.charCodeAt(0).toString(16).padStart(2, "0")).join(" ");
    const ascii = [...chunk].map((c) => (c.charCodeAt(0) >= 32 && c.charCodeAt(0) < 127 ? c : "·")).join("");
    lines.push(`${off.toString(16).padStart(6, "0")}  ${hex.padEnd(47)}  ${ascii}`);
  }
  return lines.join("\n");
}

function pretty(v: string) {
  try {
    return JSON.stringify(JSON.parse(v), null, 2);
  } catch {
    return v;
  }
}

export function Inspector({ column, value, editable, onChange, onClose }: {
  column?: GridColumn; value?: Cell; editable?: boolean; onChange?(v: unknown): void; onClose(): void;
}) {
  const [draft, setDraft] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  useEffect(() => setDraft(null), [column?.name, value]);

  const text = useMemo(() => {
    if (value === undefined) return "";
    if (column?.kind === "json" && typeof value === "string") return pretty(value);
    if (typeof value === "object" && value && !Array.isArray(value)) {
      const o = value as Record<string, any>;
      if ("$geo" in o) return JSON.stringify(o.$geo, null, 2);
      if ("$bin" in o) return "";
    }
    if (Array.isArray(value) || (typeof value === "object" && value !== null)) return JSON.stringify(value, null, 2);
    return cellText(value);
  }, [value, column?.kind]);

  const o = value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, any>) : null;

  return (
    <aside className="inspector" aria-label="Value inspector">
      <div className="inspector__head">
        <div className="grow" style={{ minWidth: 0 }}>
          <div className="inspector__col truncate">{column?.name ?? "No cell selected"}</div>
          {column && <div className="inspector__type mono">{column.type}{column.nullable === false ? " · not null" : ""}{column.pk ? " · primary key" : ""}</div>}
        </div>
        {value !== undefined && value !== null && (
          <Button variant="ghost" size="sm" icon aria-label="Copy value" onClick={() => { navigator.clipboard?.writeText(o && "$bin" in o ? cellText(value) : text); setCopied(true); setTimeout(() => setCopied(false), 1200); }}>
            {copied ? <Check /> : <Copy />}
          </Button>
        )}
        <Button variant="ghost" size="sm" icon aria-label="Close inspector" onClick={onClose}><X /></Button>
      </div>
      <div className="inspector__body">
        {value === undefined ? (
          <p className="muted inspector__hint">Select a cell to see its full value. Press Enter on a read-only cell to open it here.</p>
        ) : value === null ? (
          <div className="inspector__null">NULL</div>
        ) : o && "$bin" in o ? (
          <>
            <div className="inspector__meta">{o.mime ?? "binary"} · {bytes(o.size)}{o.size > 65536 ? " · first 64 KB shown" : ""}</div>
            {o.mime?.startsWith("image/") && <img className="inspector__img" src={`data:${o.mime};base64,${o.$bin}`} alt={column?.name} />}
            <pre className="inspector__hex">{hexDump(o.$bin)}</pre>
            <Button size="sm" onClick={() => {
              const raw = atob(o.$bin);
              const buf = new Uint8Array(raw.length);
              for (let i = 0; i < raw.length; i++) buf[i] = raw.charCodeAt(i);
              const a = document.createElement("a");
              a.href = URL.createObjectURL(new Blob([buf], { type: o.mime ?? "application/octet-stream" }));
              a.download = `${column?.name ?? "value"}.${(o.mime ?? "application/bin").split("/")[1]}`;
              a.click();
            }}><Download /> Download</Button>
          </>
        ) : o && "$geo" in o ? (
          <>
            <MapView compact columns={[{ name: column?.name ?? "geom", kind: "geometry" }]} rows={[[value]]} />
            <div className="inspector__meta">{(o.$geo as GeoJSON).type}{o.srid ? ` · SRID ${o.srid}` : ""}</div>
            {partialCell(value) && <div className="inspector__partial">{partialCell(value)} Query it with SQL to see or change every vertex.</div>}
            {o.wkt && <pre className="inspector__code">{o.wkt}</pre>}
            <pre className="inspector__code">{text}</pre>
          </>
        ) : editable && onChange && !partialCell(value) ? (
          <>
            <textarea className="textarea inspector__edit" value={draft ?? text} onChange={(e) => setDraft(e.target.value)} spellCheck={false} />
            <div className="row gap-3">
              <span className="faint inspector__meta">{(draft ?? text).length} characters</span>
              <span className="spacer" />
              {column?.nullable !== false && <Button size="sm" variant="ghost" onClick={() => onChange(null)}>Set NULL</Button>}
              <Button size="sm" variant="primary" disabled={draft === null || draft === text} onClick={() => { onChange(column?.kind === "json" ? compactJSON(draft!) : draft); setDraft(null); }}>Apply</Button>
            </div>
          </>
        ) : (
          <>
            {partialCell(value) && <div className="inspector__partial">{partialCell(value)} Query it with SQL to see or change the whole value.</div>}
            <pre className="inspector__code">{text}</pre>
          </>
        )}
      </div>
    </aside>
  );
}

function compactJSON(s: string) {
  try {
    return JSON.stringify(JSON.parse(s));
  } catch {
    return s;
  }
}
