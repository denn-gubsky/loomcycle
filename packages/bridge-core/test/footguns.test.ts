// The configurations a bridge refuses, and what a gate bridge does on its own
// so a person who leaves the platform does not stay a loomcycle user.

import type { AddressInfo } from "node:net";
import { chmod, mkdtemp, readdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { afterEach, describe, expect, it, vi } from "vitest";

import { ConfigError, EnvReader, loadSharedConfig, type Env, type SharedConfig } from "../src/config.js";
import { keyFromSecret } from "../src/gate.js";
import { FileRevokeQueue, MemoryRevokeQueue } from "../src/revokes.js";
import { createBridge, type BridgeAdapter, type BridgeContext } from "../src/start.js";
import { FileStore, MemoryStore, type ConversationRecord } from "../src/store.js";

const GATE_ENV: Env = {
  LOOMCYCLE_BASE_URL: "https://lc.example",
  BRIDGE_AGENT: "chat",
  BRIDGE_GATE: "tg",
  BRIDGE_GATE_AUDIENCE: "https://lc.example",
  BRIDGE_GATE_SECRET: "s".repeat(40),
};
const TOKEN_ENV: Env = { LOOMCYCLE_BASE_URL: "https://lc.example", LOOMCYCLE_TOKEN: "lct_x", BRIDGE_AGENT: "chat" };

function load(env: Env, gate: boolean): { cfg: SharedConfig; problems: string[] } {
  const r = new EnvReader(env);
  const cfg = loadSharedConfig(r, { port: 0, gate });
  return { cfg, problems: r.problems };
}

const quiet = { info: () => {}, warn: () => {}, error: () => {} };

/** A logger that keeps each line. */
function recording() {
  const lines: { level: string; msg: string }[] = [];
  const at = (level: string) => (msg: string) => void lines.push({ level, msg });
  return { lines, logger: { info: at("info"), warn: at("warn"), error: at("error") } };
}

/** A platform adapter that only records the context it is started with. */
function adapter(extra: Partial<BridgeAdapter<unknown>> = {}): { a: BridgeAdapter<unknown>; ctx: () => BridgeContext<unknown> } {
  let started: BridgeContext<unknown> | undefined;
  const a: BridgeAdapter<unknown> = {
    platform: { name: "test", label: "Test" },
    capabilities: { streaming: "never", maxMessageChars: 4000 },
    port: { send: async () => {} },
    async start(ctx) {
      started = ctx;
    },
    ...extra,
  };
  return { a, ctx: () => started! };
}

/** A fake loomcycle /v1/_auth: records each call; `fail` answers a revoke 503. */
function fakeAuth() {
  const calls: { path: string; payload: Record<string, any> }[] = [];
  const state = { failRevoke: false, revokeStatus: 0 };
  const fetchFn = (async (url: string, init: RequestInit) => {
    const body = JSON.parse(String(init.body));
    const jwt: string = body.assertion ?? body.request;
    calls.push({ path: new URL(url).pathname, payload: JSON.parse(Buffer.from(jwt.split(".")[1]!, "base64url").toString()) });
    if (url.endsWith("/v1/_auth/revoke")) {
      if (state.revokeStatus) return new Response(JSON.stringify({ code: "refused" }), { status: state.revokeStatus });
      return state.failRevoke ? new Response(JSON.stringify({ code: "revoke_failed" }), { status: 503 }) : new Response(null, { status: 204 });
    }
    return new Response(JSON.stringify({ created: 0, updated: 2, refused: 0, removed: 1, unchanged: 0 }), { status: 200 });
  }) as unknown as typeof fetch;
  return { calls, state, fetchFn };
}

function rec(conversationId: string, runSubject: string): ConversationRecord {
  return { conversationId, scope: "personal", ref: {}, ownerSubject: runSubject, ownerName: "x", runSubject, runName: `name of ${runSubject}`, lastActivityAt: 0 };
}

afterEach(() => vi.useRealTimers());

describe("dev mode", () => {
  it("refuses dev mode with a gate", async () => {
    const { problems } = load({ ...GATE_ENV, BRIDGE_DEV: "1" }, true);
    expect(problems).toContainEqual(expect.stringContaining("BRIDGE_DEV=1 is refused on a gate bridge"));

    // A configuration built in code is refused at start too.
    const { cfg } = load(GATE_ENV, true);
    const bridge = createBridge({ ...cfg, dev: true, bindHost: "127.0.0.1" }, adapter().a, { logger: quiet, gateKey: keyFromSecret("s".repeat(40)) });
    await expect(bridge.start()).rejects.toBeInstanceOf(ConfigError);
  });

  it("refuses dev mode off loopback", async () => {
    for (const bind of ["0.0.0.0", "::", "192.168.1.5", "bridge.example"]) {
      const { problems } = load({ ...TOKEN_ENV, BRIDGE_DEV: "1", BRIDGE_BIND: bind }, false);
      expect(problems, bind).toContainEqual(expect.stringContaining("BRIDGE_DEV=1 is refused off loopback"));
    }
    const hand: SharedConfig = { ...load(TOKEN_ENV, false).cfg, dev: true };
    delete hand.bindHost;
    await expect(createBridge(hand, adapter().a, { logger: quiet }).start()).rejects.toBeInstanceOf(ConfigError);

    // Dev mode alone binds loopback, and the listener really is there.
    const { cfg, problems } = load({ ...TOKEN_ENV, BRIDGE_DEV: "1" }, false);
    expect(problems).toEqual([]);
    expect(cfg.bindHost).toBe("127.0.0.1");
    const { a, ctx } = adapter();
    const bridge = createBridge(cfg, a, { logger: quiet });
    await bridge.start();
    const server = await ctx().serve({});
    expect((server.address() as AddressInfo).address).toBe("127.0.0.1");
    await bridge.stop();
  });

  it("binds every interface outside dev mode unless BRIDGE_BIND says otherwise", () => {
    expect(load(TOKEN_ENV, false).cfg.bindHost).toBeUndefined();
    expect(load({ ...TOKEN_ENV, BRIDGE_BIND: "10.0.0.2" }, false).cfg.bindHost).toBe("10.0.0.2");
  });
});

describe("loomcycle's URL", () => {
  it("is https unless it is this machine or the operator says the link is private", () => {
    const at = (url: string, extra: Env = {}) => load({ ...TOKEN_ENV, LOOMCYCLE_BASE_URL: url, ...extra }, false).problems;
    expect(at("http://loomcycle:8787")).toContainEqual(expect.stringContaining("LOOMCYCLE_BASE_URL must be https://"));
    expect(at("ftp://loomcycle")).toContainEqual(expect.stringContaining("LOOMCYCLE_BASE_URL must be https://"));
    expect(at("https://loomcycle.example")).toEqual([]);
    expect(at("http://127.0.0.1:8787")).toEqual([]);
    expect(at("http://localhost:8787")).toEqual([]);
    expect(at("http://[::1]:8787")).toEqual([]);
    expect(at("http://loomcycle:8787", { BRIDGE_ALLOW_INSECURE_LOOMCYCLE: "1" })).toEqual([]);
  });
});

describe("revokes a gate bridge keeps", () => {
  it("retries a failed revoke from its own store", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
    const dir = await mkdtemp(path.join(os.tmpdir(), "bridge-revokes-"));
    const spec = `file:${path.join(dir, "store.json")}`;
    const { cfg } = load({ ...GATE_ENV, BRIDGE_STORE: spec }, true);
    const key = keyFromSecret("s".repeat(40));
    const lc = fakeAuth();
    lc.state.failRevoke = true;

    const first = adapter();
    const b1 = createBridge(cfg, first.a, { logger: quiet, gateKey: key, gateFetch: lc.fetchFn });
    await b1.start();
    // The platform's revoke fails while loomcycle is down: the caller still
    // sees the failure (and answers its platform 5xx), and the bridge keeps it.
    await expect(first.ctx().gate!.revoke("tg:42")).rejects.toMatchObject({ status: 503 });
    const kept = JSON.parse(await readFile(path.join(dir, "store.json.revokes.json"), "utf8"));
    expect(kept).toMatchObject([{ subject: "tg:42", attempts: 1 }]);

    // Still down a minute later: retried, kept, backed off.
    const queued = () => new FileRevokeQueue(path.join(dir, "store.json.revokes.json")).list();
    await vi.advanceTimersByTimeAsync(61_000);
    // The file is written by real I/O, which the fake clock does not wait for.
    await vi.waitFor(async () => expect((await queued())[0]).toMatchObject({ attempts: 2 }));
    expect(lc.calls.filter((c) => c.path === "/v1/_auth/revoke")).toHaveLength(2);
    await b1.stop();

    // loomcycle is back and the bridge restarts: the kept revoke goes out
    // from the file, without the platform delivering anything again.
    lc.state.failRevoke = false;
    const b2 = createBridge(cfg, adapter().a, { logger: quiet, gateKey: key, gateFetch: lc.fetchFn });
    await b2.start();
    await vi.waitFor(async () => expect(await queued()).toEqual([]));
    const revokes = lc.calls.filter((c) => c.path === "/v1/_auth/revoke");
    expect(revokes).toHaveLength(3);
    expect(revokes[2]!.payload).toMatchObject({ sub: "42", lc_use: "revoke", iss: "tg" });
    await b2.stop();
  });
});

describe("a kept revoke ends", () => {
  it("drops a kept revoke only for a 1:1 message the person sent after it", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "setTimeout", "clearTimeout", "Date"] });
    const { cfg } = load(GATE_ENV, true);
    const revokes = new MemoryRevokeQueue();
    const lc = fakeAuth();
    lc.state.failRevoke = true;
    const { a, ctx } = adapter();
    const bridge = createBridge(cfg, a, { logger: quiet, revokes, gateKey: keyFromSecret("s".repeat(40)), gateFetch: lc.fetchFn });
    await bridge.start();
    await expect(ctx().gate!.revoke("tg:42")).rejects.toMatchObject({ status: 503 });
    const failedAt = (await revokes.list())[0]!.firstFailedAt;
    const dm = { ok: true as const, speaker: { subject: "tg:42", name: "Ann", trust: "trusted" as const }, scope: "personal" as const, conversationId: "dm-42", runAs: { subject: "tg:42", name: "Ann" } };

    // In a group the person speaks, but the group runs as itself: that says
    // nothing about the 1:1 they blocked. The revoke stays.
    await ctx().router.onMessage(
      { ok: true, speaker: { subject: "tg:42", name: "Ann", trust: "untrusted" }, scope: "group", conversationId: "grp-1", runAs: { subject: "tg:conv:abc", name: "Group" } },
      {},
      "hi all",
      { sentAt: failedAt + 60_000 },
    );
    // A 1:1 message the platform dates before the failure (a redelivery), or
    // one it gives no date for, proves nothing either.
    await ctx().router.onMessage(dm, {}, "old", { sentAt: failedAt - 1 });
    await ctx().router.onMessage(dm, {}, "undated");
    await vi.advanceTimersByTimeAsync(10);
    expect(await revokes.list()).toHaveLength(1);

    // They unblocked the bot and wrote to it: they are not gone after all.
    await ctx().router.onMessage(dm, {}, "hello again", { sentAt: failedAt + 60_000 });
    await vi.waitFor(async () => expect(await revokes.list()).toEqual([]));
    lc.state.failRevoke = false;
    await vi.advanceTimersByTimeAsync(2 * 3_600_000);
    expect(lc.calls.filter((c) => c.path === "/v1/_auth/revoke")).toHaveLength(1);
    await bridge.stop();
  });

  // A voice message and a card press are the person writing to the bot too:
  // each path the platform authenticated drops the kept revoke the same way.
  it("drops a kept revoke for a dated 1:1 voice message or card press, as for text", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "setTimeout", "clearTimeout", "Date"] });
    const { cfg } = load(GATE_ENV, true);
    const revokes = new MemoryRevokeQueue();
    const lc = fakeAuth();
    lc.state.failRevoke = true;
    const { a, ctx } = adapter();
    const bridge = createBridge(cfg, a, { logger: quiet, revokes, gateKey: keyFromSecret("s".repeat(40)), gateFetch: lc.fetchFn });
    await bridge.start();
    const dm = (n: number) => ({ ok: true as const, speaker: { subject: `tg:${n}`, name: "Ann", trust: "trusted" as const }, scope: "personal" as const, conversationId: `dm-${n}`, runAs: { subject: `tg:${n}`, name: "Ann" } });
    await expect(ctx().gate!.revoke("tg:42")).rejects.toMatchObject({ status: 503 });
    await expect(ctx().gate!.revoke("tg:43")).rejects.toMatchObject({ status: 503 });
    const failedAt = Math.max(...(await revokes.list()).map((p) => p.firstFailedAt));
    const subjects = async () => (await revokes.list()).map((p) => p.subject).sort();

    // Undated, neither path drops anything.
    await ctx().router.onVoice(dm(42), {}, { kind: "voice", ref: "v", mime: "audio/ogg" });
    await ctx().router.onCardAction(dm(43), {}, { buttonId: "nope" });
    await vi.advanceTimersByTimeAsync(10);
    expect(await subjects()).toEqual(["tg:42", "tg:43"]);

    await ctx().router.onVoice(dm(42), {}, { kind: "voice", ref: "v", mime: "audio/ogg" }, undefined, { sentAt: failedAt + 60_000 });
    await vi.waitFor(async () => expect(await subjects()).toEqual(["tg:43"]));
    await ctx().router.onCardAction(dm(43), {}, { buttonId: "nope" }, { sentAt: failedAt + 60_000 });
    await vi.waitFor(async () => expect(await subjects()).toEqual([]));
    await bridge.stop();
  });

  // loomcycle answers an unknown or already-removed person 204, so a 4xx is
  // never "nothing to revoke": it is a key, a gate name or a throttle that is
  // wrong now. Dropping the revoke would fail open; it is retried, and logged
  // at error every time it stays stuck.
  it("keeps retrying a revoke loomcycle refuses, logging it as stuck", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "setTimeout", "clearTimeout", "Date"] });
    const { cfg } = load(GATE_ENV, true);
    const revokes = new MemoryRevokeQueue();
    const lc = fakeAuth();
    const logs = recording();
    const { a, ctx } = adapter();
    const bridge = createBridge(cfg, a, { logger: logs.logger, revokes, gateKey: keyFromSecret("s".repeat(40)), gateFetch: lc.fetchFn });
    await bridge.start();

    lc.state.revokeStatus = 401;
    await expect(ctx().gate!.revoke("tg:1")).rejects.toMatchObject({ status: 401 });
    expect((await revokes.list()).map((p) => p.subject)).toEqual(["tg:1"]);
    await vi.advanceTimersByTimeAsync(3 * 60_000);
    expect(lc.calls.filter((c) => c.path === "/v1/_auth/revoke").length).toBeGreaterThanOrEqual(2);
    expect(await revokes.list()).toHaveLength(1);
    expect(logs.lines.filter((l) => l.level === "error" && l.msg.includes("stuck")).length).toBeGreaterThanOrEqual(2);

    // The operator fixes it: the next retry goes through.
    lc.state.revokeStatus = 0;
    await vi.advanceTimersByTimeAsync(60 * 60_000);
    expect(await revokes.list()).toEqual([]);
    await bridge.stop();
  });
});

describe("reconciliation", () => {
  const FIRST_RUN_BY = 6 * 60_000; // jittered, within minutes of start

  it("revokes the people it served whom the platform no longer lists, a few minutes after start", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "setTimeout", "clearTimeout", "Date"] });
    const { cfg } = load(GATE_ENV, true);
    expect(cfg.gateSyncHours).toBe(6);
    const store = new MemoryStore();
    await store.put(rec("dm-alice", "tg:alice")); // still a member
    await store.put(rec("dm-bob", "tg:bob")); // gone from the platform
    await store.put({ ...rec("chan", "tg:conv:abc"), scope: "channel" }); // a shared conversation's own user
    await store.put(rec("old", "test-carol")); // not this gate's subject
    const lc = fakeAuth();
    const { a } = adapter({ members: async () => new Set(["alice", "dave"]) });
    const bridge = createBridge(cfg, a, { logger: quiet, store, gateKey: keyFromSecret("s".repeat(40)), gateFetch: lc.fetchFn });
    await bridge.start();

    await vi.advanceTimersByTimeAsync(FIRST_RUN_BY);
    const revokes = lc.calls.filter((c) => c.path === "/v1/_auth/revoke");
    // Only bob: evidence that he left. A linked user this bridge never saw
    // (dave, or anyone a lost store forgot) is never removed for its absence.
    expect(revokes.map((c) => c.payload.sub)).toEqual(["bob"]);
    expect(lc.calls.filter((c) => c.path === "/v1/_auth/sync")).toEqual([]);
    expect(await store.get("dm-bob")).toBeUndefined();
    expect(await store.get("dm-alice")).toBeDefined();
    expect(await store.get("chan")).toBeDefined();

    // Then on the timer; bob is not revoked a second time.
    await vi.advanceTimersByTimeAsync(6 * 3_600_000);
    expect(lc.calls.filter((c) => c.path === "/v1/_auth/revoke")).toHaveLength(1);
    await bridge.stop();
  });

  // A platform may name a person by more than the key its member list gives
  // (Slack: "<team>:<user>", where the team an event names can differ from
  // the one users.list reports). memberKey maps each to the list's key.
  it("compares a person under the adapter's memberKey when it has one", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "setTimeout", "clearTimeout", "Date"] });
    const { cfg } = load(GATE_ENV, true);
    const store = new MemoryStore();
    await store.put(rec("dm-alice", "tg:t2:alice")); // listed under another team
    await store.put(rec("dm-bob", "tg:t1:bob")); // gone
    const lc = fakeAuth();
    const { a } = adapter({ members: async () => new Set(["alice", "carol"]), memberKey: (sub: string) => sub.slice(sub.lastIndexOf(":") + 1) });
    const bridge = createBridge(cfg, a, { logger: quiet, store, gateKey: keyFromSecret("s".repeat(40)), gateFetch: lc.fetchFn });
    await bridge.start();
    await vi.advanceTimersByTimeAsync(FIRST_RUN_BY);
    expect(lc.calls.filter((c) => c.path === "/v1/_auth/revoke").map((c) => c.payload.sub)).toEqual(["t1:bob"]);
    await bridge.stop();
  });

  it("skips a round that would revoke more people than its cap", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "setTimeout", "clearTimeout", "Date"] });
    const { cfg } = load(GATE_ENV, true);
    const store = new MemoryStore();
    for (const n of ["a", "b", "c", "d", "e", "f"]) await store.put(rec(`dm-${n}`, `tg:${n}`));
    const lc = fakeAuth();
    // Five of six gone: over the cap (half of six). A platform API returning
    // a wrong list looks exactly like this.
    const logs = recording();
    const bridge = createBridge(cfg, adapter({ members: async () => new Set(["a", "zed"]) }).a, {
      logger: logs.logger,
      store,
      gateKey: keyFromSecret("s".repeat(40)),
      gateFetch: lc.fetchFn,
    });
    await bridge.start();
    await vi.advanceTimersByTimeAsync(FIRST_RUN_BY + 6 * 3_600_000);
    expect(lc.calls.filter((c) => c.path === "/v1/_auth/revoke")).toEqual([]);
    expect(await store.list()).toHaveLength(6);
    // Every round over the cap says so at error.
    expect(logs.lines.filter((l) => l.level === "error" && l.msg.includes("cap"))).toHaveLength(2);
    await bridge.stop();

    // An operator who knows it is a real departure raises the cap deliberately.
    const { cfg: raised } = load({ ...GATE_ENV, BRIDGE_RECONCILE_MAX_REVOKES: "10" }, true);
    expect(raised.reconcileMaxRevokes).toBe(10);
    const applied = createBridge(raised, adapter({ members: async () => new Set(["a", "zed"]) }).a, {
      logger: quiet,
      store,
      gateKey: keyFromSecret("s".repeat(40)),
      gateFetch: lc.fetchFn,
    });
    await applied.start();
    await vi.advanceTimersByTimeAsync(FIRST_RUN_BY);
    expect(lc.calls.filter((c) => c.path === "/v1/_auth/revoke")).toHaveLength(5);
    await applied.stop();
  });

  it("says that a memory store leaves it nothing to go on after a restart", async () => {
    const { cfg } = load(GATE_ENV, true);
    const logs = recording();
    const bridge = createBridge(cfg, adapter({ members: async () => new Set(["a"]) }).a, {
      logger: logs.logger,
      gateKey: keyFromSecret("s".repeat(40)),
      gateFetch: fakeAuth().fetchFn,
    });
    await bridge.start();
    expect(logs.lines).toContainEqual(expect.objectContaining({ level: "warn", msg: expect.stringContaining("BRIDGE_STORE=file:") }));
    await bridge.stop();
  });

  it("protects a small deployment: half at most, and never everyone", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "setTimeout", "clearTimeout", "Date"] });
    const lc = fakeAuth();
    const run = async (people: string[], members: string[], env: Env = {}) => {
      const store = new MemoryStore();
      for (const n of people) await store.put(rec(`dm-${n}`, `tg:${n}`));
      const logs = recording();
      const before = lc.calls.length;
      const bridge = createBridge(load({ ...GATE_ENV, ...env }, true).cfg, adapter({ members: async () => new Set(members) }).a, {
        logger: logs.logger,
        store,
        gateKey: keyFromSecret("s".repeat(40)),
        gateFetch: lc.fetchFn,
      });
      await bridge.start();
      await vi.advanceTimersByTimeAsync(FIRST_RUN_BY);
      await bridge.stop();
      return { revoked: lc.calls.slice(before).filter((c) => c.path === "/v1/_auth/revoke").length, errors: logs.lines.filter((l) => l.level === "error") };
    };
    // Three served, two gone: over half. A wrong but non-empty list.
    expect((await run(["a", "b", "c"], ["a", "zed"])).revoked).toBe(0);
    // Three served, one gone: within half.
    expect((await run(["a", "b", "c"], ["a", "b"])).revoked).toBe(1);
    // Everyone gone is never applied by a round, cap or no cap.
    const all = await run(["a", "b"], ["zed"], { BRIDGE_RECONCILE_MAX_REVOKES: "10" });
    expect(all.revoked).toBe(0);
    expect(all.errors).toHaveLength(1);
  });

  it("refuses a cap of zero built in code, as it does from the environment", async () => {
    expect(load({ ...GATE_ENV, BRIDGE_RECONCILE_MAX_REVOKES: "0" }, true).problems).toContainEqual(expect.stringContaining("BRIDGE_RECONCILE_MAX_REVOKES"));
    const { cfg } = load(GATE_ENV, true);
    const bridge = createBridge({ ...cfg, reconcileMaxRevokes: 0 }, adapter().a, { logger: quiet, gateKey: keyFromSecret("s".repeat(40)) });
    await expect(bridge.start()).rejects.toBeInstanceOf(ConfigError);
  });

  it("never revokes on a member list it could not read, or one that is empty", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "setTimeout", "clearTimeout", "Date"] });
    const { cfg } = load(GATE_ENV, true);
    const store = new MemoryStore();
    await store.put(rec("dm-alice", "tg:alice"));
    for (const members of [async () => Promise.reject(new Error("rate limited")), async () => new Set<string>()]) {
      const lc = fakeAuth();
      const bridge = createBridge(cfg, adapter({ members }).a, { logger: quiet, store, gateKey: keyFromSecret("s".repeat(40)), gateFetch: lc.fetchFn });
      await bridge.start();
      await vi.advanceTimersByTimeAsync(FIRST_RUN_BY + 6 * 3_600_000);
      expect(lc.calls.filter((c) => c.path === "/v1/_auth/revoke")).toHaveLength(0);
      await bridge.stop();
    }
  });
});

describe("the kept-revoke file", () => {
  it("keeps a revoke in memory when its file cannot be read, and never overwrites the file", async () => {
    const dir = await mkdtemp(path.join(os.tmpdir(), "bridge-file-"));
    const file = path.join(dir, "revokes.json");
    await writeFile(file, "{ not json", { mode: 0o600 });
    const errors: string[] = [];
    const q = new FileRevokeQueue(file, { info: () => {}, warn: () => {}, error: (m: string) => void errors.push(m) });
    await q.put({ subject: "tg:1", attempts: 1, firstFailedAt: 0, nextAttemptAt: 0 });
    expect((await q.list()).map((p) => p.subject)).toEqual(["tg:1"]);
    expect(await readFile(file, "utf8")).toBe("{ not json");
    expect(errors.length).toBeGreaterThan(0);

    // The file is readable again (the fault was transient, or an operator
    // repaired it): the next operation reads it, keeps both, and writes them.
    await writeFile(file, JSON.stringify([{ subject: "tg:2", attempts: 3, firstFailedAt: 0, nextAttemptAt: 0 }]));
    await q.put({ subject: "tg:3", attempts: 1, firstFailedAt: 0, nextAttemptAt: 0 });
    expect((await q.list()).map((p) => p.subject).sort()).toEqual(["tg:1", "tg:2", "tg:3"]);
    expect(JSON.parse(await readFile(file, "utf8")).map((p: { subject: string }) => p.subject).sort()).toEqual(["tg:1", "tg:2", "tg:3"]);
  });
});

describe("the bridge's files", () => {
  const stores = {
    conversations: (file: string) => {
      const s = new FileStore(file);
      return { list: () => s.list(), write: () => s.put(rec("dm-1", "tg:1")) };
    },
    revokes: (file: string) => {
      const q = new FileRevokeQueue(file);
      return { list: () => q.list(), write: () => q.put({ subject: "tg:1", attempts: 1, firstFailedAt: 0, nextAttemptAt: 0 }) };
    },
  };

  for (const [name, open] of Object.entries(stores)) {
    it(`${name}: a file that cannot be read is never overwritten`, async () => {
      const dir = await mkdtemp(path.join(os.tmpdir(), "bridge-file-"));
      const file = path.join(dir, "data.json");
      await writeFile(file, "{ not json", { mode: 0o600 });
      const s = open(file);
      if (name === "conversations") {
        await expect(s.list()).rejects.toThrow();
        await expect(s.write()).rejects.toThrow();
      } else {
        // A revoke is kept in memory instead (the kept-revoke file test).
        await s.write();
      }
      expect(await readFile(file, "utf8")).toBe("{ not json");
    });

    it(`${name}: a write that fails does not stop the next one`, async () => {
      const dir = await mkdtemp(path.join(os.tmpdir(), "bridge-file-"));
      const file = path.join(dir, "sub", "data.json");
      const s = open(file);
      await s.list(); // no file yet
      await writeFile(path.join(dir, "sub"), "a file where the directory goes");
      await expect(s.write()).rejects.toThrow();
      await rm(path.join(dir, "sub"));
      await s.write();
      expect(JSON.parse(await readFile(file, "utf8"))).toHaveLength(1);
    });

    it(`${name}: a stale temp file a crashed write left is cleared at load`, async () => {
      const dir = await mkdtemp(path.join(os.tmpdir(), "bridge-file-"));
      const file = path.join(dir, "data.json");
      const stale = `${file}.99999999.0b0b0b0b-0000-4000-8000-000000000000.tmp`;
      await writeFile(stale, "half a write");
      await writeFile(path.join(dir, "other.json.1.x.tmp"), "not ours");
      await open(file).list();
      await vi.waitFor(async () => expect((await readdir(dir)).sort()).toEqual(["other.json.1.x.tmp"]));
    });

    it(`${name}: the file is private even when a stale temp file is in the way`, async () => {
      const dir = await mkdtemp(path.join(os.tmpdir(), "bridge-file-"));
      const file = path.join(dir, "data.json");
      // A temp file a crashed write left, readable by everyone.
      await writeFile(`${file}.${process.pid}.tmp`, "", { mode: 0o644 });
      await chmod(`${file}.${process.pid}.tmp`, 0o644);
      await open(file).write();
      expect((await stat(file)).mode & 0o777).toBe(0o600);
    });
  }
});
