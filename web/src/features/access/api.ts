import { useQuery } from "@tanstack/react-query";
import { get, post, qs } from "../../lib/api";
import type { ObjectRef } from "../../lib/types";

// Mirrors internal/driver/access.go.

export interface AccessLevel { level: string; label: string; privileges: string[]; help?: string }
export interface AccountAttr { key: string; label: string; type: "bool" | "int" | "date" | "text"; help?: string; default?: string }
export interface AccessPreset { id: string; label: string; description: string; grants: { level: string; privileges: string[] }[] }

export interface AccessModel {
  levels: AccessLevel[];
  attributes: AccountAttr[];
  presets: AccessPreset[];
  roles: boolean;
  hostPart: boolean;
  hostLabel?: string;
  hostDefault?: string;
  hostHelp?: string;
  canCreate: string[];
  password: boolean;
  covers?: Record<string, string[]>;
  dropOptions?: AccountAttr[];
  note?: string;
}

export interface Principal {
  name: string;
  kind: "user" | "role";
  login: boolean;
  memberOf: string[];
  attributes?: Record<string, unknown>;
  system?: boolean;
  /** Holds server-wide privileges only (a SQL Server server role). */
  serverOnly?: boolean;
}

export interface Grant {
  principal: string;
  level: string;
  database?: string;
  schema?: string;
  object?: string;
  privilege: string;
  withGrant?: boolean;
  via?: string;
}

export interface GrantChange extends Grant { revoke?: boolean; kind?: string }

export interface AccountOp {
  op: "create" | "alter" | "password" | "drop" | "add_role" | "remove_role";
  name: string;
  kind?: string;
  password?: string;
  attrs?: Record<string, string>;
  role?: string;
}

export interface AccessReq { grants?: GrantChange[]; account?: AccountOp; database?: string }

export interface ModelResp { supported: boolean; model?: AccessModel; canChange?: boolean }

export const LEVEL = { server: "server", database: "database", schema: "schema", tables: "tables", future: "future", table: "table" } as const;

/** Catalog kinds the table level applies to. */
export const TABLE_KINDS = ["table", "partitioned_table", "view", "materialized_view", "foreign_table"];

export function useAccessModel(connId: string) {
  return useQuery({ queryKey: ["access", connId, "model"], queryFn: () => get<ModelResp>(`c/${connId}/access/model`), staleTime: 5 * 60_000, retry: false });
}

export function usePrincipals(connId: string, enabled = true) {
  return useQuery({ queryKey: ["access", connId, "principals"], queryFn: () => get<Principal[]>(`c/${connId}/access/principals`), enabled, retry: false });
}

export function useGrants(connId: string, principal: string | undefined, db?: string, schema?: string) {
  return useQuery({
    queryKey: ["access", connId, "grants", principal ?? "", db ?? "", schema ?? ""],
    queryFn: () => get<Grant[]>(`c/${connId}/access/grants${qs({ principal, db, schema })}`),
    enabled: !!principal,
    retry: false,
  });
}

/** Another principal's grants, fetched once for copying. */
export const fetchGrants = (connId: string, principal: string, db?: string, schema?: string) =>
  get<Grant[]>(`c/${connId}/access/grants${qs({ principal, db, schema })}`);

export function useObjectAccess(connId: string, ref: ObjectRef, enabled = true) {
  return useQuery({
    queryKey: ["access", connId, "object", ref.database ?? "", ref.schema ?? "", ref.name],
    queryFn: () => get<Grant[]>(`c/${connId}/access/object${qs({ db: ref.database, schema: ref.schema, name: ref.name })}`),
    enabled,
    retry: false,
  });
}

export const previewAccess = (connId: string, req: AccessReq) => post<{ statements: string[]; script: string }>(`c/${connId}/access/preview`, req);
export const applyAccess = (connId: string, req: AccessReq & { confirm?: boolean }) => post<{ ok: boolean; statements: string[] }>(`c/${connId}/access/apply`, req);

/** Splits a MySQL account into user and host; other engines have no host. */
export function splitName(name: string, hostPart: boolean): [string, string] {
  const i = hostPart ? name.lastIndexOf("@") : -1;
  return i < 0 ? [name, ""] : [name.slice(0, i), name.slice(i + 1)];
}
