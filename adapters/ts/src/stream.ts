import type { AgentEvent, EventType } from "./types.js";

/**
 * parseSSE turns a chunked byte stream into typed AgentEvents.
 *
 * SSE framing (subset): "event: <name>\ndata: <json>\n\n". We only emit a
 * frame when both event + data have been seen since the last blank line.
 *
 * Used by `runStreaming` and `continueSession` — both POST endpoints
 * return the same SSE wire shape and the parser doesn't differentiate.
 *
 * Side-channel frames: the v0.4 `event: agent` SSE frame (and any future
 * sse.sendRaw user) emits a JSON payload that does NOT carry the `type`
 * field — the SSE event name is the only discriminator. parseSSE backfills
 * `type` from the event name in that case so consumers see a well-formed
 * AgentEvent and switch on `ev.type` uniformly.
 */
export async function* parseSSE(
  reader: ReadableStreamDefaultReader<Uint8Array>,
): AsyncIterable<AgentEvent> {
  const decoder = new TextDecoder("utf-8");
  let buf = "";
  let event = "";
  let data = "";

  const flush = (): AgentEvent | null => {
    if (!event && !data) return null;
    if (!data) {
      event = "";
      return null;
    }
    try {
      const parsed = JSON.parse(data) as AgentEvent;
      // Side-channel sendRaw frames omit `type` in the JSON payload — the
      // SSE event name is the only discriminator. Backfill it so the
      // consumer's switch on ev.type doesn't miss these.
      if (!parsed.type && event) {
        parsed.type = event as EventType;
      }
      event = "";
      data = "";
      return parsed;
    } catch {
      event = "";
      data = "";
      return null;
    }
  };

  while (true) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });

    let idx;
    while ((idx = buf.indexOf("\n")) !== -1) {
      const line = buf.slice(0, idx).replace(/\r$/, "");
      buf = buf.slice(idx + 1);

      if (line === "") {
        const ev = flush();
        if (ev) yield ev;
        continue;
      }
      if (line.startsWith("event:")) event = line.slice("event:".length).trim();
      else if (line.startsWith("data:")) data = line.slice("data:".length).trim();
    }
  }
  // Stream ended. Drain any unterminated final line still in `buf` — a
  // connection drop can land here mid-frame, and without this step the
  // last frame whose `\n` never arrived would be silently lost. Then
  // flush any pending event + data.
  if (buf.length > 0) {
    const line = buf.replace(/\r$/, "");
    if (line.startsWith("event:")) event = line.slice("event:".length).trim();
    else if (line.startsWith("data:")) data = line.slice("data:".length).trim();
  }
  const ev = flush();
  if (ev) yield ev;
}
