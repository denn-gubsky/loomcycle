import type { Connection } from "./createClient";
import type { BrowseScope, DocScope } from "../types";

// The member-safe document change feed (RFC FV): GET /v1/_document/feed. The
// map reads it itself over the host's connection (its fetch carries the
// cookie or the bearer), so it works with any @loomcycle/client, including
// those older than the client's own documentFeed.

export type FeedFrame =
  | { kind: "feed"; enabled: boolean }
  | { kind: "change"; type: string; documentId: string; chunkId: string }
  | { kind: "error"; code: string; message: string };

/** parseSSE splits an SSE byte stream into events: `event:` names it (default
 *  "message"), `data:` lines join with newlines, a blank line ends an event,
 *  `:` lines are comments (keepalives). */
export async function* parseSSE(body: ReadableStream<Uint8Array>): AsyncIterable<{ event: string; data: string }> {
  const reader = body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  let event = "message";
  let data: string[] = [];
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let nl: number;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl).replace(/\r$/, "");
        buf = buf.slice(nl + 1);
        if (line === "") {
          if (data.length) yield { event, data: data.join("\n") };
          event = "message";
          data = [];
        } else if (line.startsWith(":")) {
          // comment / keepalive
        } else if (line.startsWith("event:")) {
          event = line.slice(6).trim();
        } else if (line.startsWith("data:")) {
          data.push(line.slice(5).replace(/^ /, ""));
        }
      }
    }
  } finally {
    reader.releaseLock();
  }
}

/** toFeedFrame maps one SSE event of the reader feed to a frame, or null for
 *  what the map does not use. */
export function toFeedFrame(ev: { event: string; data: string }): FeedFrame | null {
  let p: Record<string, unknown>;
  try {
    p = JSON.parse(ev.data) as Record<string, unknown>;
  } catch {
    return null;
  }
  if (ev.event === "feed") return { kind: "feed", enabled: p.enabled === true };
  if (ev.event === "error")
    return { kind: "error", code: String(p.code ?? ""), message: String(p.error ?? p.message ?? "") };
  if (typeof p.document_id === "string" && p.document_id && typeof p.type === "string")
    return { kind: "change", type: p.type, documentId: p.document_id, chunkId: String(p.chunk_id ?? "") };
  return null;
}

/** feedOverConnection opens the reader feed with the host's own transport. */
export async function* feedOverConnection(
  conn: Connection,
  scope: DocScope,
  browse: BrowseScope | undefined,
  signal: AbortSignal,
): AsyncIterable<FeedFrame> {
  if (scope === "agent") return;
  const q = new URLSearchParams({ scope });
  if (browse?.scopeId) q.set("scope_id", browse.scopeId);
  if (browse?.tenant) q.set("tenant", browse.tenant);
  const headers: Record<string, string> = { Accept: "text/event-stream" };
  if (conn.token) headers.Authorization = `Bearer ${conn.token}`;
  const f = conn.fetch ?? fetch;
  const res = await f(`${conn.baseUrl.replace(/\/$/, "")}/v1/_document/feed?${q.toString()}`, { headers, signal });
  if (!res.ok || !res.body) {
    yield { kind: "error", code: `http_${res.status}`, message: res.statusText };
    return;
  }
  for await (const ev of parseSSE(res.body)) {
    const frame = toFeedFrame(ev);
    if (frame) yield frame;
  }
}
