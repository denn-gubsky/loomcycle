import { describe, it, expect } from "vitest";
import {
  classifyFrame,
  readSSE,
  appendChange,
  emptyBuffer,
  matchesFilter,
  describeChange,
  tailChanges,
  embedderNotice,
  type EmbedderHealth,
  type MemoryChangeRow,
} from "./changeFeed";

const row = (o: Partial<MemoryChangeRow> = {}): MemoryChangeRow => ({
  seq: 1,
  type: "memory.set",
  scope: "user",
  scope_id: "alice",
  key: "memory/fact/x",
  at: "2026-08-17T12:00:00Z",
  ...o,
});

describe("classifyFrame", () => {
  it("reads the opening status frame", () => {
    expect(classifyFrame("feed", '{"enabled":true,"since":42}')).toEqual({
      kind: "status",
      status: { enabled: true, since: 42 },
    });
    expect(classifyFrame("feed", '{"enabled":false,"since":0}')).toEqual({
      kind: "status",
      status: { enabled: false, since: 0 },
    });
  });

  // A status frame that cannot be read must NOT be treated as enabled. Defaulting
  // permissively would reinstate the exact ambiguity the frame exists to remove: a
  // disabled feed reading as a quiet one.
  it("refuses a status frame with no readable `enabled`", () => {
    expect(classifyFrame("feed", "{}")).toBeNull();
    expect(classifyFrame("feed", '{"enabled":"yes"}')).toBeNull();
    expect(classifyFrame("feed", "not json")).toBeNull();
  });

  it("reads a change coordinate and ignores unknown event names", () => {
    const f = classifyFrame("change", JSON.stringify(row({ seq: 7 })));
    expect(f).toEqual({
      kind: "change",
      change: expect.objectContaining({ seq: 7 }),
    });
    expect(classifyFrame("something-else", "{}")).toBeNull();
  });

  // The status payload deliberately carries no `type`, so a parser that
  // discriminated on the BODY would have to guess. The event name is authoritative.
  it("discriminates on the event name, not the payload shape", () => {
    expect(classifyFrame("feed", '{"enabled":true}')?.kind).toBe("status");
    expect(classifyFrame("change", JSON.stringify(row()))?.kind).toBe("change");
  });
});

describe("readSSE", () => {
  const streamOf = (text: string): ReadableStream<Uint8Array> => {
    const bytes = new TextEncoder().encode(text);
    return new ReadableStream({
      start(c) {
        // Deliberately split mid-frame: a real stream arrives in arbitrary chunks,
        // and a parser that only works on whole frames passes a naive test and
        // loses data in the browser.
        c.enqueue(bytes.slice(0, Math.floor(bytes.length / 2)));
        c.enqueue(bytes.slice(Math.floor(bytes.length / 2)));
        c.close();
      },
    });
  };

  it("yields event/data pairs across chunk boundaries and skips keepalives", async () => {
    const text =
      'event: feed\ndata: {"enabled":true}\n\n' +
      ": keepalive\n\n" +
      'event: change\ndata: {"seq":1}\n\n';
    const got: { event: string; data: string }[] = [];
    for await (const f of readSSE(streamOf(text))) got.push(f);
    expect(got).toEqual([
      { event: "feed", data: '{"enabled":true}' },
      { event: "change", data: '{"seq":1}' },
    ]);
  });
});

describe("appendChange", () => {
  it("keeps newest first, bounds the list, and COUNTS what it dropped", () => {
    let buf = emptyBuffer;
    for (let i = 1; i <= 5; i++) buf = appendChange(buf, row({ seq: i }), 3);
    expect(buf.rows.map((r) => r.seq)).toEqual([5, 4, 3]);
    // seen is every row that arrived; dropped is what the cap cost. Without both,
    // "showing 3" reads as "3 things happened".
    expect(buf.seen).toBe(5);
    expect(buf.dropped).toBe(2);
  });

  it("dedupes on seq so a reconnect does not inflate the count", () => {
    let buf = appendChange(emptyBuffer, row({ seq: 9 }));
    buf = appendChange(buf, row({ seq: 9 }));
    expect(buf.rows).toHaveLength(1);
    expect(buf.seen).toBe(1);
  });

  // The two families share a seq space in the runtime's table but arrive on
  // SEPARATE streams, so the same seq can legitimately appear once per family.
  it("does not treat two families' rows as duplicates", () => {
    let buf = appendChange(emptyBuffer, row({ seq: 3, type: "memory.set" }));
    buf = appendChange(buf, row({ seq: 3, type: "document.chunk.updated" }));
    expect(buf.rows).toHaveLength(2);
  });
});

describe("matchesFilter", () => {
  it("splits the families", () => {
    expect(
      matchesFilter(row({ type: "memory.set" }), { family: "memory" }),
    ).toBe(true);
    expect(
      matchesFilter(row({ type: "memory.set" }), { family: "documents" }),
    ).toBe(false);
    expect(
      matchesFilter(row({ type: "document.chunk.updated" }), {
        family: "documents",
      }),
    ).toBe(true);
  });

  it("matches scope_id by substring, case-insensitively", () => {
    expect(matchesFilter(row({ scope_id: "alice" }), { scopeId: "ALI" })).toBe(
      true,
    );
    expect(matchesFilter(row({ scope_id: "alice" }), { scopeId: "bob" })).toBe(
      false,
    );
  });

  it("an empty filter matches everything", () => {
    expect(matchesFilter(row(), {})).toBe(true);
    expect(
      matchesFilter(row(), { family: "", scope: "", scopeId: "", type: "" }),
    ).toBe(true);
  });
});

describe("describeChange", () => {
  it("names the coordinate, and never implies a value", () => {
    expect(describeChange(row({ key: "memory/fact/x" }))).toBe("memory/fact/x");
    expect(
      describeChange(row({ key: undefined, chunk_id: "abcdef0123456789" })),
    ).toBe("chunk abcdef012345");
    expect(
      describeChange(
        row({
          key: undefined,
          chunk_id: undefined,
          type: "memory.scope_deleted",
        }),
      ),
    ).toBe("(whole scope)");
  });
});

describe("tailChanges (transport)", () => {
  const sse = (text: string) =>
    new Response(new Blob([text]).stream(), {
      status: 200,
      headers: { "Content-Type": "text/event-stream" },
    });

  it("hits the right path per family, threads since, and yields typed frames", async () => {
    const calls: { url: string; headers: Record<string, string> }[] = [];
    const fetchImpl = (async (url: RequestInfo | URL, init?: RequestInit) => {
      calls.push({
        url: String(url),
        headers: (init?.headers ?? {}) as Record<string, string>,
      });
      return sse('event: feed\ndata: {"enabled":true,"since":5}\n\n');
    }) as typeof fetch;

    const conn = {
      baseUrl: "https://rt.example",
      token: "T",
      fetch: fetchImpl,
    };
    for await (const f of tailChanges(conn, "memory", { since: 5 })) {
      expect(f).toEqual({
        kind: "status",
        status: { enabled: true, since: 5 },
      });
    }
    for await (const _ of tailChanges(conn, "documents")) void _;

    expect(calls[0].url).toBe("https://rt.example/v1/_memory/changes?since=5");
    // No `since` → no query string at all, rather than `?since=0`: 0 is the server's
    // own default and an explicit one only invites a reader to think it means something.
    expect(calls[1].url).toBe("https://rt.example/v1/_document/changes");
    expect(calls[0].headers.Authorization).toBe("Bearer T");
    // BOTH media types, or a strict proxy in front of the runtime 406s the request —
    // the success path is event-stream and the error path is JSON.
    expect(calls[0].headers.Accept).toBe("text/event-stream, application/json");
  });

  it("surfaces the runtime's error body, not just the status", async () => {
    const fetchImpl = (async () =>
      new Response("change feed requires a persistence backend", {
        status: 503,
      })) as typeof fetch;
    await expect(async () => {
      for await (const _ of tailChanges(
        { baseUrl: "", fetch: fetchImpl },
        "memory",
      ))
        void _;
    }).rejects.toThrow(/503: change feed requires a persistence backend/);
  });

  it("omits Authorization when there is no token (open mode / cookie auth)", async () => {
    let seen: Record<string, string> = {};
    const fetchImpl = (async (_u: RequestInfo | URL, init?: RequestInit) => {
      seen = (init?.headers ?? {}) as Record<string, string>;
      return sse("");
    }) as typeof fetch;
    for await (const _ of tailChanges(
      { baseUrl: "", fetch: fetchImpl },
      "memory",
    ))
      void _;
    expect(seen.Authorization).toBeUndefined();
  });
});

describe("embedderNotice", () => {
  const h = (o: Partial<EmbedderHealth>): EmbedderHealth => ({
    state: "ok",
    calls: 10,
    failures: 0,
    ...o,
  });

  // The count is the actionable part — it is roughly how many rows need a backfill
  // to become searchable again — so a notice without it is not worth showing.
  it("names the failure count and the remedy when failing", () => {
    const n = embedderNotice(
      h({
        state: "failing",
        failures: 47,
        calls: 312,
        last_failure_kind: "timeout",
      }),
    );
    expect(n).toContain("47");
    expect(n).toContain("312");
    expect(n).toContain("timeout");
    expect(n.toLowerCase()).toContain("backfill");
    // The distinction that matters: writes are FINE, searchability is not.
    expect(n.toLowerCase()).toContain("not searchable");
  });

  it("says so when no embedder is configured", () => {
    expect(embedderNotice(h({ state: "absent" })).toLowerCase()).toContain(
      "no embedder",
    );
  });

  // `untried` is the normal state of a freshly booted runtime. Warning about it
  // would train an operator to ignore this line, which is the only thing that would
  // be showing when it matters.
  it("is silent for untried and ok", () => {
    expect(embedderNotice(h({ state: "untried", calls: 0 }))).toBe("");
    expect(embedderNotice(h({ state: "ok" }))).toBe("");
  });

  // An older runtime does not send the field at all. Inventing "healthy" for it
  // would be the same unchecked claim the opening frame exists to prevent.
  it("is silent when the runtime reported nothing", () => {
    expect(embedderNotice(undefined)).toBe("");
  });
});

describe("classifyFrame — embedder health rides the status frame", () => {
  it("carries the embedder block through", () => {
    const f = classifyFrame(
      "feed",
      '{"enabled":true,"since":0,"embedder":{"state":"failing","calls":5,"failures":5,"provider":"ollama-local","model":"embeddinggemma:latest"}}',
    );
    expect(f).toEqual({
      kind: "status",
      status: expect.objectContaining({
        enabled: true,
        embedder: expect.objectContaining({ state: "failing", failures: 5 }),
      }),
    });
  });

  // A runtime that predates the field still yields a usable status frame.
  it("still reads a status frame with no embedder block", () => {
    const f = classifyFrame("feed", '{"enabled":true,"since":0}');
    expect(f?.kind).toBe("status");
    expect(
      (f as { status: { embedder?: unknown } }).status.embedder,
    ).toBeUndefined();
  });
});
