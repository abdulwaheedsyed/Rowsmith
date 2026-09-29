import type { QueryError } from "./types";

// Paths are relative ("api/...") so they resolve against <base href>,
// letting the same build serve from "/" or a sub-path such as "/sql/".

let csrf = "";
export function setCsrf(token: string) {
  csrf = token;
}

export class ApiError extends Error {
  status: number;
  code?: string;
  detail?: any;
  constructor(status: number, message: string, code?: string, detail?: unknown) {
    super(message);
    this.status = status;
    this.code = code;
    this.detail = detail;
  }
}

type Listener = () => void;
const unauthorized = new Set<Listener>();
export function onUnauthorized(fn: Listener) {
  unauthorized.add(fn);
  return () => {
    unauthorized.delete(fn);
  };
}

function headers(json: boolean): HeadersInit {
  const h: Record<string, string> = { Accept: "application/json" };
  if (json) h["Content-Type"] = "application/json";
  if (csrf) h["X-CSRF-Token"] = csrf;
  return h;
}

async function toError(res: Response): Promise<ApiError> {
  let msg = `${res.status} ${res.statusText}`;
  let code: string | undefined;
  let detail: unknown;
  try {
    const body = await res.json();
    if (body?.error) {
      msg = body.error.message || msg;
      code = body.error.code;
      detail = body.error.detail;
    }
  } catch {
    /* non-JSON error body */
  }
  return new ApiError(res.status, msg, code, detail);
}

export async function api<T = unknown>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  const res = await fetch("api/" + path, {
    method,
    headers: headers(body !== undefined),
    body: body !== undefined ? JSON.stringify(body) : undefined,
    credentials: "same-origin",
    signal,
  });
  if (!res.ok) {
    const err = await toError(res);
    if (res.status === 401 && !path.startsWith("auth/")) unauthorized.forEach((f) => f());
    throw err;
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export const get = <T>(path: string, signal?: AbortSignal) => api<T>("GET", path, undefined, signal);
export const post = <T>(path: string, body?: unknown, signal?: AbortSignal) => api<T>("POST", path, body ?? {}, signal);
export const put = <T>(path: string, body?: unknown) => api<T>("PUT", path, body ?? {});
export const patch = <T>(path: string, body?: unknown) => api<T>("PATCH", path, body ?? {});
export const del = <T>(path: string, body?: unknown) => api<T>("DELETE", path, body);

export function qs(params: Record<string, string | number | undefined | null>) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== "") p.set(k, String(v));
  const s = p.toString();
  return s ? "?" + s : "";
}

// ---- NDJSON query streaming ---------------------------------------------------

export type StreamEvent =
  | { t: "start"; console: string }
  | { t: "stmt"; i: number; sql: string; line: number; kind: string }
  | { t: "cols"; cols: import("./types").ResultColumn[] }
  | { t: "rows"; rows: import("./types").Cell[][] }
  | { t: "end"; summary: { rowCount: number; rowsAffected?: number; truncated: boolean; durationMs: number; bytesProcessed?: number } }
  | { t: "notice"; level: string; text: string }
  | { t: "stmtEnd"; ms: number; error?: QueryError }
  | { t: "done"; ms: number; inTx: boolean; console: string; error?: QueryError };

export async function* stream(path: string, body: unknown, signal?: AbortSignal): AsyncGenerator<StreamEvent> {
  const res = await fetch("api/" + path, {
    method: "POST",
    headers: headers(true),
    body: JSON.stringify(body),
    credentials: "same-origin",
    signal,
  });
  if (!res.ok) {
    const err = await toError(res);
    if (res.status === 401) unauthorized.forEach((f) => f());
    throw err;
  }
  const reader = res.body!.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    let nl: number;
    while ((nl = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, nl);
      buf = buf.slice(nl + 1);
      if (line.trim()) yield JSON.parse(line) as StreamEvent;
    }
  }
  if (buf.trim()) yield JSON.parse(buf) as StreamEvent;
}
