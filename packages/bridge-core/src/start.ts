// Assembling a bridge: one platform adapter on the shared core.
//
// The core owns the store, the token broker, the conversations and the idle
// sweep. The adapter owns everything the platform decides: how a request is
// authenticated, who sent it, how a reply and a card are sent. An adapter
// whose platform posts webhooks mounts its routes with `ctx.serve`; one that
// brings its own server (the Teams SDK does) starts that instead.

import { LoomcycleClient } from "@loomcycle/client";
import type http from "node:http";

import { ConfigError, type SharedConfig } from "./config.js";
import type { ConversationDeps } from "./conversation.js";
import { Health } from "./health.js";
import { close, createBridgeServer, listen, type Route } from "./http.js";
import type { PlatformCapabilities, PlatformInfo } from "./platform.js";
import { consoleLogger, type BridgePort, type ClientFactory, type Logger } from "./ports.js";
import type { MessageSinkOptions, StreamSinkOptions } from "./renderer.js";
import { ConversationRouter } from "./router.js";
import { storeFromSpec, type ConversationStore } from "./store.js";
import { GateBroker, loadGateKey, type GateKey } from "./gate.js";
import { TokenBroker, type TokenSource } from "./tokens.js";

export interface BridgeContext<TRef> {
  config: SharedConfig;
  router: ConversationRouter<TRef>;
  health: Health;
  log: Logger;
  /** Serve routes on the configured port, with GET /healthz beside them. */
  serve(routes: Record<string, Route>, opts?: { maxBodyBytes?: number }): Promise<http.Server>;
  /** Set when the bridge is a loomcycle gate: subjectOf names a platform id's
   *  loomcycle subject, revoke tells loomcycle the identity is gone. */
  gate?: { subjectOf(sub: string): string; revoke(subject: string): Promise<void> };
}

export interface BridgeAdapter<TRef> {
  platform: PlatformInfo;
  capabilities: PlatformCapabilities;
  port: BridgePort<TRef>;
  /** Start receiving from the platform. */
  start(ctx: BridgeContext<TRef>): Promise<void>;
  /** Stop receiving. The core stops its own server, if one was served. */
  stop?(): Promise<void>;
  /** A gate bridge's groups for a run identity, from its conversation's
   *  reply reference (RFC FA §3.7d). undefined asserts none. */
  groupsFor?(subject: string, ref: TRef): Promise<string[] | undefined>;
}

export interface BridgeOptions<TRef> {
  store?: ConversationStore<TRef>;
  logger?: Logger;
  /** Override how loomcycle clients are built (tests, a custom fetch). */
  clients?: ClientFactory;
  stream?: StreamSinkOptions;
  message?: MessageSinkOptions;
  now?: () => number;
  /** A gate bridge's key, instead of reading it from the environment (tests). */
  gateKey?: GateKey;
  /** The fetch a gate bridge reaches loomcycle's /v1/_auth routes with (tests). */
  gateFetch?: typeof fetch;
}

export interface Bridge<TRef> {
  router: ConversationRouter<TRef>;
  health: Health;
  start(): Promise<void>;
  stop(): Promise<void>;
}

export function createBridge<TRef>(cfg: SharedConfig, adapter: BridgeAdapter<TRef>, opts: BridgeOptions<TRef> = {}): Bridge<TRef> {
  const log = opts.logger ?? consoleLogger;
  const clients: ClientFactory =
    opts.clients ?? ((token) => new LoomcycleClient({ baseUrl: cfg.loomcycleBaseUrl, authToken: token }));
  const store = opts.store ?? storeFromSpec<TRef>(cfg.store);
  const health = new Health(adapter.platform.name, opts.now);

  // Every send goes through here, so /healthz knows when the platform last
  // took a message.
  const port: BridgePort<TRef> = {
    send: async (ref, activity) => {
      await adapter.port.send(ref, activity);
      health.outbound();
    },
    ...(adapter.port.createStream ? { createStream: (ref: TRef) => adapter.port.createStream!(ref) } : {}),
  };

  // A gate bridge asks loomcycle for its users' tokens; any other bridge
  // mints them with its own operator token.
  const gateBroker = cfg.gate
    ? new GateBroker(
        {
          baseUrl: cfg.loomcycleBaseUrl,
          gate: cfg.gate.name,
          audience: cfg.gate.audience,
          token: {
            agents: [cfg.agent],
            // `interactive` is itself a lockable override, and the bridge
            // must set it; every other per-run setting stays the agent's.
            overrides: ["interactive"],
            ratePerMinute: cfg.ratePerMinute,
            rateBurst: cfg.rateBurst,
            ttlSeconds: cfg.tokenTtlSeconds,
          },
          fetch: opts.gateFetch,
          now: opts.now,
        },
        adapter.groupsFor ? (subject, ref) => adapter.groupsFor!(subject, ref as TRef) : undefined,
      )
    : undefined;
  const broker: TokenSource =
    gateBroker ??
    new TokenBroker(clients(cfg.loomcycleToken), {
      agent: cfg.agent,
      tokenName: `${adapter.platform.name}-bridge`,
      ttlSeconds: cfg.tokenTtlSeconds,
      ratePerMinute: cfg.ratePerMinute,
      rateBurst: cfg.rateBurst,
      accessMode: cfg.userAccessMode,
      now: opts.now,
    });
  const deps: ConversationDeps<TRef> = {
    platform: adapter.platform,
    capabilities: adapter.capabilities,
    agent: cfg.agent,
    store,
    broker,
    clients,
    port,
    log,
    health,
    now: opts.now,
    stream: opts.stream,
    message: opts.message,
  };
  const router = new ConversationRouter<TRef>({ idleMinutes: cfg.idleMinutes }, deps);

  let sweeper: ReturnType<typeof setInterval> | undefined;
  let server: http.Server | undefined;

  const ctx: BridgeContext<TRef> = {
    config: cfg,
    router,
    health,
    log,
    ...(gateBroker ? { gate: { subjectOf: (sub: string) => gateBroker.subjectOf(sub), revoke: (subject: string) => gateBroker.revoke(subject) } } : {}),
    async serve(routes, o) {
      if (server) throw new Error("serve called twice");
      server = createBridgeServer({ routes, health, log, maxBodyBytes: o?.maxBodyBytes });
      await listen(server, cfg.port);
      return server;
    },
  };

  const name = `${adapter.platform.name}-bridge`;
  return {
    router,
    health,
    async start() {
      if (cfg.dev) log.warn(`BRIDGE_DEV=1: inbound requests are NOT authenticated; never use this in production`);
      // The gate key is read once, here, and held in memory.
      if (gateBroker && cfg.gate) gateBroker.ready(opts.gateKey ?? (await loadGateKey(cfg.gate.keyEnv)));
      await adapter.start(ctx);
      sweeper = setInterval(() => void router.sweepIdle(), 60_000);
      sweeper.unref?.();
      log.info(`${name} listening`, { port: cfg.port, agent: cfg.agent });
    },
    async stop() {
      if (sweeper) clearInterval(sweeper);
      await router.stop();
      await adapter.stop?.();
      if (server) await close(server);
    },
  };
}

/** runCli runs a bridge until SIGINT or SIGTERM. `build` reads the
 *  configuration and returns the bridge; a ConfigError is reported as a list
 *  of problems and exit code 2. */
export async function runCli<TRef>(name: string, build: () => Bridge<TRef>): Promise<void> {
  let bridge: Bridge<TRef>;
  try {
    bridge = build();
  } catch (err) {
    if (err instanceof ConfigError) {
      for (const p of err.problems) console.error(`${name}: ${p}`);
      process.exit(2);
    }
    throw err;
  }
  const shutdown = (signal: string) => {
    console.error(`${name}: ${signal}, stopping`);
    bridge.stop().then(
      () => process.exit(0),
      () => process.exit(1),
    );
  };
  process.once("SIGINT", () => shutdown("SIGINT"));
  process.once("SIGTERM", () => shutdown("SIGTERM"));
  try {
    await bridge.start();
  } catch (err) {
    console.error(`${name}: failed to start:`, (err as Error)?.message ?? err);
    process.exit(1);
  }
}
