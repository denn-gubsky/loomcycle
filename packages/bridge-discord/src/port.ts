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
import { MAX_CONTENT, neutraliseLinks, NO_MENTIONS, renderCard, SUPPRESS_EMBEDS, textMessage, type MessageBody } from "./cards.js";

/** Where a reply goes, as the conversation store keeps it: plain JSON with no
 *  credential in it. The channel is enough: a reply goes through the newest
 *  interaction the port holds for it (see deliver), else the bot token. */
export interface DiscordRef {
  channelId: string;
  /** The server the channel is in; absent in a DM, and in a ref stored by a
   *  bridge before 0.3. Only a gate bridge reads it (its channel's groups). */
  guildId?: string;
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

/** What a delivery posted (Discord's message object), and through what. */
interface Delivered {
  message: unknown;
  via?: LastText["via"];
}

function messageIdOf(message: unknown): string | undefined {
  const id = (message as { id?: unknown } | undefined)?.id;
  return typeof id === "string" && /^\d{1,20}$/.test(id) ? id : undefined;
}

interface Tracked {
  token: string;
  channelId: string;
  expiresAt: number;
  /** A deferred response is showing and nothing has replaced it yet. */
  pending: boolean;
  fallback?: string;
  timer?: ReturnType<typeof setTimeout>;
}

/** The last text message the port posted in a channel: what a voice reply
 *  replies to, and where its label goes. In memory only, like the tokens. */
interface LastText {
  messageId: string;
  content: string;
  /** Posted through this interaction's token (as its @original response or
   *  a follow-up), else with the bot token. */
  via?: { token: string; original: boolean; expiresAt: number };
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
  private readonly lastText = new Map<string, LastText>();
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

  async send(ref: DiscordRef, activity: OutboundActivity): Promise<void> {
    if (activity.type === "typing") return this.typing(ref.channelId);
    // The core cut the text to Discord's size, but rewriting its links can
    // lengthen it, so it is cut again after.
    const isCard = "card" in activity;
    const bodies = isCard ? [renderCard(activity.card)] : splitMessage(neutraliseLinks(activity.text), MAX_CONTENT).map(textMessage);
    try {
      for (const body of bodies) {
        const posted = await this.deliver(ref, body);
        // A card is never labelled: its content is rebuilt when it is answered.
        if (isCard) this.lastText.delete(ref.channelId);
        else this.remember(ref.channelId, posted, body);
      }
    } catch (err) {
      // A failed delivery is logged, never thrown: the core would take a throw
      // from inside a turn as the run's stream failing, and end the run.
      this.log.error("could not send to Discord", { channel: ref.channelId, status: (err as DiscordApiError)?.status, route: (err as DiscordApiError)?.route });
    }
  }

  /** labelLast puts a voice reply's label on the last line of the text it
   *  goes with (a voice message itself carries no text), and returns that
   *  message's id for the voice message to reply to. When the text cannot be
   *  edited (unknown, too long, its token expired), the label is posted as a
   *  message of its own. It throws when neither works: a voice reply nobody
   *  can tell is generated is not sent. */
  async labelLast(ref: DiscordRef, label: string): Promise<string> {
    const line = `-# ${neutraliseLinks(label)}`;
    const last = this.lastText.get(ref.channelId);
    const edit = last && last.content.length + 1 + line.length <= MAX_CONTENT ? this.editorOf(ref.channelId, last) : undefined;
    if (last && edit) {
      const content = `${last.content}\n${line}`;
      try {
        await edit({ content, allowed_mentions: NO_MENTIONS, flags: SUPPRESS_EMBEDS });
        last.content = content;
        return last.messageId;
      } catch (err) {
        this.log.warn("could not label the reply text; posting the label on its own", { channel: ref.channelId, status: (err as DiscordApiError)?.status });
      }
    }
    const posted = await this.deliver(ref, textMessage(line));
    const id = messageIdOf(posted.message);
    if (!id) throw new Error("discord: the label message has no id");
    this.remember(ref.channelId, posted, textMessage(line));
    return id;
  }

  /** editorOf edits a remembered message with the credential it was posted
   *  with, or is undefined once that interaction's token is too old. */
  private editorOf(channelId: string, last: LastText): ((body: MessageBody) => Promise<unknown>) | undefined {
    const via = last.via;
    if (!via) return (body) => this.api.editMessage(channelId, last.messageId, body);
    if (this.now() >= via.expiresAt - EXPIRY_MARGIN_MS) return undefined;
    return via.original ? (body) => this.api.editOriginal(via.token, body) : (body) => this.api.editFollowUp(via.token, last.messageId, body);
  }

  private remember(channelId: string, posted: Delivered, body: MessageBody): void {
    const messageId = messageIdOf(posted.message);
    if (!messageId || typeof body.content !== "string") {
      this.lastText.delete(channelId);
      return;
    }
    if (this.lastText.size >= MAX_TRACKED) this.lastText.clear();
    // Re-inserted, so the map stays in the order channels were last written.
    this.lastText.delete(channelId);
    this.lastText.set(channelId, { messageId, content: body.content, ...(posted.via ? { via: posted.via } : {}) });
  }

  private async deliver(ref: DiscordRef, body: MessageBody): Promise<Delivered> {
    const inChannel = [...this.tracked.values()].filter((t) => t.channelId === ref.channelId && this.usable(t));
    const pending = inChannel.filter((t) => t.pending);
    if (pending.length > 0) {
      // The newest deferred response takes the message, so it lands at the
      // bottom of the channel. Older ones would otherwise sit at "thinking…"
      // (a steer, or a /stop, sent while a turn was running).
      const target = pending[pending.length - 1];
      for (const t of pending) this.claim(t);
      try {
        const message = await this.editOriginal(target.token, body);
        return { message, via: { token: target.token, original: true, expiresAt: target.expiresAt } };
      } finally {
        for (const t of pending.slice(0, -1)) {
          await this.api.editOriginal(t.token, textMessage(CONTINUED)).catch(() => undefined);
        }
      }
    }
    const newest = inChannel[inChannel.length - 1];
    if (newest) {
      const message = await this.api.followUp(newest.token, body);
      return { message, via: { token: newest.token, original: false, expiresAt: newest.expiresAt } };
    }
    return { message: await this.api.createMessage(ref.channelId, body) };
  }

  private async editOriginal(token: string, body: MessageBody): Promise<unknown> {
    try {
      return await this.api.editOriginal(token, body);
    } catch (err) {
      // The edit may have overtaken the deferred response it replaces: the
      // bridge answers Discord and starts the work in the same moment.
      if (!(err instanceof DiscordApiError && err.status === 404)) throw err;
      await new Promise((r) => setTimeout(r, this.retryUnknownMs));
      return this.api.editOriginal(token, body);
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
