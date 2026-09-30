import { useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, CornerDownRight, MoreHorizontal, Pencil, RotateCcw, Trash2 } from "lucide-react";
import { ApiError, del, get, patch, post } from "../../lib/api";
import type { User } from "../../lib/types";
import { ago } from "../../lib/format";
import { toast } from "../../lib/store";
import { Button, Menu, MenuContent, MenuItem, MenuTrigger } from "../../components/ui";
import { highlightSQL } from "../ai/Markdown";
import type { Comment, ShareDetail } from "./api";

export const initialsOf = (name: string) => name.split(/\s+/).filter(Boolean).map((w) => w[0]).slice(0, 2).join("").toUpperCase() || "?";

const MENTION = /<@([0-9a-z]{10,40})>/g;
const URL_RE = /\bhttps?:\/\/[^\s<>"')]+[^\s<>"').,;:!?]/g;

function inlineText(text: string, names: Record<string, string>, key: string): ReactNode[] {
  const out: ReactNode[] = [];
  // Inline code first; mentions and links only outside it.
  text.split(/(`[^`\n]+`)/).forEach((part, i) => {
    if (part.startsWith("`") && part.endsWith("`") && part.length > 2) {
      out.push(<code key={`${key}c${i}`} className="cm-code">{part.slice(1, -1)}</code>);
      return;
    }
    let last = 0;
    const re = new RegExp(`${MENTION.source}|${URL_RE.source}`, "g");
    let m: RegExpExecArray | null;
    let n = 0;
    while ((m = re.exec(part))) {
      if (m.index > last) out.push(part.slice(last, m.index));
      if (m[1]) out.push(<span key={`${key}m${i}-${n++}`} className="mention">@{names[m[1]] ?? "someone"}</span>);
      else out.push(<a key={`${key}u${i}-${n++}`} href={m[0]} target="_blank" rel="noopener noreferrer">{m[0]}</a>);
      last = m.index + m[0].length;
    }
    if (last < part.length) out.push(part.slice(last));
  });
  return out;
}

/** A comment's text: paragraphs, ```sql blocks```, `code`, links and @mentions. */
export function CommentBody({ body, names }: { body: string; names: Record<string, string> }) {
  const parts = body.split(/```(?:[a-z]*)\n?([\s\S]*?)```/);
  return (
    <div className="cbody">
      {parts.map((part, i) =>
        i % 2 === 1 ? (
          <pre key={i} className="cbody__code">{highlightSQL(part.replace(/\n$/, ""))}</pre>
        ) : (
          part.split(/\n{2,}/).filter((p) => p.trim()).map((para, j) => (
            <p key={`${i}-${j}`}>{para.split("\n").flatMap((line, k) => (k ? [<br key={`b${k}`} />, ...inlineText(line, names, `${i}-${j}-${k}`)] : inlineText(line, names, `${i}-${j}-${k}`)))}</p>
          ))
        ),
      )}
    </div>
  );
}

function useTeam() {
  return useQuery({ queryKey: ["users"], queryFn: () => get<User[]>("users"), staleTime: 60_000 });
}

/** A one-line version of a comment, for previews. */
export function plainText(body: string, names: Record<string, string> = {}) {
  return body.replace(MENTION, (_, id: string) => "@" + (names[id] ?? "someone")).replace(/```[a-z]*\n?/g, " ").replace(/`/g, "").replace(/\s+/g, " ").trim();
}

/** Text as typed (with @Name) to the stored form (with <@id>), and back. */
function encode(text: string, picked: Map<string, string>) {
  let out = text;
  // Longest names first, so "@Ana Maria" wins over "@Ana".
  for (const [name, id] of [...picked.entries()].sort((a, b) => b[0].length - a[0].length)) {
    out = out.split("@" + name).join(`<@${id}>`);
  }
  return out;
}

function decode(body: string, names: Record<string, string>) {
  const picked = new Map<string, string>();
  const text = body.replace(MENTION, (_, id: string) => {
    const name = names[id] ?? "someone";
    picked.set(name, id);
    return "@" + name;
  });
  return { text, picked };
}

export function Composer({ placeholder, submitLabel = "Comment", initial, names, autoFocus, onSubmit, onCancel }: {
  placeholder: string;
  submitLabel?: string;
  initial?: string;
  names: Record<string, string>;
  autoFocus?: boolean;
  onSubmit(body: string): Promise<void>;
  onCancel?(): void;
}) {
  const start = useMemo(() => decode(initial ?? "", names), [initial, names]);
  const [text, setText] = useState(start.text);
  const picked = useRef(start.picked);
  const [busy, setBusy] = useState(false);
  const [query, setQuery] = useState<{ q: string; at: number } | null>(null);
  const [active, setActive] = useState(0);
  const area = useRef<HTMLTextAreaElement>(null);
  const caret = useRef<number | null>(null);
  // Place the caret after a picked mention as soon as the text updates, so
  // the next keystroke lands in the right spot.
  useLayoutEffect(() => {
    if (caret.current === null || !area.current) return;
    area.current.setSelectionRange(caret.current, caret.current);
    caret.current = null;
  }, [text]);
  const team = useTeam();
  const me = useQueryClient().getQueryData<{ user: User }>(["me"])?.user;

  const matches = useMemo(() => {
    if (!query) return [];
    const q = query.q.toLowerCase();
    return (team.data ?? []).filter((u) => !u.disabled && u.id !== me?.id && (`${u.name} ${u.email}`.toLowerCase().includes(q))).slice(0, 6);
  }, [query, team.data, me?.id]);

  const detect = (value: string, caret: number) => {
    const before = value.slice(0, caret);
    const m = /(^|[\s(])@([\p{L}\p{N}._-]{0,24})$/u.exec(before);
    setQuery(m ? { q: m[2], at: caret - m[2].length - 1 } : null);
    setActive(0);
  };

  const pick = (u: User) => {
    if (!query) return;
    const at = area.current?.selectionStart ?? text.length;
    const next = text.slice(0, query.at) + "@" + u.name + " " + text.slice(at);
    picked.current.set(u.name, u.id);
    caret.current = query.at + u.name.length + 2;
    setText(next);
    setQuery(null);
    area.current?.focus();
  };

  const submit = async () => {
    if (!text.trim() || busy) return;
    setBusy(true);
    try {
      await onSubmit(encode(text.trim(), picked.current));
      setText("");
      picked.current = new Map();
    } catch (e) {
      toast.error("Could not post", e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const onKey = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (matches.length) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        setActive((a) => (a + (e.key === "ArrowDown" ? 1 : matches.length - 1)) % matches.length);
        return;
      }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        pick(matches[active]);
        return;
      }
      if (e.key === "Escape") {
        e.preventDefault();
        setQuery(null);
        return;
      }
    }
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      submit();
    } else if (e.key === "Escape" && onCancel) {
      onCancel();
    }
  };

  return (
    <div className="composer">
      <textarea ref={area} className="input composer__input" value={text} placeholder={placeholder} autoFocus={autoFocus} rows={Math.min(8, Math.max(2, text.split("\n").length))}
        onChange={(e) => { setText(e.target.value); detect(e.target.value, e.target.selectionStart); }}
        onClick={(e) => detect(text, e.currentTarget.selectionStart)} onKeyDown={onKey} onBlur={() => setTimeout(() => setQuery(null), 150)}
        aria-label={placeholder} />
      {matches.length > 0 && (
        <div className="composer__suggest" role="listbox" aria-label="Mention someone">
          {matches.map((u, i) => (
            <button key={u.id} role="option" aria-selected={i === active} onMouseDown={(e) => { e.preventDefault(); pick(u); }}>
              <span className="avatar avatar--xs">{initialsOf(u.name)}</span> {u.name} <span className="faint">{u.email}</span>
            </button>
          ))}
        </div>
      )}
      <div className="composer__foot">
        <span className="faint">@ to mention · `code` · ```sql blocks``` · {navigator.platform.includes("Mac") ? "⌘" : "Ctrl"}+Enter to post</span>
        <span className="spacer" />
        {onCancel && <Button size="sm" variant="ghost" onClick={onCancel}>Cancel</Button>}
        <Button size="sm" variant="primary" onClick={submit} loading={busy} disabled={!text.trim()}>{submitLabel}</Button>
      </div>
    </div>
  );
}

export function Thread({ share, root, replies }: { share: ShareDetail; root: Comment; replies: Comment[] }) {
  const qc = useQueryClient();
  const me = qc.getQueryData<{ user: User }>(["me"])?.user;
  const [replying, setReplying] = useState(false);
  const [show, setShow] = useState(false);
  const refresh = () => qc.invalidateQueries({ queryKey: ["share", share.id] });
  const isAdmin = me?.role === "owner" || me?.role === "admin";
  const canResolve = root.authorId === me?.id || share.can.edit || isAdmin;
  const resolved = root.resolvedAt > 0;

  const resolve = async (on: boolean) => {
    try {
      await post(`comments/${root.id}/resolve`, { resolved: on });
      refresh();
    } catch (e) {
      toast.error("Could not update the thread", e instanceof ApiError ? e.message : String(e));
    }
  };

  if (resolved && !show) {
    return (
      <div className="thread thread--resolved">
        <button className="thread__collapsed" onClick={() => setShow(true)}>
          <CheckCircle2 /> Resolved by {share.names[root.resolvedBy] ?? "someone"} · {root.authorName}: <span className="truncate">{plainText(root.body, share.names).slice(0, 100)}</span>
          <span className="faint">{replies.length ? `${replies.length + 1} comments` : ""} Show</span>
        </button>
      </div>
    );
  }

  return (
    <div className={`thread ${resolved ? "thread--resolved" : ""}`}>
      {[root, ...replies].map((c) => <CommentItem key={c.id} share={share} c={c} me={me} isAdmin={isAdmin} />)}
      {replying ? (
        <div className="thread__reply">
          <Composer placeholder="Reply…" submitLabel="Reply" names={share.names} autoFocus onCancel={() => setReplying(false)}
            onSubmit={async (body) => { await post(`shares/${share.id}/comments`, { body, parentId: root.id }); setReplying(false); refresh(); }} />
        </div>
      ) : (
        <div className="thread__actions">
          {!share.expired && <button onClick={() => setReplying(true)}><CornerDownRight /> Reply</button>}
          {canResolve && (resolved
            ? <button onClick={() => resolve(false)}><RotateCcw /> Reopen</button>
            : <button onClick={() => resolve(true)}><CheckCircle2 /> Resolve</button>)}
          {resolved && <button onClick={() => setShow(false)}>Hide</button>}
        </div>
      )}
    </div>
  );
}

function CommentItem({ share, c, me, isAdmin }: { share: ShareDetail; c: Comment; me?: User; isAdmin: boolean }) {
  const qc = useQueryClient();
  const [editing, setEditing] = useState(false);
  const mine = c.authorId === me?.id;
  const canDelete = mine || share.can.edit || isAdmin;
  const refresh = () => qc.invalidateQueries({ queryKey: ["share", share.id] });
  return (
    <article className="comment" id={`c-${c.id}`}>
      <span className="avatar avatar--sm comment__avatar">{initialsOf(c.authorName)}</span>
      <div className="comment__main">
        <header className="comment__head">
          <b>{c.authorName}</b>
          <a className="faint" href={`#c-${c.id}`} title={new Date(c.createdAt).toLocaleString()}>{ago(c.createdAt)}</a>
          {c.editedAt > 0 && <span className="faint">· edited</span>}
          <span className="spacer" />
          {(mine || canDelete) && !editing && (
            <Menu>
              <MenuTrigger asChild><Button size="sm" variant="ghost" icon aria-label="Comment actions"><MoreHorizontal /></Button></MenuTrigger>
              <MenuContent align="end">
                {mine && <MenuItem icon={<Pencil />} onSelect={() => setEditing(true)}>Edit</MenuItem>}
                {canDelete && <MenuItem icon={<Trash2 />} danger onSelect={async () => {
                  try {
                    await del(`comments/${c.id}`);
                    refresh();
                  } catch (e) {
                    toast.error("Could not delete it", e instanceof ApiError ? e.message : String(e));
                  }
                }}>{c.parentId ? "Delete" : "Delete thread"}</MenuItem>}
              </MenuContent>
            </Menu>
          )}
        </header>
        {editing ? (
          <Composer placeholder="Edit your comment" submitLabel="Save" initial={c.body} names={share.names} autoFocus onCancel={() => setEditing(false)}
            onSubmit={async (body) => { await patch(`comments/${c.id}`, { body }); setEditing(false); refresh(); }} />
        ) : (
          <CommentBody body={c.body} names={share.names} />
        )}
      </div>
    </article>
  );
}
