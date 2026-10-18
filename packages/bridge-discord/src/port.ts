// Sending to Discord. Without the Gateway, the bridge talks back through the
// interactions people start:
//
// - A command is answered at once with a deferred response ("thinking…"). The
//   next message to that channel replaces it (`@original`); later ones are
//   follow-ups on the newest interaction there.
// - An interaction token lasts 15 minutes. Past that, a message goes to the
//   channel with the bot token, which needs the bot in the channel.
//
// The tokens live in this process's memory only, never in the conversation
// store: they are credentials. A restart forgets them, and a message after it
// goes by the bot token until someone uses a command again.

import { splitMessage, type BridgePort, type Logger, type OutboundActivity } from "@loomcycle/bridge-core";

import { DiscordApiError, type DiscordApi } from "./api.js";
import { MAX_CONTENT, neutraliseLinks, renderCard, textMessage, type MessageBody } from "./cards.js";

/** Where a reply goes, as the conversation store keeps it: plain JSON with no
 *  credential in it. The interaction's token is held by the port. */
export interface DiscordRef {
  channelId: string;
  guildId?: string;
  interactionId: string;
}

export const TOKEN_LIFETIME_MS = 15 * 60_000;
/** A token this close to expiry is not used: a request may land after it. */
const EXPIRY_MARGIN_MS = 10_000;
/** A deferred response nothing has replaced by then gets its fallback line,
 *  so it never ends as Discord's "did not respond". */
const SETTLE_BEFORE_EXPIRY_MS = 30_000;
const MAX_TRACKED = 10_000;
/** After Discord refuses a typing indicator in a channel, skip that channel
 *  this long: a refused call counts towards Discord's invalid-request ban. */
const TYPING_BACKOFF_MS = 10 * 60_000;

export const CONTINUED = "Continued below.";

interface Tracked {
  token: string;
  channelId: string;
  expiresAt: number;
  /** A deferred response is showing and nothing has replaced it yet. */
  pending: boolean;
  fallback?: string;
  timer?: ReturnType<typeof setTimeout>;
}

export interface DeferOptions {
  /** Shown in place of the deferred response if nothing else replaces it. */
  fallback: string;
  /** Show the fallback this long after the defer, if sooner than near expiry. */
  fallbackAfterMs?: number;
}

export class DiscordPort implements BridgePort<DiscordRef> {
  /** By interaction id, oldest first (Map keeps insertion order). */
  private readonly tracked = new Map<string, Tracked>();
  private readonly noTyping = new Map<string, number>();
  private readonly now: () => number;
  private readonly retryUnknownMs: number;

  constructor(
    private readonly api: DiscordApi,
    private readonly log: Logger,
    opts: { now?: () => number; retryUnknownMs?: number } = {},
  ) {
    this.now = opts.now ?? Date.now;
    this.retryUnknownMs = opts.retryUnknownMs ?? 1000;
  }

  /** track records an interaction's token. With defer, its response was a
   *  deferred message the next send in the channel replaces. */
  track(interactionId: string, token: string, channelId: string, defer?: DeferOptions): void {
    this.prune();
    const t: Tracked = { token, channelId, expiresAt: this.now() + TOKEN_LIFETIME_MS, pending: !!defer, fallback: defer?.fallback };
    if (defer) {
      const nearExpiry = TOKEN_LIFETIME_MS - SETTLE_BEFORE_EXPIRY_MS;
      t.timer = setTimeout(() => void this.settle(t), Math.min(defer.fallbackAfterMs ?? nearExpiry, nearExpiry));
      t.timer.unref?.();
    }
    this.tracked.set(interactionId, t);
  }

  /** tokenOf returns a usable token for an interaction, for the bridge's own
   *  reply to a card press after a deferred update. */
  tokenOf(interactionId: string): string | undefined {
    const t = this.tracked.get(interactionId);
    return t && this.usable(t) ? t.token : undefined;
  }

  async send(ref: DiscordRef, activity: OutboundActivity): Promise<void> {
    if (activity.type === "typing") return this.typing(ref.channelId);
    // The core cut the text to Discord's size, but rewriting its links can
    // lengthen it, so it is cut again after.
    const bodies = "card" in activity ? [renderCard(activity.card)] : splitMessage(neutraliseLinks(activity.text), MAX_CONTENT).map(textMessage);
    try {
      for (const body of bodies) await this.deliver(ref, body);
    } catch (err) {
      // A failed delivery is logged, never thrown: the core would take a throw
      // from inside a turn as the run's stream failing, and end the run.
      this.log.error("could not send to Discord", { channel: ref.channelId, status: (err as DiscordApiError)?.status, route: (err as DiscordApiError)?.route });
    }
  }

  private async deliver(ref: DiscordRef, body: MessageBody): Promise<void> {
    const inChannel = [...this.tracked.values()].filter((t) => t.channelId === ref.channelId && this.usable(t));
    const pending = inChannel.filter((t) => t.pending);
    if (pending.length > 0) {
      // The newest deferred response takes the message, so it lands at the
      // bottom of the channel. Older ones would otherwise sit at "thinking…"
      // (a steer, or a /stop, sent while a turn was running).
      const target = pending[pending.length - 1];
      for (const t of pending) this.claim(t);
      try {
        await this.editOriginal(target.token, body);
      } finally {
        for (const t of pending.slice(0, -1)) {
          await this.api.editOriginal(t.token, textMessage(CONTINUED)).catch(() => undefined);
        }
      }
      return;
    }
    const newest = inChannel[inChannel.length - 1];
    if (newest) {
      await this.api.followUp(newest.token, body);
      return;
    }
    await this.api.createMessage(ref.channelId, body);
  }

  private async editOriginal(token: string, body: MessageBody): Promise<void> {
    try {
      await this.api.editOriginal(token, body);
    } catch (err) {
      // The edit may have overtaken the deferred response it replaces: the
      // bridge answers Discord and starts the work in the same moment.
      if (!(err instanceof DiscordApiError && err.status === 404)) throw err;
      await new Promise((r) => setTimeout(r, this.retryUnknownMs));
      await this.api.editOriginal(token, body);
    }
  }

  private async typing(channelId: string): Promise<void> {
    // A deferred response already shows Discord's own "thinking…".
    if ([...this.tracked.values()].some((t) => t.channelId === channelId && t.pending && this.usable(t))) return;
    const until = this.noTyping.get(channelId);
    if (until !== undefined && until > this.now()) return;
    try {
      await this.api.typing(channelId);
    } catch (err) {
      const s = (err as DiscordApiError)?.status;
      if (s === 401 || s === 403 || s === 404) {
        if (this.noTyping.size >= MAX_TRACKED) this.noTyping.clear();
        this.noTyping.set(channelId, this.now() + TYPING_BACKOFF_MS);
      }
    }
  }

  private async settle(t: Tracked): Promise<void> {
    if (!t.pending || !t.fallback) return;
    this.claim(t);
    await this.api.editOriginal(t.token, textMessage(t.fallback)).catch((err: unknown) => {
      this.log.warn("could not settle a deferred response", { channel: t.channelId, status: (err as DiscordApiError)?.status });
    });
  }

  private claim(t: Tracked): void {
    t.pending = false;
    if (t.timer) clearTimeout(t.timer);
    t.timer = undefined;
  }

  private usable(t: Tracked): boolean {
    return this.now() < t.expiresAt - EXPIRY_MARGIN_MS;
  }

  private prune(): void {
    // Oldest first, and every token has the same lifetime, so the expired
    // ones are at the front.
    for (const [id, t] of this.tracked) {
      if (this.usable(t) && this.tracked.size < MAX_TRACKED) break;
      this.claim(t);
      this.tracked.delete(id);
    }
  }

  /** For shutdown: stop the fallback timers. */
  stop(): void {
    for (const t of this.tracked.values()) this.claim(t);
  }
}
