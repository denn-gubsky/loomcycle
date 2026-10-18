// The core's outbound side on Telegram: messages, typing, cards, and (opt-in)
// streamed answers in private chats.

import type { BridgePort, Logger, OutboundActivity, StreamLike } from "@loomcycle/bridge-core";

import { isNotModified, isParseError, TelegramError, type TelegramApi } from "./api.js";
import { renderCard, type InlineKeyboardMarkup, type RenderedMessage } from "./cards.js";
import { markdownToHtml } from "./format.js";
import type { TelegramRef } from "./identity.js";
import { TelegramStream, type StreamTiming } from "./stream.js";

/** Telegram takes 4,096 characters, counted after the HTML is parsed, so tags
 *  do not count and Markdown markers only shrink the text. The core splits at
 *  this lower figure all the same: the plain-text fallback resends the
 *  Markdown as written, and how Telegram counts characters outside the Basic
 *  Multilingual Plane (one or two) is not documented. */
export const MAX_MESSAGE_CHARS = 4000;

function threadParams(ref: TelegramRef): Record<string, unknown> {
  return ref.threadId === undefined ? { chat_id: ref.chatId } : { chat_id: ref.chatId, message_thread_id: ref.threadId };
}

export interface TelegramPortOptions {
  api: TelegramApi;
  log: Logger;
  streaming: boolean;
  timing?: Partial<StreamTiming>;
  now?: () => number;
}

export class TelegramPort {
  constructor(private readonly opts: TelegramPortOptions) {}

  /** asBridgePort is the core's view: createStream only when streaming is on,
   *  since its presence is what makes the core stream. */
  asBridgePort(): BridgePort<TelegramRef> {
    const send = (ref: TelegramRef, a: OutboundActivity) => this.send(ref, a);
    if (!this.opts.streaming) return { send };
    return { send, createStream: (ref) => this.createStream(ref) };
  }

  async send(ref: TelegramRef, activity: OutboundActivity): Promise<void> {
    if (activity.type === "typing") {
      // Cosmetic: never wait out a flood limit for it.
      await this.opts.api.call("sendChatAction", { ...threadParams(ref), action: "typing" }, { retry: false });
      return;
    }
    const rendered: RenderedMessage =
      "card" in activity ? renderCard(activity.card) : { html: markdownToHtml(activity.text), plain: activity.text };
    await this.sendMessage(ref, rendered);
  }

  /** sendMessage sends HTML, and the plain text if Telegram refuses the HTML. */
  async sendMessage(ref: TelegramRef, m: RenderedMessage, o: { retry?: boolean } = {}): Promise<number | undefined> {
    const base = { ...threadParams(ref), ...(m.replyMarkup ? { reply_markup: m.replyMarkup } : {}) };
    try {
      const sent = await this.opts.api.call<{ message_id?: number }>("sendMessage", { ...base, text: m.html, parse_mode: "HTML" }, o);
      return sent?.message_id;
    } catch (err) {
      if (!isParseError(err)) throw this.logged(err, ref);
      this.opts.log.warn("telegram refused the HTML, resending as plain text", { chat: ref.chatId });
      const sent = await this.opts.api.call<{ message_id?: number }>("sendMessage", { ...base, text: m.plain }, o);
      return sent?.message_id;
    }
  }

  /** editMessage replaces a message's text; a keyboard not passed is removed. */
  async editMessage(
    chatId: number,
    messageId: number,
    m: { html: string; plain: string; replyMarkup?: InlineKeyboardMarkup },
    o: { retry?: boolean } = {},
  ): Promise<void> {
    const base = { chat_id: chatId, message_id: messageId, ...(m.replyMarkup ? { reply_markup: m.replyMarkup } : {}) };
    try {
      try {
        await this.opts.api.call("editMessageText", { ...base, text: m.html, parse_mode: "HTML" }, o);
      } catch (err) {
        if (!isParseError(err)) throw err;
        await this.opts.api.call("editMessageText", { ...base, text: m.plain }, o);
      }
    } catch (err) {
      if (isNotModified(err)) return;
      throw err;
    }
  }

  private createStream(ref: TelegramRef): StreamLike {
    return new TelegramStream({
      port: this,
      api: this.opts.api,
      log: this.opts.log,
      ref,
      maxChars: MAX_MESSAGE_CHARS,
      timing: this.opts.timing,
      now: this.opts.now,
    });
  }

  private logged(err: unknown, ref: TelegramRef): unknown {
    if (err instanceof TelegramError && err.migrateToChatId !== undefined) {
      // The group became a supergroup with a new id: the allow-list and the
      // conversation follow the new id, which the operator must allow.
      this.opts.log.warn("telegram group moved to a new chat id", { chat: ref.chatId, new_chat: err.migrateToChatId });
    }
    return err;
  }
}
