import type { ColumnDef, DriverInfo, ForeignKey, Index, ObjectRef, Table, TableDef } from "../../lib/types";

/** Editable rows carry a stable key for React and drag-and-drop. */
export type Col = ColumnDef & { _k: string };
export type Ix = Index & { _k: string };
export type Fk = ForeignKey & { _k: string };
export type Ck = { name: string; expression: string; _k: string };

export interface Draft {
  ref: ObjectRef;
  columns: Col[];
  dropped: Col[]; // existing columns marked for removal (can be restored)
  primaryKey: string[];
  indexes: Ix[];
  foreignKeys: Fk[];
  checks: Ck[];
  comment: string;
  options: Record<string, string>;
}

let n = 0;
export const key = () => `k${Date.now().toString(36)}${(n++).toString(36)}`;

export function fromTable(t: Table): Draft {
  return {
    ref: { ...t.ref },
    columns: t.columns.map((c) => ({ ...c, originalName: c.name, _k: key() })),
    dropped: [],
    primaryKey: [...(t.primaryKey ?? [])],
    indexes: (t.indexes ?? []).filter((i) => !i.primary).map((i) => ({ ...i, columns: [...i.columns], _k: key() })),
    foreignKeys: (t.foreignKeys ?? []).map((f) => ({ ...f, columns: [...f.columns], refColumns: [...f.refColumns], _k: key() })),
    checks: (t.checks ?? []).map((c) => ({ ...c, _k: key() })),
    comment: t.comment ?? "",
    options: { ...(t.options ?? {}) },
  };
}

const idTypes: Record<string, string> = {
  mysql: "int unsigned", postgresql: "bigint", mssql: "int", plsql: "NUMBER(19)", sqlite: "INTEGER", bigquery: "INT64",
};

export function blankColumn(drv?: DriverInfo, name = ""): Col {
  const text = drv?.dialect === "bigquery" ? "STRING" : drv?.dialect === "plsql" ? "VARCHAR2(255)" : drv?.dialect === "sqlite" ? "TEXT" : drv?.dialect === "mssql" ? "nvarchar(255)" : "varchar(255)";
  return { name, type: text, baseType: "", kind: "string", nullable: true, _k: key() };
}

export function newDraft(drv: DriverInfo | undefined, ref: ObjectRef): Draft {
  const d = drv?.design;
  const columns: Col[] = [];
  if (d?.columns) {
    columns.push({
      name: "id", type: idTypes[drv!.dialect] ?? drv!.types[0] ?? "int", baseType: "", kind: "int", nullable: false,
      autoIncrement: !!d.autoIncrement, _k: key(),
    });
  }
  return {
    ref, columns, dropped: [], primaryKey: d?.primaryKey && columns.length ? ["id"] : [], indexes: [], foreignKeys: [], checks: [],
    comment: "", options: {},
  };
}

/** The TableDef the server's generator expects. */
export function toDef(d: Draft): TableDef {
  const strip = <T extends { _k: string }>(x: T) => {
    const { _k, ...rest } = x;
    void _k;
    return rest;
  };
  return {
    ref: d.ref,
    columns: d.columns.map((c) => {
      const { _k, ...rest } = c;
      void _k;
      return { ...rest, primaryKey: d.primaryKey.includes(c.name), default: c.default === "" ? undefined : c.default };
    }),
    indexes: d.indexes.map(strip),
    foreignKeys: d.foreignKeys.map(strip),
    checks: d.checks.map(strip),
    primaryKey: d.primaryKey,
    comment: d.comment,
    options: d.options,
  };
}

const swap = (list: string[], from: string, to: string) => list.map((c) => (c === from ? to : c));

/** Renaming a column updates the key, index and foreign-key references. */
export function renameColumn(d: Draft, from: string, to: string): Draft {
  if (from === to || !from) return d;
  return {
    ...d,
    primaryKey: swap(d.primaryKey, from, to),
    indexes: d.indexes.map((i) => ({ ...i, columns: swap(i.columns, from, to) })),
    foreignKeys: d.foreignKeys.map((f) => ({ ...f, columns: swap(f.columns, from, to) })),
  };
}

/** Removing a column drops it from keys; indexes and foreign keys left empty go too. */
export function withoutColumn(d: Draft, name: string): Draft {
  const pruneIx = (i: Ix) => {
    const keep = i.columns.map((c, j) => (c === name ? -1 : j)).filter((j) => j >= 0);
    return { ...i, columns: keep.map((j) => i.columns[j]), desc: i.desc && keep.map((j) => i.desc![j]), lengths: i.lengths && keep.map((j) => i.lengths![j]) };
  };
  return {
    ...d,
    primaryKey: d.primaryKey.filter((c) => c !== name),
    indexes: d.indexes.map(pruneIx).filter((i) => i.columns.length > 0),
    foreignKeys: d.foreignKeys.filter((f) => !f.columns.includes(name)),
  };
}

export function uniqueName(base: string, taken: string[]) {
  if (!taken.includes(base)) return base;
  for (let i = 2; ; i++) if (!taken.includes(`${base}_${i}`)) return `${base}_${i}`;
}

/** Problems the editor can point at before asking the server. */
export function validate(d: Draft, design: DriverInfo["design"]): string[] {
  const out: string[] = [];
  if (!d.ref.name.trim()) out.push("Give the table a name.");
  if (design?.columns) {
    if (d.columns.length === 0) out.push("A table needs at least one column.");
    const seen = new Set<string>();
    for (const c of d.columns) {
      if (!c.name.trim()) out.push("Every column needs a name.");
      else if (seen.has(c.name.toLowerCase())) out.push(`There are two columns named ${c.name}.`);
      seen.add(c.name.toLowerCase());
      if (!c.type.trim()) out.push(`Column ${c.name || "without a name"} needs a type.`);
    }
  }
  for (const i of d.indexes) {
    if (!i.name.trim()) out.push("Every index needs a name.");
    if (i.columns.length === 0) out.push(`Index ${i.name} needs at least one column.`);
  }
  for (const f of d.foreignKeys) {
    if (!f.name.trim()) out.push("Every foreign key needs a name.");
    if (!f.refTable.name) out.push(`Foreign key ${f.name} needs a referenced table.`);
    if (f.columns.length === 0 || f.columns.length !== f.refColumns.length) out.push(`Foreign key ${f.name} needs matching column lists.`);
  }
  for (const c of d.checks) if (!c.name.trim() || !c.expression.trim()) out.push("Every check needs a name and an expression.");
  return [...new Set(out)];
}
