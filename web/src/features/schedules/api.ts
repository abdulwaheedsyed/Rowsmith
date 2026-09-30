import { useQuery } from "@tanstack/react-query";
import { get } from "../../lib/api";
import type { Environment } from "../../lib/types";

export type OutputFormat = "xlsx" | "csv" | "tsv" | "json" | "ndjson";
export type AlertKind = "" | "rows" | "value" | "changed";

export interface ScheduleConfig {
  output: { format: OutputFormat; gzip: boolean; maxRows: number; attach: boolean; preview: number };
  alert: { kind: AlertKind; op: string; value: string; column: string; edge: boolean };
  delivery: { emails: string[]; onFailure: boolean };
}

export interface Schedule {
  id: string;
  ownerId: string;
  ownerName: string;
  ownerEmail: string;
  connectionId: string;
  connectionName: string;
  driver: string;
  environment: Environment;
  database: string;
  schema: string;
  name: string;
  description: string;
  body: string;
  cron: string;
  timezone: string;
  enabled: boolean;
  pausedReason: string;
  nextRunAt: number;
  runningSince: number;
  lastRunAt: number;
  lastStatus: RunStatus | "";
  failures: number;
  createdAt: number;
  updatedAt: number;
  config: ScheduleConfig;
  webhook: { set: boolean; host?: string };
  isOwner: boolean;
}

export type RunStatus = "running" | "ok" | "alert" | "quiet" | "failed";

export interface ScheduleRun {
  id: string;
  scheduleId: string;
  trigger: "schedule" | "manual";
  triggeredBy: string;
  startedAt: number;
  finishedAt: number;
  status: RunStatus;
  rowCount: number;
  truncated: boolean;
  observed: string;
  fileName: string;
  fileType: string;
  fileSize: number;
  fileExpires: number;
  hasFile: boolean;
  error: string;
  delivery: { emailed?: string[]; skipped?: string[]; emailError?: string; attached?: boolean; webhook?: string; note?: string } | null;
}

export interface SchedulePolicy {
  email: boolean;
  domains: string[];
  anyone: boolean;
  webhooks: boolean;
  privateWebhooks: boolean;
  retentionDays: number;
  maxAttachMB: number;
  canCreate: boolean;
  minIntervalMinutes: number;
}

export interface TestResult {
  columns: string[];
  rows: string[][];
  rowCount: number;
  truncated: boolean;
  ms: number;
  alert?: { fired: boolean; observed: string; condition: string; error?: string };
}

export const defaultConfig = (): ScheduleConfig => ({
  output: { format: "xlsx", gzip: false, maxRows: 100000, attach: true, preview: 10 },
  alert: { kind: "", op: ">", value: "0", column: "", edge: false },
  delivery: { emails: [], onFailure: true },
});

export const useSchedules = (all: boolean) =>
  useQuery({ queryKey: ["schedules", all], queryFn: () => get<Schedule[]>(all ? "schedules?all=1" : "schedules"), refetchInterval: (q) => (q.state.data?.some((s) => s.runningSince) ? 3000 : 30_000) });

export const useSchedule = (id: string) =>
  useQuery({ queryKey: ["schedule", id], queryFn: () => get<Schedule>(`schedules/${id}`), refetchInterval: (q) => (q.state.data?.runningSince ? 2000 : 30_000) });

export const useRuns = (id: string, fast: boolean) =>
  useQuery({ queryKey: ["schedule-runs", id], queryFn: () => get<ScheduleRun[]>(`schedules/${id}/runs`), refetchInterval: fast ? 2000 : 30_000 });

export const usePolicy = () => useQuery({ queryKey: ["schedule-policy"], queryFn: () => get<SchedulePolicy>("schedules/policy"), staleTime: 60_000 });

export const FORMAT_LABELS: Record<OutputFormat, string> = { xlsx: "Excel", csv: "CSV", tsv: "TSV", json: "JSON", ndjson: "NDJSON" };

const OPS: Record<string, string> = { ">": "is more than", ">=": "is at least", "<": "is less than", "<=": "is at most", "=": "is", "!=": "is not" };
export const opLabel = (op: string) => OPS[op] ?? op;

export function describeAlert(a: ScheduleConfig["alert"]): string {
  switch (a.kind) {
    case "rows":
      return `row count ${opLabel(a.op)} ${a.value}`;
    case "value":
      return `${a.column || "first column"} ${opLabel(a.op)} ${a.value}`;
    case "changed":
      return "the result changes";
  }
  return "";
}
