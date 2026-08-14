import type {
  BoardRow,
  ChunkDetail,
  ChunkRow,
  DocRow,
  GroupAxis,
  LayoutKind,
  SavedView,
  SortField,
  ViewLayout,
  ViewQuery,
} from "../types";

// Pure view logic — DOM-free so it's unit-testable without a renderer. Parses a
// saved view's opaque `fields` defensively, normalizes document/chunk rows into
// one BoardRow shape, and groups/sorts for the layouts.

const LAYOUT_KINDS: readonly LayoutKind[] = ["table", "cards", "kanban", "list"];
const GROUP_AXES: readonly GroupAxis[] = ["status", "type"];
const SORT_FIELDS: readonly SortField[] = ["title", "updated", "position"];

const DEFAULT_QUERY: ViewQuery = { source: "documents" };
const DEFAULT_LAYOUT: ViewLayout = {
  kind: "table",
  groupBy: "status",
  sortBy: "updated",
  sortDir: "desc",
};

/** The value shown for a row with no value on the grouping axis. */
export const UNGROUPED = "—";

/** Parse a `type=view` root chunk into a SavedView. `{query, layout}` live in
 *  the opaque `fields`; anything missing or garbled falls back to a safe default
 *  so a hand-edited or legacy view still opens instead of throwing. */
export function parseView(documentId: string, root: ChunkDetail): SavedView {
  const f = (root.fields ?? {}) as { query?: unknown; layout?: unknown };
  return {
    documentId,
    rootChunkId: root.id,
    title: root.title,
    query: normalizeQuery(f.query),
    layout: normalizeLayout(f.layout),
    revision: root.revision,
  };
}

export function normalizeQuery(q: unknown): ViewQuery {
  if (!q || typeof q !== "object") return { ...DEFAULT_QUERY };
  const o = q as Record<string, unknown>;
  const out: ViewQuery = { source: o.source === "chunks" ? "chunks" : "documents" };
  for (const k of ["documentId", "underPath", "type", "status", "tag", "tagPrefix"] as const) {
    if (typeof o[k] === "string" && o[k]) out[k] = o[k] as string;
  }
  if (typeof o.limit === "number") out.limit = o.limit;
  return out;
}

export function normalizeLayout(l: unknown): ViewLayout {
  if (!l || typeof l !== "object") return { ...DEFAULT_LAYOUT };
  const o = l as Record<string, unknown>;
  const kind = LAYOUT_KINDS.includes(o.kind as LayoutKind) ? (o.kind as LayoutKind) : "table";
  const out: ViewLayout = { kind };
  out.groupBy = GROUP_AXES.includes(o.groupBy as GroupAxis) ? (o.groupBy as GroupAxis) : "status";
  if (SORT_FIELDS.includes(o.sortBy as SortField)) out.sortBy = o.sortBy as SortField;
  out.sortDir = o.sortDir === "asc" ? "asc" : "desc";
  if (Array.isArray(o.columns)) {
    out.columns = o.columns.filter(
      (c): c is "type" | "status" | "updated" =>
        c === "type" || c === "status" || c === "updated",
    );
  }
  return out;
}

export function docRowToBoardRow(d: DocRow): BoardRow {
  return {
    id: d.document_id,
    documentId: d.document_id,
    title: d.title,
    type: d.type,
    status: d.status,
    updatedAt: d.updated_at,
  };
}

export function chunkRowToBoardRow(c: ChunkRow): BoardRow {
  return {
    id: c.id,
    documentId: c.document_id,
    title: c.title,
    type: c.type,
    status: c.status,
    position: c.position,
  };
}

/** A single grouping bucket, preserving first-seen column order. */
export interface RowGroup {
  key: string;
  rows: BoardRow[];
}

/** Bucket rows by a single-valued axis (status | type). Rows with no value on
 *  the axis go to the UNGROUPED bucket. Column order is first-seen. */
export function groupRows(rows: BoardRow[], axis: GroupAxis): RowGroup[] {
  const order: string[] = [];
  const byKey = new Map<string, BoardRow[]>();
  for (const r of rows) {
    const key = (axis === "status" ? r.status : r.type) || UNGROUPED;
    let bucket = byKey.get(key);
    if (!bucket) {
      bucket = [];
      byKey.set(key, bucket);
      order.push(key);
    }
    bucket.push(r);
  }
  return order.map((key) => ({ key, rows: byKey.get(key)! }));
}

/** Sort rows in place-safe (returns a new array) by the chosen field. */
export function sortRows(rows: BoardRow[], by?: SortField, dir: "asc" | "desc" = "desc"): BoardRow[] {
  const sorted = [...rows];
  if (!by) return sorted;
  const sign = dir === "asc" ? 1 : -1;
  sorted.sort((a, b) => {
    let cmp: number;
    if (by === "title") cmp = a.title.localeCompare(b.title);
    else if (by === "updated") cmp = (a.updatedAt ?? 0) - (b.updatedAt ?? 0);
    else cmp = (a.position ?? 0) - (b.position ?? 0);
    return cmp * sign;
  });
  return sorted;
}

/** unix-nanoseconds → a short human "when" (or "" when unset). */
export function formatWhen(ns?: number): string {
  if (!ns) return "";
  const d = new Date(ns / 1e6);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleDateString();
}
