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
import { TokenBroker } from "./tokens.js";

export interface BridgeContext<TRef> {
  config: SharedConfig;
  router: ConversationRouter<TRef>;
  health: Health;
  log: Logger;
  /** Serve routes on the configured port, with GET /healthz beside them. */
  serve(routes: Record<string, Route>, opts?: { maxBodyBytes?: number }): Promise<http.Server>;
}

export interface BridgeAdapter<TRef> {
  platform: PlatformInfo;
  capabilities: PlatformCapabilities;
  port: BridgePort<TRef>;
  /** Start receiving from the platform. */
  start(ctx: BridgeContext<TRef>): Promise<void>;
  /** Stop receiving. The core stops its own server, if one was served. */
  stop?(): Promise<void>;
}

export interface BridgeOptions<TRef> {
  store?: ConversationStore<TRef>;
  logger?: Logger;
  /** Override how loomcycle clients are built (tests, a custom fetch). */
  clients?: ClientFactory;
  stream?: StreamSinkOptions;
  message?: MessageSinkOptions;
  now?: () => number;
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

  const broker = new TokenBroker(clients(cfg.loomcycleToken), {
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
export async function runCli(name: string, build: () => Bridge<unknown>): Promise<void> {
  let bridge: Bridge<unknown>;
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
