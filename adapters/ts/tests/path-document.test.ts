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
