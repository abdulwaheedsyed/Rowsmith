import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { ArrowUp, Check, ChevronDown, ChevronRight, CircleAlert, ClipboardCopy, CornerDownLeft, Play, Replace, Sparkles, Square, SquarePen, X } from "lucide-react";
import { ApiError, post, streamLines } from "../../lib/api";
import type { Connection } from "../../lib/types";
import { go } from "../../lib/nav";
import { Button, Spinner, Tip } from "../../components/ui";
import { Markdown, highlightSQL } from "./Markdown";
import { useAIStatus, useAssistant, type EditorContext, type Item, type Thread, type ToolStep } from "./store";
import "./assistant.css";

type AIEvent =
  | { t: "conversation"; id: string; new: boolean }
  | { t: "thinking"; text: string }
  | { t: "text"; text: string }
  | { t: "tool"; id: string; name: string; input: Record<string, unknown> }
  | { t: "toolResult"; id: string; ok: boolean; text: string }
  | { t: "refusal"; category?: string; text?: string }
  | { t: "notice"; text: string }
  | { t: "error"; message: string }
  | { t: "done"; model?: string }
  | { t: "tick" };

const NO_ACTIONS = new Set(["bash", "sh", "shell", "text", "txt", "json", "console", "output"]);

function toolLabel(s: ToolStep): string {
  const i = s.input as Record<string, string | number>;
  switch (s.name) {
    case "list_tables":
      return i.pattern ? `Listed tables matching “${i.pattern}”` : "Listed the tables";
    case "describe_table":
      return `Looked at ${i.table}`;
    case "check_query":
      return s.ok === false ? "The database rejected the query" : "Checked the query against the database";
    case "sample_rows":
      return `Read ${i.limit ?? "a few"} rows of ${i.table}`;
    case "run_query":
      return "Ran a read-only query";
  }
  return s.name;
}

function contextLabel(c: EditorContext): string {
  if (c.selection) {
    const n = c.selection.split("\n").length;
    return `the selection (${n} line${n === 1 ? "" : "s"})`;
  }
  if (c.statement) return c.line ? `the statement at line ${c.line}` : "the statement at the cursor";
  if (c.script) return "the editor";
  return "";
}

export function AssistantPanel({ tabId, conn, database, schema, isAdmin, editorContext, onInsert, onReplace, onRun, onClose }: {
  tabId: string; conn: Connection; database?: string; schema?: string; isAdmin: boolean;
  editorContext(): EditorContext; onInsert(sql: string): void; onReplace(sql: string): void; onRun(sql: string): void; onClose(): void;
}) {
  const status = useAIStatus();
  const thread = useAssistant((s) => s.threads[tabId]) ?? { items: [], busy: false };
  const update = useAssistant((s) => s.update);
  const [draft, setDraft] = useState("");
  const [useContext, setUseContext] = useState(true);
  const abort = useRef<AbortController | null>(null);
  const scroller = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  const input = useRef<HTMLTextAreaElement>(null);
  const ctx = editorContext();
  const ctxLabel = contextLabel(ctx);

  const patchLast = useCallback((fn: (it: Extract<Item, { kind: "assistant" }>) => Extract<Item, { kind: "assistant" }>) => {
    update(tabId, (t) => {
      const items = [...t.items];
      const last = items[items.length - 1];
      if (last?.kind === "assistant") items[items.length - 1] = fn(last);
      return { ...t, items };
    });
  }, [tabId, update]);

  const send = useCallback(async (message: string, context?: EditorContext) => {
    const text = message.trim();
    const current = useAssistant.getState().thread(tabId);
    if (!text || current.busy) return;
    const c = context ?? (useContext ? editorContext() : {});
    const label = contextLabel(c) || (c.error ? "the error" : c.plan ? "the plan" : "");
    update(tabId, (t) => ({ ...t, busy: true, items: [...t.items, { kind: "user", text, context: label || undefined },
      { kind: "assistant", text: "", thinking: "", tools: [], done: false }] }));
    setDraft("");
    stick.current = true;
    const ac = new AbortController();
    abort.current = ac;

    // Deltas arrive many times a second; apply them once per frame.
    let text_ = "", thinking_ = "";
    let raf = 0;
    const flush = () => {
      raf = 0;
      const t2 = text_, th = thinking_;
      text_ = "";
      thinking_ = "";
      if (t2 || th) patchLast((it) => ({ ...it, text: it.text + t2, thinking: it.thinking + th }));
    };
    const later = () => (raf ||= requestAnimationFrame(flush));
    try {
      for await (const ev of streamLines<AIEvent>(`c/${conn.id}/ai`, { conversation: current.conversation, message: text, database, schema, context: c }, ac.signal)) {
        switch (ev.t) {
          case "conversation":
            update(tabId, (t) => {
              if (ev.new && t.conversation && t.conversation !== ev.id) {
                const items = [...t.items];
                items.splice(items.length - 2, 0, { kind: "divider", text: "New conversation: the previous one expired or used a different scope" });
                return { ...t, conversation: ev.id, items };
              }
              return { ...t, conversation: ev.id };
            });
            break;
          case "text":
            text_ += ev.text;
            later();
            break;
          case "thinking":
            thinking_ += ev.text;
            later();
            break;
          case "tool":
            flush();
            patchLast((it) => ({ ...it, tools: [...it.tools, { id: ev.id, name: ev.name, input: ev.input }] }));
            break;
          case "toolResult":
            patchLast((it) => ({ ...it, tools: it.tools.map((s) => (s.id === ev.id ? { ...s, ok: ev.ok, text: ev.text } : s)) }));
            break;
          case "refusal":
            patchLast((it) => ({ ...it, refusal: ev.text || "The model declined to help with this request." }));
            break;
          case "notice":
            patchLast((it) => ({ ...it, notice: ev.text }));
            break;
          case "error":
            flush();
            patchLast((it) => ({ ...it, error: ev.message }));
            break;
          case "done":
            flush();
            patchLast((it) => ({ ...it, model: ev.model }));
            break;
        }
      }
    } catch (e) {
      flush();
      if ((e as Error).name === "AbortError") patchLast((it) => ({ ...it, notice: "Stopped." }));
      else patchLast((it) => ({ ...it, error: e instanceof ApiError ? e.message : String(e) }));
    } finally {
      if (raf) cancelAnimationFrame(raf);
      flush();
      patchLast((it) => ({ ...it, done: true }));
      update(tabId, (t) => ({ ...t, busy: false }));
      abort.current = null;
    }
  }, [tabId, conn.id, database, schema, useContext, editorContext, update, patchLast]);

  // Questions queued from elsewhere (Fix with AI, Ask about this plan).
  const pending = useAssistant((s) => s.pending[tabId]);
  useEffect(() => {
    if (!pending || !status.data?.enabled) return;
    const p = useAssistant.getState().take(tabId);
    if (!p) return;
    if (p.send) send(p.message, p.context);
    else {
      setDraft(p.message);
      input.current?.focus();
    }
  }, [pending, status.data?.enabled, send, tabId]);

  useEffect(() => input.current?.focus(), []);
  useEffect(() => () => abort.current?.abort(), []);

  // Follow the answer while it streams, unless the person scrolled up.
  useLayoutEffect(() => {
    const el = scroller.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  });

  const newConversation = () => {
    abort.current?.abort();
    const id = thread.conversation;
    if (id) post("ai/forget", { conversation: id }).catch(() => {});
    useAssistant.getState().reset(tabId);
    setDraft("");
    input.current?.focus();
  };

  const code = (sql: string, lang: string, closed: boolean) => (
    <CodeBlock sql={sql} lang={lang} closed={closed} actions={!NO_ACTIONS.has(lang)} onInsert={onInsert} onReplace={onReplace} onRun={onRun} />
  );

  const enabled = status.data?.enabled;
  const policy = !status.data ? "" : status.data.data && (conn.environment !== "production" || status.data.production)
    ? `Sent to ${status.data.providerName}: your schema, plus the few rows and read-only query results the assistant reads to check answers.`
    : `Sent to ${status.data.providerName}: your schema (names, types, keys and comments), never the data in it.`;
  const suggestions = [
    { label: "Write a query…", fill: "Write a query that " },
    ...(ctx.statement || ctx.selection ? [{ label: "Explain this", send: "Explain what this query does." }] : []),
    ...(ctx.statement || ctx.selection ? [{ label: "Make it faster", send: "How can I make this query faster?" }] : []),
    { label: "What's in this database?", send: "Give me a short overview of the tables in this database and how they relate." },
  ];

  return (
    <aside className="ai-panel" aria-label="AI assistant">
      <header className="ai-panel__head">
        <span className="ai-glyph"><Sparkles /></span>
        <div className="grow" style={{ minWidth: 0 }}>
          <div className="ai-panel__title">Assistant</div>
          <div className="ai-panel__sub truncate">{status.data?.modelName ?? ""}{database || schema ? ` · ${[database, schema].filter(Boolean).join(".")}` : ""}</div>
        </div>
        <Tip label="New conversation"><Button size="sm" variant="ghost" icon onClick={newConversation} aria-label="New conversation" disabled={thread.items.length === 0}><SquarePen /></Button></Tip>
        <Tip label="Close"><Button size="sm" variant="ghost" icon onClick={onClose} aria-label="Close assistant"><X /></Button></Tip>
      </header>

      {status.isLoading ? (
        <div className="ai-panel__center"><Spinner /></div>
      ) : !enabled ? (
        <div className="ai-panel__off">
          <Sparkles className="ai-panel__off-icon" />
          <p className="ai-panel__off-title">The assistant is off</p>
          <p className="muted">It writes, explains and fixes queries using your schema, with the model your team picks: Anthropic, OpenAI, OpenRouter, or one you host with Ollama or another OpenAI-compatible server. {isAdmin ? "Choose one under Administration → AI assistant." : "Ask an admin to set it up under Administration → AI assistant."}</p>
          {isAdmin && <Button onClick={() => go("/admin/ai")}>Set up the assistant</Button>}
        </div>
      ) : (
        <>
          <div className="ai-panel__log" ref={scroller} onScroll={(e) => {
            const el = e.currentTarget;
            stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
          }}>
            {thread.items.length === 0 && (
              <div className="ai-intro">
                <p>Ask about this database in plain words. Answers come with SQL you can insert or run; nothing runs until you do.</p>
                <div className="ai-suggest">
                  {suggestions.map((s) => (
                    <button key={s.label} className="ai-chip" onClick={() => ("send" in s && s.send ? send(s.send) : (setDraft(s.fill ?? ""), input.current?.focus()))}>{s.label}</button>
                  ))}
                </div>
              </div>
            )}
            {thread.items.map((it, i) => <Entry key={i} item={it} code={code} last={i === thread.items.length - 1} busy={thread.busy} />)}
          </div>

          <form className="ai-compose" onSubmit={(e) => { e.preventDefault(); send(draft); }}>
            {ctxLabel && (
              <button type="button" className={`ai-ctx ${useContext ? "" : "is-off"}`} onClick={() => setUseContext(!useContext)} title={useContext ? "Click to leave it out" : "Click to include it"}>
                {useContext ? <Check size={12} /> : <X size={12} />} {useContext ? "Includes" : "Leaves out"} {ctxLabel}
              </button>
            )}
            <div className="ai-compose__box">
              <textarea ref={input} className="ai-compose__input" rows={1} value={draft} placeholder="Ask about your data, or describe a query…"
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
                    e.preventDefault();
                    send(draft);
                  }
                }} />
              {thread.busy ? (
                <Button size="sm" icon onClick={() => abort.current?.abort()} aria-label="Stop"><Square /></Button>
              ) : (
                <Button size="sm" variant="primary" icon type="submit" disabled={!draft.trim()} aria-label="Send"><ArrowUp /></Button>
              )}
            </div>
            <div className="ai-compose__note faint">{policy} Enter sends · Shift+Enter adds a line</div>
          </form>
        </>
      )}
    </aside>
  );
}

function Entry({ item, code, last, busy }: { item: Item; code: (sql: string, lang: string, closed: boolean) => React.ReactNode; last: boolean; busy: boolean }) {
  const [showThinking, setShowThinking] = useState(false);
  if (item.kind === "divider") return <div className="ai-divider"><span>{item.text}</span></div>;
  if (item.kind === "user") {
    return (
      <div className="ai-user">
        {item.context && <div className="ai-user__ctx faint">with {item.context}</div>}
        <div className="ai-user__text">{item.text}</div>
      </div>
    );
  }
  const streaming = !item.done && last && busy;
  const thinkingOnly = streaming && !item.text && !item.tools.length;
  return (
    <div className="ai-answer">
      {(item.thinking || thinkingOnly) && (
        <button className="ai-think" onClick={() => setShowThinking(!showThinking)} aria-expanded={showThinking}>
          {showThinking ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
          {thinkingOnly ? <span className="ai-think__live">Thinking</span> : <span>Reasoning</span>}
        </button>
      )}
      {showThinking && item.thinking && <div className="ai-think__text">{item.thinking}</div>}
      {item.tools.length > 0 && (
        <div className="ai-tools">
          {item.tools.map((s) => (
            <details key={s.id} className={`ai-tool ${s.ok === false ? "is-err" : ""}`}>
              <summary>
                {s.ok === undefined ? <Spinner /> : s.ok ? <Check className="ai-tool__ok" /> : <CircleAlert className="ai-tool__err" />}
                <span>{toolLabel(s)}</span>
              </summary>
              {typeof s.input.sql === "string" && <pre className="ai-tool__sql mono">{highlightSQL(s.input.sql)}</pre>}
              {s.text && <pre className="ai-tool__out mono">{s.text}</pre>}
            </details>
          ))}
        </div>
      )}
      {item.text && <Markdown text={item.text} code={code} />}
      {streaming && item.text && <span className="ai-caret" aria-hidden />}
      {item.refusal && <div className="ai-alert">{item.refusal}</div>}
      {item.error && <div className="ai-alert ai-alert--err"><CircleAlert size={14} /> {item.error}</div>}
      {item.notice && <div className="ai-note faint">{item.notice}</div>}
    </div>
  );
}

function CodeBlock({ sql, lang, closed, actions, onInsert, onReplace, onRun }: {
  sql: string; lang: string; closed: boolean; actions: boolean; onInsert(s: string): void; onReplace(s: string): void; onRun(s: string): void;
}) {
  const [copied, setCopied] = useState(false);
  const body = sql.replace(/\s+$/, "");
  return (
    <div className="ai-code">
      <div className="ai-code__bar">
        <span className="ai-code__lang">{lang || "sql"}</span>
        <span className="spacer" />
        {closed && actions && (
          <>
            <Tip label="Insert at the cursor"><button className="ai-code__btn" onClick={() => onInsert(body)}><CornerDownLeft /> Insert</button></Tip>
            <Tip label="Replace everything in the editor"><button className="ai-code__btn" onClick={() => onReplace(body)}><Replace /> Replace</button></Tip>
            <Tip label="Run it (confirmations still apply)"><button className="ai-code__btn" onClick={() => onRun(body)}><Play /> Run</button></Tip>
          </>
        )}
        <Tip label="Copy"><button className="ai-code__btn" onClick={() => { navigator.clipboard?.writeText(body); setCopied(true); setTimeout(() => setCopied(false), 1200); }} aria-label="Copy">
          {copied ? <Check /> : <ClipboardCopy />}
        </button></Tip>
      </div>
      <pre className="ai-code__pre mono">{highlightSQL(body)}</pre>
    </div>
  );
}

export type { Thread };
