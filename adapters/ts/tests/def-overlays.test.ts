// adapters/ts/tests/def-overlays.test.ts — the typed HookDef / ScheduleDef /
// WebhookDef inputs (RFC EO) and ensureHookDef.
//
// The `@ts-expect-error` lines are the type-level half: `npm run
// typecheck:tests` fails if any of them STOPS being an error, i.e. if a typed
// overlay starts accepting a key the server does not decode. Vitest strips
// types, so those lines only ever run as plain JS.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient } from "./helpers.js";
import type {
  LoomcycleClient,
  HookDefInput,
  HookDefOverlay,
  ScheduleDefInput,
  ScheduleDefOverlay,
  SubstrateToolInput,
  WebhookDefInput,
  WebhookDefOverlay,
} from "../src/index.js";

function sentBody(call: unknown[] | undefined): Record<string, unknown> {
  return JSON.parse((call![1] as RequestInit).body as string);
}

describe("typed HookDef overlay (compile-time)", () => {
  it("refuses keys and shapes the server's strict decode would refuse", () => {
    const ok: HookDefOverlay = {
      event: "pre",
      match: { tools: ["WebFetch"] },
      body: { kind: "http", url: "https://hooks.example/gate", headers: { Authorization: "$cred:gate" } },
      fail_mode: "closed",
      timeout_ms: 3000,
    };
    const bad: HookDefOverlay[] = [
      // @ts-expect-error a misspelt top-level key — the server refuses unknown fields
      { evnt: "pre", event: "pre", body: { kind: "code-js", code: "function hook(ev){return {}}" } },
      // @ts-expect-error not a hook event
      { event: "before", body: { kind: "code-js", code: "x" } },
      // @ts-expect-error a code-js body takes code, not url
      { event: "pre", body: { kind: "code-js", code: "x", url: "https://x" } },
      // @ts-expect-error an http body takes url, not code
      { event: "post", body: { kind: "http", url: "https://x", code: "x" } },
      // @ts-expect-error a misspelt key inside match
      { event: "pre", match: { tool: ["Read"] }, body: { kind: "http", url: "https://x" } },
      // @ts-expect-error fail_mode is open or closed
      { event: "pre", body: { kind: "http", url: "https://x" }, fail_mode: "ignore" },
      // @ts-expect-error event and body are required on a create overlay
      { description: "no event" },
    ];
    const typed: HookDefInput = { op: "create", name: "gate", overlay: ok };
    // @ts-expect-error a typed create needs its overlay
    const missing: HookDefInput = { op: "create", name: "gate" };
    expect([ok, bad, typed, missing]).toHaveLength(4);
  });

  it("a fork overlay is partial and may clear a field with null", () => {
    const fork: HookDefInput = {
      op: "fork",
      name: "gate",
      overlay: { fail_mode: null, timeout_ms: 1000 },
    };
    // @ts-expect-error still closed on a fork
    const badFork: HookDefInput = { op: "fork", name: "gate", overlay: { failmode: "open" } };
    expect([fork, badFork]).toHaveLength(2);
  });

  it("the loose SubstrateToolInput still compiles for every method", async () => {
    const { client } = makeClient([
      jsonResponse({}), jsonResponse({}), jsonResponse({}),
    ]);
    const loose: SubstrateToolInput = { op: "create", name: "x", overlay: { anything: 1 } };
    await client.hookDef(loose);
    await client.scheduleDef(loose);
    await client.webhookDef(loose);
  });
});

describe("typed ScheduleDef / WebhookDef overlays (compile-time)", () => {
  it("checks the declared fields while staying open to others", () => {
    const sched: ScheduleDefOverlay = {
      agent: "digest",
      schedule: "0 6 * * 1",
      prompt: [{ role: "user", content: [{ type: "trusted-text", text: "weekly digest" }] }],
      on_complete: [{ kind: "channel.publish", channel: "digests" }],
      concurrency_policy: "forbid",
      max_fires: 0,
      // Open: a key the adapter does not model yet passes through.
      some_future_field: true,
    };
    const badSched: ScheduleDefOverlay[] = [
      // @ts-expect-error concurrency_policy is forbid, allow or replace
      { concurrency_policy: "skip" },
      // @ts-expect-error an mcp.call hook needs server and tool
      { on_complete: [{ kind: "mcp.call", server: "jobs" }] },
      // @ts-expect-error not a delivery a schedule has
      { delivery: "spawn" },
    ];
    const hookOp: ScheduleDefInput = {
      op: "add_hook",
      def_id: "sdf_1",
      hook: { kind: "memory.set", scope: "user", key: "last_digest" },
    };
    // @ts-expect-error remove_hook needs hook_index
    const badHookOp: ScheduleDefInput = { op: "remove_hook", def_id: "sdf_1" };

    const hook: WebhookDefOverlay = {
      delivery: "spawn",
      agent: "ingest",
      auth: { kind: "hmac", algorithm: "sha256", signing_secret_env: "LOOMCYCLE_WEBHOOK_GH_SECRET" },
      payload_mapping: { goal: "$.head_commit.message" },
    };
    const badHook: WebhookDefOverlay[] = [
      // @ts-expect-error not a delivery a webhook has
      { delivery: "run" },
      // @ts-expect-error auth.kind is hmac, bearer or none
      { auth: { kind: "basic" } },
      // @ts-expect-error a misspelt key inside a closed nested block
      { rate_limit: { rpm: 10 } },
    ];
    const typedHook: WebhookDefInput = { op: "create", name: "gh", overlay: hook };
    expect([sched, badSched, hookOp, badHookOp, hook, badHook, typedHook]).toHaveLength(7);
  });
});

describe("hookDef / scheduleDef / webhookDef send typed inputs verbatim", () => {
  it("hookDef posts the typed create to /v1/_hookdef", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ def_id: "hdf_1" })]);
    const input: HookDefInput = {
      op: "create",
      name: "gate",
      overlay: { event: "pre", body: { kind: "code-js", code: "function hook(ev){return {}}" } },
      promote: false,
    };
    await client.hookDef(input);
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_hookdef");
    expect(sentBody(call)).toEqual(input);
  });

  it("scheduleDef posts add_hook to /v1/_scheduledef", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ def_id: "sdf_2" })]);
    const input: ScheduleDefInput = {
      op: "add_hook",
      def_id: "sdf_1",
      hook: { kind: "mcp.call", server: "jobs", tool: "notify", args: { channel: "ops" } },
    };
    await client.scheduleDef(input);
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_scheduledef");
    expect(sentBody(call)).toEqual(input);
  });

  it("webhookDef posts the typed fork to /v1/_webhookdef", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ def_id: "whd_2" })]);
    const input: WebhookDefInput = {
      op: "fork",
      name: "gh",
      overlay: { delivery: "team", team: "pr-review", vars: { repo: "$.repository.full_name" } },
    };
    await client.webhookDef(input);
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_webhookdef");
    expect(sentBody(call)).toEqual(input);
  });
});

describe("ensureHookDef", () => {
  const overlay: HookDefOverlay = {
    event: "pre",
    match: { tools: ["WebFetch"] },
    body: { kind: "http", url: "https://hooks.example/gate", headers: { "X-Key": "$cred:gate" } },
    timeout_ms: 2000,
  };
  // What the server stores for `overlay`: fail_mode filled in, keys in Go
  // struct order (deliberately not the literal's order above).
  const stored = {
    event: "pre",
    match: { tools: ["WebFetch"] },
    body: { kind: "http", url: "https://hooks.example/gate", headers: { "X-Key": "$cred:gate" } },
    fail_mode: "open",
    timeout_ms: 2000,
  };

  it("creates the first version when the name is not deployed", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ matches: false, current_sha256: "", current_def_id: "", version: 0, name: "gate", deployed: false }),
      jsonResponse({ def_id: "hdf_1", name: "gate", version: 1, promoted: true }),
    ]);
    const r = await client.ensureHookDef("gate", overlay, { description: "gate outbound fetches" });
    expect(r).toEqual({ name: "gate", defId: "hdf_1", version: 1, changed: true });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(sentBody(fetchMock.mock.calls[0])).toEqual({ op: "verify", name: "gate" });
    expect(sentBody(fetchMock.mock.calls[1])).toEqual({
      op: "create",
      name: "gate",
      overlay,
      description: "gate outbound fetches",
    });
  });

  it("writes nothing when the active definition is the same once normalized", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ matches: false, current_sha256: "sha256:aa", current_def_id: "hdf_3", version: 3, name: "gate", deployed: true }),
      jsonResponse({ def_id: "hdf_3", name: "gate", version: 3, definition: stored }),
    ]);
    const r = await client.ensureHookDef("gate", overlay);
    expect(r).toEqual({ name: "gate", defId: "hdf_3", version: 3, changed: false });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    // The read is by the def_id verify reported, not by name.
    expect(sentBody(fetchMock.mock.calls[1])).toEqual({ op: "get", def_id: "hdf_3" });
  });

  it("treats the server's defaults and dropped empties as no change", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ deployed: true, current_def_id: "hdf_1", version: 1, name: "g", matches: false, current_sha256: "x" }),
      jsonResponse({ def_id: "hdf_1", name: "g", version: 1, definition: { event: "run_end", body: { kind: "code-js", code: "c" }, fail_mode: "open" } }),
    ]);
    const r = await client.ensureHookDef("g", {
      description: "  ",
      event: "run_end",
      match: { tools: [] },
      body: { kind: "code-js", code: "c" },
      timeout_ms: 0,
    });
    expect(r.changed).toBe(false);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("trims a description as the server does, not as String.prototype.trim does", async () => {
    const deployed = () =>
      jsonResponse({ deployed: true, current_def_id: "hdf_1", version: 1, name: "g", matches: false, current_sha256: "x" });
    const active = () =>
      jsonResponse({ def_id: "hdf_1", name: "g", version: 1, definition: { description: "x", event: "run_end", body: { kind: "code-js", code: "c" }, fail_mode: "open" } });
    const body = { kind: "code-js", code: "c" } as const;

    // Go's TrimSpace trims U+0085 — the server would store "x": no change.
    const same = makeClient([deployed(), active()]);
    expect((await same.client.ensureHookDef("g", { description: "x\u0085", event: "run_end", body })).changed).toBe(false);

    // It keeps U+FEFF, which trim() would drop — the server stores "x\uFEFF",
    // a different description, so this must write.
    const differs = makeClient([deployed(), active(), jsonResponse({ def_id: "hdf_2", name: "g", version: 2 })]);
    expect((await differs.client.ensureHookDef("g", { description: "x\uFEFF", event: "run_end", body })).changed).toBe(true);
    expect(differs.fetchMock).toHaveBeenCalledTimes(3);
  });

  it("creates a new version when the active definition differs", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ deployed: true, current_def_id: "hdf_3", version: 3, name: "gate", matches: false, current_sha256: "x" }),
      jsonResponse({ def_id: "hdf_3", name: "gate", version: 3, definition: { ...stored, fail_mode: "closed" } }),
      jsonResponse({ def_id: "hdf_4", name: "gate", version: 4, promoted: true }),
    ]);
    const r = await client.ensureHookDef("gate", overlay);
    expect(r).toEqual({ name: "gate", defId: "hdf_4", version: 4, changed: true });
    expect(sentBody(fetchMock.mock.calls[2])).toEqual({ op: "create", name: "gate", overlay });
  });

  it("a changed header value is a change", async () => {
    const { client } = makeClient([
      jsonResponse({ deployed: true, current_def_id: "hdf_3", version: 3, name: "gate", matches: false, current_sha256: "x" }),
      jsonResponse({ def_id: "hdf_3", name: "gate", version: 3, definition: { ...stored, body: { ...stored.body, headers: { "X-Key": "$cred:old" } } } }),
      jsonResponse({ def_id: "hdf_4", name: "gate", version: 4 }),
    ]);
    expect((await client.ensureHookDef("gate", overlay)).changed).toBe(true);
  });

  it("a changed or added signing secret is a change", async () => {
    const signed: HookDefOverlay = { ...overlay, body: { ...overlay.body, kind: "http", url: "https://hooks.example/gate", signing_secret: "$cred:sign-new" } };
    const rotated = makeClient([
      jsonResponse({ deployed: true, current_def_id: "hdf_3", version: 3, name: "gate", matches: false, current_sha256: "x" }),
      jsonResponse({ def_id: "hdf_3", name: "gate", version: 3, definition: { ...stored, body: { ...stored.body, signing_secret: "$cred:sign-old" } } }),
      jsonResponse({ def_id: "hdf_4", name: "gate", version: 4 }),
    ]);
    expect((await rotated.client.ensureHookDef("gate", signed)).changed).toBe(true);
    const added = makeClient([
      jsonResponse({ deployed: true, current_def_id: "hdf_3", version: 3, name: "gate", matches: false, current_sha256: "x" }),
      jsonResponse({ def_id: "hdf_3", name: "gate", version: 3, definition: stored }),
      jsonResponse({ def_id: "hdf_4", name: "gate", version: 4 }),
    ]);
    expect((await added.client.ensureHookDef("gate", signed)).changed).toBe(true);
    const same = makeClient([
      jsonResponse({ deployed: true, current_def_id: "hdf_3", version: 3, name: "gate", matches: false, current_sha256: "x" }),
      jsonResponse({ def_id: "hdf_3", name: "gate", version: 3, definition: { ...stored, body: { ...stored.body, signing_secret: "$cred:sign-new" } } }),
    ]);
    expect((await same.client.ensureHookDef("gate", signed)).changed).toBe(false);
  });

  it("forwards the abort signal on every call", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ deployed: false, current_def_id: "", version: 0, name: "g", matches: false, current_sha256: "" }),
      jsonResponse({ def_id: "hdf_1", name: "g", version: 1 }),
    ]);
    const ac = new AbortController();
    await client.ensureHookDef("g", { event: "pre", body: { kind: "http", url: "https://x" } }, { signal: ac.signal });
    for (const call of fetchMock.mock.calls) {
      expect((call[1] as RequestInit).signal).toBe(ac.signal);
    }
  });
});

describe("strictly-typed create / fork methods (compile-time, bare literals)", () => {
  // Never called: these lines exist for `npm run typecheck:tests`. Each
  // directive sits directly above the line TypeScript reports the error on.
  async function typeOnly(client: LoomcycleClient): Promise<void> {
    // HookDef is closed: ANY unknown key fails, even in a bare literal — the
    // case hookDef(...) cannot catch because of its loose fallback.
    await client.createHookDef("g", {
      event: "pre",
      body: { kind: "http", url: "https://x" },
      // @ts-expect-error misspelt top-level key
      timout_ms: 10,
    });
    await client.createHookDef("g", {
      event: "pre",
      // @ts-expect-error a code-js body takes code, not url
      body: { kind: "code-js", code: "c", url: "https://x" },
    });
    // @ts-expect-error a create overlay needs event and body
    await client.createHookDef("g", { description: "d" });
    await client.forkHookDef("g", {
      fail_mode: null,
      // @ts-expect-error misspelt key on a fork
      timeoutms: 5,
    });
    await client.ensureHookDef("g", {
      event: "pre",
      body: { kind: "http", url: "https://x" },
      // @ts-expect-error ensureHookDef's overlay is closed too
      evnt: "post",
    });

    // ScheduleDef / WebhookDef overlays keep [extra]: a misspelt key compiles
    // (the server drops it), so only a wrong type on a DECLARED key fails.
    await client.createScheduleDef("s", { agent: "a", schedulee: "0 6 * * 1" });
    await client.createScheduleDef("s", {
      agent: "a",
      // @ts-expect-error concurrency_policy is forbid, allow or replace
      concurrency_policy: "skip",
    });
    await client.forkScheduleDef("s", {
      // @ts-expect-error max_fires is a number
      max_fires: "3",
    });
    await client.createWebhookDef("w", { agent: "a", delivry: "spawn" });
    await client.createWebhookDef("w", {
      // @ts-expect-error not a delivery a webhook has
      delivery: "run",
    });
    await client.forkWebhookDef("w", {
      // @ts-expect-error enabled is a boolean
      enabled: "yes",
    });
  }

  it("is a type-level check only", () => {
    expect(typeof typeOnly).toBe("function");
  });
});

describe("strictly-typed create / fork methods (wire)", () => {
  const cases = [
    {
      method: "HookDef",
      path: "/v1/_hookdef",
      create: (c: LoomcycleClient) =>
        c.createHookDef("g", { event: "pre", body: { kind: "http", url: "https://x" } }, { description: "why", promote: false }),
      fork: (c: LoomcycleClient) => c.forkHookDef("g", { timeout_ms: 500 }, { parentDefId: "hdf_1", promote: true }),
      name: "g",
      createOverlay: { event: "pre", body: { kind: "http", url: "https://x" } },
      forkOverlay: { timeout_ms: 500 },
    },
    {
      method: "ScheduleDef",
      path: "/v1/_scheduledef",
      create: (c: LoomcycleClient) =>
        c.createScheduleDef("s", { agent: "a", schedule: "0 6 * * 1" }, { description: "why", promote: false }),
      fork: (c: LoomcycleClient) => c.forkScheduleDef("s", { max_fires: 0 }, { parentDefId: "sdf_1", promote: true }),
      name: "s",
      createOverlay: { agent: "a", schedule: "0 6 * * 1" },
      forkOverlay: { max_fires: 0 },
    },
    {
      method: "WebhookDef",
      path: "/v1/_webhookdef",
      create: (c: LoomcycleClient) =>
        c.createWebhookDef("w", { delivery: "spawn", agent: "a" }, { description: "why", promote: false }),
      fork: (c: LoomcycleClient) => c.forkWebhookDef("w", { enabled: true }, { parentDefId: "whd_1", promote: true }),
      name: "w",
      createOverlay: { delivery: "spawn", agent: "a" },
      forkOverlay: { enabled: true },
    },
  ];

  for (const tc of cases) {
    it(`create${tc.method} posts op:create with the options as wire fields, promote:false included`, async () => {
      const { client, fetchMock } = makeClient([jsonResponse({ def_id: "d_1", version: 1 })]);
      const row = await tc.create(client);
      expect(row.def_id).toBe("d_1");
      const call = fetchMock.mock.calls[0]!;
      expect(call[0]).toBe(`http://test-loomcycle:8787${tc.path}`);
      expect(sentBody(call)).toEqual({
        op: "create",
        name: tc.name,
        overlay: tc.createOverlay,
        description: "why",
        promote: false,
      });
    });

    it(`fork${tc.method} posts op:fork with parent_def_id`, async () => {
      const { client, fetchMock } = makeClient([jsonResponse({ def_id: "d_2", version: 2 })]);
      await tc.fork(client);
      const body = sentBody(fetchMock.mock.calls[0]);
      expect(body.op).toBe("fork");
      expect(body.name).toBe(tc.name);
      expect(body.parent_def_id).toMatch(/_1$/);
      expect(body.overlay).toEqual(tc.forkOverlay);
      expect(body.promote).toBe(true);
    });
  }

  it("leaves promote and parent_def_id off when not given, so the server's default applies", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ def_id: "d_1" })]);
    await client.forkHookDef("g", { description: "only this" });
    expect(sentBody(fetchMock.mock.calls[0])).toEqual({
      op: "fork",
      name: "g",
      overlay: { description: "only this" },
    });
  });
});
