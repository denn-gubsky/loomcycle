// adapters/ts/tests/transcription.test.ts — an OCR or speech-to-text pass a
// File tool call made is reported on the run's stream with what it spent.
// Typed end to end: `npm run typecheck:tests` fails if the union cannot name
// the event or AgentEvent cannot carry its payload.

import { describe, expect, it } from "vitest";

import { makeClient, sseResponse } from "./helpers.js";
import type { AgentEvent, EventType, TranscriptionInfo } from "../src/index.js";

describe("transcription on a run's stream", () => {
  it("is delivered with the pass's source, engine, units and cost", async () => {
    const ocr: TranscriptionInfo = {
      ref: "volume:inbox:documents/scan.pdf",
      out: "volume:inbox:documents/scan.ocr.md",
      purpose: "ocr",
      engine: "vision",
      provider: "anthropic",
      model: "claude-haiku-5-5",
      pages: 12,
      input_tokens: 9000,
      output_tokens: 4000,
      cost: 0.04,
      currency: "USD",
      status: "ok",
      run_id: "r_1",
    };
    const stt: TranscriptionInfo = {
      ref: "volume:inbox:call.mp4",
      purpose: "stt",
      engine: "command",
      seconds: 2550,
      status: "partial",
      dry_run: true,
    };
    const { client } = makeClient([
      sseResponse([
        'event: agent\ndata: {"agent_id":"a_1","run_id":"r_1","session_id":"s1"}\n\n',
        `event: transcription\ndata: ${JSON.stringify({ type: "transcription", transcription: ocr })}\n\n`,
        `event: transcription\ndata: ${JSON.stringify({ type: "transcription", transcription: stt })}\n\n`,
      ]),
    ]);
    const seen: AgentEvent[] = [];
    for await (const ev of client.streamRunByID("r_1")) {
      seen.push(ev);
    }

    const name: EventType = "transcription";
    const got: (TranscriptionInfo | undefined)[] = seen.filter((ev) => ev.type === name).map((ev) => ev.transcription);
    expect(got).toEqual([ocr, stt]);
    // An unpriced pass carries no cost at all, not a zero.
    expect(got[1]?.cost).toBeUndefined();
  });
});
