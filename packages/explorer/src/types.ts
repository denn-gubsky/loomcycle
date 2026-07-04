// Public data types for @loomcycle/explorer. These are the shapes the Path /
// Document components render and the data layer produces. They mirror the
// loomcycle Web UI's api.ts (the same off-run /v1/_path + /v1/_document endpoint
// JSON) but live here so the package has no dependency on the app's global api
// module — the runtime is reached through an injected LoomcycleClient /
// ExplorerDataLayer instead (see lib/dataLayer).

// PathScope selects WHICH dirent tree to browse. It is a subtree SELECTOR, not
// an authority grant — the runtime resolves the caller's tenant + subject from
// the authenticated principal and re-authorizes. Documents are agent|user only
// (tenant is refused), hence the narrower DocScope.
export type PathScope = "agent" | "user" | "tenant";
export type DocScope = "agent" | "user";

// BrowseScope is the RFC AS off-run browse-by-subject override: which subject's
// tree (scopeId → ?scope_id=) and, for an admin, which tenant (tenant →
// ?tenant=). CALLER-AUTHORITATIVE only in that the SERVER re-checks it — a
// substrate:tenant principal's tenant is ignored (confined to its own tenant);
// scopeId picks any subject it may see. Unset fields are omitted, so the server
// falls back to the caller's own subject.
export interface BrowseScope {
  scopeId?: string;
  tenant?: string;
}

// PathEntry is one dirent in an `ls` listing.
export interface PathEntry {
  name: string;
  kind: string; // directory | document | volume_mount | memory_entry
  full_path: string;
  resource_ref?: unknown;
}

// ChunkRow is the structural record returned by query_chunks (no body — kept
// light; fetch the body with ExplorerDataLayer.documentGetChunk).
export interface ChunkRow {
  id: string;
  document_id: string;
  parent_id?: string;
  position: number;
  title: string;
  type?: string;
  status?: string;
  revision: number;
}

// ChunkDetail adds the Markdown body + typed fields (from get_chunk).
export interface ChunkDetail extends ChunkRow {
  body: string;
  fields?: unknown;
}

// Principal is the authenticated identity behind the session (resolved by the
// host, e.g. via GET /v1/_me). The explorer only reads `subject` — it is passed
// into the renderAssistant slot's context so a host-provided Document Assistant
// can scope its agent run to the same subject the viewer shows. Kept minimal on
// purpose; a host may pass its full whoami result (extra fields are ignored).
export interface Principal {
  subject: string;
}

// AssistantContext is what the DocumentViewer's optional renderAssistant slot
// receives. The package deliberately does NOT bundle the run-stream machinery
// (LiveRunPane / useRunStream) — a host wires its own assistant panel and reads
// this context to target the right document + subject.
export interface AssistantContext {
  documentId: string;
  scope: string;
  subject?: string;
  // onChanged lets an injected assistant refresh the viewer after it mutates the
  // document (e.g. at each turn boundary) — it calls the viewer's own reload, so
  // the tree + selected chunk re-fetch. Without it the viewer would show stale
  // content until a manual reload.
  onChanged?: () => void;
}
