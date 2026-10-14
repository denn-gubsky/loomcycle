// adapters/ts/tests/change-feeds.test.ts — RFC CD Part C memory/document
// change feeds: streamMemoryChanges + streamDocumentChanges.

import { describe, expect, it } from "vitest";

import { errorResponse, makeClient, sseResponse } from "./helpers.js";
import {
  AuthError,
  LoomcycleError,
  UnavailableError,
  type ChangeFeedFrame,
} from "../src/index.js";

// Frames exactly as the runtime writes them: sendRaw("feed", …) carries no
// `type`; sendRaw("change", store.MemoryChange) carries the change's own type.
const FEED =
  'event: feed\ndata: {"enabled":true,"since":42,"embedder":{"state":"failing","provider":"ollama","model":"nomic","calls":10,"failures":3,"last_failure_kind":"unreachable"}}\n\n';
const SET =
  'event: change\ndata: {"seq":43,"tenant":"acme","type":"memory.set","scope":"user","scope_id":"u1","key":"prefs","at":"2026-10-14T10:00:00Z"}\n\n';
const SCOPE_DELETED =
  'event: change\ndata: {"seq":44,"tenant":"acme","type":"memory.scope_deleted","scope":"agent","scope_id":"bot","at":"2026-10-14T10:00:01Z"}\n\n';

async function collect(src: AsyncIterable<ChangeFeedFrame>): Promise<ChangeFeedFrame[]> {
  const out: ChangeFeedFrame[] = [];
  for await (const f of src) out.push(f);
  return out;
}

describe("streamMemoryChanges", () => {
  it("GETs /v1/_memory/changes?since=N and yields the feed frame then typed changes", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse([FEED, ": keepalive\n\n", SET, SCOPE_DELETED]),
    ]);

    const frames = await collect(client.streamMemoryChanges({ since: 42 }));

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_memory/changes?since=42");
    expect(call[1]!.method).toBe("GET");
    const headers = call[1]!.headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-bearer");
    expect(headers.Accept).toContain("text/event-stream");

    // The keepalive comment yields nothing; the feed frame is first.
    expect(frames.map((f) => f.kind)).toEqual(["feed", "change", "change"]);
    expect(frames[0]).toEqual({
      kind: "feed",
      // No backfilled `type` leaks into the status payload.
      payload: {
        enabled: true,
        since: 42,
        embedder: {
          state: "failing",
          provider: "ollama",
          model: "nomic",
          calls: 10,
          failures: 3,
          last_failure_kind: "unreachable",
        },
      },
    });
    const set = frames[1]!;
    if (set.kind !== "change") throw new Error("expected a change frame");
    expect(set.payload).toEqual({
      seq: 43,
      tenant: "acme",
      type: "memory.set",
      scope: "user",
      scope_id: "u1",
      key: "prefs",
      at: "2026-10-14T10:00:00Z",
    });
    const del = frames[2]!;
    if (del.kind !== "change") throw new Error("expected a change frame");
    expect(del.payload.type).toBe("memory.scope_deleted");
    expect(del.payload.key).toBeUndefined();
  });

  it("reports a disabled feed on the feed frame and sends no since when omitted or 0", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse(['event: feed\ndata: {"enabled":false,"since":0}\n\n']),
      sseResponse(['event: feed\ndata: {"enabled":false,"since":0}\n\n']),
    ]);

    const frames = await collect(client.streamMemoryChanges());
    await collect(client.streamMemoryChanges({ since: 0 }));

    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/_memory/changes");
    expect(fetchMock.mock.calls[1]![0]).toBe("http://test-loomcycle:8787/v1/_memory/changes");
    expect(frames).toEqual([{ kind: "feed", payload: { enabled: false, since: 0 } }]);
  });

  it("skips frames it does not recognise and a feed frame without enabled", async () => {
    const { client } = makeClient([
      sseResponse([
        // A status we cannot read must not surface as (implicitly) enabled.
        'event: feed\ndata: {"since":0}\n\n',
        // An event name a newer server might add.
        'event: lag\ndata: {"behind":12}\n\n',
        SET,
      ]),
    ]);

    const frames = await collect(client.streamMemoryChanges());

    expect(frames.map((f) => f.kind)).toEqual(["change"]);
  });

  it.each([-1, 1.5, Number.NaN])(
    "refuses since=%s before any request instead of letting the server replay from 0",
    async (since) => {
      const { client, fetchMock } = makeClient([]);

      await expect(collect(client.streamMemoryChanges({ since }))).rejects.toThrow(
        /non-negative integer/,
      );
      expect(fetchMock).not.toHaveBeenCalled();
    },
  );

  it("ends iteration with an AbortError when the signal aborts mid-stream", async () => {
    const encoder = new TextEncoder();
    // A stream that stays open after the feed frame, like the real feed, and
    // errors when the request's signal aborts, as fetch does.
    const { client } = makeClient([
      (req) =>
        new Response(
          new ReadableStream({
            start(controller) {
              controller.enqueue(encoder.encode(FEED));
              req.signal.addEventListener("abort", () =>
                controller.error(new DOMException("aborted by signal", "AbortError")),
              );
            },
          }),
          { status: 200, headers: { "Content-Type": "text/event-stream" } },
        ),
    ]);

    const ac = new AbortController();
    const seen: string[] = [];
    let caught: unknown;
    try {
      for await (const f of client.streamMemoryChanges({ signal: ac.signal })) {
        seen.push(f.kind);
        ac.abort();
      }
    } catch (e) {
      caught = e;
    }

    expect(seen).toEqual(["feed"]);
    expect((caught as Error | undefined)?.name).toBe("AbortError");
  });

  it("maps a 403 (member token) to a LoomcycleError carrying the status", async () => {
    const { client } = makeClient([errorResponse(403, "forbidden: requires substrate:tenant")]);

    const err = await collect(client.streamMemoryChanges()).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(LoomcycleError);
    expect(err).not.toBeInstanceOf(AuthError);
    expect((err as LoomcycleError).status).toBe(403);
  });

  it("maps a 503 (no persistence backend) to UnavailableError", async () => {
    const { client } = makeClient([
      errorResponse(503, "change feed requires a persistence backend"),
    ]);

    const err = await collect(client.streamMemoryChanges()).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(UnavailableError);
  });
});

describe("streamDocumentChanges", () => {
  it("GETs /v1/_document/changes and yields document.chunk changes with chunk_id", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse([
        'event: feed\ndata: {"enabled":true,"since":7,"embedder":{"state":"ok","calls":1,"failures":0}}\n\n',
        'event: change\ndata: {"seq":9,"type":"document.chunk.updated","scope":"user","scope_id":"u1","chunk_id":"ch_abc","at":"2026-10-14T10:00:00Z"}\n\n',
      ]),
    ]);

    const frames = await collect(client.streamDocumentChanges({ since: 7 }));

    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_document/changes?since=7",
    );
    expect(frames[0]!.kind).toBe("feed");
    const ch = frames[1]!;
    if (ch.kind !== "change") throw new Error("expected a change frame");
    expect(ch.payload.type).toBe("document.chunk.updated");
    expect(ch.payload.chunk_id).toBe("ch_abc");
    // The shared "" tenant omits the field rather than sending "".
    expect(ch.payload.tenant).toBeUndefined();
  });
});
