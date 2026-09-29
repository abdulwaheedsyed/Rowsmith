import { create } from "zustand";
import { persist, createJSONStorage } from "zustand/middleware";
import type { ObjectRef } from "./types";

export type TabKind = "overview" | "browse" | "query" | "structure" | "definition" | "processes" | "variables" | "users" | "diagram";

export interface Tab {
  id: string;
  connId: string;
  kind: TabKind;
  title: string;
  ref?: ObjectRef;
  database?: string;
  schema?: string;
  sql?: string;
  preview?: boolean; // replaced by the next preview open, like an editor's preview tab
  savedQueryId?: string;
  state?: Record<string, unknown>; // per-tab view state (filters, sort, split sizes…)
}

export interface Scope {
  database?: string;
  schema?: string;
}

interface WorkspaceState {
  owner: string;
  tabs: Tab[];
  active: Record<string, string>;
  scope: Record<string, Scope>;
  expanded: Record<string, boolean>;
  recent: string[]; // connection ids, most recent first
  setOwner(userId: string): void;
  openTab(t: Omit<Tab, "id"> & { id?: string }): string;
  closeTab(id: string): void;
  closeOthers(id: string): void;
  closeConnectionTabs(connId: string): void;
  setActive(connId: string, id: string): void;
  updateTab(id: string, patch: Partial<Tab>): void;
  pinTab(id: string): void;
  moveTab(id: string, toIndex: number): void;
  setScope(connId: string, s: Scope): void;
  toggle(key: string, open?: boolean): void;
  touchRecent(connId: string): void;
}

let seq = 0;
const newId = () => `t${Date.now().toString(36)}${(seq++).toString(36)}`;

function sameTarget(a: Tab, b: Omit<Tab, "id">) {
  if (a.connId !== b.connId || a.kind !== b.kind) return false;
  if (b.kind === "query") return false;
  if (b.kind === "processes" || b.kind === "variables" || b.kind === "users" || b.kind === "overview") return true;
  if (b.kind === "diagram") return a.database === b.database && a.schema === b.schema;
  const r1 = a.ref, r2 = b.ref;
  return !!r1 && !!r2 && r1.name === r2.name && (r1.database ?? "") === (r2.database ?? "") && (r1.schema ?? "") === (r2.schema ?? "");
}

export const useWorkspace = create<WorkspaceState>()(
  persist(
    (set, get) => ({
      owner: "",
      tabs: [],
      active: {},
      scope: {},
      expanded: {},
      recent: [],
      setOwner(userId) {
        if (get().owner !== userId) set({ owner: userId, tabs: [], active: {}, scope: {}, expanded: {}, recent: [] });
      },
      openTab(t) {
        const s = get();
        const existing = s.tabs.find((x) => sameTarget(x, t));
        if (existing) {
          const patch: Partial<Tab> = {};
          if (!t.preview && existing.preview) patch.preview = false;
          set({
            tabs: s.tabs.map((x) => (x.id === existing.id ? { ...x, ...patch } : x)),
            active: { ...s.active, [t.connId]: existing.id },
          });
          return existing.id;
        }
        const id = t.id ?? newId();
        const tab: Tab = { ...t, id };
        let tabs = s.tabs;
        const current = s.tabs.find((x) => x.id === s.active[t.connId]);
        if (t.preview) {
          const old = tabs.find((x) => x.connId === t.connId && x.preview);
          if (old) {
            tabs = tabs.map((x) => (x.id === old.id ? tab : x));
            set({ tabs, active: { ...s.active, [t.connId]: id } });
            return id;
          }
        }
        // Insert after the active tab of this connection.
        const idx = current ? tabs.indexOf(current) + 1 : tabs.length;
        tabs = [...tabs.slice(0, idx), tab, ...tabs.slice(idx)];
        set({ tabs, active: { ...s.active, [t.connId]: id } });
        return id;
      },
      closeTab(id) {
        const s = get();
        const tab = s.tabs.find((x) => x.id === id);
        if (!tab) return;
        const siblings = s.tabs.filter((x) => x.connId === tab.connId);
        const pos = siblings.indexOf(tab);
        const tabs = s.tabs.filter((x) => x.id !== id);
        const active = { ...s.active };
        if (active[tab.connId] === id) {
          const next = siblings[pos + 1] ?? siblings[pos - 1];
          if (next && next.id !== id) active[tab.connId] = next.id;
          else delete active[tab.connId];
        }
        set({ tabs, active });
      },
      closeOthers(id) {
        const s = get();
        const tab = s.tabs.find((x) => x.id === id);
        if (!tab) return;
        set({ tabs: s.tabs.filter((x) => x.connId !== tab.connId || x.id === id), active: { ...s.active, [tab.connId]: id } });
      },
      closeConnectionTabs(connId) {
        const s = get();
        const active = { ...s.active };
        delete active[connId];
        set({ tabs: s.tabs.filter((x) => x.connId !== connId), active });
      },
      setActive(connId, id) {
        set({ active: { ...get().active, [connId]: id } });
      },
      updateTab(id, patch) {
        set({ tabs: get().tabs.map((x) => (x.id === id ? { ...x, ...patch } : x)) });
      },
      pinTab(id) {
        set({ tabs: get().tabs.map((x) => (x.id === id ? { ...x, preview: false } : x)) });
      },
      moveTab(id, toIndex) {
        const tabs = [...get().tabs];
        const from = tabs.findIndex((x) => x.id === id);
        if (from < 0) return;
        const [t] = tabs.splice(from, 1);
        tabs.splice(Math.max(0, Math.min(tabs.length, toIndex)), 0, t);
        set({ tabs });
      },
      setScope(connId, sc) {
        set({ scope: { ...get().scope, [connId]: { ...get().scope[connId], ...sc } } });
      },
      toggle(key, open) {
        const cur = !!get().expanded[key];
        set({ expanded: { ...get().expanded, [key]: open ?? !cur } });
      },
      touchRecent(connId) {
        set({ recent: [connId, ...get().recent.filter((x) => x !== connId)].slice(0, 12) });
      },
    }),
    {
      name: "rowsmith.workspace",
      version: 1,
      storage: createJSONStorage(() => {
        try {
          return localStorage;
        } catch {
          return sessionStorage;
        }
      }),
    },
  ),
);

// ---- UI preferences ------------------------------------------------------------

type Theme = "system" | "light" | "dark";

interface UIState {
  theme: Theme;
  navWidth: number;
  navOpen: boolean; // mobile drawer / desktop collapse
  paletteOpen: boolean;
  setTheme(t: Theme): void;
  setNavWidth(w: number): void;
  setNavOpen(open: boolean): void;
  setPaletteOpen(open: boolean): void;
}

function applyTheme(t: Theme) {
  const root = document.documentElement;
  if (t === "system") delete root.dataset.theme;
  else root.dataset.theme = t;
  try {
    if (t === "system") localStorage.removeItem("rowsmith.theme");
    else localStorage.setItem("rowsmith.theme", t);
  } catch {
    /* storage unavailable */
  }
}

function initialTheme(): Theme {
  try {
    const t = localStorage.getItem("rowsmith.theme");
    if (t === "light" || t === "dark") return t;
  } catch {
    /* ignore */
  }
  return "system";
}

export const useUI = create<UIState>()(
  persist(
    (set) => ({
      theme: initialTheme(),
      navWidth: 272,
      navOpen: true,
      paletteOpen: false,
      setTheme(t) {
        applyTheme(t);
        set({ theme: t });
      },
      setNavWidth(w) {
        set({ navWidth: Math.max(200, Math.min(520, w)) });
      },
      setNavOpen(open) {
        set({ navOpen: open });
      },
      setPaletteOpen(open) {
        set({ paletteOpen: open });
      },
    }),
    { name: "rowsmith.ui", partialize: (s) => ({ navWidth: s.navWidth, navOpen: s.navOpen }) as UIState },
  ),
);

// ---- Toasts ---------------------------------------------------------------------

export interface Toast {
  id: number;
  kind: "success" | "error" | "info";
  title: string;
  body?: string;
}

interface ToastState {
  toasts: Toast[];
  push(t: Omit<Toast, "id">, ms?: number): void;
  dismiss(id: number): void;
}

let toastSeq = 1;
export const useToasts = create<ToastState>((set, get) => ({
  toasts: [],
  push(t, ms = 4200) {
    const id = toastSeq++;
    set({ toasts: [...get().toasts.slice(-3), { ...t, id }] });
    if (ms > 0) setTimeout(() => get().dismiss(id), ms);
  },
  dismiss(id) {
    set({ toasts: get().toasts.filter((x) => x.id !== id) });
  },
}));

export const toast = {
  success: (title: string, body?: string) => useToasts.getState().push({ kind: "success", title, body }),
  error: (title: string, body?: string) => useToasts.getState().push({ kind: "error", title, body }, 7000),
  info: (title: string, body?: string) => useToasts.getState().push({ kind: "info", title, body }),
};
