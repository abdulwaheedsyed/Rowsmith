import { create } from "zustand";
import type { Cell, QueryError, ResultColumn, StatementKind, Plan, PendingStatement } from "../../lib/types";

export interface ResultSet {
  columns: ResultColumn[];
  rows: Cell[][];
  summary?: { rowCount: number; rowsAffected?: number; truncated: boolean; durationMs: number; bytesProcessed?: number };
}

export interface StmtRun {
  index: number;
  sql: string;
  line: number;
  kind: StatementKind;
  sets: ResultSet[];
  notices: { level: string; text: string }[];
  error?: QueryError;
  ms?: number;
  done: boolean;
}

export interface RunState {
  status: "idle" | "running" | "done" | "error" | "cancelled";
  startedAt?: number;
  ms?: number;
  stmts: StmtRun[];
  error?: string;
  inTx?: boolean;
  console?: string;
  plan?: Plan;
  planError?: string;
  planLoading?: boolean;
  confirm?: { statements: PendingStatement[]; environment: string };
  lineOffset: number; // for selection runs: editor line of the selection start - 1
}

interface RunsStore {
  runs: Record<string, RunState>;
  get(tab: string): RunState;
  set(tab: string, fn: (r: RunState) => RunState): void;
  clear(tab: string): void;
}

const empty: RunState = { status: "idle", stmts: [], lineOffset: 0 };

// Results live outside components so they survive tab switches.
export const useRuns = create<RunsStore>((set, get) => ({
  runs: {},
  get: (tab) => get().runs[tab] ?? empty,
  set(tab, fn) {
    set({ runs: { ...get().runs, [tab]: fn(get().runs[tab] ?? empty) } });
  },
  clear(tab) {
    const runs = { ...get().runs };
    delete runs[tab];
    set({ runs });
  },
}));
