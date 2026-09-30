import { useQuery } from "@tanstack/react-query";
import { get } from "../../lib/api";
import type { Cell, ResultColumn } from "../../lib/types";

export interface ShareCan {
  edit: boolean;
  delete: boolean;
  results: boolean;
  run: boolean;
}

export interface Share {
  id: string;
  authorId: string;
  authorName: string;
  connectionId: string;
  connectionName: string;
  driver: string;
  database: string;
  schema: string;
  title: string;
  description: string;
  body: string;
  audience: "team" | "people";
  people: { id: string; name: string }[];
  hasResult: boolean;
  resultRows: number;
  resultAt: number;
  expiresAt: number;
  expired: boolean;
  lastActivity: number;
  createdAt: number;
  comments: number;
  openThreads: number;
  unread?: boolean;
  can: ShareCan;
}

export interface Comment {
  id: string;
  shareId: string;
  authorId: string;
  authorName: string;
  parentId: string;
  line: number;
  body: string;
  resolvedAt: number;
  resolvedBy: string;
  createdAt: number;
  editedAt: number;
}

export interface Snapshot {
  columns: ResultColumn[];
  rows: Cell[][];
  rowCount: number;
  truncated: boolean;
  ms: number;
}

/** The detail endpoint lists the comments instead of counting them. */
export type ShareDetail = Omit<Share, "comments"> & {
  comments: Comment[];
  result?: Snapshot;
  names: Record<string, string>;
};

export interface Notification {
  id: string;
  kind: "shared" | "comment" | "reply" | "mention";
  actorId: string;
  actorName: string;
  shareId: string;
  title: string;
  commentId: string;
  excerpt: string;
  createdAt: number;
  readAt: number;
}

export const shareUrl = (id: string) => new URL(`q/${id}`, document.baseURI).toString();

export const useShares = (all: boolean) => useQuery({ queryKey: ["shares", all], queryFn: () => get<Share[]>(all ? "shares?all=1" : "shares") });

export const useShare = (id: string) =>
  useQuery({ queryKey: ["share", id], queryFn: () => get<ShareDetail>(`shares/${id}`), refetchInterval: 20_000, retry: false });

export const useNotifications = () =>
  useQuery({ queryKey: ["notifications"], queryFn: () => get<{ items: Notification[]; unread: number }>("notifications"), refetchInterval: 45_000 });

/** Threads: first comments with their replies, oldest first. */
export function threads(comments: Comment[]) {
  const roots = comments.filter((c) => !c.parentId);
  return roots.map((root) => ({ root, replies: comments.filter((c) => c.parentId === root.id) }));
}
