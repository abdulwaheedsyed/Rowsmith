import { useQuery } from "@tanstack/react-query";
import { get } from "../../lib/api";

export interface Endpoint {
  connectionId: string;
  database: string;
  schema: string;
}

export interface MigrateOptions {
  nameCase: "keep" | "lower" | "upper" | "";
  existing: "fail" | "replace" | "keep";
  existingRows: "append" | "replace";
  data: boolean;
  indexes: boolean;
  foreignKeys: boolean;
  objects: boolean;
  verify: "counts" | "contents";
  stopOnError: boolean;
}

export interface ColumnPlan {
  source: string;
  sourceType: string;
  target: string;
  type: string;
  nullable: boolean;
  default?: string | null;
  autoIncrement?: boolean;
  generated?: string;
  values?: string[];
  noCopy?: boolean;
  include: boolean;
  notes: string[];
  lossy?: boolean;
}

export interface IndexPlan {
  name: string;
  columns: string[];
  unique: boolean;
  type?: string;
  include: boolean;
  note?: string;
}

export interface FKPlan {
  name: string;
  columns: string[];
  refTable: string;
  refColumns: string[];
  onDelete?: string;
  include: boolean;
  note?: string;
}

export interface TablePlan {
  source: { database?: string; schema?: string; name: string; kind?: string };
  target: string;
  include: boolean;
  exists: boolean;
  rows?: number;
  columns: ColumnPlan[];
  primaryKey: string[];
  indexes: IndexPlan[];
  foreignKeys: FKPlan[];
  notes: string[];
}

export interface ObjectNote {
  name: string;
  kind: string;
  reason?: string;
}

export interface Plan {
  sourceEngine: string;
  targetEngine: string;
  sameEngine: boolean;
  options: MigrateOptions;
  tables: TablePlan[];
  objects: ObjectNote[];
  skipped: ObjectNote[];
  warnings: string[];
  typeChoices: string[];
}

export interface Side {
  label: string;
  engine: string;
  version: string;
  environment: string;
  name?: string;
}

export interface PlanResponse {
  plan: Plan;
  source: Side;
  target: Side;
}

export interface Verification {
  sourceRows: number;
  targetRows: number;
  counts: "match" | "differ" | "";
  contents: "match" | "differ" | "skipped";
  columns?: string[];
  note?: string;
}

export interface TableRun {
  source: string;
  target: string;
  status: "waiting" | "creating" | "copying" | "indexing" | "verifying" | "done" | "failed" | "skipped" | "cancelled";
  rows: number;
  total: number;
  started: number;
  finished: number;
  error?: string;
  notes: string[];
  verify?: Verification;
}

export interface JobView {
  status: "running" | "done" | "failed" | "cancelled";
  phase: string;
  started: number;
  finished: number;
  tables: TableRun[];
  objects: { name: string; kind: string; status: string; error?: string }[];
  log: { at: number; level: "info" | "warn" | "error"; text: string }[];
  error?: string;
}

export interface Migration {
  id: string;
  userId: string;
  userName: string;
  sourceId: string;
  sourceLabel: string;
  targetId: string;
  targetLabel: string;
  status: "running" | "done" | "failed" | "cancelled" | "interrupted";
  tables: number;
  rows: number;
  startedAt: number;
  finishedAt: number;
  progress?: JobView;
  plan?: Plan;
}

export const useMigrations = (all: boolean) =>
  useQuery({ queryKey: ["migrations", all], queryFn: () => get<Migration[]>(all ? "migrations?all=1" : "migrations"),
    refetchInterval: (q) => (q.state.data?.some((m) => m.status === "running") ? 2000 : false) });

export const useMigration = (id: string) =>
  useQuery({ queryKey: ["migration", id], queryFn: () => get<Migration>(`migrations/${id}`),
    refetchInterval: (q) => (q.state.data?.status === "running" ? 700 : false) });

export const defaultOptions = (): MigrateOptions => ({
  nameCase: "", existing: "fail", existingRows: "append", data: true, indexes: true, foreignKeys: true, objects: false, verify: "contents", stopOnError: false,
});

export const ENGINE_NAMES: Record<string, string> = {
  mysql: "MySQL", mariadb: "MariaDB", postgres: "PostgreSQL", mssql: "SQL Server", oracle: "Oracle", sqlite: "SQLite", bigquery: "BigQuery", mongodb: "MongoDB",
};
