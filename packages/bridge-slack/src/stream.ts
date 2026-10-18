// One streamed Slack message: chat.startStream on the first text, then
// chat.appendStream at most about once a second, then chat.stopStream.
//
// The core calls emit() for every text delta, synchronously; the calls to
// Slack run in order on a chain behind it. A streamed message must be a reply
// in a thread. If Slack refuses to start the stream (the workspace or channel
// does not allow it), the turn is sent as ordinary messages instead, so the
// answer is never lost.

import type { Logger, StreamLike } from "@loomcycle/bridge-core";

import { slackErrorCode, type SlackApi } from "./api.js";
import { neutralise } from "./text.js";

export interface SlackStreamOptions {
  channel: string;
  threadTs: string;
  /** Characters per streamed message; past it the stream continues in a new message. */
  maxChars: number;
  /** Send buffered text no more often than this. */
  intervalMs?: number;
  /** Sends text the stream could not, as ordinary messages. */
  fallback: (text: string) => Promise<void>;
  log: Logger;
  /** For logs only. */
  conversation: string;
  now?: () => number;
}

export const DEFAULT_STREAM_INTERVAL_MS = 1000;

export class SlackStream implements StreamLike {
  /** True once the person stopped the stream in Slack. */
  canceled = false;
  private pending = "";
  private ts?: string;
  private inMessage = 0;
  private failed = false;
  private lastFlush = 0;
  private timer?: ReturnType<typeof setTimeout>;
  private chain: Promise<void> = Promise.resolve();
  private readonly intervalMs: number;
  private readonly now: () => number;

  constructor(
    private readonly api: SlackApi,
    private readonly opts: SlackStreamOptions,
  ) {
    this.intervalMs = opts.intervalMs ?? DEFAULT_STREAM_INTERVAL_MS;
    this.now = opts.now ?? Date.now;
  }

  /** Slack shows no interim status in a stream; the message starts with text. */
  update(): void {}

  emit(text: string): void {
    if (!text || this.canceled) return;
    // Neutralised over the whole unsent buffer, so a mention split across
    // two deltas is still caught.
    this.pending = neutralise(this.pending + text);
    if (!this.failed) this.schedule();
  }

  async close(): Promise<void> {
    if (this.timer) clearTimeout(this.timer);
    this.timer = undefined;
    this.enqueue(async () => {
      await this.flush(true);
      await this.stopMessage();
      if (this.pending && !this.canceled) {
        const rest = this.pending;
        this.pending = "";
        await this.opts.fallback(rest);
      }
    });
    await this.chain;
  }

  private schedule(): void {
    if (this.timer) return;
    const wait = Math.max(0, this.lastFlush + this.intervalMs - this.now());
    this.timer = setTimeout(() => {
      this.timer = undefined;
      this.enqueue(() => this.flush(false));
    }, wait);
    this.timer.unref?.();
  }

  private enqueue(fn: () => Promise<void>): void {
    this.chain = this.chain.then(fn).catch((err: unknown) => {
      this.opts.log.warn("slack stream step failed", { conversation: this.opts.conversation, error: slackErrorCode(err) ?? (err as Error)?.name });
    });
  }

  private async flush(final: boolean): Promise<void> {
    this.lastFlush = this.now();
    while (!this.failed && !this.canceled && this.pending) {
      if (this.ts && this.inMessage >= this.opts.maxChars) await this.stopMessage();
      let piece = this.pending.slice(0, this.opts.maxChars - this.inMessage);
      // Hold back what may be the start of a mention ("<", "@he") until the
      // next delta shows whether it is one.
      if (!final && piece === this.pending) piece = piece.replace(/(<|@[A-Za-z]{0,8})$/, "");
      if (!piece) return;
      const method = this.ts ? "chat.appendStream" : "chat.startStream";
      try {
        if (!this.ts) {
          const r = await this.api.call(method, { channel: this.opts.channel, thread_ts: this.opts.threadTs, markdown_text: piece });
          if (typeof r.ts !== "string") throw new Error("no ts");
          this.ts = r.ts;
        } else {
          await this.api.call(method, { channel: this.opts.channel, ts: this.ts, markdown_text: piece });
        }
      } catch (err) {
        const code = slackErrorCode(err);
        if (code === "stopped_by_user") {
          this.canceled = true;
          this.pending = "";
          return;
        }
        // The rest of the turn goes out as messages when the stream closes.
        this.failed = true;
        this.opts.log.warn("slack stream failed, sending the turn as messages", {
          conversation: this.opts.conversation,
          method,
          error: code ?? (err as Error)?.name,
        });
        return;
      }
      this.pending = this.pending.slice(piece.length);
      this.inMessage += piece.length;
    }
  }

  private async stopMessage(): Promise<void> {
    const ts = this.ts;
    this.ts = undefined;
    this.inMessage = 0;
    if (!ts) return;
    try {
      await this.api.call("chat.stopStream", { channel: this.opts.channel, ts });
    } catch (err) {
      const code = slackErrorCode(err);
      if (code === "stopped_by_user") {
        this.canceled = true;
        return;
      }
      // What was appended is already shown; nothing is lost but the stop.
      this.opts.log.warn("slack stream stop failed", { conversation: this.opts.conversation, error: code ?? (err as Error)?.name });
    }
  }
}
