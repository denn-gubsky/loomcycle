// The change-feed tail (RFC CF §9).
//
// `GET /v1/_memory/changes` and `/v1/_document/changes` are SSE feeds of
// VALUE-FREE change coordinates: each frame names what changed — scope, scope_id,
// key or chunk_id — and never the value. A reader that wants content pulls it
// through the data API. That is a privacy property, not an oversight, so nothing
// here invents a "value" column to fill.
//
// WHY THIS LIVES IN THE PACKAGE rather than behind a client method: consuming SSE
// needs the base URL and the fetch the host configured, which the `connection`
// data source has and a bare `client` does not. Wiring it here means the shipping
// console works today; the day @loomcycle/client grows a change-feed method, the
// data layer built from a client can implement the same optional hook and this
// file becomes its transport rather than the only one.

/** One change coordinate, exactly as the runtime's store.MemoryChange serialises. */
export interface MemoryChangeRow {
  seq: number;
  tenant?: string;
  type:
    | "memory.set"
    | "memory.delete"
    | "memory.scope_deleted"
    | "document.chunk.updated"
    | "document.chunk.deleted";
  scope: "agent" | "user" | "tenant";
  scope_id: string;
  key?: string;
  chunk_id?: string;
  at: string;
}

/** The opening frame: whether writes are actually being captured, and the cursor
 *  the server accepted. Both matter — see ChangeFeedPanel for what a missing
 *  `enabled: false` would cost a reader. */
export interface ChangeFeedStatus {
  enabled: boolean;
  since: number;
}

export type ChangeFeedFrame =
  | { kind: "status"; status: ChangeFeedStatus }
  | { kind: "change"; change: MemoryChangeRow };

/** Which endpoint to tail. The runtime splits the families across two routes, so a
 *  reader wanting both opens both. */
export type ChangeFamily = "memory" | "documents";

export const CHANGE_FEED_PATHS: Record<ChangeFamily, string> = {
  memory: "/v1/_memory/changes",
  documents: "/v1/_document/changes",
};

/** The env var an operator sets to turn the feed on. Named in the UI because
 *  "disabled" without the remedy is a dead end. */
export const CHANGE_FEED_ENV = "LOOMCYCLE_MEMORY_CHANGES_ENABLED";

// ---------------------------------------------------------------- frame parsing

/** classifyFrame turns one raw SSE frame into a typed frame, or null when it is
 *  neither (a keepalive comment, an unknown event name, unparseable JSON).
 *
 *  DISCRIMINATED ON THE EVENT NAME, not on the payload's shape. The runtime sends
 *  `event: feed` for the status and `event: change` for a coordinate, and the
 *  status payload deliberately carries no `type` field — so sniffing the body
 *  would have to guess, while the name is authoritative. */
export function classifyFrame(
  event: string,
  data: string,
): ChangeFeedFrame | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch {
    return null;
  }
  if (!parsed || typeof parsed !== "object") return null;
  if (event === "feed") {
    const p = parsed as Partial<ChangeFeedStatus>;
    // `enabled` is required. A status frame we cannot read must NOT default to
    // enabled: the whole point of the frame is to stop a disabled feed reading as
    // a quiet one, and a permissive default reinstates exactly that.
    if (typeof p.enabled !== "boolean") return null;
    return {
      kind: "status",
      status: { enabled: p.enabled, since: Number(p.since) || 0 },
    };
  }
  if (event === "change") {
    const p = parsed as Partial<MemoryChangeRow>;
    if (typeof p.type !== "string" || typeof p.seq !== "number") return null;
    return { kind: "change", change: p as MemoryChangeRow };
  }
  return null;
}

/** readSSE yields {event, data} pairs from a byte stream.
 *
 *  A local parser rather than the adapter's: @loomcycle/client does not export
 *  parseSSE, and its version coerces every frame into an AgentEvent — a shape a
 *  change coordinate is not. Comment lines (`: keepalive`) are skipped, which is
 *  what keeps a quiet feed from looking like a broken one. */
export async function* readSSE(
  stream: ReadableStream<Uint8Array>,
): AsyncIterable<{ event: string; data: string }> {
  const reader = stream.getReader();
  const decoder = new TextDecoder("utf-8");
  let buf = "";
  let event = "";
  let data = "";
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx: number;
      while ((idx = buf.indexOf("\n")) !== -1) {
        const line = buf.slice(0, idx).replace(/\r$/, "");
        buf = buf.slice(idx + 1);
        if (line === "") {
          if (event && data) yield { event, data };
          event = "";
          data = "";
          continue;
        }
        if (line.startsWith(":")) continue; // keepalive comment
        if (line.startsWith("event:")) event = line.slice(6).trim();
        else if (line.startsWith("data:")) data = line.slice(5).trim();
      }
    }
  } finally {
    reader.releaseLock();
  }
}

// ------------------------------------------------------------------ the buffer

/** A bounded, newest-first view of the rows seen, with the count DROPPED because
 *  of the bound.
 *
 *  `seen` and `dropped` are tracked rather than derived from `rows.length` on
 *  purpose: a tail that silently forgets is a tail that lies about how much
 *  happened, and "showing the latest 200" reads as "200 things happened" unless
 *  the total is beside it. */
export interface ChangeBuffer {
  rows: MemoryChangeRow[];
  seen: number;
  dropped: number;
}

export const emptyBuffer: ChangeBuffer = { rows: [], seen: 0, dropped: 0 };

/** How many coordinates the panel retains. The feed is a firehose on a busy
 *  store — one consolidation pass writes a row per fact plus one per chunk — and
 *  an unbounded list is a memory leak with a scrollbar. */
export const CHANGE_BUFFER_CAP = 200;

/** appendChange adds one row, newest first, dropping the oldest past the cap.
 *
 *  DEDUPED ON seq, because a reconnect resumes from the last cursor and the
 *  server is inclusive at the boundary in the case where the cursor was never
 *  advanced — a duplicate row would otherwise inflate `seen` and make a quiet
 *  feed look busy. */
export function appendChange(
  buf: ChangeBuffer,
  row: MemoryChangeRow,
  cap = CHANGE_BUFFER_CAP,
): ChangeBuffer {
  if (buf.rows.some((r) => r.seq === row.seq && r.type === row.type))
    return buf;
  const rows = [row, ...buf.rows];
  const dropped = buf.dropped + Math.max(0, rows.length - cap);
  return { rows: rows.slice(0, cap), seen: buf.seen + 1, dropped };
}

// ------------------------------------------------------------------ filtering

export interface ChangeFilter {
  /** "" = every family. */
  family?: ChangeFamily | "";
  /** "" = every scope. */
  scope?: MemoryChangeRow["scope"] | "";
  /** Substring match on scope_id; "" = every scope_id. Substring rather than exact
   *  because an operator watching one user does not want to type an id exactly, and
   *  the feed is tenant-wide so SOME narrowing is the difference between readable
   *  and not. */
  scopeId?: string;
  /** "" = every type. */
  type?: MemoryChangeRow["type"] | "";
}

export function isDocumentChange(t: MemoryChangeRow["type"]): boolean {
  return t === "document.chunk.updated" || t === "document.chunk.deleted";
}

export function matchesFilter(row: MemoryChangeRow, f: ChangeFilter): boolean {
  if (f.family === "memory" && isDocumentChange(row.type)) return false;
  if (f.family === "documents" && !isDocumentChange(row.type)) return false;
  if (f.scope && row.scope !== f.scope) return false;
  if (f.type && row.type !== f.type) return false;
  if (
    f.scopeId &&
    !row.scope_id.toLowerCase().includes(f.scopeId.toLowerCase())
  )
    return false;
  return true;
}

/** describeChange renders the coordinate a row names — the key for a memory write,
 *  the chunk for a document one. Never a value: the feed does not carry one, and a
 *  UI that showed an empty "value" column would imply it should. */
export function describeChange(row: MemoryChangeRow): string {
  if (row.key) return row.key;
  if (row.chunk_id) return `chunk ${row.chunk_id.slice(0, 12)}`;
  if (row.type === "memory.scope_deleted") return "(whole scope)";
  return "—";
}

// ------------------------------------------------------------------ transport

/** The subset of Connection this transport needs. Declared locally so the tail can
 *  be handed a plain object in a test without constructing a client. */
export interface ChangeFeedConnection {
  baseUrl: string;
  token?: string;
  fetch?: typeof fetch;
}

/** tailChanges opens one family's feed and yields typed frames until the signal
 *  aborts or the server closes.
 *
 *  Accepts BOTH media types, like every other SSE call in this codebase: the
 *  success path is text/event-stream and the ERROR path is application/json, and a
 *  strict proxy in front of the runtime 406s a request that only advertises one.
 *  (Same reason as the MCP HTTP-transport hardening note in CLAUDE.md.) */
export async function* tailChanges(
  conn: ChangeFeedConnection,
  family: ChangeFamily,
  opts: { since?: number; signal?: AbortSignal } = {},
): AsyncIterable<ChangeFeedFrame> {
  const headers: Record<string, string> = {
    Accept: "text/event-stream, application/json",
  };
  if (conn.token) headers.Authorization = `Bearer ${conn.token}`;
  const qs = opts.since
    ? `?since=${encodeURIComponent(String(opts.since))}`
    : "";
  const doFetch =
    conn.fetch ?? ((i: RequestInfo | URL, x?: RequestInit) => fetch(i, x));
  const resp = await doFetch(conn.baseUrl + CHANGE_FEED_PATHS[family] + qs, {
    method: "GET",
    headers,
    signal: opts.signal,
  });
  if (!resp.ok) {
    // The body is the runtime's typed error text; surfaced verbatim because the
    // panel shows it, and "403" alone does not tell an operator that the feed needs
    // a tenant-scoped token.
    throw new Error(
      `change feed ${resp.status}: ${(await resp.text()).slice(0, 300)}`,
    );
  }
  if (!resp.body) throw new Error("change feed: response has no body");
  for await (const { event, data } of readSSE(resp.body)) {
    const frame = classifyFrame(event, data);
    if (frame) yield frame;
  }
}
