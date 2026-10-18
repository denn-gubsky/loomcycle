import { mkdtemp, readdir, readFile, stat } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { optionLabels } from "../src/cards.js";
import { ConfigError, EnvReader, loadSharedConfig } from "../src/config.js";
import { Health } from "../src/health.js";
import { close, createBridgeServer, listen } from "../src/http.js";
import { sharedSubject } from "../src/identity.js";
import { FileStore } from "../src/store.js";
import { parseCommand, splitMessage } from "../src/text.js";
import { TokenBroker } from "../src/tokens.js";
import { safeEqual } from "../src/verify.js";
import { FakeLoomcycle, silentLogger } from "./fakes.js";

describe("token broker", () => {
  const opts = { agent: "helper", tokenName: "test-bridge", ttlSeconds: 43200, ratePerMinute: 6, rateBurst: 10, accessMode: "isolated" as const };

  it("mints once for a burst of requests from one user", async () => {
    const lc = new FakeLoomcycle();
    const broker = new TokenBroker(lc.client("admin"), opts);
    const tokens = await Promise.all(Array.from({ length: 10 }, () => broker.tokenFor("test-u")));
    expect(new Set(tokens).size).toBe(1);
    expect(lc.mints).toHaveLength(1);
    expect(lc.callsTo("createUser")).toHaveLength(1);
  });

  it("treats an already-registered user as registered", async () => {
    const lc = new FakeLoomcycle();
    lc.existingUsers.add("test-u");
    const broker = new TokenBroker(lc.client("admin"), opts);
    await expect(broker.tokenFor("test-u")).resolves.toMatch(/^tok-/);
  });

  it("re-mints shortly before the token expires", async () => {
    const lc = new FakeLoomcycle();
    let now = 0;
    lc.now = () => now;
    lc.ttlMs = 60 * 60_000;
    const broker = new TokenBroker(lc.client("admin"), { ...opts, now: () => now });
    const first = await broker.tokenFor("test-u");
    now += 45 * 60_000;
    expect(await broker.tokenFor("test-u")).toBe(first);
    now += 6 * 60_000; // inside the 10-minute renewal window
    expect(await broker.tokenFor("test-u")).not.toBe(first);
    expect(lc.mints).toHaveLength(2);
    expect(lc.callsTo("createUser")).toHaveLength(1);
  });

  it("labels every token with the platform's name", async () => {
    const lc = new FakeLoomcycle();
    await new TokenBroker(lc.client("admin"), { ...opts, tokenName: "slack-bridge" }).tokenFor("slack-u");
    expect(lc.mints[0].opts.name).toBe("slack-bridge");
  });
});

describe("shared config", () => {
  const base = {
    LOOMCYCLE_BASE_URL: "http://loomcycle:8787/",
    LOOMCYCLE_TOKEN: "t",
    BRIDGE_AGENT: "helper",
  };

  it("reads defaults", () => {
    const r = new EnvReader(base);
    const cfg = loadSharedConfig(r, { port: 3000 });
    r.check("test-bridge");
    expect(cfg).toMatchObject({
      loomcycleBaseUrl: "http://loomcycle:8787",
      agent: "helper",
      tokenTtlSeconds: 43200,
      ratePerMinute: 6,
      rateBurst: 10,
      userAccessMode: "isolated",
      idleMinutes: 1440,
      store: "memory",
      port: 3000,
      dev: false,
    });
  });

  it("lists every problem at once, the platform's own included", () => {
    const r = new EnvReader({ BRIDGE_USER_ACCESS_MODE: "admin", BRIDGE_STORE: "redis", BRIDGE_DEV: "yes", PORT: "99999" });
    loadSharedConfig(r, { port: 3000 });
    r.required("SLACK_BOT_TOKEN");
    try {
      r.check("slack-bridge");
      expect.unreachable();
    } catch (err) {
      const e = err as ConfigError;
      expect(e).toBeInstanceOf(ConfigError);
      expect(e.problems).toEqual(
        expect.arrayContaining([
          "LOOMCYCLE_BASE_URL is required",
          "LOOMCYCLE_TOKEN is required",
          "BRIDGE_AGENT is required",
          'BRIDGE_USER_ACCESS_MODE must be one of "isolated", "tenant"',
          'BRIDGE_STORE must be "memory" or "file:<path>"',
          "BRIDGE_DEV must be 1 or 0",
          "PORT must be an integer from 1 to 65535",
          "SLACK_BOT_TOKEN is required",
        ]),
      );
    }
  });

  it("reads comma lists", () => {
    expect(new EnvReader({ X: " A, b ,," }).list("X", { lower: true })).toEqual(["a", "b"]);
    expect(new EnvReader({}).list("X")).toEqual([]);
  });
});

describe("cards", () => {
  it("reads string and object options", () => {
    expect(optionLabels(["a", { label: "b" }, { value: "c" }, 7])).toEqual(["a", "b", "c"]);
    expect(optionLabels(undefined)).toEqual([]);
  });
});

describe("identity", () => {
  it("names a shared conversation by its parts, the same way every time", () => {
    const a = sharedSubject("slack", "T1", "C1");
    expect(a).toMatch(/^slack-conv-[0-9a-f]{32}$/);
    expect(sharedSubject("slack", "T1", "C1")).toBe(a);
    expect(sharedSubject("slack", "T1", "C2")).not.toBe(a);
    // The prefix keeps platforms apart even for equal parts.
    expect(sharedSubject("telegram", "T1", "C1").startsWith("telegram-conv-")).toBe(true);
  });
});

describe("text", () => {
  it("recognises only whole-message commands", () => {
    expect(parseCommand(" /NEW ")).toBe("new");
    expect(parseCommand("/new please")).toBeUndefined();
  });

  it("splits long text at line breaks", () => {
    const parts = splitMessage("aaaa\nbbbb\ncccc", 10);
    expect(parts).toEqual(["aaaa\nbbbb", "cccc"]);
    expect(splitMessage("x".repeat(25), 10)).toEqual(["x".repeat(10), "x".repeat(10), "x".repeat(5)]);
  });
});

describe("file store", () => {
  it("survives a restart and keeps the file private", async () => {
    const dir = await mkdtemp(path.join(os.tmpdir(), "bridge-core-"));
    const file = path.join(dir, "conversations.json");
    const a = new FileStore<{ chat: string }>(file);
    await a.put({
      conversationId: "c1",
      scope: "personal",
      ref: { chat: "c1" },
      ownerSubject: "test-u",
      ownerName: "U",
      runSubject: "test-u",
      runName: "U",
      sessionId: "s1",
      lastActivityAt: 5,
    });
    const b = new FileStore<{ chat: string }>(file);
    expect((await b.get("c1"))?.sessionId).toBe("s1");
    expect((await b.get("c1"))?.ref).toEqual({ chat: "c1" });
    expect((await stat(file)).mode & 0o777).toBe(0o600);
    expect(JSON.parse(await readFile(file, "utf8"))).toHaveLength(1);
  });
});

describe("safeEqual", () => {
  it("matches only equal values, and never a missing one", () => {
    expect(safeEqual("abc", "abc")).toBe(true);
    expect(safeEqual("abc", "abd")).toBe(false);
    expect(safeEqual("abc", "abcd")).toBe(false);
    expect(safeEqual(undefined, "abc")).toBe(false);
    expect(safeEqual(undefined, undefined)).toBe(false);
  });
});

describe("http listener", () => {
  async function serve(maxBodyBytes?: number) {
    const health = new Health("test", () => Date.parse("2026-10-18T00:00:00Z"));
    let seen: Buffer | undefined;
    const server = createBridgeServer({
      routes: {
        "POST /hook": async (req) => {
          seen = req.body;
          return { status: 200, body: { ok: true, q: req.query.get("q") } };
        },
        "POST /boom": async () => {
          throw new Error("the secret is hunter2");
        },
      },
      health,
      log: silentLogger,
      maxBodyBytes,
    });
    await listen(server, 0, "127.0.0.1");
    const addr = server.address();
    if (!addr || typeof addr === "string") throw new Error("no address");
    return { base: `http://127.0.0.1:${addr.port}`, server, health, seen: () => seen };
  }

  it("hands a route the exact bytes, before anything parses them", async () => {
    const s = await serve();
    try {
      const raw = '{"b":1,  "a":2}';
      const res = await fetch(`${s.base}/hook?q=1`, { method: "POST", body: raw, headers: { "content-type": "application/json" } });
      expect(await res.json()).toEqual({ ok: true, q: "1" });
      expect(s.seen()?.toString("utf8")).toBe(raw);
    } finally {
      await close(s.server);
    }
  });

  it("refuses a body over the limit with 413", async () => {
    const s = await serve(16);
    try {
      const res = await fetch(`${s.base}/hook`, { method: "POST", body: "x".repeat(64) });
      expect(res.status).toBe(413);
      expect(s.seen()).toBeUndefined();
    } finally {
      await close(s.server);
    }
  });

  it("answers 404 for an unknown route, and 500 without detail when a route throws", async () => {
    const s = await serve();
    try {
      expect((await fetch(`${s.base}/nope`, { method: "POST" })).status).toBe(404);
      expect((await fetch(`${s.base}/hook`)).status).toBe(404);
      const boom = await fetch(`${s.base}/boom`, { method: "POST" });
      expect(boom.status).toBe(500);
      expect(await boom.text()).not.toContain("hunter2");
    } finally {
      await close(s.server);
    }
  });

  it("serves /healthz with times only", async () => {
    const s = await serve();
    try {
      s.health.inbound();
      const body = (await (await fetch(`${s.base}/healthz`)).json()) as Record<string, unknown>;
      expect(Object.keys(body).sort()).toEqual(["last_inbound_at", "last_loomcycle_at", "last_outbound_at", "ok", "platform", "started_at"]);
      expect(body).toMatchObject({ ok: true, platform: "test", last_inbound_at: "2026-10-18T00:00:00.000Z", last_outbound_at: null });
    } finally {
      await close(s.server);
    }
  });
});

describe("the core is a core", () => {
  // Only loomcycle's client and node built-ins: a platform SDK in here would
  // make every bridge install it.
  it("imports no platform SDK", async () => {
    const dir = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "src");
    const imports = new Set<string>();
    for (const f of await readdir(dir)) {
      const src = await readFile(path.join(dir, f), "utf8");
      for (const m of src.matchAll(/from\s+"([^"]+)"/g)) imports.add(m[1]);
    }
    const external = [...imports].filter((i) => !i.startsWith("./") && !i.startsWith("node:"));
    expect(external.sort()).toEqual(["@loomcycle/client"]);
  });
});
