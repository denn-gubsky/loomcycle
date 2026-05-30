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

// v1.x RFC E ScheduleDef — mirror of agentDef + skillDef. Fork
// auto-promotes by default per RFC E's worked example, so the
// happy-path response shape includes `promoted: true`.
describe("scheduleDef", () => {
  it("posts JSON to /v1/_scheduledef and returns the row envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "sd_abc",
        name: "job-search-alice",
        version: 1,
        promoted: true,
      }),
    ]);

    const result = (await client.scheduleDef({
      op: "create",
      name: "job-search-alice",
      overlay: {
        agent: "researcher",
        schedule: "0 6 * * *",
        user_id: "alice",
      },
    })) as Record<string, unknown>;

    expect(result.def_id).toBe("sd_abc");
    expect(result.name).toBe("job-search-alice");
    expect(result.version).toBe(1);
    expect(result.promoted).toBe(true);

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_scheduledef");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("create");
    expect(body.overlay.schedule).toBe("0 6 * * *");
  });

  it("raises SubstrateToolRefusedError when fork hits required_credentials gate", async () => {
    const refusalBody = JSON.stringify({
      code: "tool_refused",
      tool: "ScheduleDef",
      error:
        'fork: required credential "slack" missing from user_credentials (template required: [jobs slack])',
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
      await client.scheduleDef({
        op: "fork",
        name: "job-search-template",
        overlay: { user_id: "alice", user_credentials: { jobs: "x" } },
      });
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(SubstrateToolRefusedError);
    const err = caught as SubstrateToolRefusedError;
    expect(err.tool).toBe("ScheduleDef");
    expect(err.message).toContain("required credential");
    expect(err.message).toContain("slack");
  });

  it("forwards bearer auth in the Authorization header", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ok: true })]);
    await client.scheduleDef({ op: "list", name: "job-search-template" });
    const headers = (fetchMock.mock.calls[0]![1] as RequestInit)
      .headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-bearer");
  });
});

// v1.x RFC G A2AServerCardDef — mirror of scheduleDef.
describe("a2aServerCardDef", () => {
  it("posts JSON to /v1/_a2aservercarddef and returns the row envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "ascd_abc",
        name: "billing-card",
        version: 1,
        promoted: true,
      }),
    ]);

    const result = (await client.a2aServerCardDef({
      op: "create",
      name: "billing-card",
      overlay: { description: "billing peer" },
    })) as Record<string, unknown>;

    expect(result.def_id).toBe("ascd_abc");
    expect(result.name).toBe("billing-card");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_a2aservercarddef");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("create");
  });

  it("raises SubstrateToolRefusedError on 422 with code=tool_refused", async () => {
    const refusalBody = JSON.stringify({
      code: "tool_refused",
      tool: "A2AServerCardDef",
      error: "create: name collides with a static a2a_server_cards entry",
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
      await client.a2aServerCardDef({ op: "create", name: "x", overlay: {} });
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(SubstrateToolRefusedError);
    expect((caught as SubstrateToolRefusedError).tool).toBe("A2AServerCardDef");
  });

  it("forwards bearer auth in the Authorization header", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ok: true })]);
    await client.a2aServerCardDef({ op: "list", name: "billing-card" });
    const headers = (fetchMock.mock.calls[0]![1] as RequestInit)
      .headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-bearer");
  });
});

// v1.x RFC G A2AAgentDef — mirror of scheduleDef.
describe("a2aAgentDef", () => {
  it("posts JSON to /v1/_a2aagentdef and returns the row envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "aad_abc",
        name: "remote-billing",
        version: 1,
        promoted: true,
      }),
    ]);

    const result = (await client.a2aAgentDef({
      op: "create",
      name: "remote-billing",
      overlay: { url: "https://peer.example/a2a" },
    })) as Record<string, unknown>;

    expect(result.def_id).toBe("aad_abc");
    expect(result.name).toBe("remote-billing");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_a2aagentdef");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("create");
  });

  it("raises AuthError on 401", async () => {
    const { client } = makeClient([errorResponse(401, "invalid token")]);
    await expect(
      client.a2aAgentDef({ op: "create", name: "x", overlay: {} }),
    ).rejects.toBeInstanceOf(AuthError);
  });

  it("forwards bearer auth in the Authorization header", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ok: true })]);
    await client.a2aAgentDef({ op: "list", name: "remote-billing" });
    const headers = (fetchMock.mock.calls[0]![1] as RequestInit)
      .headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-bearer");
  });
});

// RFC H WH-3 / mirrors the a2aAgentDef suite above.
describe("webhookDef", () => {
  it("posts JSON to /v1/_webhookdef and returns the row envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "whd_abc",
        name: "gh-push",
        version: 1,
        promoted: true,
      }),
    ]);

    const result = (await client.webhookDef({
      op: "create",
      name: "gh-push",
      overlay: { delivery: "spawn", agent: "ingest" },
    })) as Record<string, unknown>;

    expect(result.def_id).toBe("whd_abc");
    expect(result.name).toBe("gh-push");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_webhookdef");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("create");
  });

  it("raises AuthError on 401", async () => {
    const { client } = makeClient([errorResponse(401, "invalid token")]);
    await expect(
      client.webhookDef({ op: "create", name: "x", overlay: {} }),
    ).rejects.toBeInstanceOf(AuthError);
  });

  it("forwards bearer auth in the Authorization header", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ok: true })]);
    await client.webhookDef({ op: "list", name: "gh-push" });
    const headers = (fetchMock.mock.calls[0]![1] as RequestInit)
      .headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-bearer");
  });
});
