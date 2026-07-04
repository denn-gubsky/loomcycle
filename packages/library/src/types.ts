// Public data types for @loomcycle/library. These are the shapes the Library
// components render and the data layer produces. They mirror the loomcycle Web
// UI's api.ts (the same endpoint JSON) but live here so the package has no
// dependency on the app's global api module — the runtime is reached through an
// injected LoomcycleClient / LibraryDataLayer instead (see lib/dataLayer).

// SubstrateKind is the op-discriminated substrate family. The Library only
// drives agentdef / skilldef / mcpserverdef, but the full union is kept so
// LineagePanel's exhaustive `newCtaLabel` switch (and any host that reuses
// these types) stays complete.
export type SubstrateKind =
  | "agentdef"
  | "skilldef"
  | "mcpserverdef"
  | "webhookdef"
  | "a2aservercarddef"
  | "a2aagentdef"
  | "memorybackenddef"
  | "volumedef";

// DefRow is one version of a substrate definition (a node in the lineage tree).
// `definition` is the op-varying overlay body — the kind-specific renderers cast
// it to their expected shape.
export interface DefRow {
  def_id: string;
  name: string;
  version: number;
  parent_def_id?: string;
  description?: string;
  retired?: boolean;
  bootstrapped_from_static?: boolean;
  content_sha256?: string;
  created_at: string;
  created_by_agent_id?: string;
  created_by_run_id?: string;
  definition?: unknown;
}

// DefListByNameResponse is the `{op:"list", name}` response — every version of
// one declared name.
export interface DefListByNameResponse {
  name: string;
  versions: DefRow[];
}

// LibraryEntry is one row of the /v1/_library/* endpoints — a unified view that
// merges the static (yaml) side with the substrate (runtime) side.
export interface LibraryEntry {
  name: string;
  source: "static-only" | "dynamic-only" | "both";
  in_static: boolean;
  in_substrate: boolean;
  version_count: number;
  active_def_id?: string;
  latest_version?: number;
  last_updated?: string;
  /** Agents only (soft reclaim): live (non-retired) version count, and whether
   *  the active pointer references a retired row. Absent for skills + mcp. */
  live_version_count?: number;
  active_retired?: boolean;
  /** Static-side definition payload (same JSON shape as the substrate body).
   *  Omitted when in_static is false. */
  static_definition?: unknown;
}

export interface LibraryListResponse {
  entries: LibraryEntry[];
}

// ServerCapabilities is the non-secret, booleans-only runtime posture the UI
// uses to gate affordances (RFC AU). NEVER carries allowlist contents or any
// secret. Absent fields → treat as false (safe default).
export interface ServerCapabilities {
  /** LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO — gates the stdio MCP import path. */
  mcp_allow_dynamic_stdio?: boolean;
  /** Whether any http host allowlist is configured (presence only). */
  http_host_allowlist_configured?: boolean;
}

// Principal is the authenticated identity behind the session (resolved by the
// host, e.g. via GET /v1/_me). The Library only reads `is_admin` (to decide
// tenant-vs-admin posture when `tenantScope` isn't given explicitly); the other
// fields are kept for parity so a host can pass its whoami result straight in.
export interface Principal {
  tenant_id: string;
  subject: string;
  scopes: string[];
  is_admin: boolean;
  legacy: boolean;
  open_mode?: boolean;
  capabilities?: ServerCapabilities;
}
