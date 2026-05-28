/**
 * LoomcycleClient method-level tests. One block per method group;
 * each block asserts (a) the request URL + headers + body are
 * correct, (b) the response is unwrapped into the documented
 * surface shape.
 *
 * The fetch mock is supplied via constructor; no global fetch
 * monkey-patching. Auth header is asserted on the first test in
 * each block (consistent across methods).
 */

import { describe, expect, it } from "vitest";
import {
  errorResponse,
  jsonResponse,
  makeClient,
  noContentResponse,
  sseResponse,
} from "./helpers.js";

describe("runStreaming", () => {
  it("POSTs to /v1/runs with bearer + SSE Accept + yields parsed events", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse([
        'event: started\ndata: {"type":"started"}\n\n',
        'event: text\ndata: {"type":"text","text":"hi"}\n\n',
        'event: done\ndata: {"type":"done","stop_reason":"end_turn"}\n\n',
      ]),
    ]);

    const events = [];
    for await (const ev of client.runStreaming({
      agent: "qa-agent",
      segments: [{ role: "user", content: [{ type: "trusted-text", text: "hi" }] }],
      allowedTools: ["Read"],
    })) {
      events.push(ev);
    }

    expect(events.length).toBe(3);
    expect(events[0]!.type).toBe("started");
    expect(events[2]!.stop_reason).toBe("end_turn");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/runs");
    expect(call[1]!.method).toBe("POST");
    const headers = call[1]!.headers as Record<string, string>;
    expect(headers["Authorization"]).toBe("Bearer test-bearer");
    expect(headers["Accept"]).toBe("text/event-stream, application/json");
    const body = JSON.parse(call[1]!.body as string);
    expect(body).toEqual({
      agent: "qa-agent",
      segments: [{ role: "user", content: [{ type: "trusted-text", text: "hi" }] }],
      allowed_tools: ["Read"],
    });
  });

  it("throws AuthError on 401 before yielding any events", async () => {
    const { AuthError } = await import("../src/errors.js");
    const { client } = makeClient([errorResponse(401, "invalid token")]);
    let caught: unknown;
    try {
      for await (const _ of client.runStreaming({ agent: "x", segments: [] })) {
        // unreachable
      }
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(AuthError);
    expect((caught as { status: number }).status).toBe(401);
  });

  it("throws BackpressureError on 429", async () => {
    const { BackpressureError } = await import("../src/errors.js");
    const { client } = makeClient([errorResponse(429, "queue full")]);
    let caught: unknown;
    try {
      for await (const _ of client.runStreaming({ agent: "x", segments: [] })) {}
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(BackpressureError);
  });

  it("forwards the v0.4+/v0.8.x wire fields when set", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse(['event: done\ndata: {"type":"done"}\n\n']),
    ]);
    for await (const _ of client.runStreaming({
      agent: "qa",
      segments: [],
      allowedHosts: ["example.com"],
      webSearchFilter: "drop",
      sessionId: "s1",
      tenantId: "t1",
      userId: "u1",
      agentId: "a1",
      userTier: "high",
      userBearer: "user-token-xyz",
    })) {}
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body).toEqual({
      agent: "qa",
      segments: [],
      allowed_hosts: ["example.com"],
      web_search_filter: "drop",
      session_id: "s1",
      tenant_id: "t1",
      user_id: "u1",
      agent_id: "a1",
      user_tier: "high",
      user_bearer: "user-token-xyz",
    });
  });

  // v1.x RFC F per-tool credentials map.
  it("forwards user_credentials when set", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse(['event: done\ndata: {"type":"done"}\n\n']),
    ]);
    for await (const _ of client.runStreaming({
      agent: "qa",
      segments: [],
      userCredentials: {
        jobs: "jobs-bearer",
        slack: "xoxb-slack",
        telegram: "tg-token",
      },
    })) {}
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body.user_credentials).toEqual({
      jobs: "jobs-bearer",
      slack: "xoxb-slack",
      telegram: "tg-token",
    });
  });

  // omitted-when-undefined contract: existing callers see no wire shape change.
  it("omits user_credentials when undefined (no shape change for v0.8.x callers)", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse(['event: done\ndata: {"type":"done"}\n\n']),
    ]);
    for await (const _ of client.runStreaming({
      agent: "qa",
      segments: [],
      userBearer: "legacy-bearer",
    })) {}
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect("user_credentials" in body).toBe(false);
    expect(body.user_bearer).toBe("legacy-bearer");
  });

  it("omits allowed_hosts when null (pass-through semantics)", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse(['event: done\ndata: {"type":"done"}\n\n']),
    ]);
    for await (const _ of client.runStreaming({
      agent: "qa",
      segments: [],
      allowedHosts: null,
    })) {}
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect("allowed_hosts" in body).toBe(false);
  });

  it("sends allowed_hosts: [] for deny-all", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse(['event: done\ndata: {"type":"done"}\n\n']),
    ]);
    for await (const _ of client.runStreaming({
      agent: "qa",
      segments: [],
      allowedHosts: [],
    })) {}
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body.allowed_hosts).toEqual([]);
  });

  it("parses retry / host_widened / session / agent side-channel events", async () => {
    const { client } = makeClient([
      sseResponse([
        'event: started\ndata: {"type":"started"}\n\n',
        'event: retry\ndata: {"type":"retry","retry":{"provider":"anthropic","attempt":1,"wait_ms":2000,"reason":"retry-after header"}}\n\n',
        // sendRaw side-channel: JSON payload has NO `type` field; parser
        // backfills it from the SSE event name.
        'event: session\ndata: {"text":"s-123"}\n\n',
        'event: agent\ndata: {"agent_id":"a-1","run_id":"r-1","session_id":"s-123","parent_agent_id":null}\n\n',
        'event: host_widened\ndata: {"type":"host_widened","host_widening":{"tool_call_id":"tc1","tool_name":"WebFetch","url":"https://x.com/a","hook_owner":"jobs","hook_name":"narrow","hosts_added":["x.com"]}}\n\n',
        'event: done\ndata: {"type":"done"}\n\n',
      ]),
    ]);
    const events = [];
    for await (const ev of client.runStreaming({ agent: "qa", segments: [] })) {
      events.push(ev);
    }
    expect(events.map((e) => e.type)).toEqual([
      "started",
      "retry",
      "session",
      "agent",
      "host_widened",
      "done",
    ]);
    expect(events[1]!.retry?.provider).toBe("anthropic");
    expect(events[1]!.retry?.wait_ms).toBe(2000);
    expect(events[2]!.text).toBe("s-123");
    expect(events[3]!.agent_id).toBe("a-1");
    expect(events[3]!.parent_agent_id).toBeNull();
    expect(events[4]!.host_widening?.hosts_added).toEqual(["x.com"]);
  });
});

describe("continueSession", () => {
  it("POSTs to /v1/sessions/{id}/messages with body shape", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse(['event: text\ndata: {"type":"text","text":"hi"}\n\n']),
    ]);
    const events = [];
    for await (const ev of client.continueSession({
      sessionId: "sess-1",
      segments: [{ role: "user", content: [{ type: "trusted-text", text: "again" }] }],
    })) {
      events.push(ev);
    }
    expect(events.length).toBe(1);
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/sessions/sess-1/messages",
    );
  });

  it("encodes session_id in the URL path", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse([]),
    ]);
    const it = client.continueSession({
      sessionId: "sess with spaces",
      segments: [],
    });
    await it[Symbol.asyncIterator]().next();
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/sessions/sess%20with%20spaces/messages",
    );
  });

  it("forwards continuation-side wire fields when set", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse(['event: done\ndata: {"type":"done"}\n\n']),
    ]);
    for await (const _ of client.continueSession({
      sessionId: "s1",
      segments: [],
      allowedHosts: ["a.com", "b.com"],
      webSearchFilter: "keep",
      agentId: "a2",
      userTier: "free",
      userBearer: "cont-token",
    })) {}
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body).toEqual({
      segments: [],
      allowed_hosts: ["a.com", "b.com"],
      web_search_filter: "keep",
      agent_id: "a2",
      user_tier: "free",
      user_bearer: "cont-token",
    });
  });

  // v1.x RFC F per-tool credentials map on continuations — mirrors
  // the runStreaming test above; ensures no future refactor accidentally
  // drops the field from one path while keeping the other.
  it("forwards user_credentials on continueSession when set", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse(['event: done\ndata: {"type":"done"}\n\n']),
    ]);
    for await (const _ of client.continueSession({
      sessionId: "s1",
      segments: [],
      userCredentials: {
        jobs: "jobs-bearer",
        slack: "xoxb-slack",
      },
    })) {}
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body.user_credentials).toEqual({
      jobs: "jobs-bearer",
      slack: "xoxb-slack",
    });
  });
});

describe("getAgent / cancelAgent / listUserAgents", () => {
  it("getAgent GETs /v1/agents/{id} and returns the parsed body", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        agent_id: "a1",
        run_id: "r1",
        session_id: "s1",
        agent: "qa",
        parent_agent_id: null,
        user_id: "u1",
        status: "running",
        started_at: "2026-05-19T00:00:00Z",
        completed_at: null,
        stop_reason: null,
        error: null,
        usage: { input_tokens: 100, output_tokens: 50 },
        last_heartbeat_at: null,
        live: true,
      }),
    ]);

    const agent = await client.getAgent("a1");
    expect(agent.agent_id).toBe("a1");
    expect(agent.status).toBe("running");
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/agents/a1",
    );
    expect(fetchMock.mock.calls[0]![1]!.method).toBe("GET");
  });

  it("getAgent throws AgentNotFoundError on 404", async () => {
    const { AgentNotFoundError } = await import("../src/errors.js");
    const { client } = makeClient([
      errorResponse(404, "no run found for agent_id"),
    ]);
    await expect(client.getAgent("a-missing")).rejects.toBeInstanceOf(
      AgentNotFoundError,
    );
  });

  it("cancelAgent POSTs reason + returns cancelledCount", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ cancelled_count: 3 }),
    ]);
    const result = await client.cancelAgent("a1", { reason: "user requested" });
    expect(result.cancelledCount).toBe(3);
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body).toEqual({ reason: "user requested" });
  });

  it("listUserAgents filters by status query param", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ agents: [] }),
    ]);
    await client.listUserAgents("u1", { status: "running" });
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/users/u1/agents?status=running",
    );
  });
});

describe("getTranscript / health / listUsers", () => {
  it("getTranscript GETs /v1/sessions/{id}/transcript", async () => {
    const { client } = makeClient([
      jsonResponse({
        session: { id: "s1", user_id: "u1", agent: "qa", created_at: "2026-05-19T00:00:00Z" },
        events: [],
      }),
    ]);
    const t = await client.getTranscript("s1");
    expect(t.session.id).toBe("s1");
    expect(t.events).toEqual([]);
  });

  it("health hits /healthz (no /v1 prefix)", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ ok: true, commit: "abc", built: "2026-05-19", uptime_seconds: 42 }),
    ]);
    const h = await client.health();
    expect(h.ok).toBe(true);
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/healthz");
  });

  it("listUsers returns the users array shape", async () => {
    const { client } = makeClient([
      jsonResponse({
        users: [
          { user_id: "u1", running_count: 1, total_count: 5, last_started_at: "2026-05-19T00:00:00Z" },
        ],
      }),
    ]);
    const r = await client.listUsers();
    expect(r.users.length).toBe(1);
    expect(r.users[0]!.user_id).toBe("u1");
  });
});

describe("pauseRuntime / resumeRuntime / getRuntimeState", () => {
  it("pauseRuntime POSTs /v1/_pause with optional timeout_ms", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        state: "paused",
        duration_ms: 42,
        force_cancelled_count: 1,
        paused_runs_count: 2,
      }),
    ]);
    const r = await client.pauseRuntime({ timeoutMs: 5000 });
    expect(r.state).toBe("paused");
    expect(r.paused_runs_count).toBe(2);
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body).toEqual({ timeout_ms: 5000 });
  });

  it("pauseRuntime without timeoutMs sends no body", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ state: "paused", duration_ms: 0, force_cancelled_count: 0, paused_runs_count: 0 }),
    ]);
    await client.pauseRuntime();
    expect(fetchMock.mock.calls[0]![1]!.body).toBeUndefined();
  });

  it("pauseRuntime throws AlreadyPausingError on 409", async () => {
    const { AlreadyPausingError } = await import("../src/errors.js");
    const { client } = makeClient([
      errorResponse(409, "already_pausing: runtime is already paused"),
    ]);
    await expect(client.pauseRuntime()).rejects.toBeInstanceOf(AlreadyPausingError);
  });

  it("pauseRuntime throws PauseNotConfiguredError on 503", async () => {
    const { PauseNotConfiguredError, UnavailableError } = await import(
      "../src/errors.js"
    );
    const { client } = makeClient([
      errorResponse(503, "pause manager not configured on this server"),
    ]);
    let caught: unknown;
    try {
      await client.pauseRuntime();
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(PauseNotConfiguredError);
    // back-compat: PauseNotConfiguredError is-a UnavailableError
    expect(caught).toBeInstanceOf(UnavailableError);
  });

  it("resumeRuntime throws NotPausedError on 409", async () => {
    const { NotPausedError } = await import("../src/errors.js");
    const { client } = makeClient([
      errorResponse(409, "not_paused: runtime is not paused"),
    ]);
    await expect(client.resumeRuntime()).rejects.toBeInstanceOf(NotPausedError);
  });

  it("getRuntimeState GETs /v1/_state", async () => {
    const { client } = makeClient([
      jsonResponse({ state: "paused", paused_runs_count: 3 }),
    ]);
    const s = await client.getRuntimeState();
    expect(s.state).toBe("paused");
    expect(s.paused_runs_count).toBe(3);
  });
});

describe("snapshot lifecycle", () => {
  it("createSnapshot POSTs label + flags", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        id: "snap_abc",
        created_at: "2026-05-19T00:00:00Z",
        label: "before-deploy",
        schema_version: 1,
        byte_size: 1024,
      }),
    ]);
    const r = await client.createSnapshot({
      label: "before-deploy",
      includeHistory: true,
      includeHistorySince: "2026-05-01T00:00:00Z",
      maxBytes: 1048576,
    });
    expect(r.id).toBe("snap_abc");
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body).toEqual({
      label: "before-deploy",
      include_history: true,
      include_history_since: "2026-05-01T00:00:00Z",
      max_bytes: 1048576,
    });
  });

  it("createSnapshot throws SnapshotTooLargeError on 413", async () => {
    const { SnapshotTooLargeError } = await import("../src/errors.js");
    const { client } = makeClient([
      errorResponse(413, "snapshot exceeds size cap"),
    ]);
    await expect(client.createSnapshot()).rejects.toBeInstanceOf(
      SnapshotTooLargeError,
    );
  });

  it("listSnapshots GETs with limit + label_contains params", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ entries: [{ id: "snap_a", created_at: "x", schema_version: 1, byte_size: 1 }] }),
    ]);
    const r = await client.listSnapshots({ limit: 10, labelContains: "prod" });
    expect(r.length).toBe(1);
    expect(fetchMock.mock.calls[0]![0]).toContain("limit=10");
    expect(fetchMock.mock.calls[0]![0]).toContain("label_contains=prod");
  });

  it("getSnapshot returns the full envelope including json_content", async () => {
    const { client } = makeClient([
      jsonResponse({
        id: "snap_x",
        created_at: "x",
        schema_version: 1,
        byte_size: 100,
        json_content: { sections: { memory: { version: "1.0", entries: [] } } },
      }),
    ]);
    const env = await client.getSnapshot("snap_x");
    expect(env.id).toBe("snap_x");
    expect((env.json_content as { sections: unknown }).sections).toBeDefined();
  });

  it("getSnapshot throws SnapshotNotFoundError on 404", async () => {
    const { SnapshotNotFoundError } = await import("../src/errors.js");
    const { client } = makeClient([
      errorResponse(404, "no snapshot with id snap_nope"),
    ]);
    await expect(client.getSnapshot("snap_nope")).rejects.toBeInstanceOf(
      SnapshotNotFoundError,
    );
  });

  it("exportSnapshotURL returns the URL synchronously (no fetch)", () => {
    const { client, fetchMock } = makeClient([]);
    const url = client.exportSnapshotURL("snap_abc");
    expect(url).toBe("http://test-loomcycle:8787/v1/_snapshots/snap_abc/export");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("restoreSnapshot with snapshotId posts to /v1/_snapshots/{id}/restore", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ memory_restored: 3, paused_runs_restored: 1 }),
    ]);
    const r = await client.restoreSnapshot({ snapshotId: "snap_xyz" });
    expect(r.memory_restored).toBe(3);
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_snapshots/snap_xyz/restore",
    );
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body).toEqual({});
  });

  it("restoreSnapshot with inline json uses 'inline' path segment + json body", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ memory_restored: 0 }),
    ]);
    const envelope = { schema_version: 1, sections: {} };
    await client.restoreSnapshot({ json: envelope, includeHistory: true });
    expect(fetchMock.mock.calls[0]![0]).toContain("/v1/_snapshots/inline/restore");
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body).toEqual({ include_history: true, json: envelope });
  });

  it("restoreSnapshot requires snapshotId or json (not both, not neither)", async () => {
    const { client } = makeClient([]);
    await expect(client.restoreSnapshot({})).rejects.toThrow(/snapshotId or json/);
    await expect(
      client.restoreSnapshot({ snapshotId: "x", json: {} }),
    ).rejects.toThrow(/only one/);
  });

  it("restoreSnapshot throws SnapshotVersionError on 422", async () => {
    const { SnapshotVersionError } = await import("../src/errors.js");
    const { client } = makeClient([
      errorResponse(422, "snapshot section version newer than reader supports"),
    ]);
    await expect(
      client.restoreSnapshot({ snapshotId: "x" }),
    ).rejects.toBeInstanceOf(SnapshotVersionError);
  });

  it("deleteSnapshot DELETEs and returns void on 204", async () => {
    const { client, fetchMock } = makeClient([noContentResponse()]);
    await client.deleteSnapshot("snap_x");
    expect(fetchMock.mock.calls[0]![1]!.method).toBe("DELETE");
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_snapshots/snap_x",
    );
  });
});

describe("memory admin", () => {
  it("listMemoryScopes GETs the scopes index", async () => {
    const { client } = makeClient([
      jsonResponse({ scopes: [{ name: "agent", description: "..." }] }),
    ]);
    const r = await client.listMemoryScopes();
    expect(r.scopes.length).toBe(1);
  });

  it("listMemoryEntries encodes prefix + limit", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ scope: "agent", scope_id: "a1", entries: [], truncated: false }),
    ]);
    await client.listMemoryEntries("agent", "a1", { prefix: "events/", limit: 50 });
    const url = fetchMock.mock.calls[0]![0];
    expect(url).toContain("/v1/_memory/scopes/agent/a1/keys");
    expect(url).toContain("prefix=events%2F");
    expect(url).toContain("limit=50");
  });

  it("getMemoryEntry encodes path segments", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ scope: "agent", scope_id: "a1", entry: { key: "k", value: 1, created_at: "x", updated_at: "x" } }),
    ]);
    await client.getMemoryEntry("agent", "a1", "events/2026");
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_memory/scopes/agent/a1/keys/events%2F2026",
    );
  });
});

describe("interruption", () => {
  it("listUserInterrupts defaults to status=pending", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ interrupts: [], total: 0 }),
    ]);
    await client.listUserInterrupts("u1");
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/users/u1/interrupts?status=pending",
    );
  });

  it("resolveInterrupt posts the kind+answer+resolvedBy body", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ok: true })]);
    await client.resolveInterrupt("r1", "intr_xyz", {
      answer: "yes",
      resolvedBy: "operator-dashboard",
    });
    const url = fetchMock.mock.calls[0]![0];
    expect(url).toBe(
      "http://test-loomcycle:8787/v1/runs/r1/interrupts/intr_xyz/resolve",
    );
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body).toEqual({
      kind: "question",
      answer: "yes",
      resolved_by: "operator-dashboard",
    });
  });

  // Regression: PR #141 review surfaced that 404 on the interrupt
  // routes had no test coverage. The new NotFoundError base ensures
  // an interrupt 404 (which doesn't mention "agent" in the body)
  // falls through to NotFoundError, not AgentNotFoundError.
  it("listRunInterrupts on 404 raises NotFoundError (not AgentNotFoundError)", async () => {
    const { client } = makeClient([errorResponse(404, "run not found")]);
    await expect(client.listRunInterrupts("missing-run")).rejects.toMatchObject({
      name: "NotFoundError",
      status: 404,
    });
  });

  it("resolveInterrupt on 404 raises NotFoundError when interrupt is unknown", async () => {
    const { client } = makeClient([
      errorResponse(404, "interrupt not found"),
    ]);
    await expect(
      client.resolveInterrupt("r1", "intr_gone", { answer: "yes" }),
    ).rejects.toMatchObject({ name: "NotFoundError", status: 404 });
  });
});

describe("hook management", () => {
  it("registerHook POSTs the snake_case body shape", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ id: "hook_abc" }),
    ]);
    const resp = await client.registerHook({
      owner: "jobs-search-web",
      name: "scan",
      phase: "pre",
      tools: ["WebFetch"],
      callbackUrl: "https://callback.local/h",
      failMode: "closed",
      timeoutMs: 2500,
    });
    expect(resp.id).toBe("hook_abc");
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/hooks");
    expect(call[1]!.method).toBe("POST");
    const body = JSON.parse(call[1]!.body as string);
    expect(body).toEqual({
      owner: "jobs-search-web",
      name: "scan",
      phase: "pre",
      tools: ["WebFetch"],
      callback_url: "https://callback.local/h",
      fail_mode: "closed",
      timeout_ms: 2500,
    });
  });

  it("registerHook omits optional fields when not set", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ id: "hook_min" }),
    ]);
    await client.registerHook({
      owner: "x",
      name: "y",
      phase: "post",
      callbackUrl: "https://e.test/h",
    });
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    // Omitted: agents, tools, fail_mode (defaults at server), timeout_ms.
    expect(body).toEqual({
      owner: "x",
      name: "y",
      phase: "post",
      callback_url: "https://e.test/h",
    });
  });

  it("registerHook on 400 raises InvalidArgumentError", async () => {
    const { client } = makeClient([
      errorResponse(400, "invalid_registration: callback_url required"),
    ]);
    await expect(
      client.registerHook({
        owner: "x",
        name: "y",
        phase: "pre",
        callbackUrl: "",
      }),
    ).rejects.toMatchObject({ name: "InvalidArgumentError", status: 400 });
  });

  it("listHooks unwraps the envelope and returns the array", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        hooks: [
          {
            id: "hook_a",
            owner: "x",
            name: "y",
            phase: "pre",
            agents: ["*"],
            tools: ["WebFetch"],
            callback_url: "https://e.test/h",
            fail_mode: "open",
            timeout_ms: 0,
            registered_at: "2026-05-19T11:00:00Z",
          },
        ],
      }),
    ]);
    const hooks = await client.listHooks();
    expect(hooks).toHaveLength(1);
    expect(hooks[0]!.id).toBe("hook_a");
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/hooks");
    expect(fetchMock.mock.calls[0]![1]!.method).toBe("GET");
  });

  it("deleteHook DELETEs the encoded id and returns void", async () => {
    const { client, fetchMock } = makeClient([noContentResponse()]);
    await client.deleteHook("hook_with spaces");
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/hooks/hook_with%20spaces",
    );
    expect(fetchMock.mock.calls[0]![1]!.method).toBe("DELETE");
  });

  it("deleteHook on 404 raises HookNotFoundError", async () => {
    const { client } = makeClient([
      errorResponse(404, `no hook with id "hook_gone"`),
    ]);
    await expect(client.deleteHook("hook_gone")).rejects.toMatchObject({
      name: "HookNotFoundError",
      status: 404,
    });
  });

  // Regression: the 404 dispatch in fetch-helpers.ts puts "hook" BEFORE
  // "agent" in the keyword priority. Without the new branch, the hook
  // 404 body — which doesn't say "agent" — would fall through to
  // NotFoundError (base); this test pins the typed-error class.
  it("deleteHook 404 with mixed-case keyword still routes to HookNotFoundError", async () => {
    const { client } = makeClient([
      errorResponse(404, "No HOOK with id matches"),
    ]);
    await expect(client.deleteHook("hook_x")).rejects.toMatchObject({
      name: "HookNotFoundError",
    });
  });

  // Priority guard: when the 404 body contains BOTH "hook" and "agent"
  // (e.g. a hook id like "hook_agent_scan"), the "hook" check must
  // still win. Hook IDs containing "agent" are not unusual — anyone
  // registering a hook for an agent-related concern would name it
  // that way. Pins the keyword-ordering invariant in raiseFromResponse.
  it("deleteHook 404 with body containing both 'hook' and 'agent' routes to HookNotFoundError", async () => {
    const { client } = makeClient([
      errorResponse(404, `no hook with id "hook_agent_scan"`),
    ]);
    await expect(client.deleteHook("hook_agent_scan")).rejects.toMatchObject({
      name: "HookNotFoundError",
      status: 404,
    });
  });
});

describe("v0.9.x n8n RFC Phase 0 — listChannels + streamUserRunStates", () => {
  it("listChannels GETs /v1/_channels and returns the envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        channels: [
          {
            name: "_system/alarms",
            scope: "global",
            semantic: "broadcast",
            publisher: "system",
            message_count: 4,
            oldest_visible_at: "2026-05-20T10:00:00Z",
            newest_visible_at: "2026-05-21T15:30:00Z",
          },
          { name: "scratch", message_count: 0 },
        ],
      }),
    ]);
    const resp = await client.listChannels();
    expect(resp.channels).toHaveLength(2);
    expect(resp.channels[0]?.name).toBe("_system/alarms");
    expect(resp.channels[0]?.message_count).toBe(4);
    expect(resp.channels[1]?.message_count).toBe(0);
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_channels",
    );
    expect((fetchMock.mock.calls[0]![1]!.headers as Record<string, string>).Authorization).toBe(
      "Bearer test-bearer",
    );
  });

  it("streamUserRunStates yields stream_open then run_state frames", async () => {
    const frames = [
      `event: stream_open\ndata: ${JSON.stringify({
        user_id: "user-a",
        filter_status: null,
        filter_agent: "",
        keepalive_interval: 25,
      })}\n\n`,
      `event: run_state\ndata: ${JSON.stringify({
        run_id: "r1",
        agent_id: "ag1",
        agent: "researcher",
        user_id: "user-a",
        status: "running",
        ts: "2026-05-22T00:00:00Z",
      })}\n\n`,
      `event: run_state\ndata: ${JSON.stringify({
        run_id: "r1",
        agent_id: "ag1",
        agent: "researcher",
        user_id: "user-a",
        status: "completed",
        stop_reason: "end_turn",
        ts: "2026-05-22T00:00:01Z",
      })}\n\n`,
    ];
    const { client, fetchMock } = makeClient([sseResponse(frames)]);

    const items = [];
    for await (const item of client.streamUserRunStates("user-a")) {
      items.push(item);
    }
    expect(items).toHaveLength(3);
    expect(items[0]?.kind).toBe("open");
    expect(items[1]?.kind).toBe("event");
    if (items[1]?.kind === "event") {
      expect(items[1].payload.run_id).toBe("r1");
      expect(items[1].payload.status).toBe("running");
    }
    if (items[2]?.kind === "event") {
      expect(items[2].payload.status).toBe("completed");
      expect(items[2].payload.stop_reason).toBe("end_turn");
    }
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/users/user-a/agents/stream",
    );
  });

  it("streamUserRunStates encodes status + agent filters as query params", async () => {
    const { client, fetchMock } = makeClient([sseResponse([])]);
    const iter = client.streamUserRunStates("user-a", {
      statuses: ["completed", "failed"],
      agent: "writer",
    });
    for await (const _ of iter) {
      // drain
    }
    const url = fetchMock.mock.calls[0]![0] as string;
    expect(url).toContain("/v1/users/user-a/agents/stream?");
    expect(url).toContain("status=completed%2Cfailed");
    expect(url).toContain("agent=writer");
  });

  it("streamUserRunStates ignores keepalive comment lines", async () => {
    const frames = [
      ": keepalive\n\n",
      `event: run_state\ndata: ${JSON.stringify({
        run_id: "r1",
        agent_id: "ag1",
        agent: "x",
        user_id: "user-a",
        status: "running",
        ts: "2026-05-22T00:00:00Z",
      })}\n\n`,
    ];
    const { client } = makeClient([sseResponse(frames)]);
    const items = [];
    for await (const item of client.streamUserRunStates("user-a")) {
      items.push(item);
    }
    expect(items).toHaveLength(1);
    expect(items[0]?.kind).toBe("event");
  });

  it("streamUserRunStates raises typed error on 401", async () => {
    const { client } = makeClient([errorResponse(401, "bad bearer")]);
    await expect(async () => {
      for await (const _ of client.streamUserRunStates("user-a")) {
        // unreached
      }
    }).rejects.toMatchObject({ name: "AuthError", status: 401 });
  });
});

describe("v0.9.x Channel CRUD — publish / subscribe / peek / ack", () => {
  it("publishChannel scope=global POSTs to /v1/_channels/{name}/publish", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        msg_id: "msg_abc123",
        channel: "team-updates",
        created_at: "2026-05-22T12:00:00.000Z",
      }),
    ]);
    const resp = await client.publishChannel("team-updates", {
      scope: "global",
      payload: { event: "hello" },
    });
    expect(resp.msg_id).toBe("msg_abc123");
    expect(resp.channel).toBe("team-updates");
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_channels/team-updates/publish",
    );
    expect(fetchMock.mock.calls[0]![1]!.method).toBe("POST");
    expect(JSON.parse(fetchMock.mock.calls[0]![1]!.body as string)).toEqual({
      payload: { event: "hello" },
    });
  });

  it("publishChannel scope=user uses /v1/users/{userId}/channels/{name}/publish", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        msg_id: "msg_def456",
        channel: "inbox",
        created_at: "2026-05-22T12:00:00.000Z",
      }),
    ]);
    await client.publishChannel("inbox", {
      scope: "user",
      userId: "alice",
      payload: { subject: "hi" },
    });
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/users/alice/channels/inbox/publish",
    );
  });

  it("publishChannel scope=user without userId throws synchronously", async () => {
    const { client } = makeClient([]);
    await expect(
      client.publishChannel("inbox", {
        scope: "user",
        payload: {},
      } as never),
    ).rejects.toThrow(/userId/);
  });

  it("publishChannel maps wire 404 to NotFoundError", async () => {
    const { client } = makeClient([
      errorResponse(404, "channel team-updates not declared in operator yaml"),
    ]);
    await expect(
      client.publishChannel("team-updates", {
        scope: "global",
        payload: {},
      }),
    ).rejects.toMatchObject({ name: "NotFoundError", status: 404 });
  });

  it("subscribeChannel POSTs the long-poll body and returns the batch", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        channel: "team-updates",
        messages: [
          {
            id: "msg_1",
            value: { event: "first" },
            published_at: "2026-05-22T12:00:00.000Z",
          },
        ],
        next_cursor: "cur_xyz",
      }),
    ]);
    const resp = await client.subscribeChannel("team-updates", {
      scope: "global",
      waitMs: 5000,
      maxMessages: 25,
    });
    expect(resp.messages).toHaveLength(1);
    expect(resp.next_cursor).toBe("cur_xyz");
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_channels/team-updates/subscribe",
    );
    expect(JSON.parse(fetchMock.mock.calls[0]![1]!.body as string)).toEqual({
      wait_ms: 5000,
      max_messages: 25,
    });
  });

  it("peekChannel GETs with query params and never POSTs", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        channel: "team-updates",
        messages: [
          { id: "msg_1", value: { x: 1 }, published_at: "2026-05-22T12:00:00.000Z" },
        ],
      }),
    ]);
    const resp = await client.peekChannel("team-updates", {
      scope: "global",
      maxMessages: 5,
      fromCursor: "cur_0",
    });
    expect(resp.messages).toHaveLength(1);
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_channels/team-updates/peek?from_cursor=cur_0&max_messages=5",
    );
    expect(fetchMock.mock.calls[0]![1]!.method).toBe("GET");
  });

  it("ackChannel POSTs the cursor and maps 409 to ConflictError", async () => {
    // First call: happy path.
    const happy = makeClient([jsonResponse({ ok: true })]);
    const r = await happy.client.ackChannel("team-updates", {
      scope: "global",
      cursor: "cur_xyz",
    });
    expect(r.ok).toBe(true);
    expect(happy.fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_channels/team-updates/ack",
    );

    // Second call: cursor regression. The server returns
    // `code: channel_cursor_regression` in the JSON body; the
    // adapter dispatches that to the typed ChannelCursorRegressionError.
    const conflict = makeClient([
      errorResponse(
        409,
        JSON.stringify({
          code: "channel_cursor_regression",
          error: "cursor older than committed",
        }),
      ),
    ]);
    await expect(
      conflict.client.ackChannel("team-updates", {
        scope: "global",
        cursor: "cur_0",
      }),
    ).rejects.toMatchObject({
      name: "ChannelCursorRegressionError",
      status: 409,
    });
  });

  it("channel names with slashes are URL-encoded", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        msg_id: "msg_1",
        channel: "findings/alpha",
        created_at: "2026-05-22T12:00:00.000Z",
      }),
    ]);
    await client.publishChannel("findings/alpha", {
      scope: "global",
      payload: {},
    });
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_channels/findings%2Falpha/publish",
    );
  });
});

describe("v0.9.x n8n polish — debug toggle + parentAgentId filter", () => {
  it("runStreaming yields no _meta events when debug is omitted (default)", async () => {
    const { client } = makeClient([
      sseResponse([
        'event: text\ndata: {"type":"text","text":"hi"}\n\n',
        'event: done\ndata: {"type":"done"}\n\n',
      ]),
    ]);
    const types: string[] = [];
    for await (const ev of client.runStreaming({
      agent: "qa",
      segments: [],
    })) {
      types.push(ev.type);
    }
    expect(types).toEqual(["text", "done"]);
  });

  it("runStreaming with debug:true brackets the real events with synthetic _meta open + close", async () => {
    const { client } = makeClient([
      sseResponse([
        'event: text\ndata: {"type":"text","text":"hi"}\n\n',
        'event: done\ndata: {"type":"done"}\n\n',
      ]),
    ]);
    const events = [];
    for await (const ev of client.runStreaming({
      agent: "qa",
      segments: [],
      debug: true,
    })) {
      events.push(ev);
    }
    expect(events.length).toBe(4);
    expect(events[0]!.type).toBe("_meta");
    expect(events[0]!.meta_subtype).toBe("stream_open");
    expect(events[1]!.type).toBe("text");
    expect(events[2]!.type).toBe("done");
    expect(events[3]!.type).toBe("_meta");
    expect(events[3]!.meta_subtype).toBe("stream_close");
    expect(events[3]!.meta_reason).toBe("eof");
  });

  it("continueSession debug:true wraps the inner stream too", async () => {
    const { client } = makeClient([
      sseResponse(['event: done\ndata: {"type":"done"}\n\n']),
    ]);
    const events = [];
    for await (const ev of client.continueSession({
      sessionId: "s1",
      segments: [],
      debug: true,
    })) {
      events.push(ev);
    }
    expect(events.map((e) => e.type)).toEqual([
      "_meta",
      "done",
      "_meta",
    ]);
    expect(events[0]!.meta_subtype).toBe("stream_open");
    expect(events[2]!.meta_subtype).toBe("stream_close");
  });

  it("streamUserRunStates with debug:true yields a kind=close item on EOF", async () => {
    const { client } = makeClient([
      sseResponse([
        `event: run_state\ndata: ${JSON.stringify({
          run_id: "r1",
          agent_id: "ag1",
          agent: "x",
          user_id: "u",
          status: "running",
          ts: "2026-05-22T00:00:00Z",
        })}\n\n`,
      ]),
    ]);
    const kinds = [];
    for await (const item of client.streamUserRunStates("u", { debug: true })) {
      kinds.push(item.kind);
    }
    expect(kinds).toEqual(["event", "close"]);
  });

  it("streamUserRunStates without debug yields no close item", async () => {
    const { client } = makeClient([
      sseResponse([
        `event: run_state\ndata: ${JSON.stringify({
          run_id: "r1",
          agent_id: "ag1",
          agent: "x",
          user_id: "u",
          status: "running",
          ts: "2026-05-22T00:00:00Z",
        })}\n\n`,
      ]),
    ]);
    const kinds = [];
    for await (const item of client.streamUserRunStates("u")) {
      kinds.push(item.kind);
    }
    expect(kinds).toEqual(["event"]);
  });

  it("streamUserRunStates parentAgentId filters out events whose parent_agent_id differs", async () => {
    const { client } = makeClient([
      sseResponse([
        `event: stream_open\ndata: ${JSON.stringify({
          user_id: "u",
          filter_status: null,
          filter_agent: "",
          keepalive_interval: 25,
        })}\n\n`,
        // Two events with different parent_agent_ids.
        `event: run_state\ndata: ${JSON.stringify({
          run_id: "r1",
          agent_id: "ag1",
          agent: "x",
          user_id: "u",
          parent_agent_id: "parent_target",
          status: "completed",
          ts: "2026-05-22T00:00:00Z",
        })}\n\n`,
        `event: run_state\ndata: ${JSON.stringify({
          run_id: "r2",
          agent_id: "ag2",
          agent: "x",
          user_id: "u",
          parent_agent_id: "parent_other",
          status: "completed",
          ts: "2026-05-22T00:00:01Z",
        })}\n\n`,
      ]),
    ]);
    const items = [];
    for await (const item of client.streamUserRunStates("u", {
      parentAgentId: "parent_target",
    })) {
      items.push(item);
    }
    // open frame always yielded, then only the event with the matching
    // parent — the second event is filtered out client-side.
    expect(items.length).toBe(2);
    expect(items[0]!.kind).toBe("open");
    expect(items[1]!.kind).toBe("event");
    expect(items[1]!.kind === "event" && items[1]!.payload.run_id).toBe("r1");
  });

  it("listUserAgents parentAgentId narrows the result client-side", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        agents: [
          { agent_id: "a1", run_id: "r1", session_id: "s1", agent: "x", parent_agent_id: "parent_target", status: "running", started_at: "2026-05-22T00:00:00Z" },
          { agent_id: "a2", run_id: "r2", session_id: "s2", agent: "x", parent_agent_id: "parent_other", status: "running", started_at: "2026-05-22T00:00:01Z" },
          { agent_id: "a3", run_id: "r3", session_id: "s3", agent: "x", parent_agent_id: "parent_target", status: "running", started_at: "2026-05-22T00:00:02Z" },
        ],
      }),
    ]);
    const out = await client.listUserAgents("u", {
      parentAgentId: "parent_target",
    });
    expect(out.length).toBe(2);
    expect(out.map((a) => a.agent_id).sort()).toEqual(["a1", "a3"]);
    // Server-side: no query param for parent (filter is client-side).
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/users/u/agents",
    );
  });

  it("listUserAgents returns all agents when parentAgentId is omitted or empty string", async () => {
    const seed = [
      { agent_id: "a1", run_id: "r1", session_id: "s1", agent: "x", parent_agent_id: "p", status: "running", started_at: "2026-05-22T00:00:00Z" },
      { agent_id: "a2", run_id: "r2", session_id: "s2", agent: "x", parent_agent_id: null, status: "running", started_at: "2026-05-22T00:00:01Z" },
    ];
    const omitted = makeClient([jsonResponse({ agents: seed })]);
    expect((await omitted.client.listUserAgents("u")).length).toBe(2);

    const empty = makeClient([jsonResponse({ agents: seed })]);
    expect(
      (await empty.client.listUserAgents("u", { parentAgentId: "" })).length,
    ).toBe(2);
  });

  // Defensive: when the dataset contains a row with parent_agent_id === null
  // AND the filter is a non-null string, the null row MUST be excluded.
  // JS `null === "parent_target"` is false at runtime, but a future
  // refactor that uses == or a different comparator could regress this.
  it("listUserAgents excludes parent_agent_id=null rows when filter is set to a non-null string", async () => {
    const { client } = makeClient([
      jsonResponse({
        agents: [
          { agent_id: "match",  run_id: "r1", session_id: "s1", agent: "x", parent_agent_id: "parent_target", status: "running", started_at: "2026-05-22T00:00:00Z" },
          { agent_id: "null_p", run_id: "r2", session_id: "s2", agent: "x", parent_agent_id: null,             status: "running", started_at: "2026-05-22T00:00:01Z" },
          { agent_id: "other",  run_id: "r3", session_id: "s3", agent: "x", parent_agent_id: "parent_other",  status: "running", started_at: "2026-05-22T00:00:02Z" },
        ],
      }),
    ]);
    const out = await client.listUserAgents("u", {
      parentAgentId: "parent_target",
    });
    // Only the "match" row should survive. The null-parent row must
    // NOT silently match the "parent_target" filter.
    expect(out.map((a) => a.agent_id)).toEqual(["match"]);
  });

  // Stream-side error with debug:true: the close frame fires (with
  // meta_reason set to the error class name) before the error
  // propagates to the consumer's try/catch. Simulates the abort path
  // by building an SSE response whose stream errors on first read.
  it("runStreaming debug:true yields a stream_close frame before re-throwing on stream-side error", async () => {
    const failingStream = new ReadableStream({
      start(controller) {
        const err = new DOMException("aborted by signal", "AbortError");
        controller.error(err);
      },
    });
    const failingResponse = () =>
      new Response(failingStream, {
        status: 200,
        headers: { "Content-Type": "text/event-stream" },
      });

    const { client } = makeClient([failingResponse]);
    const events: Array<{ type: string; meta_subtype?: string; meta_reason?: string }> = [];
    let caught: unknown;
    try {
      for await (const ev of client.runStreaming({
        agent: "qa",
        segments: [],
        debug: true,
      })) {
        events.push({
          type: ev.type,
          meta_subtype: ev.meta_subtype,
          meta_reason: ev.meta_reason,
        });
      }
    } catch (e) {
      caught = e;
    }
    // Two synthetic frames before the throw — open then close
    // (with meta_reason captured from the inner error's `.name`).
    expect(events.length).toBe(2);
    expect(events[0]!.meta_subtype).toBe("stream_open");
    expect(events[1]!.meta_subtype).toBe("stream_close");
    expect(events[1]!.meta_reason).toBe("AbortError");
    // The original error still surfaces to the consumer.
    expect((caught as Error | undefined)?.name).toBe("AbortError");
  });

  // streamUserRunStates analogue: a close item with reason set to the
  // error class name fires before the stream-side throw propagates.
  it("streamUserRunStates debug:true yields a kind=close item with error reason before re-throwing", async () => {
    const failingStream = new ReadableStream({
      start(controller) {
        const err = new DOMException("aborted by signal", "AbortError");
        controller.error(err);
      },
    });
    const failingResponse = () =>
      new Response(failingStream, {
        status: 200,
        headers: { "Content-Type": "text/event-stream" },
      });

    const { client } = makeClient([failingResponse]);
    const items: Array<{ kind: string; reason?: string }> = [];
    let caught: unknown;
    try {
      for await (const item of client.streamUserRunStates("u", { debug: true })) {
        items.push({
          kind: item.kind,
          reason: item.kind === "close" ? item.payload.reason : undefined,
        });
      }
    } catch (e) {
      caught = e;
    }
    expect(items.length).toBe(1);
    expect(items[0]!.kind).toBe("close");
    expect(items[0]!.reason).toBe("AbortError");
    expect((caught as Error | undefined)?.name).toBe("AbortError");
  });
});
