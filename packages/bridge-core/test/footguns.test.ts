// The configurations a bridge refuses, and what a gate bridge does on its own
// so a person who leaves the platform does not stay a loomcycle user.

import type { AddressInfo } from "node:net";

import { afterEach, describe, expect, it, vi } from "vitest";

import { ConfigError, EnvReader, loadSharedConfig, type Env, type SharedConfig } from "../src/config.js";
import { keyFromSecret } from "../src/gate.js";
import { createBridge, type BridgeAdapter, type BridgeContext } from "../src/start.js";

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
