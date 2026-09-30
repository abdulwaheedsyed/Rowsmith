import { useState } from "react";
import * as RPopover from "@radix-ui/react-popover";
import { useQueryClient } from "@tanstack/react-query";
import { AtSign, Bell, CornerDownRight, Link2, MessageSquare } from "lucide-react";
import { post } from "../../lib/api";
import { ago } from "../../lib/format";
import { go } from "../../lib/nav";
import { Tip } from "../../components/ui";
import { useNotifications, type Notification } from "./api";
import { plainText } from "./Comments";
import "./shares.css";

const KIND: Record<Notification["kind"], { icon: typeof Bell; text: string }> = {
  shared: { icon: Link2, text: "shared a query with you" },
  comment: { icon: MessageSquare, text: "commented on your query" },
  reply: { icon: CornerDownRight, text: "replied in a thread you're in" },
  mention: { icon: AtSign, text: "mentioned you" },
};

export function NotificationBell({ side = "right" }: { side?: "right" | "bottom" }) {
  const qc = useQueryClient();
  const n = useNotifications();
  const [open, setOpen] = useState(false);
  const unread = n.data?.unread ?? 0;
  const readAll = async () => {
    await post("notifications/read", { ids: [] });
    qc.invalidateQueries({ queryKey: ["notifications"] });
    qc.invalidateQueries({ queryKey: ["shares"] });
  };
  const openOne = async (x: Notification) => {
    setOpen(false);
    if (!x.readAt) post("notifications/read", { ids: [x.id] }).then(() => qc.invalidateQueries({ queryKey: ["notifications"] }));
    go(`/q/${x.shareId}${x.commentId ? `#c-${x.commentId}` : ""}`);
  };
  return (
    <RPopover.Root open={open} onOpenChange={setOpen}>
      <Tip label={unread ? `${unread} unread` : "Notifications"} side={side === "right" ? "right" : "bottom"}>
        <RPopover.Trigger asChild>
          <button className={`rail__icon bell ${unread ? "has-unread" : ""}`} aria-label={unread ? `Notifications, ${unread} unread` : "Notifications"}>
            <Bell />
            {unread > 0 && <span className="bell__badge">{unread > 99 ? "99+" : unread}</span>}
          </button>
        </RPopover.Trigger>
      </Tip>
      <RPopover.Portal>
        <RPopover.Content className="notes" side={side} align="end" sideOffset={8} collisionPadding={12}>
          <div className="notes__head">
            <b>Notifications</b>
            <span className="spacer" />
            {unread > 0 && <button className="notes__readall" onClick={readAll}>Mark all read</button>}
          </div>
          <div className="notes__list">
            {!n.data?.items.length && <p className="muted notes__empty">When someone shares a query with you, mentions you, or comments on your queries, it shows up here.</p>}
            {n.data?.items.map((x) => {
              const k = KIND[x.kind] ?? KIND.comment;
              return (
                <button key={x.id} className={`note ${x.readAt ? "" : "is-unread"}`} onClick={() => openOne(x)}>
                  <k.icon className="note__icon" />
                  <span className="note__main">
                    <span><b>{x.actorName || "Someone"}</b> {k.text}</span>
                    <span className="note__title truncate">{x.title || "a deleted query"}</span>
                    {x.excerpt && <span className="note__excerpt">{plainText(x.excerpt)}</span>}
                  </span>
                  <span className="note__when faint">{ago(x.createdAt)}</span>
                </button>
              );
            })}
          </div>
        </RPopover.Content>
      </RPopover.Portal>
    </RPopover.Root>
  );
}
