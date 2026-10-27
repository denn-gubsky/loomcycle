// The Teams side: the SDK app, its two routes, and the port the core sends
// through. Everything between (tokens, conversations, turns, cards' logic) is
// @loomcycle/bridge-core's.

import {
  createBridge,
  type Bridge,
  type BridgeAdapter,
  type BridgeOptions,
  type CardResult,
  type ConversationRouter,
  type Logger,
  type OutboundActivity,
  type StreamLike,
} from "@loomcycle/bridge-core";
import { stripMentionsText, type AdaptiveCardActionResponse, type ConversationReference } from "@microsoft/teams.api";
import { App } from "@microsoft/teams.apps";

import { parseCardAction, renderCard } from "./cards.js";
import type { TeamsConfig } from "./config.js";
import { groupsOf, identify, uninstalledSub } from "./identity.js";

const ADAPTIVE_CARD = "application/vnd.microsoft.card.adaptive";

/** A Teams conversation reference, stored as plain JSON. */
export type ConversationRef = {
  conversation: { id: string; conversationType?: string; tenantId?: string };
  serviceUrl: string;
  [key: string]: unknown;
};

/** TeamsBridgeApp exposes the SDK's activity sender so the bridge can send
 *  and stream to a stored conversation reference outside a handler: a
 *  loomcycle turn outlives the request that started it. */
export class TeamsBridgeApp extends App {
  createStreamFor(ref: ConversationRef): StreamLike {
    return this.activitySender.createStream(ref as unknown as ConversationReference);
  }

  async sendTo(ref: ConversationRef, activity: OutboundActivity): Promise<void> {
    const params =
      activity.type === "typing"
        ? { type: "typing" }
        : "card" in activity
          ? { type: "message", attachments: [{ contentType: ADAPTIVE_CARD, content: renderCard(activity.card) }] }
          : { type: "message", text: activity.text, textFormat: "markdown" };
    await this.activitySender.send(params as never, ref as unknown as ConversationReference);
  }
}

export type TeamsBridgeOptions = BridgeOptions<ConversationRef> & { logger?: Logger };

export interface TeamsBridge extends Bridge<ConversationRef> {
  app: TeamsBridgeApp;
  router: ConversationRouter<ConversationRef>;
}

export function createTeamsBridge(cfg: TeamsConfig, opts: TeamsBridgeOptions = {}): TeamsBridge {
  const { shared } = cfg;
  if (!shared.dev && process.env.DANGEROUSLY_ALLOW_UNAUTHENTICATED_REQUESTS) {
    (opts.logger ?? console).warn("DANGEROUSLY_ALLOW_UNAUTHENTICATED_REQUESTS is ignored: set BRIDGE_DEV=1 for local testing");
  }
  const app = new TeamsBridgeApp({
    // Always explicit: the SDK would otherwise read the env var itself, and
    // a stray one must not turn off request validation in production.
    dangerouslyAllowUnauthenticatedRequests: shared.dev,
  });
  // The bridge's gate name in loomcycle; unset for a bridge with a token.
  const gate = shared.gate?.name;

  const adapter: BridgeAdapter<ConversationRef> = {
    platform: { name: "teams", label: "Teams" },
    // Teams streams in 1:1 chats only; group chats and channels get one
    // message per turn. Teams refuses messages over about 28 KB.
    capabilities: { streaming: "personal-only", maxMessageChars: 20000 },
    port: {
      send: (ref, activity) => app.sendTo(ref, activity),
      createStream: (ref) => app.createStreamFor(ref),
    },
    /** groupsFor is what loomcycle is told about a run identity's groups
     *  (RFC FA §3.7d): the Entra tenant the conversation is in, from its
     *  stored reference. No lookups: Teams says nothing more without Graph. */
    async groupsFor(_subject, ref) {
      return groupsOf(ref);
    },
    async start(ctx) {
      app.on("message", async ({ activity, ref }) => {
        // Card submits from older clients arrive as messages with no text.
        const text = stripMentionsText(activity) ?? "";
        await ctx.router.onMessage(identify(activity, cfg.allowedTenants, gate), ref as unknown as ConversationRef, text);
      });
      app.on("card.action", async ({ activity, ref }) => {
        const res = await ctx.router.onCardAction(
          identify(activity, cfg.allowedTenants, gate),
          ref as unknown as ConversationRef,
          parseCardAction(activity.value),
        );
        return toInvokeBody(res ?? { message: "This card is no longer active." });
      });
      const g = ctx.gate;
      if (g) {
        // A person who uninstalls the app from their 1:1 chat has left the
        // bot, and the bridge, as the gate for Teams, tells loomcycle so (RFC
        // FA §3.7c): the user is disabled at once and removed per the gate's
        // policy. The activity arrives on the same Bot Connector-authenticated
        // route as a message, and passes the same tenant fence.
        //
        // The revoke is awaited before Teams is answered, and a failed one
        // fails the turn: the SDK answers a handler that throws with a 500,
        // so the Bot Connector redelivers the activity instead of the person
        // staying active in loomcycle (fail closed, not open). The error
        // thrown carries a fixed message: the SDK logs it.
        app.on("install.remove", async ({ activity }) => {
          const sub = uninstalledSub(activity, cfg.allowedTenants);
          if (!sub) return;
          try {
            await g.revoke(g.subjectOf(sub));
          } catch (err) {
            ctx.log.warn("teams revoke failed", { status: (err as { status?: number })?.status });
            throw new Error("teams-bridge: the revoke did not reach loomcycle; Teams will redeliver");
          }
          ctx.log.info("teams user uninstalled the app; revoked in loomcycle", { conversation: activity.conversation?.id });
        });
      }
      await app.start(shared.port);
    },
    async stop() {
      await app.stop();
    },
  };

  const bridge = createBridge(shared, adapter, opts);
  return { ...bridge, app };
}

function toInvokeBody(res: CardResult): AdaptiveCardActionResponse {
  // Our cards are plain JSON built in cards.ts; the SDK types them as IAdaptiveCard.
  return "card" in res
    ? { statusCode: 200, type: ADAPTIVE_CARD, value: renderCard(res.card) as never }
    : { statusCode: 200, type: "application/vnd.microsoft.activity.message", value: res.message };
}
