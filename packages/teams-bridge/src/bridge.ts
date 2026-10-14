// The Teams side: the SDK app, its two routes, and the TeamsPort the rest of
// the bridge talks to.

import { LoomcycleClient } from "@loomcycle/client";
import { stripMentionsText, type AdaptiveCardActionResponse, type ConversationReference } from "@microsoft/teams.api";
import { App } from "@microsoft/teams.apps";

import type { BridgeConfig } from "./config.js";
import type { CardResult } from "./conversation.js";
import { consoleLogger, type ClientFactory, type ConversationRef, type Logger, type OutboundActivity, type StreamLike, type TeamsPort } from "./ports.js";
import { ConversationRouter } from "./router.js";
import { storeFromSpec, type ConversationStore } from "./store.js";
import { TokenBroker } from "./tokens.js";

const ADAPTIVE_CARD = "application/vnd.microsoft.card.adaptive";

/** BridgeApp exposes the SDK's activity sender so the bridge can send and
 *  stream to a stored conversation reference outside a handler: a loomcycle
 *  turn outlives the request that started it. */
export class BridgeApp extends App {
  createStreamFor(ref: ConversationRef): StreamLike {
    return this.activitySender.createStream(ref as unknown as ConversationReference);
  }

  async sendTo(ref: ConversationRef, activity: OutboundActivity): Promise<void> {
    const params =
      activity.type === "typing"
        ? { type: "typing" }
        : "card" in activity
          ? { type: "message", attachments: [{ contentType: ADAPTIVE_CARD, content: activity.card }] }
          : { type: "message", text: activity.text, textFormat: "markdown" };
    await this.activitySender.send(params as never, ref as unknown as ConversationReference);
  }
}

export interface TeamsBridgeOptions {
  store?: ConversationStore;
  logger?: Logger;
  /** Override how loomcycle clients are built (tests, a custom fetch). */
  clients?: ClientFactory;
}

export interface TeamsBridge {
  app: BridgeApp;
  router: ConversationRouter;
  start(): Promise<void>;
  stop(): Promise<void>;
}

export function createTeamsBridge(cfg: BridgeConfig, opts: TeamsBridgeOptions = {}): TeamsBridge {
  const log = opts.logger ?? consoleLogger;
  const clients: ClientFactory =
    opts.clients ?? ((token) => new LoomcycleClient({ baseUrl: cfg.loomcycleBaseUrl, authToken: token }));
  const store = opts.store ?? storeFromSpec(cfg.store);

  if (!cfg.dev && process.env.DANGEROUSLY_ALLOW_UNAUTHENTICATED_REQUESTS) {
    log.warn("DANGEROUSLY_ALLOW_UNAUTHENTICATED_REQUESTS is ignored: set TEAMS_BRIDGE_DEV=1 for local testing");
  }
  const app = new BridgeApp({
    // Always explicit: the SDK would otherwise read the env var itself, and
    // a stray one must not turn off request validation in production.
    dangerouslyAllowUnauthenticatedRequests: cfg.dev,
  });

  const port: TeamsPort = {
    send: (ref, activity) => app.sendTo(ref, activity),
    createStream: (ref) => app.createStreamFor(ref),
  };
  const broker = new TokenBroker(clients(cfg.loomcycleToken), {
    agent: cfg.agent,
    ttlSeconds: cfg.tokenTtlSeconds,
    ratePerMinute: cfg.ratePerMinute,
    rateBurst: cfg.rateBurst,
    accessMode: cfg.userAccessMode,
  });
  const router = new ConversationRouter(
    { allowedTenants: cfg.allowedTenants, idleMinutes: cfg.idleMinutes },
    { agent: cfg.agent, store, broker, clients, port, log },
  );

  app.on("message", async ({ activity, ref }) => {
    // Card submits from older clients arrive as messages with no text.
    const text = stripMentionsText(activity) ?? "";
    await router.onMessage(activity, ref as unknown as ConversationRef, text);
  });

  app.on("card.action", async ({ activity, ref }) => {
    const res = await router.onCardAction(activity, ref as unknown as ConversationRef);
    return toInvokeBody(res ?? { message: "This card is no longer active." });
  });

  let sweeper: ReturnType<typeof setInterval> | undefined;

  return {
    app,
    router,
    async start() {
      if (cfg.dev) log.warn("TEAMS_BRIDGE_DEV=1: inbound requests are NOT authenticated; never use this in production");
      await app.start(cfg.port);
      sweeper = setInterval(() => void router.sweepIdle(), 60_000);
      sweeper.unref?.();
      log.info("teams-bridge listening", { port: cfg.port, agent: cfg.agent });
    },
    async stop() {
      if (sweeper) clearInterval(sweeper);
      await router.stop();
      await app.stop();
    },
  };
}

function toInvokeBody(res: CardResult): AdaptiveCardActionResponse {
  // Our cards are plain JSON built in cards.ts; the SDK types them as IAdaptiveCard.
  return "card" in res
    ? { statusCode: 200, type: ADAPTIVE_CARD, value: res.card as never }
    : { statusCode: 200, type: "application/vnd.microsoft.activity.message", value: res.message };
}
