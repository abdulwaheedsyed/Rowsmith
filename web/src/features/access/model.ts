import { LEVEL, type AccessModel, type AccessPreset, type Grant, type GrantChange } from "./api";

// The privilege editor's state: what a principal holds directly, what it
// gets from roles and broader levels, and the changes not yet applied.

/** Changes not yet applied: true grants, false revokes. */
export type Pending = Map<string, boolean>;

const SEP = "\u0001";
export const pkey = (level: string, object: string, priv: string) => [level, object, priv].join(SEP);
export function unkey(k: string) {
  const [level, object, privilege] = k.split(SEP);
  return { level, object, privilege };
}

export interface Held {
  direct: Set<string>;
  withGrant: Set<string>;
  /** Where privileges not held directly come from: roles, PUBLIC, other creators. */
  via: Map<string, string[]>;
}

export function held(grants: Grant[]): Held {
  const h: Held = { direct: new Set(), withGrant: new Set(), via: new Map() };
  for (const g of grants) {
    const k = pkey(g.level, g.object ?? "", g.privilege);
    if (g.via) h.via.set(k, [...(h.via.get(k) ?? []), g.via]);
    else {
      h.direct.add(k);
      if (g.withGrant) h.withGrant.add(k);
    }
  }
  return h;
}

export interface Ctx {
  model: AccessModel;
  held: Held;
  /** The scope's tables: rows of the table level, and what "all tables" means. */
  tables: string[];
  pending: Pending;
}

export const privsOf = (m: AccessModel, level: string) => m.levels.find((l) => l.level === level)?.privileges ?? [];
export const hasLevel = (m: AccessModel, level: string) => m.levels.some((l) => l.level === level);
export const labelOf = (m: AccessModel, level: string) => m.levels.find((l) => l.level === level)?.label ?? level;

/** Whether a privilege is held directly once the pending changes apply. */
export function isOn(c: Ctx, level: string, object: string, priv: string): boolean {
  const own = c.pending.get(pkey(level, object, priv));
  if (own !== undefined) return own;
  if (level === LEVEL.table) {
    const all = c.pending.get(pkey(LEVEL.tables, "", priv));
    if (all !== undefined) return all;
  }
  return c.held.direct.has(pkey(level, object, priv));
}

export type Agg = "all" | "some" | "none";

function count(tables: string[], on: (t: string) => boolean, fallback: boolean): Agg {
  if (!tables.length) return fallback ? "all" : "none";
  let n = 0;
  for (const t of tables) if (on(t)) n++;
  return n === 0 ? "none" : n === tables.length ? "all" : "some";
}

/** The "all tables" row: whether every table has the privilege. */
export const tablesAgg = (c: Ctx, priv: string): Agg =>
  count(c.tables, (t) => isOn(c, LEVEL.table, t, priv), isOn(c, LEVEL.tables, "", priv));

const heldAgg = (c: Ctx, priv: string): Agg =>
  count(c.tables, (t) => c.held.direct.has(pkey(LEVEL.table, t, priv)), c.held.direct.has(pkey(LEVEL.tables, "", priv)));

/** The broader levels whose grants already give this privilege. */
export function coveredBy(c: Ctx, level: string, priv: string): string[] {
  return (c.model.covers?.[level] ?? [])
    .filter((b) => isOn(c, b, "", priv) || c.held.via.has(pkey(b, "", priv)))
    .map((b) => labelOf(c.model, b));
}

// ---- Changes. These mutate c.pending; callers pass a copy. ------------------------

function setCell(c: Ctx, level: string, object: string, priv: string, want: boolean) {
  const k = pkey(level, object, priv);
  const base = level === LEVEL.table ? c.pending.get(pkey(LEVEL.tables, "", priv)) ?? c.held.direct.has(k) : c.held.direct.has(k);
  if (base === want) c.pending.delete(k);
  else c.pending.set(k, want);
}

/** Sets the "all tables" level, which replaces any per-table change. */
function setTables(c: Ctx, priv: string, want: boolean) {
  for (const t of c.tables) c.pending.delete(pkey(LEVEL.table, t, priv));
  const k = pkey(LEVEL.tables, "", priv);
  const before = heldAgg(c, priv);
  if (want ? before === "all" : before === "none") c.pending.delete(k);
  else c.pending.set(k, want);
}

function setExactly(c: Ctx, level: string, privs: Set<string>) {
  for (const p of privsOf(c.model, level)) {
    const want = privs.has(p);
    if (level === LEVEL.tables) {
      if (tablesAgg(c, p) !== (want ? "all" : "none")) setTables(c, p, want);
    } else if (isOn(c, level, "", p) !== want) setCell(c, level, "", p, want);
  }
}

const fork = (c: Ctx, fresh = false): Ctx => ({ ...c, pending: fresh ? new Map() : new Map(c.pending) });

export function toggle(c: Ctx, level: string, object: string, priv: string): Pending {
  const n = fork(c);
  if (level === LEVEL.tables) setTables(n, priv, tablesAgg(c, priv) !== "all");
  else setCell(n, level, object, priv, !isOn(c, level, object, priv));
  return n.pending;
}

/** Sets one privilege on many tables at once (a column header). */
export function setColumn(c: Ctx, tables: string[], priv: string, want: boolean): Pending {
  const n = fork(c);
  for (const t of tables) setCell(n, LEVEL.table, t, priv, want);
  return n.pending;
}

/** Sets every privilege on one table (a row's "all" box). */
export function setRow(c: Ctx, table: string, want: boolean): Pending {
  const n = fork(c);
  for (const p of privsOf(c.model, LEVEL.table)) setCell(n, LEVEL.table, table, p, want);
  return n.pending;
}

/** The levels a preset sets, in the editor's scope. */
const presetLevels = (m: AccessModel) => new Set(m.presets.flatMap((p) => p.grants.map((g) => g.level)));
const wanted = (preset: AccessPreset | null, level: string) => new Set(preset?.grants.find((g) => g.level === level)?.privileges ?? []);

/** Privileges a preset gives every table, through "all tables" or a level that covers tables. */
function broadForTables(m: AccessModel, preset: AccessPreset | null) {
  const out = new Set<string>();
  for (const lv of [LEVEL.tables, ...(m.covers?.[LEVEL.table] ?? [])]) for (const p of wanted(preset, lv)) out.add(p);
  return out;
}

/** Replaces the scope's access with a preset; null removes all of it. */
export function presetChanges(c: Ctx, preset: AccessPreset | null): Pending {
  const n = fork(c, true);
  const levels = presetLevels(c.model);
  for (const l of c.model.levels) {
    if (l.level === LEVEL.server || l.level === LEVEL.table) continue;
    if (!preset || levels.has(l.level)) setExactly(n, l.level, wanted(preset, l.level));
  }
  if (hasLevel(c.model, LEVEL.table)) {
    const broad = broadForTables(c.model, preset);
    for (const t of c.tables) for (const p of privsOf(c.model, LEVEL.table)) {
      if (!broad.has(p) && isOn(n, LEVEL.table, t, p)) setCell(n, LEVEL.table, t, p, false);
    }
  }
  return n.pending;
}

/** The preset the scope's access matches exactly, "none" for no access. */
export function matchingPreset(c: Ctx): string | null {
  const levels = presetLevels(c.model);
  const tablePrivs = privsOf(c.model, LEVEL.table);
  const matches = (preset: AccessPreset | null) => {
    for (const l of c.model.levels) {
      if (l.level === LEVEL.server || l.level === LEVEL.table || (preset && !levels.has(l.level))) continue;
      const want = wanted(preset, l.level);
      for (const p of l.privileges) {
        const on = l.level === LEVEL.tables ? tablesAgg(c, p) : isOn(c, l.level, "", p) ? "all" : "none";
        if (on !== (want.has(p) ? "all" : "none")) return false;
      }
    }
    const broad = broadForTables(c.model, preset);
    return !c.tables.some((t) => tablePrivs.some((p) => !broad.has(p) && isOn(c, LEVEL.table, t, p)));
  };
  for (const p of c.model.presets) if (matches(p)) return p.id;
  // Server-wide privileges reach this scope too, so it isn't "no access".
  const serverWide = [...c.held.direct, ...c.held.via.keys()].some((k) => unkey(k).level === LEVEL.server);
  return !serverWide && matches(null) ? "none" : null;
}

/** Replaces the scope's access with another principal's direct grants. */
export function copyChanges(c: Ctx, from: Grant[]): Pending {
  const n = fork(c, true);
  const src = held(from);
  const whole = new Set<string>();
  for (const l of c.model.levels) {
    if (l.level === LEVEL.server || l.level === LEVEL.table) continue;
    const want = new Set(l.privileges.filter((p) => src.direct.has(pkey(l.level, "", p))));
    if (l.level === LEVEL.tables) {
      // Only what the source has on every table moves as "all tables"; the rest goes table by table.
      for (const p of want) whole.add(p);
      for (const p of want) if (tablesAgg(n, p) !== "all") setTables(n, p, true);
      continue;
    }
    setExactly(n, l.level, want);
  }
  if (hasLevel(c.model, LEVEL.table)) {
    for (const t of c.tables) for (const p of privsOf(c.model, LEVEL.table)) {
      if (whole.has(p)) continue;
      const want = src.direct.has(pkey(LEVEL.table, t, p));
      if (isOn(n, LEVEL.table, t, p) !== want) setCell(n, LEVEL.table, t, p, want);
    }
  }
  return n.pending;
}

export function changesOf(pending: Pending, principal: string, kind: string, scope: { database?: string; schema?: string }): GrantChange[] {
  return [...pending].map(([k, grant]) => {
    const { level, object, privilege } = unkey(k);
    const g: GrantChange = { principal, kind, level, privilege };
    if (!grant) g.revoke = true;
    if (level !== LEVEL.server && scope.database) g.database = scope.database;
    if (level !== LEVEL.server && level !== LEVEL.database && scope.schema) g.schema = scope.schema;
    if (object) g.object = object;
    return g;
  });
}
