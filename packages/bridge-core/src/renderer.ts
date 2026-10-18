// Delivering one agent turn to the platform (RFC EN §6, generalised by RFC EQ).
//
// Where the platform can stream, the answer shows as it is written. A stream
// may have a time limit (Teams: two minutes), so a long turn is rolled over
// into a new message before it. Elsewhere the turn gets a typing indicator
// and then its text as one message, split at the platform's size limit.

import type { BridgePort, StreamLike } from "./ports.js";
import { splitMessage } from "./text.js";

export interface TurnSink {
  /** The agent started a tool. Only the tool's name is ever shown. */
  tool(name: string): void;
  /** More of the answer. */
  text(t: string): Promise<void>;
  /** A line from the bridge, not the agent (a budget warning, say). */
  notice(t: string): Promise<void>;
  /** End the turn. note is shown when given, and stands in for an empty turn. */
  finish(note?: string): Promise<void>;
}

export interface StreamSinkOptions {
  /** Close the message and continue in a new one after this long. Teams'
   *  limit is two minutes; the default stays clear of it. */
  rolloverMs?: number;
  /** How often to look for the user's Stop. */
  stopPollMs?: number;
  /** Called once when the user presses Stop. */
  onStop?: () => void;
  now?: () => number;
}

export const DEFAULT_ROLLOVER_MS = 100_000;

export class StreamSink<TRef> implements TurnSink {
  private stream?: StreamLike;
  private startedAt = 0;
  private emitted = false;
  private stopped = false;
  private timer?: ReturnType<typeof setInterval>;
  private readonly rolloverMs: number;
  private readonly now: () => number;

  constructor(
    private readonly openStream: (ref: TRef) => StreamLike,
    private readonly port: BridgePort<TRef>,
    private readonly ref: TRef,
    private readonly opts: StreamSinkOptions = {},
  ) {
    this.rolloverMs = opts.rolloverMs ?? DEFAULT_ROLLOVER_MS;
    this.now = opts.now ?? Date.now;
  }

  private open(): StreamLike {
    if (!this.stream) {
      this.stream = this.openStream(this.ref);
      this.startedAt = this.now();
      this.emitted = false;
      if (!this.timer) {
        this.timer = setInterval(() => this.checkStop(), this.opts.stopPollMs ?? 1000);
        this.timer.unref?.();
      }
    }
    return this.stream;
  }

  private checkStop(): boolean {
    if (!this.stopped && this.stream?.canceled) {
      this.stopped = true;
      this.opts.onStop?.();
    }
    return this.stopped;
  }

  tool(name: string): void {
    if (this.checkStop()) return;
    const s = this.open();
    // Informative updates only show before the answer starts.
    if (!this.emitted) s.update(`Using ${name}…`);
  }

  async text(t: string): Promise<void> {
    if (!t || this.checkStop()) return;
    if (this.stream && this.emitted && this.now() - this.startedAt >= this.rolloverMs) {
      await this.stream.close();
      this.stream = undefined;
    }
    this.open().emit(t);
    this.emitted = true;
  }

  async notice(t: string): Promise<void> {
    await this.text(`\n\n_${t}_\n\n`);
  }

  async finish(note?: string): Promise<void> {
    if (this.timer) clearInterval(this.timer);
    this.timer = undefined;
    const s = this.stream;
    this.stream = undefined;
    if (this.stopped || s?.canceled) return;
    if (!s) {
      if (note) await this.port.send(this.ref, { type: "message", text: note });
      return;
    }
    // A stream with only informative updates is not closed empty: Teams, for
    // one, would leave its "working" bubble up.
    if (!this.emitted) s.emit(note ?? "Done.");
    else if (note) s.emit(`\n\n${note}`);
    await s.close();
  }
}

export interface MessageSinkOptions {
  /** The platform's longest message, in characters. */
  maxMessageChars?: number;
  /** Re-send the typing indicator this often while the turn runs. */
  typingEveryMs?: number;
  /** Stop sending typing after this many, for a turn that runs very long. */
  maxTypings?: number;
}

export const DEFAULT_MAX_MESSAGE_CHARS = 4000;

export class MessageSink<TRef> implements TurnSink {
  private buf = "";
  private typings = 0;
  private timer?: ReturnType<typeof setInterval>;
  private started = false;

  constructor(
    private readonly port: BridgePort<TRef>,
    private readonly ref: TRef,
    private readonly opts: MessageSinkOptions = {},
  ) {}

  private start(): void {
    if (this.started) return;
    this.started = true;
    this.typing();
    this.timer = setInterval(() => this.typing(), this.opts.typingEveryMs ?? 3000);
    this.timer.unref?.();
  }

  private typing(): void {
    if (this.typings >= (this.opts.maxTypings ?? 100)) {
      if (this.timer) clearInterval(this.timer);
      return;
    }
    this.typings++;
    // A lost typing indicator is cosmetic.
    this.port.send(this.ref, { type: "typing" }).catch(() => {});
  }

  tool(): void {
    this.start();
  }

  async text(t: string): Promise<void> {
    this.start();
    this.buf += t;
  }

  async notice(t: string): Promise<void> {
    this.start();
    this.buf += `\n\n_${t}_\n\n`;
  }

  async finish(note?: string): Promise<void> {
    if (this.timer) clearInterval(this.timer);
    this.timer = undefined;
    let out = this.buf.trim();
    this.buf = "";
    if (note) out = out ? `${out}\n\n${note}` : note;
    for (const part of splitMessage(out, this.opts.maxMessageChars ?? DEFAULT_MAX_MESSAGE_CHARS)) {
      await this.port.send(this.ref, { type: "message", text: part });
    }
  }
}
