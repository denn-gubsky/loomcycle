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
