// Public data types for @loomcycle/loomboard. The @loomcycle/client Document tool
// is typed `DocumentToolResponse = unknown`, so the response shapes below are the
// package's own contract, transcribed from the runtime's document.go responses
// (query_documents / query_chunks / get_chunk / create_document). A saved view is
// itself a `type=view` Document, dogfooding the primitive it renders (RFC BT).

import type { Connection } from "./lib/createClient";

export type { Connection };

/** Which Document store a board reads / writes. "agent" is off-run
 *  operator-private and not a board scope; boards use "user" (personal) or
 *  "tenant" (shared across the tenant — RFC CB lets a non-isolated member reach
 *  it). */
export type BoardScope = "user" | "tenant";

/** The filter half of a saved view — the RFC BS queryable axes ONLY (type /
 *  status / tags / path). `fields.*` is deliberately absent: it lives in the
 *  Memory k/v blob and is not SQL-queryable, so filtering by it is a future core
 *  change (RFC BS §7 promote-a-field), not a P1 workaround. */
export interface ViewQuery {
  /** Board granularity: whole Documents, or the chunks within a document / path. */
  source: "documents" | "chunks";
  /** chunks source: restrict to one document's chunks. */
  documentId?: string;
  /** Path-tree subtree to query at / under. */
  underPath?: string;
  /** Single-valued indexed axes (`type` is subtype-expanded server-side). */
  type?: string;
  status?: string;
  /** Multi-valued tag facet: exact membership, or a slash-nested prefix. */
  tag?: string;
  tagPrefix?: string;
  /** Row cap. The server clamps to 100 (default) / 1000 (max); there is no
   *  pagination, so a big board must narrow by type / status / tag / path. */
  limit?: number;
}

export type LayoutKind = "table" | "cards" | "kanban" | "list";

/** The single-valued axis a kanban buckets by. Tags are multi-valued (a row
 *  lands in several columns at once), so tag-grouping is a later add. */
export type GroupAxis = "status" | "type";

export type SortField = "title" | "updated" | "position";

/** The presentation half of a saved view. */
export interface ViewLayout {
  kind: LayoutKind;
  /** kanban: the column axis (default "status"). */
  groupBy?: GroupAxis;
  sortBy?: SortField;
  sortDir?: "asc" | "desc";
  /** table: extra columns beyond the title, drawn from the row axes. */
  columns?: Array<"type" | "status" | "updated">;
}

/** A saved view: a `type=view` Document whose `{query, layout}` live in its root
 *  chunk's `fields`. */
export interface SavedView {
  documentId: string;
  rootChunkId: string;
  title: string;
  query: ViewQuery;
  layout: ViewLayout;
  /** The root chunk's revision — passed back to updateView for optimistic
   *  concurrency when editing. */
  revision?: number;
  /** unix nanoseconds. */
  updatedAt?: number;
}

/** The normalized row the layouts render — unifies a document row and a chunk
 *  row so one set of renderers serves both board granularities. */
export interface BoardRow {
  /** The chunk id, or (documents board) the document_id. */
  id: string;
  documentId: string;
  title: string;
  type?: string;
  status?: string;
  /** unix nanoseconds (÷1e6 for a JS Date). */
  updatedAt?: number;
  position?: number;
}

// ---- Wire response shapes (document.go; DocumentToolResponse is `unknown`) ----

export interface DocRow {
  document_id: string;
  title: string;
  root_chunk_id: string;
  type?: string;
  status?: string;
  created_at?: number;
  updated_at?: number;
}
export interface QueryDocumentsResponse {
  documents: DocRow[];
}

export interface ChunkRow {
  id: string;
  document_id: string;
  title: string;
  position: number;
  revision: number;
  parent_id?: string;
  type?: string;
  status?: string;
}
export interface QueryChunksResponse {
  chunks: ChunkRow[];
  /** Present when a `type` filter widened to include subtypes. */
  type_expanded_to?: string[];
}

/** The full chunk row — the only shape carrying `fields` / `tags` / `body`. */
export interface ChunkDetail {
  id: string;
  document_id: string;
  title: string;
  body: string;
  revision: number;
  position: number;
  parent_id?: string;
  type?: string;
  status?: string;
  fields?: Record<string, unknown>;
  tags?: string[];
}

export interface TypeRow {
  name: string;
  document_id: string;
  fields?: unknown;
}
export interface ListTypesResponse {
  types: TypeRow[];
}

export interface CreateDocumentResponse {
  document_id: string;
  root_chunk_id: string;
  title: string;
  path?: string;
  path_warning?: string;
}
