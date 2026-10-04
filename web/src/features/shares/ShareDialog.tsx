import { useMemo, useState } from "react";
import { create } from "zustand";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, Copy, Link2, Users, UserRound } from "lucide-react";
import { ApiError, get, patch, post } from "../../lib/api";
import type { User } from "../../lib/types";
import { toast } from "../../lib/store";
import { go } from "../../lib/nav";
import { Alert, Button, Dialog, Field } from "../../components/ui";
import { shareUrl, type Share } from "./api";
import "./shares.css";

export interface ShareSeed {
  connectionId: string;
  connectionName: string;
  database?: string;
  schema?: string;
  title?: string;
  body: string;
  canResult: boolean; // the query only reads, so a snapshot can be taken
}

type Req = { kind: "new"; seed: ShareSeed } | { kind: "edit"; share: Share };

const useShareDialog = create<{ req: Req | null; set(r: Req | null): void }>((set) => ({ req: null, set: (req) => set({ req }) }));
export const openShareDialog = (seed: ShareSeed) => useShareDialog.getState().set({ kind: "new", seed });
export const openShareSettings = (share: Share) => useShareDialog.getState().set({ kind: "edit", share });

export function ShareDialogHost() {
  const req = useShareDialog((s) => s.req);
  if (!req) return null;
  return <ShareDialog key={req.kind === "edit" ? req.share.id : "new"} req={req} onClose={() => useShareDialog.getState().set(null)} />;
}

const EXPIRY = [
  { days: 0, label: "Never" },
  { days: 1, label: "1 day" },
  { days: 7, label: "7 days" },
  { days: 30, label: "30 days" },
];

function ShareDialog({ req, onClose }: { req: Req; onClose(): void }) {
  const qc = useQueryClient();
  const editing = req.kind === "edit" ? req.share : null;
  const seed = req.kind === "new" ? req.seed : null;
  const users = useQuery({ queryKey: ["users"], queryFn: () => get<User[]>("users"), staleTime: 60_000 });
  const me = qc.getQueryData<{ user: User }>(["me"])?.user;
  const [title, setTitle] = useState(editing?.title ?? seed?.title ?? "");
  const [description, setDescription] = useState(editing?.description ?? "");
  const [audience, setAudience] = useState<"team" | "people">(editing?.audience ?? "team");
  const [people, setPeople] = useState<string[]>(editing?.people.map((p) => p.id) ?? []);
  const [includeResult, setIncludeResult] = useState(false);
  const [expires, setExpires] = useState<number | null>(editing ? null : 0); // null keeps the current expiry
  const [filter, setFilter] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [created, setCreated] = useState<Share | null>(null);
  const [copied, setCopied] = useState(false);

  const others = useMemo(() => (users.data ?? []).filter((u) => u.id !== me?.id && !u.disabled), [users.data, me?.id]);
  const shown = others.filter((u) => !filter || `${u.name} ${u.email}`.toLowerCase().includes(filter.toLowerCase()));
  const connName = editing?.connectionName ?? seed?.connectionName ?? "";

  const submit = async () => {
    setBusy(true);
    setError("");
    try {
      if (editing) {
        const body: Record<string, unknown> = { title, description, audience, people };
        if (expires !== null) body.expiresDays = expires;
        const next = await patch<Share>(`shares/${editing.id}`, body);
        qc.invalidateQueries({ queryKey: ["share", editing.id] });
        qc.invalidateQueries({ queryKey: ["shares"] });
        toast.success("Sharing updated", next.audience === "team" ? "Everyone on the team can open it." : `${next.people.length} ${next.people.length === 1 ? "person" : "people"} can open it.`);
        onClose();
        return;
      }
      const share = await post<Share>("shares", { connectionId: seed!.connectionId, database: seed!.database, schema: seed!.schema, title, description,
        body: seed!.body, audience, people, includeResult, expiresDays: expires ?? 0 });
      qc.invalidateQueries({ queryKey: ["shares"] });
      setCreated(share);
      copy(share.id);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const copy = async (id: string) => {
    try {
      await navigator.clipboard.writeText(shareUrl(id));
      setCopied(true);
    } catch {
      setCopied(false);
    }
  };

  if (created) {
    return (
      <Dialog open onOpenChange={(o) => !o && onClose()} title="Link ready"
        description={created.audience === "team" ? "Anyone on the team can open it and join the discussion." : `Shared with ${created.people.map((p) => p.name).join(", ")}. They were notified.`}
        footer={<><Button onClick={onClose}>Done</Button><Button variant="primary" onClick={() => { onClose(); go(`/q/${created.id}`); }}>Open the discussion</Button></>}>
        <div className="sharelink">
          <input className="input input--mono" readOnly value={shareUrl(created.id)} onFocus={(e) => e.currentTarget.select()} aria-label="Link" />
          <Button onClick={() => copy(created.id)}>{copied ? <Check /> : <Copy />} {copied ? "Copied" : "Copy"}</Button>
        </div>
        {created.hasResult && <p className="faint sharelink__note">The result ({created.resultRows.toLocaleString()} rows) is included for people who can read {connName}.</p>}
      </Dialog>
    );
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()} title={editing ? "Sharing" : "Share this query"}
      description={editing ? "Change who can open it, or when the link expires." : `A link to this exact query on ${connName}, with a place to discuss it.`}
      footer={<><Button onClick={onClose}>Cancel</Button><Button variant="primary" onClick={submit} loading={busy} disabled={!title.trim() || (audience === "people" && !people.length)}>
        <Link2 /> {editing ? "Save" : "Create link"}</Button></>}>
      <div className="sharedlg">
        {error && <Alert kind="danger">{error}</Alert>}
        <Field label="Title" htmlFor="share-title">
          <input id="share-title" className="input" value={title} onChange={(e) => setTitle(e.target.value)} maxLength={200} placeholder="What this query answers" autoFocus />
        </Field>
        <Field label="Note" htmlFor="share-note" help="Optional: what to look at, or what you want to know.">
          <textarea id="share-note" className="input sharedlg__note" value={description} onChange={(e) => setDescription(e.target.value)} maxLength={4000} rows={2} />
        </Field>
        <div className="sharedlg__who" role="radiogroup" aria-label="Who can open it">
          <button role="radio" aria-checked={audience === "team"} className="sharedlg__opt" onClick={() => setAudience("team")}>
            <Users /> <span><b>Everyone on the team</b><small>Anyone signed in to Rowsmith</small></span>
          </button>
          <button role="radio" aria-checked={audience === "people"} className="sharedlg__opt" onClick={() => setAudience("people")}>
            <UserRound /> <span><b>Chosen people</b><small>They get a notification</small></span>
          </button>
        </div>
        {audience === "people" && (
          <div className="peoplepick">
            <input className="input" value={filter} onChange={(e) => setFilter(e.target.value)} placeholder="Find people" aria-label="Find people" />
            <div className="peoplepick__list">
              {shown.map((u) => (
                <label key={u.id} className="peoplepick__row">
                  <input type="checkbox" checked={people.includes(u.id)} onChange={(e) => setPeople(e.target.checked ? [...people, u.id] : people.filter((p) => p !== u.id))} />
                  <span className="avatar avatar--xs">{u.name.split(/\s+/).map((w) => w[0]).slice(0, 2).join("").toUpperCase()}</span>
                  <span className="grow truncate">{u.name}</span>
                  <span className="faint truncate">{u.email}</span>
                </label>
              ))}
              {!shown.length && <p className="faint peoplepick__empty">No one matches.</p>}
            </div>
          </div>
        )}
        {!editing && (
          <label className={`check ${seed?.canResult ? "" : "is-disabled"}`}>
            <input type="checkbox" checked={includeResult} disabled={!seed?.canResult} onChange={(e) => setIncludeResult(e.target.checked)} />
            <span>Include the result <span className="faint">{seed?.canResult
              ? `— runs it once more, read-only, and keeps up to 500 rows. Only people who can read ${connName} see them.`
              : "— only for queries that only read."}</span></span>
          </label>
        )}
        <Field label="Link expires" group>
          <div className="segmented">
            {editing && <button aria-pressed={expires === null} onClick={() => setExpires(null)}>{editing.expiresAt ? `Keep (${new Date(editing.expiresAt).toLocaleDateString()})` : "Keep (never)"}</button>}
            {EXPIRY.map((x) => <button key={x.days} aria-pressed={expires === x.days} onClick={() => setExpires(x.days)}>{x.label}</button>)}
          </div>
        </Field>
        <p className="faint sharedlg__fine">The link only opens for people signed in to this Rowsmith. The query is saved as it is now; later edits in your tab don't change it.</p>
      </div>
    </Dialog>
  );
}
