import { create } from "zustand";

export interface TabStatus {
  rows?: string;
  ms?: number;
  note?: string;
  inTx?: boolean;
  running?: boolean;
}

interface StatusState {
  byTab: Record<string, TabStatus>;
  set(tabId: string, s: TabStatus): void;
}

export const useStatus = create<StatusState>((set, get) => ({
  byTab: {},
  set(tabId, s) {
    const cur = get().byTab[tabId];
    if (cur && JSON.stringify(cur) === JSON.stringify(s)) return;
    set({ byTab: { ...get().byTab, [tabId]: s } });
  },
}));
