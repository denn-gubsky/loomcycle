import { mkdtemp, readFile, stat } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { describe, expect, it } from "vitest";

import { optionLabels, parseCardAction, questionCard, VERB } from "../src/cards.js";
import { ConfigError, loadConfig } from "../src/config.js";
import { identify, subjectFor } from "../src/identity.js";
import { FileStore } from "../src/store.js";
import { parseCommand, splitMessage } from "../src/text.js";
import { TokenBroker } from "../src/tokens.js";
import { FakeLoomcycle } from "./fakes.js";

const T = "11111111-1111-1111-1111-111111111111";
const U = "AAAAAAAA-aaaa-aaaa-aaaa-aaaaaaaaaaaa";

describe("identity", () => {
  it("maps the Entra object id to a stable, lower-cased subject", () => {
    expect(subjectFor(U)).toBe("teams-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa");
    expect(subjectFor("not-a-guid")).toBeUndefined();
  });

  it("reads the tenant from the conversation or, failing that, channelData", () => {
    const fromChannelData = identify(
      { from: { aadObjectId: U }, conversation: { id: "c" }, channelData: { tenant: { id: T.toUpperCase() } } },
      new Set([T]),
    );
    expect(fromChannelData).toMatchObject({ ok: true, tenantId: T, scope: "personal" });
  });

  it("refuses a missing user or a tenant outside the allowlist", () => {
    expect(identify({ conversation: { id: "c", tenantId: T } }, new Set([T]))).toEqual({ ok: false, reason: "no_user" });
    expect(identify({ from: { aadObjectId: U }, conversation: { id: "c", tenantId: "x" } }, new Set([T]))).toEqual({
      ok: false,
      reason: "tenant_not_allowed",
    });
  });
});

describe("token broker", () => {
  const opts = { agent: "helper", ttlSeconds: 43200, ratePerMinute: 6, rateBurst: 10, accessMode: "isolated" as const };

  it("mints once for a burst of requests from one user", async () => {
    const lc = new FakeLoomcycle();
    const broker = new TokenBroker(lc.client("admin"), opts);
    const tokens = await Promise.all(Array.from({ length: 10 }, () => broker.tokenFor("teams-u")));
    expect(new Set(tokens).size).toBe(1);
    expect(lc.mints).toHaveLength(1);
    expect(lc.callsTo("createUser")).toHaveLength(1);
  });

  it("treats an already-registered user as registered", async () => {
    const lc = new FakeLoomcycle();
    lc.existingUsers.add("teams-u");
    const broker = new TokenBroker(lc.client("admin"), opts);
    await expect(broker.tokenFor("teams-u")).resolves.toMatch(/^tok-/);
  });

  it("re-mints shortly before the token expires", async () => {
    const lc = new FakeLoomcycle();
    let now = 0;
    lc.now = () => now;
    lc.ttlMs = 60 * 60_000;
    const broker = new TokenBroker(lc.client("admin"), { ...opts, now: () => now });
    const first = await broker.tokenFor("teams-u");
    now += 45 * 60_000;
    expect(await broker.tokenFor("teams-u")).toBe(first);
    now += 6 * 60_000; // inside the 10-minute renewal window
    expect(await broker.tokenFor("teams-u")).not.toBe(first);
    expect(lc.mints).toHaveLength(2);
    expect(lc.callsTo("createUser")).toHaveLength(1);
  });
});

describe("config", () => {
  const base = {
    LOOMCYCLE_BASE_URL: "http://loomcycle:8787/",
    LOOMCYCLE_TOKEN: "t",
    TEAMS_BRIDGE_AGENT: "helper",
    TEAMS_BRIDGE_ALLOWED_TENANTS: `${T.toUpperCase()}, `,
    CLIENT_ID: "bot",
  };

  it("reads defaults and normalises tenants", () => {
    const cfg = loadConfig(base);
    expect(cfg.loomcycleBaseUrl).toBe("http://loomcycle:8787");
    expect([...cfg.allowedTenants]).toEqual([T]);
    expect(cfg).toMatchObject({ tokenTtlSeconds: 43200, userAccessMode: "isolated", idleMinutes: 1440, store: "memory", dev: false });
  });

  it("refuses to start without bot credentials unless in dev mode", () => {
    const { CLIENT_ID: _drop, ...noBot } = base;
    expect(() => loadConfig(noBot)).toThrow(ConfigError);
    expect(loadConfig({ ...noBot, TEAMS_BRIDGE_DEV: "1" }).dev).toBe(true);
  });

  it("lists every problem at once", () => {
    try {
      loadConfig({ TEAMS_BRIDGE_USER_ACCESS_MODE: "admin" });
      expect.unreachable();
    } catch (err) {
      expect((err as ConfigError).problems.length).toBeGreaterThanOrEqual(5);
    }
  });
});

describe("cards", () => {
  it("reads string and object options", () => {
    expect(optionLabels(["a", { label: "b" }, { value: "c" }, 7])).toEqual(["a", "b", "c"]);
    expect(optionLabels(undefined)).toEqual([]);
  });

  it("offers a text field when the question has no options", () => {
    const card = questionCard({ runId: "r", interruptId: "q", question: "Why?", options: [] }) as { body: Array<{ type: string }> };
    expect(card.body.some((b) => b.type === "Input.Text")).toBe(true);
  });

  it("ignores card actions the bridge did not issue", () => {
    expect(parseCardAction({ action: { verb: "other", data: { runId: "r" } } })).toBeUndefined();
    expect(parseCardAction({ action: { verb: VERB.approve, data: {} } })).toBeUndefined();
    expect(parseCardAction({ action: { verb: VERB.answer, data: { runId: "r", interruptId: "q", answer: " yes " } } })).toEqual({
      verb: VERB.answer,
      runId: "r",
      interruptId: "q",
      answer: "yes",
      feedback: undefined,
    });
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
    const dir = await mkdtemp(path.join(os.tmpdir(), "teams-bridge-"));
    const file = path.join(dir, "conversations.json");
    const a = new FileStore(file);
    await a.put({
      conversationId: "c1",
      scope: "personal",
      ref: { conversation: { id: "c1" }, serviceUrl: "https://x/" },
      ownerSubject: "teams-u",
      ownerName: "U",
      sessionId: "s1",
      lastActivityAt: 5,
    });
    const b = new FileStore(file);
    expect((await b.get("c1"))?.sessionId).toBe("s1");
    expect((await stat(file)).mode & 0o777).toBe(0o600);
    expect(JSON.parse(await readFile(file, "utf8"))).toHaveLength(1);
  });
});
