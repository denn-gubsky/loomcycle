// adapters/ts/tests/substrate.test.ts — v0.8.22 substrate admin
// methods (agentDef / skillDef). Mirror of the existing memory-
// admin / interrupt resolve test patterns.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient, errorResponse } from "./helpers.js";
import {
  InvalidArgumentError,
  AuthError,
  SubstrateToolRefusedError,
  UnavailableError,
} from "../src/index.js";

describe("agentDef", () => {
  it("posts JSON to /v1/_agentdef and returns the row envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "def_abc",
        name: "reviewer",
        version: 1,
        promoted: true,
      }),
    ]);

    const result = (await client.agentDef({
      op: "create",
      name: "reviewer",
      overlay: { system_prompt: "hi" },
    })) as Record<string, unknown>;

    expect(result.def_id).toBe("def_abc");
    expect(result.name).toBe("reviewer");
    expect(result.version).toBe(1);

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_agentdef");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("create");
    expect(body.overlay.system_prompt).toBe("hi");
  });

  it("forwards bearer auth in the Authorization header", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ok: true })]);
    await client.agentDef({ op: "list", name: "reviewer" });
    const headers = (fetchMock.mock.calls[0]![1] as RequestInit)
      .headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-bearer");
  });

  it("raises AuthError on 401", async () => {
    const { client } = makeClient([errorResponse(401, "invalid token")]);
    await expect(
      client.agentDef({ op: "create", name: "x", overlay: {} }),
    ).rejects.toBeInstanceOf(AuthError);
  });

  it("raises InvalidArgumentError on 400", async () => {
    const { client } = makeClient([errorResponse(400, "bad op")]);
    await expect(
      client.agentDef({ op: "create" } as any),
    ).rejects.toBeInstanceOf(InvalidArgumentError);
  });

  // v0.9.x — the overlay field is server-side opaque (the AgentDef
  // tool owns the schema). The adapter must pass it through
  // verbatim so new fields like max_iterations work without an
  // adapter version bump. This test pins the contract.
  it("passes max_iterations through the overlay verbatim", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ def_id: "def_xyz", name: "discovery", version: 1 }),
    ]);
    await client.agentDef({
      op: "create",
      name: "discovery",
      overlay: { system_prompt: "explore", max_iterations: 64 },
    });
    const body = JSON.parse(
      (fetchMock.mock.calls[0]![1] as RequestInit).body as string,
    );
    expect(body.overlay.max_iterations).toBe(64);
    expect(body.overlay.system_prompt).toBe("explore");
  });
});

describe("skillDef", () => {
  it("posts JSON to /v1/_skilldef", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "sdf_abc",
        name: "voice-applier",
        version: 1,
        promoted: true,
      }),
    ]);

    const result = (await client.skillDef({
      op: "create",
      name: "voice-applier",
      overlay: { body: "VOICE BODY" },
    })) as Record<string, unknown>;

    expect(result.def_id).toBe("sdf_abc");
    expect(result.name).toBe("voice-applier");

    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_skilldef",
    );
  });

  it("raises SubstrateToolRefusedError on 422 with code=tool_refused", async () => {
    const refusalBody = JSON.stringify({
      code: "tool_refused",
      tool: "SkillDef",
      error: "overlay.body is required and must contain non-whitespace content",
    });
    const { client } = makeClient([
      () =>
        new Response(refusalBody, {
          status: 422,
          headers: { "Content-Type": "application/json" },
        }),
    ]);

    let caught: unknown;
    try {
      await client.skillDef({ op: "create", name: "x", overlay: { body: "" } });
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(SubstrateToolRefusedError);
    const err = caught as SubstrateToolRefusedError;
    expect(err.tool).toBe("SkillDef");
    expect(err.message).toContain("body is required");
  });

  it("raises UnavailableError on 503", async () => {
    const { client } = makeClient([errorResponse(503, "store_unavailable")]);
    await expect(
      client.skillDef({ op: "list", name: "x" }),
    ).rejects.toBeInstanceOf(UnavailableError);
  });
});
