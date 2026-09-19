/**
 * `context` on the typed client.
 *
 * Before this the field did not exist on RunOptions AND runBody assembled the
 * wire body from a strict allow-list — so the key was dropped even by a caller
 * who cast through `as any` to force it past the type checker. That combination
 * made A/B-ing distillation modes impossible from any typed consumer, which is
 * why these tests assert the BODY rather than the types.
 */

import { describe, it, expect } from "vitest";
import { makeClient, sseResponse } from "./helpers.js";

const done = () => sseResponse(['event: done\ndata: {"type":"done","stop_reason":"end_turn"}\n\n']);
const seg = [{ role: "user" as const, content: [{ type: "trusted-text" as const, text: "hi" }] }];

describe("context options", () => {
  it("runStreaming maps every context field to its snake_case wire key", async () => {
    const { client, fetchMock } = makeClient([done()]);
    for await (const _ of client.runStreaming({
      agent: "qa",
      segments: seg,
      context: {
        mode: "recap",
        keepLastN: 2,
        reasoning: "recap",
        recapMaxChars: 4096,
        autorecapAtPct: 60,
        stateSchema: { type: "object" },
        onInvalidPatch: "fail",
        maxPatchRetries: 1,
        recall: true,
        harvestToMemory: true,
      },
    })) {
      void _;
    }
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body.context).toEqual({
      mode: "recap",
      keep_last_n: 2,
      reasoning: "recap",
      recap_max_chars: 4096,
      // Not autocompact_at_pct: the context block spells this one without the
      // underscore after "auto", unlike compaction's. Getting it wrong means
      // the server silently ignores the threshold.
      autorecap_at_pct: 60,
      state_schema: { type: "object" },
      on_invalid_patch: "fail",
      max_patch_retries: 1,
      recall: true,
      harvest_to_memory: true,
    });
  });

  // THE TWIN. The file's own comment warns that a field added to one body
  // assembler and not the other is invisible on the wire from that path,
  // silently — and continueSession is the path a chat actually uses after the
  // first turn, so a context override that worked on run 1 would vanish.
  it("continueSession maps context too", async () => {
    const { client, fetchMock } = makeClient([done()]);
    for await (const _ of client.continueSession({
      sessionId: "s1",
      segments: seg,
      context: { mode: "recap", keepLastN: 2, recapMaxChars: 4096 },
    })) {
      void _;
    }
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body.context).toEqual({ mode: "recap", keep_last_n: 2, recap_max_chars: 4096 });
  });

  it("omits the block entirely when unset, so the agent's own context is inherited", async () => {
    const { client, fetchMock } = makeClient([done(), done()]);
    for await (const _ of client.runStreaming({ agent: "qa", segments: seg })) void _;
    expect("context" in JSON.parse(fetchMock.mock.calls[0]![1]!.body as string)).toBe(false);

    for await (const _ of client.continueSession({ sessionId: "s1", segments: seg })) void _;
    expect("context" in JSON.parse(fetchMock.mock.calls[1]![1]!.body as string)).toBe(false);
  });

  // Per-field merge: sending one key must not blank the others server-side, so
  // the block carries only what was set rather than a fully-populated object.
  it("sends only the fields that were set", async () => {
    const { client, fetchMock } = makeClient([done()]);
    for await (const _ of client.runStreaming({
      agent: "qa",
      segments: seg,
      context: { mode: "append" },
    })) {
      void _;
    }
    expect(JSON.parse(fetchMock.mock.calls[0]![1]!.body as string).context).toEqual({ mode: "append" });
  });

  // The A/B the experiment needs: three arms differing only in `context`.
  it("carries a distinct context per arm, which is what makes the A/B possible", async () => {
    const { client, fetchMock } = makeClient([done(), done(), done()]);
    const arms: Array<Record<string, unknown>> = [
      { mode: "append" },
      { mode: "recap", keepLastN: 2, recapMaxChars: 4096 },
      { mode: "recap" },
    ];
    for (const context of arms) {
      for await (const _ of client.runStreaming({ agent: "qa", segments: seg, context })) void _;
    }
    const sent = fetchMock.mock.calls.map(
      (c) => JSON.parse(c[1]!.body as string).context,
    );
    expect(sent).toEqual([
      { mode: "append" },
      { mode: "recap", keep_last_n: 2, recap_max_chars: 4096 },
      { mode: "recap" },
    ]);
  });
});

/**
 * DRIFT GUARD: the wire keys contextToWire emits must be exactly the json tags
 * config.Context declares.
 *
 * This is the failure the whole RFC is about, one layer out: a key the client
 * sends and the server does not read is dropped in silence — no error, no
 * warning, just a setting that does nothing. `autorecap_at_pct` is the live
 * example, one character away from compaction's `autocompact_at_pct`.
 *
 * Reads the Go source rather than restating the list, because a hand-written
 * copy here would be a third spelling of the same vocabulary and free to drift
 * from both.
 */
import { readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";

describe("contextToWire matches config.Context", () => {
  const goFile = fileURLToPath(new URL("../../../internal/config/config.go", import.meta.url));
  const clientFile = fileURLToPath(new URL("../src/client.ts", import.meta.url));

  it("emits exactly the json tags the Go struct declares", () => {
    if (!existsSync(goFile)) {
      // Standalone checkout of the adapter: nothing to compare against.
      expect(existsSync(clientFile)).toBe(true);
      return;
    }
    const go = readFileSync(goFile, "utf8");
    const start = go.indexOf("type Context struct {");
    expect(start, "type Context struct not found — did it move or get renamed?").toBeGreaterThan(-1);
    const body = go.slice(start, go.indexOf("\n}", start));
    const goKeys = [...body.matchAll(/json:"([a-z0-9_]+)/g)].map((m) => m[1]!).sort();

    const ts = readFileSync(clientFile, "utf8");
    const fnStart = ts.indexOf("function contextToWire(");
    expect(fnStart, "contextToWire not found").toBeGreaterThan(-1);
    const fnBody = ts.slice(fnStart, ts.indexOf("\n}", fnStart));
    const tsKeys = [...fnBody.matchAll(/w\.([a-z0-9_]+) =/g)].map((m) => m[1]!).sort();

    expect(goKeys.length).toBeGreaterThan(5); // non-vacuity
    expect(tsKeys).toEqual(goKeys);
  });
});
