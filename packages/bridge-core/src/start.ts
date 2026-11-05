// Assembling a bridge: one platform adapter on the shared core.
//
// The core owns the store, the token broker, the conversations and the idle
// sweep. The adapter owns everything the platform decides: how a request is
// authenticated, who sent it, how a reply and a card are sent. An adapter
// whose platform posts webhooks mounts its routes with `ctx.serve`; one that
// brings its own server (the Teams SDK does) starts that instead.

import { LoomcycleClient } from "@loomcycle/client";
import type http from "node:http";

import { ConfigError, DEFAULT_GATE_SYNC_HOURS, devModeProblem, type SharedConfig } from "./config.js";
import type { ConversationDeps } from "./conversation.js";
import { Health } from "./health.js";
import { close, createBridgeServer, listen, type Route } from "./http.js";
import type { PlatformCapabilities, PlatformInfo } from "./platform.js";
import { consoleLogger, type BridgePort, type ClientFactory, type Logger } from "./ports.js";
import type { MessageSinkOptions, StreamSinkOptions } from "./renderer.js";
import { ConversationRouter } from "./router.js";
import { storeFromSpec, type ConversationStore } from "./store.js";
import { GateBroker, loadGateKey, type GateKey } from "./gate.js";
import { retryDelayMs, revokeQueueFromSpec, type RevokeQueue } from "./revokes.js";
import { statusOf, TokenBroker, type TokenSource } from "./tokens.js";

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
  /** A gate bridge whose platform can list its people: the platform id (the
   *  gate's `sub`, without "<gate>:") of every person who is a member now.
   *  The list must be complete: throw rather than return a first page. Every
   *  BRIDGE_GATE_SYNC_HOURS the core revokes each person it served who is not
   *  on it. */
  members?(): Promise<ReadonlySet<string>>;
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
  /** Where a gate bridge keeps revokes loomcycle has not taken yet. Default:
   *  beside BRIDGE_STORE's file ("<path>.revokes.json"), else in memory. */
  revokes?: RevokeQueue;
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
  const now = opts.now ?? Date.now;
  const gateName = cfg.gate?.name ?? "";

  // A revoke loomcycle did not take is kept and retried by the bridge itself:
  // the platform's own redelivery gives up after minutes or an hour.
  const revokes = gateBroker ? (opts.revokes ?? revokeQueueFromSpec(cfg.store)) : undefined;
  // A person a kept revoke names who writes to the bot again has come back
  // (unblocked it, reinstalled the app): the revoke is dropped, not delivered.
  const onSeen = (subject: string): void => {
    if (!revokes || !subject.startsWith(`${gateName}:`)) return;
    void revokes
      .list()
      .then(async (all) => {
        if (!all.some((p) => p.subject === subject)) return;
        await revokes.delete(subject);
        log.info("a kept revoke was dropped: the person wrote again", {});
      })
      .catch((err: unknown) => log.warn("kept revokes could not be read", { error: (err as Error)?.name }));
  };
  const router = new ConversationRouter<TRef>({ idleMinutes: cfg.idleMinutes, onSeen }, deps);
  const keepRevoke = async (subject: string): Promise<void> => {
    try {
      const prev = (await revokes!.list()).find((p) => p.subject === subject);
      const attempts = (prev?.attempts ?? 0) + 1;
      await revokes!.put({ subject, attempts, firstFailedAt: prev?.firstFailedAt ?? now(), nextAttemptAt: now() + retryDelayMs(attempts) });
    } catch (err) {
      log.error("a failed revoke could not be kept for retry", { error: (err as Error)?.name });
    }
  };
  const revoke = async (subject: string): Promise<void> => {
    try {
      await gateBroker!.revoke(subject);
    } catch (err) {
      await keepRevoke(subject);
      if (stuck(err)) log.error("a revoke is stuck: loomcycle refused it; check the gate's key and name", { status: statusOf(err) });
      throw err;
    }
    await revokes!.delete(subject).catch((err: unknown) => log.warn("a delivered revoke stayed queued", { error: (err as Error)?.name }));
  };
  let retrying = false;
  // `due` false retries every kept revoke, backoff or not: at start, after a
  // restart that may well have been the fix (loomcycle back, a key replaced).
  const retryRevokes = async (due = true): Promise<void> => {
    if (!revokes || retrying) return;
    retrying = true;
    try {
      for (const p of await revokes.list()) {
        if (due && p.nextAttemptAt > now()) continue;
        try {
          await gateBroker!.revoke(p.subject);
          await revokes.delete(p.subject);
          log.info("a kept revoke reached loomcycle", { attempts: p.attempts });
        } catch (err) {
          await revokes.put({ ...p, attempts: p.attempts + 1, nextAttemptAt: now() + retryDelayMs(p.attempts + 1) });
          if (stuck(err)) {
            log.error("a kept revoke is stuck: loomcycle refused it; check the gate's key and name", { status: statusOf(err), attempts: p.attempts + 1 });
          } else {
            log.warn("a kept revoke failed again; it is retried later", { status: statusOf(err), attempts: p.attempts + 1 });
          }
        }
      }
    } catch (err) {
      log.warn("kept revokes could not be retried", { error: (err as Error)?.name });
    } finally {
      retrying = false;
    }
  };

  // Reconciliation acts only on evidence: a person this bridge served (a
  // run subject in its store) whom the platform's member list no longer has
  // is revoked, through the same path as a platform's own leave event. A user
  // the store does not know (a lost or replaced store, another bridge's) is
  // never touched, and a shared conversation's own user is not a person. A
  // round that would revoke more than its cap is skipped whole: a member list
  // read from the wrong workspace looks exactly like everyone leaving.
  let syncing = false;
  const reconcile = async (): Promise<void> => {
    if (!gateBroker || !adapter.members || syncing) return;
    syncing = true;
    try {
      let members: ReadonlySet<string>;
      try {
        members = await adapter.members();
      } catch (err) {
        log.warn("reconcile skipped: the platform's member list could not be read", { error: (err as Error)?.name });
        return;
      }
      if (members.size === 0) {
        log.warn("reconcile skipped: the platform listed no members", {});
        return;
      }
      const prefix = `${gateName}:`;
      const records = await store.list();
      const people = new Set(
        records.map((r) => r.runSubject).filter((s) => s.startsWith(prefix) && s.length > prefix.length && !s.startsWith(`${prefix}conv:`)),
      );
      const pending = new Set((await revokes!.list()).map((p) => p.subject));
      const gone = [...people].filter((s) => !members.has(s.slice(prefix.length)) && !pending.has(s));
      const cap = cfg.reconcileMaxRevokes ?? reconcileCap(people.size);
      if (gone.length > cap) {
        // Never a part of them: a short member list must not drain the users
        // a round at a time. Said every round until someone looks.
        log.error("reconcile revoked nobody: more people gone than the cap allows (BRIDGE_RECONCILE_MAX_REVOKES)", {
          gone: gone.length,
          cap,
          people: people.size,
        });
        return;
      }
      let revoked = 0;
      for (const subject of gone) {
        try {
          await revoke(subject);
        } catch {
          continue; // kept for retry; logged by revoke
        }
        revoked++;
        // Their conversations go too: the person has left the platform, and
        // a record kept would be revoked again every round.
        for (const r of records) if (r.runSubject === subject) await store.delete(r.conversationId);
      }
      if (gone.length > 0) log.info("reconcile revoked people the platform no longer lists", { gone: gone.length, revoked });
    } catch (err) {
      log.warn("reconcile failed", { error: (err as Error)?.name });
    } finally {
      syncing = false;
    }
  };

  let sweeper: ReturnType<typeof setInterval> | undefined;
  let revokeTimer: ReturnType<typeof setInterval> | undefined;
  let syncTimer: ReturnType<typeof setInterval> | undefined;
  let firstSync: ReturnType<typeof setTimeout> | undefined;
  let server: http.Server | undefined;

  const ctx: BridgeContext<TRef> = {
    config: cfg,
    router,
    health,
    log,
    ...(gateBroker ? { gate: { subjectOf: (sub: string) => gateBroker.subjectOf(sub), revoke } } : {}),
    async serve(routes, o) {
      if (server) throw new Error("serve called twice");
      server = createBridgeServer({ routes, health, log, maxBodyBytes: o?.maxBodyBytes });
      await listen(server, cfg.port, cfg.bindHost);
      return server;
    },
  };

  const name = `${adapter.platform.name}-bridge`;
  return {
    router,
    health,
    async start() {
      // Also checked when the configuration is read; a config built in code
      // is held to the same rule here.
      const devProblem = devModeProblem(cfg);
      if (devProblem) throw new ConfigError(name, [devProblem]);
      if (cfg.dev) log.warn(`BRIDGE_DEV=1: inbound requests are NOT authenticated; never use this in production`);
      // The gate key is read once, here, and held in memory.
      if (gateBroker && cfg.gate) gateBroker.ready(opts.gateKey ?? (await loadGateKey(cfg.gate.keyEnv)));
      await adapter.start(ctx);
      sweeper = setInterval(() => void router.sweepIdle(), 60_000);
      sweeper.unref?.();
      if (revokes) {
        // Revokes kept before a restart go out now, then on the minute.
        void retryRevokes(false);
        revokeTimer = setInterval(() => void retryRevokes(), 60_000);
        revokeTimer.unref?.();
      }
      const syncHours = cfg.gateSyncHours ?? DEFAULT_GATE_SYNC_HOURS;
      if (gateBroker && adapter.members && syncHours > 0) {
        if (opts.store === undefined && cfg.store === "memory") {
          // Reconciliation acts on the people in the store; a memory store
          // starts empty after every restart.
          log.warn("reconcile has nothing to go on after a restart with the memory store: set BRIDGE_STORE=file:<path>", {});
        }
        // The first round a few minutes after start (jittered, so replicas
        // and restarts do not line up), then on the interval.
        firstSync = setTimeout(() => void reconcile(), 120_000 + Math.floor(Math.random() * 180_000));
        firstSync.unref?.();
        syncTimer = setInterval(() => void reconcile(), syncHours * 3_600_000);
        syncTimer.unref?.();
      }
      log.info(`${name} listening`, { port: cfg.port, agent: cfg.agent });
    },
    async stop() {
      if (sweeper) clearInterval(sweeper);
      if (revokeTimer) clearInterval(revokeTimer);
      if (syncTimer) clearInterval(syncTimer);
      if (firstSync) clearTimeout(firstSync);
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

/** reconcileCap is the most people one reconcile round may revoke: 10% of
 *  the people the bridge serves, never fewer than five. */
function reconcileCap(people: number): number {
  return Math.max(5, Math.floor(people / 10));
}

/** stuck: loomcycle refused the revoke (a 4xx other than a timeout or
 *  throttling). It answers an unknown or already-removed person 204, so a
 *  refusal is never "nothing to revoke" but a key, a gate name or a server
 *  setting that is wrong: the revoke is kept and retried, never dropped, and
 *  the operator is told at error. */
function stuck(err: unknown): boolean {
  const s = statusOf(err);
  return s !== undefined && s >= 400 && s < 500 && s !== 408 && s !== 429;
}
