// POST /discord/interactions: verify, then answer within Discord's 3 seconds,
// then do the work.

import { header, type CardResult, type ConversationRouter, type Logger, type RawResponse, type Route } from "@loomcycle/bridge-core";
import type { KeyObject } from "node:crypto";

import type { DiscordApi } from "./api.js";
import { NO_MENTIONS, parseCustomId, renderCard, SUPPRESS_EMBEDS, textMessage } from "./cards.js";
import { HELP_TEXT, MAX_PROMPT_CHARS } from "./commands.js";
import { isSnowflake, type DiscordConfig } from "./config.js";
import { identify, NOT_HERE, placeOf, type Fences, type Place } from "./identity.js";
import { CallbackType, EPHEMERAL, InteractionType, type Interaction } from "./interaction.js";
import type { DeferOptions, DiscordPort, DiscordRef } from "./port.js";
import { verifyInteraction } from "./verify.js";

export interface InteractionDeps {
  cfg: Fences & Pick<DiscordConfig, "dev">;
  key: KeyObject;
  port: DiscordPort;
  api: DiscordApi;
  router: ConversationRouter<DiscordRef>;
  log: Logger;
  now: () => number;
  /** How long a button press may take before the bridge acks it and finishes later. */
  inlineCardMs?: number;
}

/** Interaction tokens are opaque; this keeps one from bending the URL path it is put in. */
const TOKEN = /^[A-Za-z0-9._-]{1,1000}$/;
/** Longer than the 5-minute signature window, so a replay inside it is refused. */
const SEEN_TTL_MS = 10 * 60_000;
const MAX_SEEN = 50_000;
const INLINE_CARD_MS = 2000;

export const NO_LONGER_ACTIVE = "This button is no longer active.";

const FALLBACK: Record<string, DeferOptions> = {
  ask: { fallback: "Still working: the reply will follow as a new message." },
  new: { fallback: "Done." },
  // A /stop on a run waiting for its next message stops nothing and says nothing.
  stop: { fallback: "Stop requested.", fallbackAfterMs: 10_000 },
};

/** A bounded set of recently seen interaction ids. */
class Seen {
  private readonly at = new Map<string, number>();
  constructor(private readonly now: () => number) {}

  /** add returns false when id was seen within the TTL. */
  add(id: string): boolean {
    const t = this.now();
    for (const [k, seenAt] of this.at) {
      if (t - seenAt < SEEN_TTL_MS && this.at.size < MAX_SEEN) break;
      this.at.delete(k);
    }
    if (this.at.has(id)) return false;
    this.at.set(id, t);
    return true;
  }
}

function json(body: object): RawResponse {
  return { status: 200, body };
}

function ephemeral(text: string): RawResponse {
  return json({ type: CallbackType.CHANNEL_MESSAGE_WITH_SOURCE, data: { ...textMessage(text), flags: EPHEMERAL | SUPPRESS_EMBEDS } });
}

/** refOf is where replies go, as the conversation store keeps it. */
function refOf(place: Place): DiscordRef {
  return place.guildId ? { channelId: place.channelId, guildId: place.guildId } : { channelId: place.channelId };
}

function promptOf(ix: Interaction): string {
  const v = ix.data?.options?.find((o) => o.name === "prompt")?.value;
  return typeof v === "string" ? v.trim().slice(0, MAX_PROMPT_CHARS) : "";
}

export function interactionRoute(deps: InteractionDeps): Route {
  const seen = new Seen(deps.now);
  const { cfg, port, log } = deps;

  const command = (ix: Interaction & { id: string; token: string }): RawResponse => {
    const id = identify(ix, cfg);
    if (!id.ok) {
      log.warn("interaction refused", { reason: id.reason, conversation: id.conversationId });
      // Every interaction must be answered, so each refused one hears why.
      return ephemeral(id.notice ?? NOT_HERE);
    }
    const name = ix.data?.name ?? "";
    if (name === "help") return ephemeral(HELP_TEXT);
    const text = name === "ask" ? promptOf(ix) : name === "new" || name === "stop" ? `/${name}` : undefined;
    if (text === undefined) return ephemeral("Unknown command. Try /help.");
    if (!text) return ephemeral("Write something after /ask.");
    const place = placeOf(ix)!;
    const ref = refOf(place);
    port.track(ix.id, ix.token, place.channelId, FALLBACK[name]);
    // After this response is written: the core's first reply edits the
    // deferred message, which must exist by then.
    setImmediate(() => {
      deps.router.onMessage(id, ref, text).catch((err: unknown) => {
        log.error("message failed", { conversation: id.conversationId, error: (err as Error)?.name });
      });
    });
    return json({ type: CallbackType.DEFERRED_CHANNEL_MESSAGE_WITH_SOURCE, data: { flags: SUPPRESS_EMBEDS, allowed_mentions: NO_MENTIONS } });
  };

  const answerCard = (r: CardResult | undefined): RawResponse => {
    if (!r) return ephemeral(NO_LONGER_ACTIVE);
    if ("card" in r) return json({ type: CallbackType.UPDATE_MESSAGE, data: renderCard(r.card) });
    return ephemeral(r.message);
  };

  const component = async (ix: Interaction & { id: string; token: string }): Promise<RawResponse> => {
    const input = parseCustomId(ix.data?.custom_id);
    if (!input) return ephemeral(NO_LONGER_ACTIVE);
    const id = identify(ix, cfg);
    if (!id.ok) {
      log.warn("interaction refused", { reason: id.reason, conversation: id.conversationId });
      return ephemeral(id.notice ?? NOT_HERE);
    }
    const place = placeOf(ix)!;
    const ref = refOf(place);
    // Its token also carries the agent's next messages in this channel.
    port.track(ix.id, ix.token, place.channelId);
    const work = deps.router.onCardAction(id, ref, input).catch((err: unknown): CardResult => {
      log.error("card action failed", { conversation: id.conversationId, error: (err as Error)?.name });
      return { message: "Something went wrong. Please try again." };
    });
    let timer: ReturnType<typeof setTimeout> | undefined;
    const slow = new Promise<"slow">((r) => (timer = setTimeout(() => r("slow"), deps.inlineCardMs ?? INLINE_CARD_MS)));
    const first = await Promise.race([work.then((r) => ({ r })), slow]).finally(() => clearTimeout(timer));
    if (first !== "slow") return answerCard(first.r);
    // loomcycle is slow to answer: ack now with a deferred update, and finish
    // the card (or say why not) when the action returns.
    void work
      .then(async (r) => {
        if (r && "card" in r) await deps.api.editOriginal(ix.token, renderCard(r.card));
        else await deps.api.followUp(ix.token, { ...textMessage(r?.message ?? NO_LONGER_ACTIVE), flags: EPHEMERAL | SUPPRESS_EMBEDS });
      })
      .catch((err: unknown) => log.error("card action failed", { conversation: id.conversationId, status: (err as { status?: number })?.status }));
    return json({ type: CallbackType.DEFERRED_UPDATE_MESSAGE });
  };

  return async (req) => {
    if (!cfg.dev) {
      const ok = verifyInteraction(deps.key, header(req, "x-signature-ed25519"), header(req, "x-signature-timestamp"), req.body, deps.now());
      // Discord probes the endpoint with bad signatures and expects a 401.
      if (!ok) return { status: 401, body: "unauthorized" };
    }
    let ix: Interaction;
    try {
      ix = JSON.parse(req.body.toString("utf8")) as Interaction;
    } catch {
      return { status: 400, body: "bad request" };
    }
    if (!ix || typeof ix !== "object") return { status: 400, body: "bad request" };
    if (ix.type === InteractionType.PING) return json({ type: CallbackType.PONG });
    if (!isSnowflake(ix.id) || typeof ix.token !== "string" || !TOKEN.test(ix.token)) return { status: 400, body: "bad request" };
    if (!seen.add(ix.id)) return { status: 409, body: "already handled" };
    const valid = ix as Interaction & { id: string; token: string };
    if (ix.type === InteractionType.APPLICATION_COMMAND) return command(valid);
    if (ix.type === InteractionType.MESSAGE_COMPONENT) return component(valid);
    return { status: 400, body: "bad request" };
  };
}
