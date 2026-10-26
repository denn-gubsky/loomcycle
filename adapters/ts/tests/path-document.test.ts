// adapters/ts/tests/path-document.test.ts — v1.4.0: the RFC AL Path VFS
// (POST /v1/_path) and RFC AK Document (POST /v1/_document) tools on the
// wire. Mirror of the substrate-admin test patterns; both pass an
// op-discriminated input through verbatim and return the tool's result.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient, errorResponse } from "./helpers.js";
import {
  InvalidArgumentError,
  AuthError,
  SubstrateToolRefusedError,
  UnavailableError,
} from "../src/index.js";
import type {
  DocumentRelatedResult,
  DocumentSearchFilter,
  DocumentSearchResult,
  DocumentToolInput,
} from "../src/index.js";

describe("path", () => {
  it("posts JSON to /v1/_path and returns the ls envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ path: "/", entries: [] }),
    ]);

    const result = (await client.path({
      op: "ls",
      scope: "user",
      path: "/",
    })) as Record<string, unknown>;

    expect(result.path).toBe("/");
    expect(result.entries).toEqual([]);

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_path");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("ls");
    expect(body.scope).toBe("user");
  });

  it("forwards bearer auth in the Authorization header", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ entries: [] })]);
    await client.path({ op: "ls", path: "/" });
    const headers = (fetchMock.mock.calls[0]![1] as RequestInit)
      .headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-bearer");
  });

  it("appends scope_id + tenant as query params for browse-by-subject", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ path: "/", entries: [] })]);
    await client.path(
      { op: "ls", path: "/", scope: "user" },
      { scopeId: "u1", tenant: "acme" },
    );
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe(
      "http://test-loomcycle:8787/v1/_path?scope_id=u1&tenant=acme",
    );
    // The browse override rides the URL, not the body.
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("ls");
    expect(body.scope_id).toBeUndefined();
    expect(body.tenant).toBeUndefined();
  });

  it("omits the query string entirely when no browse opts are given", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ path: "/", entries: [] })]);
    await client.path({ op: "ls", path: "/", scope: "user" });
    // Regression guard: no browse override → URL is exactly /v1/_path (no `?`).
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/_path");
  });

  it("raises SubstrateToolRefusedError on 422 with code=tool_refused", async () => {
    const refusalBody = JSON.stringify({
      code: "tool_refused",
      tool: "Path",
      error: "rm: path has descendants — pass recursive:true",
    });
    const { client } = makeClient([
      () =>
        new Response(refusalBody, {
          status: 422,
          headers: { "Content-Type": "application/json" },
        }),
    ]);
    await expect(
      client.path({ op: "rm", path: "/docs" }),
    ).rejects.toBeInstanceOf(SubstrateToolRefusedError);
  });

  it("raises AuthError on 401", async () => {
    const { client } = makeClient([errorResponse(401, "invalid token")]);
    await expect(client.path({ op: "ls", path: "/" })).rejects.toBeInstanceOf(
      AuthError,
    );
  });
});

describe("document", () => {
  it("posts JSON to /v1/_document and returns the create envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ document_id: "d1", root_chunk_id: "c0", path: "/docs/launch" }),
    ]);

    const result = (await client.document({
      op: "create_document",
      scope: "user",
      title: "Launch plan",
      path: "/docs/launch",
    })) as Record<string, unknown>;

    expect(result.document_id).toBe("d1");
    expect(result.root_chunk_id).toBe("c0");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_document");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("create_document");
    expect(body.title).toBe("Launch plan");
  });

  it("passes the optimistic revision + fields through verbatim", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ updated: true })]);
    await client.document({
      op: "update_chunk",
      scope: "user",
      id: "c1",
      revision: 3,
      fields: { owner: "alice" },
    });
    const body = JSON.parse(
      (fetchMock.mock.calls[0]![1] as RequestInit).body as string,
    );
    expect(body.revision).toBe(3);
    expect(body.fields.owner).toBe("alice");
  });

  it("posts export_md with include_metadata through verbatim", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ markdown: "# Launch plan\n" }),
    ]);
    const result = (await client.document({
      op: "export_md",
      document_id: "d1",
      include_metadata: false,
    })) as Record<string, unknown>;
    expect(result.markdown).toBe("# Launch plan\n");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_document");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("export_md");
    expect(body.document_id).toBe("d1");
    // An explicit false must survive (distinct from unset).
    expect(body.include_metadata).toBe(false);
  });

  it("types the search / related filter arguments and the filter echo", async () => {
    const echo: DocumentSearchFilter = {
      applied: { under_paths: ["/qg/norms"], types: ["norm.clause"], statuses: ["in_force"], as_of: 1747180800000000000 },
      matched_documents: 14,
      matched_chunks: 312,
      type_expanded_to: ["norm.clause", "norm.clause.transitional"],
      inverted: false,
    };
    const { client, fetchMock } = makeClient([
      jsonResponse({ chunks: [{ chunk_id: "c1", score: 0.8, rank_score: 0.03, document_id: "d1" }], filter: echo }),
      jsonResponse({ related: [], filter: { ...echo, matched_chunks: 0 } }),
    ]);
    const input: DocumentToolInput = {
      op: "search",
      query: "minimum capital",
      under_path: "/qg/norms",
      under_paths: ["/qg/decisions"],
      exclude_under_path: "/qg/reports",
      exclude_under_paths: ["/qg/passports/source"],
      document_ids: ["d1"],
      exclude_document_ids: ["d9"],
      type: "norm.clause",
      types: ["norm.annex"],
      status: "in_force",
      statuses: ["transitional"],
      tag: "regulator/nbu",
      tag_prefix: "regulator",
      as_of: 1747180800000000000,
    };
    const res = (await client.document(input)) as DocumentSearchResult;
    expect(res.filter?.matched_chunks).toBe(312);
    expect(res.chunks[0]!.chunk_id).toBe("c1");
    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body.under_paths).toEqual(["/qg/decisions"]);
    expect(body.exclude_document_ids).toEqual(["d9"]);
    expect(body.as_of).toBe(1747180800000000000);

    const rel = (await client.document({ op: "related", id: "c1", exclude_document_ids: ["d1"] })) as DocumentRelatedResult;
    expect(rel.related).toEqual([]);
    expect(rel.filter?.matched_chunks).toBe(0);
  });

  it("types a chunk's validity period on create/update and on search hits", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ id: "c1", revision: 1 }),
      jsonResponse({ id: "c1", revision: 2 }),
      jsonResponse({ chunks: [{ chunk_id: "c1", score: 0.8, rank_score: 0.03, valid_at: 1704067200000000000 }] }),
    ]);
    const create: DocumentToolInput = {
      op: "create_chunk",
      document_id: "d1",
      title: "Clause 7 (2024 edition)",
      valid_at: 1704067200000000000,
      invalid_at: 1767225600000000000,
    };
    await client.document(create);
    // null clears an end on update_chunk, so it must survive serialisation.
    await client.document({ op: "update_chunk", id: "c1", revision: 1, invalid_at: null });
    const res = (await client.document({ op: "search", query: "reserves", as_of: 1748736000000000000 })) as DocumentSearchResult;
    expect(res.chunks[0]!.valid_at).toBe(1704067200000000000);
    expect(res.chunks[0]!.invalid_at).toBeUndefined();
    const createBody = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(createBody.invalid_at).toBe(1767225600000000000);
    const updateBody = JSON.parse((fetchMock.mock.calls[1]![1] as RequestInit).body as string);
    expect("invalid_at" in updateBody).toBe(true);
    expect(updateBody.invalid_at).toBeNull();
  });

  it("types graph_recall's body_chars and keeps an explicit 0", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ chunks: [], seeds: 0, hops: 1, truncated: false })]);
    const input: DocumentToolInput = { op: "graph_recall", seed_ids: ["c1"], body_chars: 0 };
    await client.document(input);
    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    // 0 asks for titles only; dropping it as falsy would ask for the default excerpts.
    expect(body.body_chars).toBe(0);
  });

  it("appends scope_id + tenant as query params for browse-by-subject", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ document_id: "d1" })]);
    await client.document(
      { op: "get_document", id: "d1", scope: "user" },
      { scopeId: "u1", tenant: "acme" },
    );
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_document?scope_id=u1&tenant=acme",
    );
  });

  it("raises UnavailableError on 503 (SQL Memory / connector unwired)", async () => {
    const { client } = makeClient([errorResponse(503, "connector not wired")]);
    await expect(
      client.document({ op: "create_document", title: "x" }),
    ).rejects.toBeInstanceOf(UnavailableError);
  });

  it("raises InvalidArgumentError on 400", async () => {
    const { client } = makeClient([errorResponse(400, "bad op")]);
    await expect(
      client.document({ op: "create_document" } as any),
    ).rejects.toBeInstanceOf(InvalidArgumentError);
  });
});
