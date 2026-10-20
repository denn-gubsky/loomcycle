// One streamed answer in a private chat: a message sent once text starts and
// edited as it grows.
//
// Edits are throttled: at most one per minIntervalMs, and a 429 pushes the
// next one back by its retry_after. An answer past one message's length is
// finished page by page, each page a message of its own.
//
// Not sendMessageDraft, the Bot API's own streaming preview: it documents no
// link_preview_options, so a draft holding a URL the agent was steered into
// writing might be fetched for a preview. sendMessage and editMessageText both
// take the option, and the port always sets it.

import { splitMessage, type Logger, type StreamLike } from "@loomcycle/bridge-core";

import { TelegramError, type TelegramApi } from "./api.js";
import { markdownToHtml } from "./format.js";
import type { TelegramRef } from "./identity.js";
import type { TelegramPort } from "./port.js";

export interface StreamTiming {
  /** Least time between two updates of one message. */
  minIntervalMs: number;
}

export const DEFAULT_TIMING: StreamTiming = { minIntervalMs: 1000 };

export interface TelegramStreamDeps {
  port: TelegramPort;
  api: TelegramApi;
  log: Logger;
  ref: TelegramRef;
  maxChars: number;
  timing?: Partial<StreamTiming>;
  now?: () => number;
}

export class TelegramStream implements StreamLike {
  /** Telegram has no Stop button on a message; /stop is the way. */
  readonly canceled = false;
  private text = "";
  private status = "";
  private shown?: string;
  private nextAt = 0;
  private messageId?: number;
  private timer?: ReturnType<typeof setTimeout>;
  private chain: Promise<void> = Promise.resolve();
  private done = false;
  private readonly timing: StreamTiming;
  private readonly now: () => number;

  constructor(private readonly d: TelegramStreamDeps) {
    this.timing = { ...DEFAULT_TIMING, ...d.timing };
    this.now = d.now ?? Date.now;
    // Until the first text there is no message to edit: show typing instead.
    void d.api.call("sendChatAction", { ...this.target(), action: "typing" }, { retry: false }).catch(() => undefined);
  }

  update(text: string): void {
    if (this.done) return;
    this.status = text;
    // A status shows only until the answer starts.
    if (!this.text) this.schedule();
  }

  emit(text: string): void {
    if (this.done || !text) return;
    this.text += text;
    this.schedule();
  }

  close(): Promise<void> {
    if (this.done) return this.chain;
    this.done = true;
    if (this.timer) clearTimeout(this.timer);
    this.timer = undefined;
    return this.enqueue(() => this.flushAll());
  }

  private target(): Record<string, unknown> {
    const { chatId, threadId } = this.d.ref;
    return threadId === undefined ? { chat_id: chatId } : { chat_id: chatId, message_thread_id: threadId };
  }

  private enqueue(fn: () => Promise<void>): Promise<void> {
    this.chain = this.chain.then(fn).catch((err: unknown) => {
      this.d.log.warn("telegram stream update failed", {
        chat: this.d.ref.chatId,
        ...(err instanceof TelegramError ? { method: err.method, code: err.code } : { error: (err as Error)?.name }),
      });
    });
    return this.chain;
  }

  private schedule(): void {
    if (this.done || this.timer) return;
    const wait = Math.max(0, this.nextAt - this.now());
    this.timer = setTimeout(() => {
      this.timer = undefined;
      void this.enqueue(() => this.show());
    }, wait);
    this.timer.unref?.();
  }

  /** pageOut finishes every full page but the last as a message of its own,
   *  and returns the text still to show. */
  private async pageOut(): Promise<string> {
    const snapshot = this.text;
    const pages = splitMessage(snapshot, this.d.maxChars);
    for (const page of pages.slice(0, -1)) await this.persist(page);
    // Text emitted while the pages were sent follows the last one.
    this.text = (pages.at(-1) ?? "") + this.text.slice(snapshot.length);
    return this.text;
  }

  /** persist makes page final: the open message's last edit, or a new
   *  message. The next text starts a new message. */
  private async persist(page: string): Promise<void> {
    const m = { html: markdownToHtml(page), plain: page };
    if (this.messageId === undefined) await this.d.port.sendMessage(this.d.ref, m);
    else if (page !== this.shown) await this.d.port.editMessage(this.d.ref.chatId, this.messageId, m);
    this.messageId = undefined;
    this.shown = undefined;
  }

  /** flushAll finishes everything written. Nothing is emitted once the
   *  stream is done, so the text cannot grow under it. */
  private async flushAll(): Promise<void> {
    const last = await this.pageOut();
    if (last.trim()) await this.persist(last);
  }

  private async show(): Promise<void> {
    if (this.done) return;
    // A preview past a page (text that arrived while pages were sent) is cut;
    // the next update pages it out.
    const body = (await this.pageOut()).slice(0, this.d.maxChars);
    if (this.done) return;
    const visible = body || (this.status ? `_${this.status}_` : "");
    if (!visible || visible === this.shown) return;
    const m = { html: markdownToHtml(visible), plain: visible };
    try {
      if (this.messageId === undefined) this.messageId = await this.d.port.sendMessage(this.d.ref, m, { retry: false });
      else await this.d.port.editMessage(this.d.ref.chatId, this.messageId, m, { retry: false });
      this.shown = visible;
      this.nextAt = this.now() + this.timing.minIntervalMs;
    } catch (err) {
      if (!(err instanceof TelegramError && err.code === 429)) throw err;
      this.nextAt = this.now() + (err.retryAfter ?? 1) * 1000;
      if (this.timer) clearTimeout(this.timer);
      this.timer = undefined;
      this.schedule();
    }
  }
}
