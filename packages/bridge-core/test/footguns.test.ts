// The configurations a bridge refuses, and what a gate bridge does on its own
// so a person who leaves the platform does not stay a loomcycle user.

import type { AddressInfo } from "node:net";
import { chmod, mkdtemp, readFile, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { afterEach, describe, expect, it, vi } from "vitest";

import { ConfigError, EnvReader, loadSharedConfig, type Env, type SharedConfig } from "../src/config.js";
import { keyFromSecret } from "../src/gate.js";
import { FileRevokeQueue } from "../src/revokes.js";
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
  const state = { failRevoke: false };
  const fetchFn = (async (url: string, init: RequestInit) => {
    const body = JSON.parse(String(init.body));
    const jwt: string = body.assertion ?? body.request;
    calls.push({ path: new URL(url).pathname, payload: JSON.parse(Buffer.from(jwt.split(".")[1]!, "base64url").toString()) });
    if (url.endsWith("/v1/_auth/revoke")) {
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

describe("reconciliation", () => {
  it("runs a complete sync on the timer", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
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

    await vi.advanceTimersByTimeAsync(6 * 3_600_000 - 1_000);
    expect(lc.calls.filter((c) => c.path === "/v1/_auth/sync")).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(1_000);
    const syncs = lc.calls.filter((c) => c.path === "/v1/_auth/sync");
    expect(syncs).toHaveLength(1);
    expect(syncs[0]!.payload).toMatchObject({ lc_use: "sync", iss: "tg", complete: true });
    // bob is left out, so loomcycle removes him; dave was never this
    // bridge's user, so he is not made one.
    expect(syncs[0]!.payload.users.map((u: { sub: string }) => u.sub).sort()).toEqual(["alice", "conv:abc"]);

    await vi.advanceTimersByTimeAsync(6 * 3_600_000);
    expect(lc.calls.filter((c) => c.path === "/v1/_auth/sync")).toHaveLength(2);
    await bridge.stop();
  });

  it("never syncs on a member list it could not read, or one that is empty", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
    const { cfg } = load(GATE_ENV, true);
    const store = new MemoryStore();
    await store.put(rec("dm-alice", "tg:alice"));
    for (const members of [async () => Promise.reject(new Error("rate limited")), async () => new Set<string>()]) {
      const lc = fakeAuth();
      const bridge = createBridge(cfg, adapter({ members }).a, { logger: quiet, store, gateKey: keyFromSecret("s".repeat(40)), gateFetch: lc.fetchFn });
      await bridge.start();
      await vi.advanceTimersByTimeAsync(6 * 3_600_000);
      expect(lc.calls.filter((c) => c.path === "/v1/_auth/sync")).toHaveLength(0);
      await bridge.stop();
    }
  });

  it("is off with a memory store, which forgets shared conversations on a restart", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
    const { cfg } = load(GATE_ENV, true);
    expect(cfg.store).toBe("memory");
    const lc = fakeAuth();
    const bridge = createBridge(cfg, adapter({ members: async () => new Set(["alice"]) }).a, {
      logger: quiet,
      gateKey: keyFromSecret("s".repeat(40)),
      gateFetch: lc.fetchFn,
    });
    await bridge.start();
    await vi.advanceTimersByTimeAsync(12 * 3_600_000);
    expect(lc.calls.filter((c) => c.path === "/v1/_auth/sync")).toHaveLength(0);
    await bridge.stop();
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
      await expect(s.list()).rejects.toThrow();
      await expect(s.write()).rejects.toThrow();
      expect(await readFile(file, "utf8")).toBe("{ not json");
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
