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

  // F14: the full capability set (channels / evaluation_scopes / interruption)
  // must reach the overlay so an MCP/HTTP-authored agent can be a complete
  // interactive/multi-agent agent, not just tool-bearing.
  it("passes channels / evaluation_scopes / interruption through the overlay", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ def_id: "def_abc", name: "coordinator", version: 1 }),
    ]);
    await client.agentDef({
      op: "create",
      name: "coordinator",
      overlay: {
        tools: ["Channel", "Evaluation", "Interruption"],
        evaluation_scopes: ["submit_self", "read_any"],
        channels: { publish: ["findings"], subscribe: ["tasks"] },
        interruption: { enabled: true, kinds: ["question"], max_pending: 3 },
      },
    });
    const body = JSON.parse(
      (fetchMock.mock.calls[0]![1] as RequestInit).body as string,
    );
    expect(body.overlay.evaluation_scopes).toEqual(["submit_self", "read_any"]);
    expect(body.overlay.channels).toEqual({
      publish: ["findings"],
      subscribe: ["tasks"],
    });
    expect(body.overlay.interruption).toEqual({
      enabled: true,
      kinds: ["question"],
      max_pending: 3,
    });
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

describe("memoryBackendDef", () => {
  it("posts JSON to /v1/_memorybackenddef and returns the row envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "mbd_abc",
        name: "primary",
        version: 1,
        promoted: true,
      }),
    ]);

    const result = (await client.memoryBackendDef({
      op: "create",
      name: "primary",
      overlay: { kind: "mem9", config: { base_url: "https://m.example.com", api_key_env: "LOOMCYCLE_M_KEY" } },
    })) as Record<string, unknown>;

    expect(result.def_id).toBe("mbd_abc");
    expect(result.name).toBe("primary");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_memorybackenddef");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("create");
  });

  it("raises AuthError on 401", async () => {
    const { client } = makeClient([errorResponse(401, "invalid token")]);
    await expect(
      client.memoryBackendDef({ op: "create", name: "x", overlay: {} }),
    ).rejects.toBeInstanceOf(AuthError);
  });

  it("forwards bearer auth in the Authorization header", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ok: true })]);
    await client.memoryBackendDef({ op: "list", name: "primary" });
    const headers = (fetchMock.mock.calls[0]![1] as RequestInit)
      .headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-bearer");
  });
});

describe("operatorTokenDef", () => {
  it("posts JSON to /v1/_operatortokendef and returns the row envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "otd_abc",
        name: "alice",
        tenant_id: "acme",
        subject: "alice",
        token: "lct_zN7kExample",
        token_suffix: "zN7kEx",
      }),
    ]);

    const result = (await client.operatorTokenDef({
      op: "create",
      name: "alice",
      tenant_id: "acme",
      scopes: ["runs:create", "runs:read"],
    } as Record<string, unknown>)) as Record<string, unknown>;

    expect(result.def_id).toBe("otd_abc");
    expect(result.token).toBe("lct_zN7kExample");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_operatortokendef");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("create");
    expect(body.tenant_id).toBe("acme");
  });

  it("raises AuthError on 401", async () => {
    const { client } = makeClient([errorResponse(401, "invalid token")]);
    await expect(
      client.operatorTokenDef({ op: "list", name: "alice" } as Record<string, unknown>),
    ).rejects.toBeInstanceOf(AuthError);
  });
});

describe("ensureMcpServer / mcpServerDefVerify (v0.18.0)", () => {
  it("create path: posts op:create with the overlay and returns changed=true on a fresh mint", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "mdf_1",
        name: "jobs",
        version: 1,
        retired: false,
        bootstrapped_from_static: false,
        created_at: "2026-06-02T00:00:00Z",
        promoted: true,
      }),
    ]);
    const r = await client.ensureMcpServer({
      name: "jobs",
      url: "http://localhost:3000/api/mcp",
      headers: {
        Authorization: "Bearer ${run.credentials.jobs:-${LOOMCYCLE_JOBS_SEARCH_API_TOKEN}}",
      },
    });
    expect(r).toEqual({ name: "jobs", defId: "mdf_1", version: 1, changed: true });

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toContain("/v1/_mcpserverdef");
    const body = JSON.parse((init as RequestInit).body as string);
    expect(body.op).toBe("create");
    expect(body.name).toBe("jobs");
    expect(body.overlay.transport).toBe("http");
    expect(body.overlay.url).toBe("http://localhost:3000/api/mcp");
    // header placeholder must be forwarded LITERAL (not resolved)
    expect(body.overlay.headers.Authorization).toContain("${run.credentials.jobs");
  });

  it("auto-discovery: surfaces discoveredToolCount from the create response (no rediscover)", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "mdf_1",
        name: "jobs",
        version: 1,
        retired: false,
        bootstrapped_from_static: false,
        created_at: "2026-06-03T00:00:00Z",
        promoted: true,
        discovered: 7, // loomcycle auto-discovered at ingestion
      }),
    ]);
    const r = await client.ensureMcpServer({ name: "jobs", url: "http://localhost:3000/api/mcp" });
    expect(r.changed).toBe(true);
    expect(r.discoveredToolCount).toBe(7);
    // Exactly one call — no separate rediscover needed for the count.
    expect(fetchMock.mock.calls.length).toBe(1);
  });

  it("dedup path: changed=false when create reports deduplicated", async () => {
    const { client } = makeClient([
      jsonResponse({
        def_id: "mdf_1",
        name: "jobs",
        version: 1,
        retired: false,
        bootstrapped_from_static: false,
        created_at: "2026-06-02T00:00:00Z",
        deduplicated: true,
      }),
    ]);
    const r = await client.ensureMcpServer({ name: "jobs", url: "http://localhost:3000/api/mcp" });
    expect(r.changed).toBe(false);
    expect(r.defId).toBe("mdf_1");
  });

  it("rediscover path: runs create then rediscover and surfaces discoveredToolCount", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "mdf_1",
        name: "jobs",
        version: 1,
        retired: false,
        bootstrapped_from_static: false,
        created_at: "2026-06-02T00:00:00Z",
        deduplicated: true,
      }),
      jsonResponse({ def_id: "mdf_2", name: "jobs", version: 2, discovered: 19 }),
    ]);
    const r = await client.ensureMcpServer({
      name: "jobs",
      url: "http://localhost:3000/api/mcp",
      rediscover: true,
    });
    expect(fetchMock.mock.calls.length).toBe(2);
    expect(JSON.parse((fetchMock.mock.calls[1]![1] as RequestInit).body as string).op).toBe(
      "rediscover",
    );
    // create deduped but rediscover minted a new version → changed
    expect(r.changed).toBe(true);
    expect(r.version).toBe(2);
    expect(r.discoveredToolCount).toBe(19);
  });

  it("mcpServerDefVerify posts op:verify and returns the typed result", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        matches: true,
        current_sha256: "sha256:abc",
        current_def_id: "mdf_1",
        version: 3,
        name: "jobs",
        deployed: true,
      }),
    ]);
    const r = await client.mcpServerDefVerify("jobs", "sha256:abc");
    expect(r.matches).toBe(true);
    expect(r.current_def_id).toBe("mdf_1");
    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body.op).toBe("verify");
    expect(body.content_sha256).toBe("sha256:abc");
  });
});

describe("ensureCodeAgent (v0.19.0, RFC J)", () => {
  it("posts op:create with a code-js overlay carrying the inline code_body", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "adf_1",
        name: "research-batch",
        version: 1,
        retired: false,
        bootstrapped_from_static: false,
        created_at: "2026-06-03T00:00:00Z",
        promoted: true,
      }),
    ]);
    const r = await client.ensureCodeAgent({
      name: "research-batch",
      code: "function run(input){ return {final_text: '{}'}; }",
      tools: ["Agent"],
      description: "deterministic batch orchestrator",
    });
    expect(r).toEqual({ name: "research-batch", defId: "adf_1", version: 1, changed: true });

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toContain("/v1/_agentdef");
    const body = JSON.parse((init as RequestInit).body as string);
    expect(body.op).toBe("create");
    expect(body.name).toBe("research-batch");
    expect(body.overlay.provider).toBe("code-js");
    expect(body.overlay.code_body).toContain("function run(input)");
    expect(body.overlay.tools).toEqual(["Agent"]);
    expect(body.description).toBe("deterministic batch orchestrator");
  });

  it("changed=false when create reports deduplicated", async () => {
    const { client } = makeClient([
      jsonResponse({
        def_id: "adf_1",
        name: "research-batch",
        version: 1,
        retired: false,
        bootstrapped_from_static: false,
        created_at: "2026-06-03T00:00:00Z",
        deduplicated: true,
      }),
    ]);
    const r = await client.ensureCodeAgent({
      name: "research-batch",
      code: "function run(input){ return {final_text: '{}'}; }",
    });
    expect(r.changed).toBe(false);
    expect(r.defId).toBe("adf_1");
  });
});
