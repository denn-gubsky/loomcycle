import { createContext, useContext, type ReactNode } from "react";
import type { LoomcycleClient, DocumentToolInput } from "@loomcycle/client";
import type {
  BrowseScope,
  ChunkDetail,
  ChunkRow,
  DocEdge,
  DocScope,
  PathEntry,
  PathScope,
} from "../types";
import type { DocSummary, DocumentMeta } from "./colorScheme";
import type { AssetFetch } from "./createClient";

// ChunkPatch is the subset of chunk fields update_chunk accepts. `fields` is
// unknown (arbitrary typed JSON the chunk carries); a blank/absent value leaves
// the existing fields untouched (the caller only sends what changed).
export interface ChunkPatch {
  body?: string;
  fields?: unknown;
  title?: string;
  status?: string;
  type?: string;
}

// ExplorerDataLayer is the narrow data contract the Path / Document components
// need — the ten off-run reads/writes the browser makes. Decoupling behind this
// interface lets a host inject the default client-backed implementation
// (dataLayerFromClient), or a custom one (e.g. a cookie-authed same-origin
// fetcher) without the components importing any global api module. Every method
// takes an optional `browse` (RFC AS browse-by-subject) threaded to the runtime.
export interface ExplorerDataLayer {
  pathLs(
    path: string,
    scope: PathScope,
    recursive: boolean,
    browse?: BrowseScope,
  ): Promise<{ path: string; entries: PathEntry[] }>;
  pathMkdir(
    path: string,
    scope: PathScope,
    browse?: BrowseScope,
  ): Promise<unknown>;
  pathMv(
    from: string,
    to: string,
    scope: PathScope,
    browse?: BrowseScope,
  ): Promise<unknown>;
  pathRm(
    path: string,
    scope: PathScope,
    recursive: boolean,
    browse?: BrowseScope,
  ): Promise<unknown>;
  documentCreate(
    title: string,
    path: string,
    scope: DocScope,
    browse?: BrowseScope,
  ): Promise<{
    document_id: string;
    root_chunk_id?: string;
    title?: string;
    path?: string;
  }>;
  documentDelete(
    id: string,
    scope: DocScope,
    browse?: BrowseScope,
  ): Promise<unknown>;
  documentGet(
    id: string,
    scope: DocScope,
    browse?: BrowseScope,
  ): Promise<DocumentMeta>;
  // documentsSummary returns per-document display metadata (type/status + RFC BN
  // color settings) for a set of ids and/or a Path subtree — the Path tree's
  // one-call coloring source (dirents carry only a document_id).
  documentsSummary(
    opts: { documentIds?: string[]; underPath?: string },
    scope: DocScope,
    browse?: BrowseScope,
  ): Promise<{ documents: DocSummary[] }>;
  documentQueryChunks(
    documentId: string,
    scope: DocScope,
    browse?: BrowseScope,
  ): Promise<{ chunks: ChunkRow[] }>;
  documentGetChunk(
    id: string,
    scope: DocScope,
    browse?: BrowseScope,
  ): Promise<ChunkDetail>;
  // documentGetEdges returns every cross-reference edge touching a document
  // (RFC BN P4) — the References list + relationship graph source.
  documentGetEdges(
    documentId: string,
    scope: DocScope,
    browse?: BrowseScope,
  ): Promise<{ edges: DocEdge[] }>;
  // documentAssetObjectUrl fetches an image chunk's bytes (RFC BO) WITH auth and
  // returns a blob object-URL for an <img src> — a bare <img src=/v1/...> can't
  // carry a bearer header. The CALLER must URL.revokeObjectURL when done. Absent
  // when the data source can't do a raw binary GET (a bare client with no
  // connection) → the viewer falls back to a placeholder.
  documentAssetObjectUrl?(
    chunkId: string,
    scope: DocScope,
    browse?: BrowseScope,
  ): Promise<string>;
  documentUpdateChunk(
    id: string,
    revision: number,
    patch: ChunkPatch,
    scope: DocScope,
    browse?: BrowseScope,
  ): Promise<ChunkDetail>;
  documentExportMd(
    documentId: string,
    scope: DocScope,
    includeMetadata: boolean,
    browse?: BrowseScope,
  ): Promise<{ markdown: string; title: string; document_id: string }>;
}

// dataLayerFromClient maps a @loomcycle/client instance onto the
// ExplorerDataLayer. Path ops route to client.path (POST /v1/_path); document
// ops to client.document (POST /v1/_document) — both op-discriminated.
//
// browse is passed straight through as the client's opts second argument: a
// BrowseScope ({scopeId?, tenant?}) is structurally the client's browse opts, so
// the SDK serializes it to ?scope_id= / ?tenant= (empty/undefined → the caller's
// own subject, byte-identical to the pre-RFC-AS behaviour). Responses are
// op-varying `unknown`, cast to the kept shapes the components consume.
export function dataLayerFromClient(client: LoomcycleClient, assetFetch?: AssetFetch): ExplorerDataLayer {
  return {
    pathLs: (path, scope, recursive, browse) =>
      client.path(
        { op: "ls", path, scope, recursive },
        browse,
      ) as Promise<{ path: string; entries: PathEntry[] }>,
    pathMkdir: (path, scope, browse) =>
      client.path({ op: "mkdir", path, scope }, browse),
    pathMv: (from, to, scope, browse) =>
      client.path({ op: "mv", path: from, to, scope }, browse),
    pathRm: (path, scope, recursive, browse) =>
      client.path({ op: "rm", path, scope, recursive }, browse),
    documentCreate: (title, path, scope, browse) =>
      client.document(
        { op: "create_document", title, path, scope },
        browse,
      ) as Promise<{
        document_id: string;
        root_chunk_id?: string;
        title?: string;
        path?: string;
      }>,
    documentDelete: (id, scope, browse) =>
      client.document({ op: "delete_document", id, scope }, browse),
    documentGet: (id, scope, browse) =>
      client.document({ op: "get_document", id, scope }, browse) as Promise<DocumentMeta>,
    documentsSummary: (opts, scope, browse) =>
      client.document(
        // documents_summary + document_ids are RFC BN wire additions the pinned
        // SDK type doesn't enumerate; the /v1/_document passthrough accepts them
        // verbatim, so cast past DocumentToolInput (same pattern as update_chunk).
        {
          op: "documents_summary",
          scope,
          ...(opts.documentIds ? { document_ids: opts.documentIds } : {}),
          ...(opts.underPath ? { under_path: opts.underPath } : {}),
        } as unknown as DocumentToolInput,
        browse,
      ) as Promise<{ documents: DocSummary[] }>,
    documentQueryChunks: (documentId, scope, browse) =>
      client.document(
        { op: "query_chunks", document_id: documentId, scope, limit: 1000 },
        browse,
      ) as Promise<{ chunks: ChunkRow[] }>,
    documentGetChunk: (id, scope, browse) =>
      client.document({ op: "get_chunk", id, scope }, browse) as Promise<ChunkDetail>,
    documentGetEdges: (documentId, scope, browse) =>
      client.document(
        // get_edges is an RFC BN P4 wire op the pinned SDK type doesn't
        // enumerate; the /v1/_document passthrough accepts it verbatim.
        { op: "get_edges", document_id: documentId, scope } as unknown as DocumentToolInput,
        browse,
      ) as Promise<{ edges: DocEdge[] }>,
    documentUpdateChunk: (id, revision, patch, scope, browse) =>
      client.document(
        // `patch.fields` is unknown but DocumentToolInput.fields is a typed
        // Record; the cast reconciles the spread (the server accepts arbitrary
        // JSON there). Op order + arg names mirror the wire contract exactly.
        {
          op: "update_chunk",
          id,
          revision,
          scope,
          ...patch,
        } as DocumentToolInput,
        browse,
      ) as Promise<ChunkDetail>,
    documentExportMd: (documentId, scope, includeMetadata, browse) =>
      client.document(
        {
          op: "export_md",
          document_id: documentId,
          scope,
          include_metadata: includeMetadata,
        },
        browse,
      ) as Promise<{ markdown: string; title: string; document_id: string }>,
    // Present only when an AssetFetch was supplied (the connection path); a bare
    // client can't do the raw binary GET, so the op is omitted and the viewer
    // shows a placeholder.
    ...(assetFetch
      ? {
          documentAssetObjectUrl: async (chunkId, scope, browse) => {
            const q = new URLSearchParams({ scope });
            if (browse?.scopeId) q.set("scope_id", browse.scopeId);
            if (browse?.tenant) q.set("tenant", browse.tenant);
            const resp = await assetFetch(
              `/v1/_document/asset/${encodeURIComponent(chunkId)}?${q.toString()}`,
            );
            if (!resp.ok) throw new Error(`asset fetch failed: ${resp.status}`);
            const blob = await resp.blob();
            return URL.createObjectURL(blob);
          },
        }
      : {}),
  };
}

// The data layer reaches the components through context — no module-global
// singleton. The root (<PathExplorer> / <DocumentViewer>) builds it once
// (useMemo over connection identity) and provides it; nested panels/modals read
// it via useExplorerData().
const ExplorerDataContext = createContext<ExplorerDataLayer | null>(null);

export function ExplorerDataProvider({
  value,
  children,
}: {
  value: ExplorerDataLayer;
  children: ReactNode;
}) {
  return (
    <ExplorerDataContext.Provider value={value}>
      {children}
    </ExplorerDataContext.Provider>
  );
}

export function useExplorerData(): ExplorerDataLayer {
  const v = useContext(ExplorerDataContext);
  if (!v) {
    throw new Error(
      "useExplorerData must be used within <PathExplorer> or <DocumentViewer> (no ExplorerDataLayer in context)",
    );
  }
  return v;
}
