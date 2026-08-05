// adapters/ts/tests/memory-search.test.ts — v1.47.0 RFC BV memory-view SDK:
// memorySearch (POST /v1/_memory/search), memoryEmbedStats (GET
// /v1/_memory/embed_stats), reembedMemory (POST /v1/_memory/reembed).
// Mirrors the mock-fetch pattern in embeddings.test.ts / memory-admin.test.ts:
// assert the wire URL/method/body-or-query mapping + the parsed response.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient } from "./helpers.js";
import type {
  MemoryEmbedStatsResponse,
  MemoryReembedResponse,
  MemorySearchResponse,
} from "../src/index.js";

describe("memorySearch", () => {
  it("POSTs snake_case body (scopeId→scope_id, topK→top_k) and parses every kind", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        scope: "user",
        scope_id: "alice",
        entries: [
          {
            key: "doc.chunk:c123",
            value: "the deploy runbook",
            score: 0.91,
            rank_score: 0.88,
            embedded_with: { provider: "openai", model: "text-embedding-3-small" },
            kind: "document",
            chunk_id: "c123",
          },
          {
            key: "prefs/tone",
            value: { polite: true },
            score: 0.77,
            rank_score: 0.72,
            embedded_with: { provider: "openai", model: "text-embedding-3-small" },
            // RFC BW refined this union: "memory" became "fact" | "note". A k/v row
            // with no server-stamped provenance is a note.
            kind: "note",
          },
        ],
        query_embedding_dim: 1536,
        truncated: false,
      } satisfies MemorySearchResponse),
    ]);

    const resp = await client.memorySearch({
      query: "how do I deploy",
      scope: "user",
      scopeId: "alice",
      topK: 5,
      rank: { alpha: 0.5 },
      dedup: { enabled: true },
    });

    // Parsed response: a document hit (with chunk_id) and a memory hit.
    expect(resp.entries).toHaveLength(2);
    expect(resp.entries[0]!.kind).toBe("document");
    expect(resp.entries[0]!.chunk_id).toBe("c123");
    expect(resp.entries[1]!.kind).toBe("note");
    expect(resp.entries[1]!.chunk_id).toBeUndefined();
    expect(resp.query_embedding_dim).toBe(1536);
    expect(resp.truncated).toBe(false);

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_memory/search");
    expect(init.method).toBe("POST");
    const body = JSON.parse(init.body as string);
    // camelCase → snake_case mapping; rank/dedup pass through as-is.
    expect(body).toEqual({
      query: "how do I deploy",
      scope: "user",
      scope_id: "alice",
      top_k: 5,
      rank: { alpha: 0.5 },
      dedup: { enabled: true },
    });
    // No camelCase leaks onto the wire.
    expect("scopeId" in body).toBe(false);
    expect("topK" in body).toBe(false);
  });

  it("omits top_k / rank / dedup when not provided", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        scope: "agent",
        scope_id: "qa",
        entries: [],
        query_embedding_dim: 0,
        truncated: false,
      } satisfies MemorySearchResponse),
    ]);
    await client.memorySearch({ query: "x", scope: "agent", scopeId: "qa" });
    const body = JSON.parse(fetchMock.mock.calls[0]![1].body as string);
    expect(body).toEqual({ query: "x", scope: "agent", scope_id: "qa" });
  });
});

describe("memoryEmbedStats", () => {
  it("GETs /v1/_memory/embed_stats?scope=user and parses models + bytes", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        scope: "user",
        models: [
          { provider: "openai", model: "text-embedding-3-small", dimension: 1536, row_count: 42 },
          { provider: "ollama", model: "nomic-embed-text", dimension: 768, row_count: 7 },
        ],
        total_embedding_bytes: 987654,
      } satisfies MemoryEmbedStatsResponse),
    ]);

    const resp = await client.memoryEmbedStats("user");

    expect(resp.models).toHaveLength(2);
    expect(resp.models[0]!.row_count).toBe(42);
    expect(resp.models[1]!.dimension).toBe(768);
    expect(resp.total_embedding_bytes).toBe(987654);

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_memory/embed_stats?scope=user");
    expect(init.method).toBe("GET");
  });
});

describe("reembedMemory", () => {
  it("defaults to a dry run: no dry_run=false in the query, parses the plan", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        scope: "user",
        scope_id: "alice",
        dry_run: true,
        rows_total: 12,
        rows_to_reembed: 12,
        current_embedder: { provider: "openai", model: "text-embedding-3-small", dimension: 1536 },
        sample_keys: ["prefs/tone", "notes/1"],
        sample_keys_capped: false,
      } satisfies MemoryReembedResponse),
    ]);

    const resp = await client.reembedMemory("user", "alice");

    // Narrow on the discriminant to reach the dry-run arm.
    expect(resp.dry_run).toBe(true);
    if (resp.dry_run) {
      expect(resp.rows_to_reembed).toBe(12);
      expect(resp.sample_keys).toEqual(["prefs/tone", "notes/1"]);
      expect(resp.current_embedder.dimension).toBe(1536);
    }

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(init.method).toBe("POST");
    // An omitted dryRun must NOT send dry_run=false — the server-side
    // default (true) must stand so a caller can't accidentally reembed.
    expect(url).not.toContain("dry_run=false");
    const parsed = new URL(url as string);
    expect(parsed.pathname).toBe("/v1/_memory/reembed");
    expect(parsed.searchParams.get("scope")).toBe("user");
    expect(parsed.searchParams.get("scope_id")).toBe("alice");
    expect(parsed.searchParams.has("dry_run")).toBe(false);
    // No body is sent — the handler reads only query params.
    expect(init.body).toBeFalsy();
  });

  it("sends dry_run=false + limit when committing", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        scope: "user",
        scope_id: "alice",
        dry_run: false,
        rows_reembedded: 10,
        rows_failed: 2,
        current_embedder: { provider: "openai", model: "text-embedding-3-small", dimension: 1536 },
        failed_keys: ["notes/9", "notes/10"],
      } satisfies MemoryReembedResponse),
    ]);

    const resp = await client.reembedMemory("user", "alice", { dryRun: false, limit: 500 });

    expect(resp.dry_run).toBe(false);
    if (!resp.dry_run) {
      expect(resp.rows_reembedded).toBe(10);
      expect(resp.rows_failed).toBe(2);
      expect(resp.failed_keys).toEqual(["notes/9", "notes/10"]);
    }

    const url = new URL(fetchMock.mock.calls[0]![0] as string);
    expect(url.searchParams.get("dry_run")).toBe("false");
    expect(url.searchParams.get("limit")).toBe("500");
  });
});

describe("memorySearch sources (RFC BW)", () => {
  it("sends sources only when set, so an omitted selector keeps the span-everything default", async () => {
    const empty = () =>
      jsonResponse({
        scope: "user",
        scope_id: "u1",
        entries: [],
        query_embedding_dim: 3,
        truncated: false,
      } satisfies MemorySearchResponse);
    const { client, fetchMock } = makeClient([empty(), empty(), empty()]);

    await client.memorySearch({ query: "q", scope: "user", scopeId: "u1" });
    const first = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    expect("sources" in first).toBe(false);

    await client.memorySearch({ query: "q", scope: "user", scopeId: "u1", sources: ["facts"] });
    expect(JSON.parse(String(fetchMock.mock.calls[1][1]?.body)).sources).toEqual(["facts"]);

    await client.memorySearch({
      query: "q",
      scope: "user",
      scopeId: "u1",
      sources: ["facts", "notes", "documents"],
    });
    expect(JSON.parse(String(fetchMock.mock.calls[2][1]?.body)).sources).toEqual([
      "facts",
      "notes",
      "documents",
    ]);
  });

  it("round-trips the refined kind taxonomy", async () => {
    // RFC BW §7 changed `kind` from "memory" | "document" to
    // "fact" | "note" | "document". Code switching on "document" is unaffected;
    // this pins that the finer values survive the parse, and that only a document
    // hit carries chunk_id — which is what makes it followable to get_chunk.
    const { client } = makeClient([
      jsonResponse({
        scope: "user",
        scope_id: "u1",
        query_embedding_dim: 3,
        truncated: false,
        entries: [
          {
            key: "memory/fact/x",
            value: "distilled",
            score: 0.9,
            rank_score: 0.9,
            embedded_with: { provider: "p", model: "m" },
            kind: "fact",
          },
          {
            key: "scratch/y",
            value: "jotted",
            score: 0.8,
            rank_score: 0.8,
            embedded_with: { provider: "p", model: "m" },
            kind: "note",
          },
          {
            key: "doc.chunk:z",
            value: "prose",
            score: 0.7,
            rank_score: 0.7,
            embedded_with: { provider: "p", model: "m" },
            kind: "document",
            chunk_id: "z",
          },
        ],
      } satisfies MemorySearchResponse),
    ]);
    const res = await client.memorySearch({ query: "q", scope: "user", scopeId: "u1" });
    expect(res.entries.map((e) => e.kind)).toEqual(["fact", "note", "document"]);
    expect(res.entries[2].chunk_id).toBe("z");
    expect(res.entries[0].chunk_id).toBeUndefined();
  });
});
