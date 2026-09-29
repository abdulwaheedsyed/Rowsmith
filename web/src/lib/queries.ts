import { useQuery } from "@tanstack/react-query";
import { get, qs } from "./api";
import type { CatalogTable, Connection, Database, DbObject, DriverInfo, ObjectRef, Schema, ServerInfo, Table, Access } from "./types";

export function useDrivers() {
  return useQuery({ queryKey: ["drivers"], queryFn: () => get<DriverInfo[]>("drivers"), staleTime: Infinity });
}

export function useDriver(id?: string) {
  const d = useDrivers();
  return d.data?.find((x) => x.id === id);
}

export function useConnections() {
  return useQuery({ queryKey: ["connections"], queryFn: () => get<Connection[]>("connections") });
}

export function useConnection(id?: string) {
  const list = useConnections();
  return list.data?.find((c) => c.id === id);
}

export interface ServerResp {
  server: ServerInfo;
  driver: DriverInfo;
  readOnly: boolean;
  access: Access;
  environment: string;
  name: string;
  color: string;
}

export function useServer(connId?: string) {
  return useQuery({
    queryKey: ["server", connId],
    queryFn: () => get<ServerResp>(`c/${connId}/server`),
    enabled: !!connId,
    staleTime: 5 * 60_000,
    retry: false,
  });
}

export function useDatabases(connId?: string, enabled = true) {
  return useQuery({
    queryKey: ["dbs", connId],
    queryFn: () => get<Database[]>(`c/${connId}/databases`),
    enabled: !!connId && enabled,
    staleTime: 60_000,
    retry: false,
  });
}

export function useSchemas(connId?: string, db?: string, enabled = true) {
  return useQuery({
    queryKey: ["schemas", connId, db ?? ""],
    queryFn: () => get<Schema[]>(`c/${connId}/schemas${qs({ db })}`),
    enabled: !!connId && enabled,
    staleTime: 60_000,
    retry: false,
  });
}

export function useObjects(connId?: string, db?: string, schema?: string, enabled = true) {
  return useQuery({
    queryKey: ["objects", connId, db ?? "", schema ?? ""],
    queryFn: () => get<DbObject[]>(`c/${connId}/objects${qs({ db, schema })}`),
    enabled: !!connId && enabled,
    staleTime: 60_000,
    retry: false,
  });
}

export function describeKey(connId: string, ref: ObjectRef) {
  return ["describe", connId, ref.database ?? "", ref.schema ?? "", ref.name];
}

export function useDescribe(connId?: string, ref?: ObjectRef) {
  return useQuery({
    queryKey: connId && ref ? describeKey(connId, ref) : ["describe", "none"],
    queryFn: () => get<Table>(`c/${connId}/describe${qs({ db: ref!.database, schema: ref!.schema, name: ref!.name, kind: ref!.kind })}`),
    enabled: !!connId && !!ref?.name,
    staleTime: 60_000,
    retry: false,
  });
}

export function useCatalog(connId?: string, db?: string, schema?: string, enabled = true) {
  return useQuery({
    queryKey: ["catalog", connId, db ?? "", schema ?? ""],
    queryFn: () => get<CatalogTable[]>(`c/${connId}/catalog${qs({ db, schema })}`),
    enabled: !!connId && enabled,
    staleTime: 5 * 60_000,
    retry: false,
  });
}

export function refLabel(ref: ObjectRef, withDb = false) {
  const parts = [];
  if (withDb && ref.database) parts.push(ref.database);
  if (ref.schema && ref.schema !== "public") parts.push(ref.schema);
  parts.push(ref.name);
  return parts.join(".");
}

const RESERVED = new Set(
  ("add all alter and any as asc between by case cast check collate column constraint create cross current_date current_time " +
    "current_timestamp current_user database default delete desc distinct drop else end except exists false fetch for foreign from " +
    "full grant group having in index inner insert intersect interval into is join key left like limit natural not null offset on " +
    "or order outer primary references right row rows select session_user set some table then to true union unique update user " +
    "using values view when where window with").split(" "),
);

/** Quotes an identifier only when needed, so generated SQL stays readable. */
export function quoteIdent(name: string, q: string) {
  if (!q) return name;
  const simple = q === '"' ? /^[a-z_][a-z0-9_]*$/ : /^[A-Za-z_][A-Za-z0-9_]*$/;
  if (simple.test(name) && !RESERVED.has(name.toLowerCase())) return name;
  const close = q === "[" ? "]" : q;
  return q + name.split(close).join(close + close) + close;
}

export function qualified(ref: ObjectRef, q: string, withDb = false) {
  const parts: string[] = [];
  if (withDb && ref.database) parts.push(quoteIdent(ref.database, q));
  if (ref.schema) parts.push(quoteIdent(ref.schema, q));
  parts.push(quoteIdent(ref.name, q));
  return parts.join(".");
}
