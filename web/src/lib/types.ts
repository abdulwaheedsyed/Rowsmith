// Mirrors the Go API types (internal/driver, internal/store).

export type FieldType = "text" | "password" | "number" | "select" | "textarea" | "file" | "bool";

export interface Field {
  key: string;
  label: string;
  type: FieldType;
  required?: boolean;
  secret?: boolean;
  default?: unknown;
  placeholder?: string;
  help?: string;
  options?: { value: string; label: string }[];
  section?: "" | "auth" | "tls" | "advanced";
  span?: number;
  showIf?: Record<string, string[]>;
}

export interface Caps {
  databases: boolean;
  schemas: boolean;
  sql: boolean;
  transactions: boolean;
  editRows: boolean;
  ddl: boolean;
  createDatabase: boolean;
  foreignKeys: boolean;
  explain: boolean;
  processes: boolean;
  variables: boolean;
  users: boolean;
  geometry: boolean;
  documents: boolean;
  dump: boolean;
  costEstimate: boolean;
}

export interface KindInfo {
  kind: string;
  label: string;
  icon: string;
  browse: boolean;
}

export interface DriverInfo {
  id: string;
  name: string;
  description: string;
  order: number;
  dialect: string;
  defaultPort?: number;
  fields: Field[];
  ssh: boolean;
  caps: Caps;
  kinds: KindInfo[];
  types: string[];
  urlSchemes: string[];
  quoteChar: string;
  design?: TableDesign;
}

/** What the structure editor may offer for an engine (driver.TableDesign). */
export interface TableDesign {
  columns: boolean;
  reorderColumns: boolean;
  autoIncrement: boolean;
  columnComments: boolean;
  tableComment: boolean;
  collation: boolean;
  generated: boolean;
  generatedVirtual: boolean;
  generatedStored: boolean;
  onUpdate: boolean;
  checks: boolean;
  primaryKey: boolean;
  foreignKeys: boolean;
  indexes: boolean;
  partialIndexes: boolean;
  indexLengths: boolean;
  indexTypes?: string[];
  fkActions?: string[];
  options?: Field[];
  note?: string;
}

export type Environment = "production" | "staging" | "development" | "local";
export type Access = "" | "read" | "write" | "manage";
export type Role = "owner" | "admin" | "member" | "viewer";

export interface SSHHop {
  host: string;
  port: number;
  user: string;
  auth: "password" | "key";
}
export interface SSHSettings {
  enabled: boolean;
  hops: SSHHop[];
}

export interface Connection {
  id: string;
  ownerId: string;
  ownerName?: string;
  name: string;
  driver: string;
  color: string;
  environment: Environment;
  folder: string;
  params: Record<string, unknown>;
  ssh: Partial<SSHSettings>;
  readOnly: boolean;
  teamAccess: Access;
  notes: string;
  createdAt: number;
  updatedAt: number;
  lastUsedAt: number;
  access: Access;
  secretsSet: Record<string, boolean>;
}

export interface ServerInfo {
  product: string;
  version: string;
  user: string;
  database?: string;
  extras?: Record<string, string>;
}

export interface Database {
  name: string;
  size?: number;
  collation?: string;
  owner?: string;
  tables?: number;
  system?: boolean;
  comment?: string;
}

export interface Schema {
  name: string;
  owner?: string;
  system?: boolean;
}

export interface DbObject {
  name: string;
  kind: string;
  rows?: number;
  size?: number;
  engine?: string;
  comment?: string;
  collation?: string;
  updated?: string;
  extra?: string;
  extension?: string;
  ownedBy?: string;
}

export interface ObjectRef {
  database?: string;
  schema?: string;
  name: string;
  kind?: string;
}

export type ValueKind =
  | "int" | "float" | "decimal" | "bool" | "string" | "text" | "binary" | "date" | "time" | "datetime"
  | "timestamp" | "interval" | "json" | "uuid" | "geometry" | "array" | "object" | "enum" | "other";

export interface Column {
  name: string;
  type: string;
  baseType: string;
  kind: ValueKind;
  nullable: boolean;
  default?: string;
  autoIncrement?: boolean;
  generated?: string;
  generatedStored?: boolean;
  primaryKey?: boolean;
  comment?: string;
  collation?: string;
  enum?: string[];
  unsigned?: boolean;
  length?: number;
  precision?: number;
  scale?: number;
  srid?: number;
  geometryType?: string;
  onUpdate?: string;
}

export interface Index {
  name: string;
  columns: string[];
  unique: boolean;
  primary: boolean;
  type?: string;
  where?: string;
  lengths?: number[];
  desc?: boolean[];
  comment?: string;
  definition?: string;
}

export interface ForeignKey {
  name: string;
  columns: string[];
  refTable: ObjectRef;
  refColumns: string[];
  onUpdate?: string;
  onDelete?: string;
  table?: ObjectRef;
}

export interface Table {
  ref: ObjectRef;
  kind: string;
  columns: Column[];
  indexes: Index[] | null;
  foreignKeys: ForeignKey[] | null;
  referenced: ForeignKey[] | null;
  checks: { name: string; expression: string }[] | null;
  triggers: { name: string; timing: string; event: string; statement?: string }[] | null;
  primaryKey: string[] | null;
  rowKey: string[] | null;
  rowKeyKind?: "primary" | "unique" | "rowid" | "all" | "_id";
  comment?: string;
  options?: Record<string, string>;
  rowEstimate?: number;
  size?: number;
  ddl?: string;
  definition?: string;
  editable: boolean;
}

/** Desired table state sent to the DDL generator (driver.TableDef). */
export interface ColumnDef extends Column {
  originalName?: string;
}

export interface TableDef {
  ref: ObjectRef;
  columns: ColumnDef[];
  indexes: Index[];
  foreignKeys: ForeignKey[];
  checks: { name: string; expression: string }[];
  primaryKey: string[];
  comment: string;
  options: Record<string, string>;
}

export interface ResultColumn {
  name: string;
  type: string;
  kind: ValueKind;
  nullable?: boolean;
  table?: string;
}

export type Cell =
  | null
  | boolean
  | number
  | string
  | { $bin: string; size: number; mime?: string }
  | { $text: string; size: number }
  | { $geo: GeoJSON; srid?: number; wkt?: string }
  | Record<string, unknown>
  | unknown[];

// Minimal GeoJSON geometry shape.
export interface GeoJSON {
  type: string;
  coordinates?: unknown;
  geometries?: GeoJSON[];
}

export interface Result {
  columns: ResultColumn[];
  rows: Cell[][];
  truncated: boolean;
  rowsAffected?: number;
  durationMs: number;
  sql?: string;
}

export interface Filter {
  column: string;
  op: string;
  value?: unknown;
  values?: unknown[];
}

export interface Sort {
  column: string;
  desc: boolean;
}

export interface BrowseRequest {
  ref: ObjectRef;
  columns?: string[];
  filters?: Filter[];
  where?: string;
  search?: string;
  sort?: Sort[];
  offset: number;
  limit: number;
}

export interface RowEdit {
  op: "insert" | "update" | "delete";
  key?: Record<string, Cell>;
  values?: Record<string, unknown>;
}

export interface QueryError {
  message: string;
  code?: string;
  detail?: string;
  hint?: string;
  position?: number;
  line?: number;
}

export interface PlanNode {
  operation: string;
  object?: string;
  detail?: string;
  cost?: number;
  rows?: number;
  actualRows?: number;
  timeMs?: number;
  loops?: number;
  props?: Record<string, string>;
  children?: PlanNode[];
}

export interface Plan {
  root?: PlanNode;
  raw: string;
  format: string;
  totals?: Record<string, unknown>;
}

export interface User {
  id: string;
  email: string;
  name: string;
  role: Role;
  mfaEnabled?: boolean;
  disabled?: boolean;
  mustChangePassword?: boolean;
  prefs?: Record<string, unknown>;
  lastLoginAt?: number;
  createdAt?: number;
}

export type SessionStage = "mfa" | "enroll" | "full";

export interface Me {
  user: User;
  stage: SessionStage;
  csrf: string;
  recoveryCodesLeft: number;
}

export interface Bootstrap {
  product: string;
  version: string;
  setupRequired: boolean;
  requireMfa: boolean;
  basePath: string;
}

export interface HistoryEntry {
  id: number;
  userId: string;
  userName?: string;
  connectionId: string;
  database: string;
  body: string;
  startedAt: number;
  durationMs: number;
  rowCount: number;
  status: "ok" | "error" | "cancelled";
  error: string;
}

export interface SavedQuery {
  id: string;
  ownerId: string;
  ownerName: string;
  connectionId: string;
  database: string;
  name: string;
  description: string;
  body: string;
  tags: string[];
  visibility: "private" | "team";
  createdAt: number;
  updatedAt: number;
}

export interface Note {
  id: string;
  connectionId: string;
  objectPath: string;
  authorId: string;
  authorName: string;
  body: string;
  createdAt: number;
}

export interface AuditEntry {
  id: number;
  at: number;
  userId: string;
  userName: string;
  ip: string;
  action: string;
  target: string;
  detail: Record<string, unknown>;
}

export interface CatalogTable {
  schema?: string;
  name: string;
  kind: string;
  columns: { name: string; type: string; pk?: boolean }[];
  fks?: ForeignKey[];
}

export type StatementKind = "read" | "write" | "ddl" | "dcl" | "tcl" | "session" | "unknown";

export interface Danger {
  level: "" | "caution" | "destructive";
  reason?: string;
}

export interface PendingStatement {
  index: number;
  sql: string;
  line: number;
  danger: Danger;
}
