import type { AgentEvent } from "./types.js";

/**
 * parseSSE turns a chunked byte stream into typed AgentEvents.
 *
 * SSE framing (subset): "event: <name>\ndata: <json>\n\n". We only emit a
 * frame when both event + data have been seen since the last blank line.
 *
 * Used by `runStreaming` and `continueSession` — both POST endpoints
 * return the same SSE wire shape and the parser doesn't differentiate.
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
  // Trailing frame without a final blank line.
  const ev = flush();
  if (ev) yield ev;
}
