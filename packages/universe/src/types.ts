// Wire + public types of @loomcycle/universe.
//
// GraphSnapshot mirrors the runtime's `Document op=graph_snapshot` answer (RFC
// FQ §2.2): column-oriented, parallel arrays, indexes into the same response.
// It is typed here rather than imported from @loomcycle/client so the package
// works against any client version that passes the op through.

export type DocScope = "agent" | "user" | "tenant";

/** RFC AS browse-by-subject override, threaded to every runtime call. */
export interface BrowseScope {
  scopeId?: string;
  tenant?: string;
}

export type SnapshotInclude = "documents" | "chunks" | "edges" | "dirents" | "facts" | "projection";

export interface GraphSnapshotInput {
  scope: DocScope;
  include?: SnapshotInclude[];
  under_path?: string;
  document_ids?: string[];
  chunk_ids?: string[];
  types?: string[];
  statuses?: string[];
  tag_prefix?: string;
  as_of?: number;
  page_documents?: number;
  /** The previous page's `page.next_cursor` (the op pages on `after`, as get_edges does). */
  after?: string;
  edge_budget?: number;
}

export interface SnapshotExternal {
  /** The negative index used in from_idx / to_idx. */
  idx: number;
  id: string;
  document_id: string;
}

export interface GraphSnapshot {
  page: { documents: number; next_cursor?: string; clamped_to?: number };
  codes: {
    types?: string[];
    statuses?: string[];
    kinds?: string[];
    fact_classes?: string[];
    methods?: string[];
  };
  documents?: {
    ids: string[];
    titles: string[];
    root_chunk_ids: string[];
    type_code: number[];
    status_code: number[];
    created_at?: number[];
    updated_at?: number[];
    color_enabled?: boolean[];
    color_scheme?: (Record<string, string> | null)[];
    paths?: string[][];
  };
  chunks?: {
    ids: string[];
    document_idx: number[];
    parent_idx: number[];
    position?: number[];
    type_code: number[];
    status_code: number[];
    titles: string[];
    valid_at?: (number | null)[];
    invalid_at?: (number | null)[];
    updated_at?: number[];
    is_fact?: boolean[];
    fact_class_code?: number[];
    fact_confidence?: (number | null)[];
    fact_retired?: boolean[];
  };
  edges?: {
    ids?: string[];
    from_idx: number[];
    to_idx: number[];
    kind_code: number[];
    auto?: boolean[];
    created_at?: number[];
    status_code?: number[];
    method_code?: number[];
    confidence?: (number | null)[];
    valid_at?: (number | null)[];
    invalid_at?: (number | null)[];
    external?: SnapshotExternal[];
    truncated?: boolean;
  };
  /** First page only; `document_ids` names a document on any page. */
  dirents?: { paths: string[]; kinds: string[]; document_idx: number[]; document_ids?: string[] };
  projection?: { model: string; basis_version: number; xyz: number[]; missing: number[] };
}

/** One hit of the semantic (Document `search`) mode. */
export interface SearchHit {
  chunk_id: string;
  score: number;
  title?: string;
  type?: string;
  document_id?: string;
}

/** What the navigator reports and is told: a document, optionally at a chunk. */
export interface UniverseSelection {
  documentId?: string;
  /** A chunk of the document; absent = the document itself (its root). */
  chunkId?: string;
  /** The document's Path name, when it has one. */
  path?: string;
}
