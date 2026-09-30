import { create } from "zustand";
import { useQuery } from "@tanstack/react-query";
import { get } from "../../lib/api";

export type AIProvider = "anthropic" | "openai";

export interface AIStatus {
  enabled: boolean;
  ready: boolean;
  provider: AIProvider;
  /** Where requests go: Anthropic, OpenAI, OpenRouter or the host of a custom endpoint. */
  providerName: string;
  model: string;
  modelName: string;
  data: boolean;
  production: boolean;
  anthropicModels: { ID: string; Name: string }[];
  /** Admins only: an Anthropic key is set in the server environment. */
  envKey?: boolean;
}

export const useAIStatus = () => useQuery({ queryKey: ["ai-status"], queryFn: () => get<AIStatus>("ai"), staleTime: 60_000 });

/** What the person is looking at, sent with a question. */
export interface EditorContext {
  statement?: string;
  selection?: string;
  script?: string;
  error?: string;
  plan?: string;
  line?: number;
}

export interface ToolStep {
  id: string;
  name: string;
  input: Record<string, unknown>;
  ok?: boolean;
  text?: string;
}

export type Item =
  | { kind: "user"; text: string; context?: string }
  | { kind: "assistant"; text: string; thinking: string; tools: ToolStep[]; done: boolean; error?: string; refusal?: string; notice?: string; model?: string }
  | { kind: "divider"; text: string };

export interface Thread {
  conversation?: string;
  items: Item[];
  busy: boolean;
}

interface Pending {
  message: string;
  context?: EditorContext;
  send: boolean;
}

interface AssistantState {
  threads: Record<string, Thread>;
  open: Record<string, boolean>;
  pending: Record<string, Pending | undefined>;
  setOpen(tab: string, open: boolean): void;
  thread(tab: string): Thread;
  update(tab: string, fn: (t: Thread) => Thread): void;
  /** Opens the panel for a tab with a question, optionally sending it right away. */
  ask(tab: string, message: string, context?: EditorContext, send?: boolean): void;
  take(tab: string): Pending | undefined;
  reset(tab: string): void;
}

const empty: Thread = { items: [], busy: false };

export const useAssistant = create<AssistantState>((set, get) => ({
  threads: {},
  open: {},
  pending: {},
  setOpen: (tab, open) => set({ open: { ...get().open, [tab]: open } }),
  thread: (tab) => get().threads[tab] ?? empty,
  update: (tab, fn) => set({ threads: { ...get().threads, [tab]: fn(get().threads[tab] ?? empty) } }),
  ask: (tab, message, context, send = true) => set({ open: { ...get().open, [tab]: true }, pending: { ...get().pending, [tab]: { message, context, send } } }),
  take: (tab) => {
    const p = get().pending[tab];
    if (p) set({ pending: { ...get().pending, [tab]: undefined } });
    return p;
  },
  reset: (tab) => set({ threads: { ...get().threads, [tab]: empty } }),
}));
