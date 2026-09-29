import { useEffect, useRef, useState } from "react";
import type { Cell, GeoJSON, ValueKind } from "../../lib/types";
import { useTheme } from "../../lib/hooks";
import { Spinner } from "../../components/ui";
import workerUrl from "maplibre-gl/dist/maplibre-gl-worker.mjs?worker&url";
import "./map.css";

export function hasGeometry(cols: { name: string; kind: ValueKind }[]) {
  return cols.some((c) => c.kind === "geometry");
}

interface Props {
  columns: { name: string; kind: ValueKind }[];
  rows: Cell[][];
  onPick?(row: number): void;
  compact?: boolean;
}

function styleURL(theme: "dark" | "light") {
  return theme === "dark" ? "https://tiles.openfreemap.org/styles/dark" : "https://tiles.openfreemap.org/styles/positron";
}

// Renders every geometry cell as a feature. MapLibre is loaded on demand so
// the map code never slows down the rest of the app.
export function MapView({ columns, rows, onPick, compact }: Props) {
  const el = useRef<HTMLDivElement>(null);
  const map = useRef<any>(null);
  const theme = useTheme();
  const [ready, setReady] = useState(false);
  const [error, setError] = useState("");
  const [geoCol, setGeoCol] = useState(() => columns.findIndex((c) => c.kind === "geometry"));
  const geoCols = columns.map((c, i) => ({ ...c, i })).filter((c) => c.kind === "geometry");

  const features = rows
    .map((r, idx) => {
      const v = r[geoCol] as { $geo?: GeoJSON } | null;
      if (!v || typeof v !== "object" || !("$geo" in v)) return null;
      return { type: "Feature", id: idx, properties: { row: idx }, geometry: v.$geo };
    })
    .filter(Boolean);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const maplibre = await import("maplibre-gl");
        // Bundled by Vite as a same-origin module worker, which the strict CSP allows.
        maplibre.setWorkerUrl(workerUrl);
        await import("maplibre-gl/dist/maplibre-gl.css");
        if (cancelled || !el.current) return;
        const m = new maplibre.Map({
          container: el.current,
          style: styleURL(theme),
          center: [0, 20],
          zoom: 1,
          attributionControl: { compact: true },
        });
        m.addControl(new maplibre.NavigationControl({ showCompass: false }), "top-right");
        m.on("load", () => !cancelled && setReady(true));
        m.on("error", (e: any) => {
          // Basemap unreachable (offline or blocked): keep drawing features on a plain background.
          if (String(e?.error?.message ?? "").includes("Failed to fetch")) setError("Basemap unavailable — showing geometry only.");
        });
        map.current = m;
      } catch (e) {
        setError(String(e));
      }
    })();
    return () => {
      cancelled = true;
      map.current?.remove();
      map.current = null;
    };
  }, [theme]);

  useEffect(() => {
    const m = map.current;
    if (!m || !ready) return;
    const data = { type: "FeatureCollection", features } as any;
    const src = m.getSource("rs");
    if (src) src.setData(data);
    else {
      m.addSource("rs", { type: "geojson", data });
      const accent = "#e8b84a";
      m.addLayer({ id: "rs-fill", type: "fill", source: "rs", filter: ["==", ["geometry-type"], "Polygon"], paint: { "fill-color": accent, "fill-opacity": 0.18 } });
      m.addLayer({ id: "rs-line", type: "line", source: "rs", filter: ["in", ["geometry-type"], ["literal", ["LineString", "Polygon"]]], paint: { "line-color": accent, "line-width": 2 } });
      m.addLayer({ id: "rs-point", type: "circle", source: "rs", filter: ["==", ["geometry-type"], "Point"],
        paint: { "circle-radius": 5, "circle-color": accent, "circle-stroke-color": theme === "dark" ? "#0b0f17" : "#ffffff", "circle-stroke-width": 1.5 } });
      for (const layer of ["rs-fill", "rs-line", "rs-point"]) {
        m.on("click", layer, (e: any) => {
          const row = e.features?.[0]?.properties?.row;
          if (row !== undefined) onPick?.(Number(row));
        });
        m.on("mouseenter", layer, () => (m.getCanvas().style.cursor = "pointer"));
        m.on("mouseleave", layer, () => (m.getCanvas().style.cursor = ""));
      }
    }
    // Fit to the data.
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    const visit = (c: any) => {
      if (typeof c?.[0] === "number") {
        minX = Math.min(minX, c[0]); maxX = Math.max(maxX, c[0]);
        minY = Math.min(minY, c[1]); maxY = Math.max(maxY, c[1]);
      } else if (Array.isArray(c)) c.forEach(visit);
    };
    features.forEach((f: any) => visit(f.geometry.coordinates ?? f.geometry.geometries?.map((g: any) => g.coordinates)));
    if (isFinite(minX) && Math.abs(minX) <= 180 && Math.abs(maxY) <= 90) {
      m.fitBounds([[minX, minY], [maxX, maxY]], { padding: 48, maxZoom: 16, duration: 0 });
    }
  }, [ready, features.length, geoCol]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div className={`mapview ${compact ? "mapview--compact" : ""}`}>
      <div ref={el} className="mapview__map" />
      {!ready && !error && <div className="mapview__loading"><Spinner large /></div>}
      <div className="mapview__bar">
        {geoCols.length > 1 && (
          <select className="select" value={geoCol} onChange={(e) => setGeoCol(Number(e.target.value))} aria-label="Geometry column">
            {geoCols.map((c) => <option key={c.i} value={c.i}>{c.name}</option>)}
          </select>
        )}
        <span className="mapview__count">{features.length} feature{features.length === 1 ? "" : "s"}{rows.length > features.length ? ` · ${rows.length - features.length} without geometry` : ""}</span>
        {error && <span className="mapview__err">{error}</span>}
      </div>
    </div>
  );
}
