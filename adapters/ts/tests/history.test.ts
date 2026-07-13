// adapters/ts/tests/history.test.ts — RFC BE: the History tool
// (POST /v1/_history) on the wire. Browse/search/annotate past chats (a chat =
// a session). Mirror of the substrate-admin / path-document test patterns: an
// op-discriminated input passes through verbatim and the tool's result comes
// back. The owner is resolved server-side from the principal, so — unlike
// path/document — there is NO scope_id/tenant browse override on this method.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient, errorResponse } from "./helpers.js";
import {
  InvalidArgumentError,
  AuthError,
  SubstrateToolRefusedError,
} from "../src/index.js";

describe("history", () => {
  it("posts JSON to /v1/_history and returns the list envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ scope: "tenant", chats: [], total: 0 }),
    ]);

    const result = (await client.history({
      op: "list",
      scope: "tenant",
    })) as Record<string, unknown>;

    expect(result.scope).toBe("tenant");
    expect(result.chats).toEqual([]);
    expect(result.total).toBe(0);

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_history");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("list");
    expect(body.scope).toBe("tenant");
  });

  it("passes the rename title through verbatim", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ updated: true })]);
    await client.history({
      op: "rename",
      session_id: "s1",
      title: "Q3 launch planning",
    });
    const body = JSON.parse(
      (fetchMock.mock.calls[0]![1] as RequestInit).body as string,
    );
    expect(body.op).toBe("rename");
    expect(body.session_id).toBe("s1");
    expect(body.title).toBe("Q3 launch planning");
  });

  it("passes the annotate description + tags through verbatim", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ updated: true })]);
    await client.history({
      op: "annotate",
      session_id: "s1",
      description: "planning thread",
      tags: ["launch", "q3"],
    });
    const body = JSON.parse(
      (fetchMock.mock.calls[0]![1] as RequestInit).body as string,
    );
    expect(body.description).toBe("planning thread");
    expect(body.tags).toEqual(["launch", "q3"]);
  });

  it("survives an explicit pinned:false (distinct from unset)", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ pinned: false })]);
    await client.history({ op: "pin", session_id: "s1", pinned: false });
    const body = JSON.parse(
      (fetchMock.mock.calls[0]![1] as RequestInit).body as string,
    );
    // An explicit false must survive serialization (unpin != leave-as-is).
    expect(body.pinned).toBe(false);
  });

  it("forwards bearer auth in the Authorization header", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ chats: [] })]);
    await client.history({ op: "list" });
    const headers = (fetchMock.mock.calls[0]![1] as RequestInit)
      .headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-bearer");
  });

  it("raises SubstrateToolRefusedError when a tenant caller requests scope:global", async () => {
    const refusalBody = JSON.stringify({
      code: "tool_refused",
      tool: "History",
      error: 'history: scope "global" not permitted (allowed: self, user, tenant)',
    });
    const { client } = makeClient([
      () =>
        new Response(refusalBody, {
          status: 422,
          headers: { "Content-Type": "application/json" },
        }),
    ]);
    await expect(
      client.history({ op: "list", scope: "global" }),
    ).rejects.toBeInstanceOf(SubstrateToolRefusedError);
  });

  it("raises InvalidArgumentError on 400", async () => {
    const { client } = makeClient([errorResponse(400, "bad op")]);
    await expect(
      client.history({ op: "list" } as any),
    ).rejects.toBeInstanceOf(InvalidArgumentError);
  });

  it("raises AuthError on 401", async () => {
    const { client } = makeClient([errorResponse(401, "invalid token")]);
    await expect(client.history({ op: "list" })).rejects.toBeInstanceOf(
      AuthError,
    );
  });
});
