/**
 * Tests for src/stream.ts:parseSSE — the SSE frame parser shared
 * by runStreaming + continueSession.
 *
 * Drives synthetic chunked byte input through the parser and
 * asserts the right AgentEvents come out in order.
 */

import { describe, expect, it } from "vitest";
import { parseSSE } from "../src/stream.js";

/** streamFromChunks builds a ReadableStream that emits each
 *  string as a separate chunk. Lets us test chunk-boundary
 *  handling in parseSSE (the parser must tolerate splits
 *  mid-line, mid-frame, etc.). */
function streamFromChunks(chunks: string[]): ReadableStreamDefaultReader<Uint8Array> {
  const encoder = new TextEncoder();
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const c of chunks) controller.enqueue(encoder.encode(c));
      controller.close();
    },
  });
  return stream.getReader();
}

describe("parseSSE", () => {
  it("parses 3 well-formed frames into 3 events in order", async () => {
    const reader = streamFromChunks([
      'event: started\ndata: {"type":"started"}\n\n',
      'event: text\ndata: {"type":"text","text":"hi"}\n\n',
      'event: done\ndata: {"type":"done","stop_reason":"end_turn"}\n\n',
    ]);
    const out = [];
    for await (const ev of parseSSE(reader)) out.push(ev);
    expect(out.length).toBe(3);
    expect(out[0]!.type).toBe("started");
    expect(out[1]!.text).toBe("hi");
    expect(out[2]!.stop_reason).toBe("end_turn");
  });

  it("handles chunk boundaries mid-frame", async () => {
    // Same 1 frame split across many tiny chunks.
    const reader = streamFromChunks([
      "event: tex",
      "t\ndata: {",
      '"type":"text"',
      ',"text":"hello"',
      "}\n\n",
    ]);
    const out = [];
    for await (const ev of parseSSE(reader)) out.push(ev);
    expect(out.length).toBe(1);
    expect(out[0]!.text).toBe("hello");
  });

  it("handles \\r\\n line endings", async () => {
    const reader = streamFromChunks([
      'event: text\r\ndata: {"type":"text","text":"crlf"}\r\n\r\n',
    ]);
    const out = [];
    for await (const ev of parseSSE(reader)) out.push(ev);
    expect(out.length).toBe(1);
    expect(out[0]!.text).toBe("crlf");
  });

  it("drops frames with malformed JSON without throwing", async () => {
    const reader = streamFromChunks([
      'event: bad\ndata: { not json\n\n',
      'event: text\ndata: {"type":"text","text":"ok"}\n\n',
    ]);
    const out = [];
    for await (const ev of parseSSE(reader)) out.push(ev);
    expect(out.length).toBe(1);
    expect(out[0]!.text).toBe("ok");
  });

  it("emits the trailing frame even without a final blank line", async () => {
    const reader = streamFromChunks([
      'event: text\ndata: {"type":"text","text":"last"}\n',
    ]);
    const out = [];
    for await (const ev of parseSSE(reader)) out.push(ev);
    expect(out.length).toBe(1);
    expect(out[0]!.text).toBe("last");
  });

  it("ignores frames with only event: and no data:", async () => {
    const reader = streamFromChunks([
      "event: heartbeat\n\n",
      'event: text\ndata: {"type":"text","text":"after-heartbeat"}\n\n',
    ]);
    const out = [];
    for await (const ev of parseSSE(reader)) out.push(ev);
    expect(out.length).toBe(1);
    expect(out[0]!.text).toBe("after-heartbeat");
  });

  it("drains a trailing line with no final newline", async () => {
    // Connection drop mid-frame: the last line never received its `\n`.
    // The pre-fix parser silently lost this frame; the drain-on-eof path
    // recovers it.
    const reader = streamFromChunks([
      'event: text\ndata: {"type":"text","text":"truncated"}',
    ]);
    const out = [];
    for await (const ev of parseSSE(reader)) out.push(ev);
    expect(out.length).toBe(1);
    expect(out[0]!.text).toBe("truncated");
  });

  it("backfills `type` from SSE event-name when JSON payload omits it", async () => {
    // The v0.4 `event: agent` side-channel uses sse.sendRaw — the JSON
    // payload carries only {agent_id, run_id, session_id, parent_agent_id}.
    // Consumers switch on ev.type, so the parser backfills `type` from
    // the SSE event-name so these frames don't slip past.
    const reader = streamFromChunks([
      'event: agent\ndata: {"agent_id":"a-1","run_id":"r-1","session_id":"s-1","parent_agent_id":null}\n\n',
    ]);
    const out = [];
    for await (const ev of parseSSE(reader)) out.push(ev);
    expect(out.length).toBe(1);
    expect(out[0]!.type).toBe("agent");
    expect(out[0]!.agent_id).toBe("a-1");
    expect(out[0]!.parent_agent_id).toBeNull();
  });
});
