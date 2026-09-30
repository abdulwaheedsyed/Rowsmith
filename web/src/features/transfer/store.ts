import { create } from "zustand";
import type { Connection, ObjectRef } from "../../lib/types";

export interface BrowseSpec {
  filters?: unknown[];
  sort?: { column: string; desc?: boolean }[];
  search?: string;
  where?: string;
  columns?: string[];
}

export type ExportSource =
  | { kind: "table"; ref: ObjectRef; browse?: BrowseSpec; rows?: number; filtered?: boolean }
  | { kind: "query"; sql: string; database?: string; schema?: string }
  | { kind: "dump"; database?: string; schema?: string };

export type ImportTarget =
  | { kind: "table"; ref?: ObjectRef; database?: string; schema?: string }
  | { kind: "sql"; database?: string; schema?: string };

type Request = { type: "export"; conn: Connection; source: ExportSource } | { type: "import"; conn: Connection; target: ImportTarget };

export const useTransfer = create<{ req: Request | null; set(r: Request | null): void }>((set) => ({
  req: null,
  set: (req) => set({ req }),
}));

export const openExport = (conn: Connection, source: ExportSource) => useTransfer.getState().set({ type: "export", conn, source });
export const openImport = (conn: Connection, target: ImportTarget) => useTransfer.getState().set({ type: "import", conn, target });

// Per-viewer memory of the last choices; storage may be unavailable.
export function remember<T>(key: string, fallback: T): T {
  try {
    const v = localStorage.getItem("rowsmith.transfer." + key);
    return v ? { ...fallback, ...JSON.parse(v) } : fallback;
  } catch {
    return fallback;
  }
}

export function keep(key: string, value: unknown) {
  try {
    localStorage.setItem("rowsmith.transfer." + key, JSON.stringify(value));
  } catch {
    /* private mode or storage disabled */
  }
}
