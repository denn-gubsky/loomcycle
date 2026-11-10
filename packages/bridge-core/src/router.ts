// Maps inbound messages to conversations. Platform-free: a bridge package
// identifies the sender (and applies its allow-list) and passes the result in,
// so this is tested without any platform SDK.

import type { CardInput } from "./cards.js";
import { Conversation, type CardResult, type ConversationDeps } from "./conversation.js";
import type { Identified, Scope } from "./identity.js";
import type { VoiceIn } from "./voice.js";

/** What a message says about who is present: who spoke, where, and when the
 *  platform says they sent it. */
export interface SeenMessage {
  speaker: string;
  runAs: string;
  scope: Scope;
  /** Epoch ms the platform dates the message; undefined when it gives none. */
  sentAt?: number;
}

export interface RouterOptions {
  idleMinutes: number;
  /** Called for each message, voice message and card press the platform
   *  authenticated. */
  onSeen?(seen: SeenMessage): void;
}

export interface MessageOptions {
  /** Epoch ms the platform dates the message or press (Telegram's date,
   *  Slack's ts, Discord's snowflake, a Teams activity's timestamp). Leave it
   *  undefined when the platform gives no date of its own: a date the bridge
   *  made up (its own clock) cannot tell a redelivery from a new message. */
  sentAt?: number;
}

/** sentAtOf is a platform's date for MessageOptions: epoch ms when it is a
 *  finite positive number, else undefined. */
export function sentAtOf(ms: number | undefined): number | undefined {
  return typeof ms === "number" && Number.isFinite(ms) && ms > 0 ? ms : undefined;
}

type Ok = Extract<Identified, { ok: true }>;
type Refused = Extract<Identified, { ok: false }>;

export class ConversationRouter<TRef> {
  private readonly convs = new Map<string, Conversation<TRef>>();
  private readonly loading = new Map<string, Promise<Conversation<TRef>>>();
  /** Conversations already told why they are refused, so they hear it once. */
  private readonly refused = new Set<string>();

  constructor(
    private readonly opts: RouterOptions,
    private readonly deps: ConversationDeps<TRef>,
  ) {}

  /** onMessage routes one message and returns once it is queued; the reply is
   *  rendered in the background. */
  async onMessage(id: Identified, ref: TRef, text: string, opts: MessageOptions = {}): Promise<void> {
    this.deps.health?.inbound();
    if (!id.ok) {
      await this.refuse(id, ref);
      return;
    }
    this.seen(id, opts);
    const conv = await this.conversation(id, ref);
    conv.updateRef(ref);
    conv.adoptGateSubjects(id.runAs, id.speaker);
    conv.onMessage(id.speaker, text).catch((err: unknown) => {
      this.deps.log.error("message failed", { conversation: id.conversationId, error: (err as Error)?.name });
      this.deps.port
        .send(ref, { type: "message", text: "Something went wrong. Please try again." })
        .catch(() => undefined);
    });
  }

  /** onVoice routes one voice message (RFC GA) like onMessage routes text:
   *  the conversation downloads and transcribes it, then answers it as it
   *  would the same words typed. */
  async onVoice(id: Identified, ref: TRef, voice: VoiceIn, opts: MessageOptions = {}): Promise<void> {
    this.deps.health?.inbound();
    if (!id.ok) {
      await this.refuse(id, ref);
      return;
    }
    this.seen(id, opts);
    const conv = await this.conversation(id, ref);
    conv.updateRef(ref);
    conv.adoptGateSubjects(id.runAs, id.speaker);
    conv.onVoice(id.speaker, voice).catch((err: unknown) => {
      this.deps.log.error("voice message failed", { conversation: id.conversationId, error: (err as Error)?.name });
      this.deps.port
        .send(ref, { type: "message", text: "Something went wrong. Please try again." })
        .catch(() => undefined);
    });
  }

  /** onCardAction answers a card. input undefined = not one of ours. */
  async onCardAction(id: Identified, ref: TRef, input: CardInput | undefined, opts: MessageOptions = {}): Promise<CardResult | undefined> {
    if (!input) return undefined;
    this.deps.health?.inbound();
    if (!id.ok) return { message: id.notice ?? "This bot isn't available to you." };
    this.seen(id, opts);
    const conv = await this.conversation(id, ref);
    conv.adoptGateSubjects(id.runAs, id.speaker);
    return conv.onCardAction(id.speaker, input);
  }

  /** sweepIdle ends runs nobody has written to for idleMinutes: the ones this
   *  process drives, and the ones an earlier process left parked in loomcycle
   *  (known only from the store). Idle conversations leave memory; the store
   *  brings them back on their next message. */
  async sweepIdle(): Promise<void> {
    const idleMs = this.opts.idleMinutes * 60_000;
    const now = (this.deps.now ?? Date.now)();
    await Promise.all(
      [...this.convs.entries()].map(async ([id, c]) => {
        await c.endIfIdle(idleMs).catch(() => undefined);
        if (!c.hasLiveRun && now - c.record.lastActivityAt >= idleMs) this.convs.delete(id);
      }),
    );
    for (const rec of await this.deps.store.list().catch(() => [])) {
      if (!rec.agentId || this.convs.has(rec.conversationId) || now - rec.lastActivityAt < idleMs) continue;
      try {
        const token = await this.deps.broker.tokenFor(rec.runSubject, rec.runName, rec.ref);
        await this.deps.clients(token).cancelAgent(rec.agentId, { reason: `${this.deps.platform.name}-bridge: idle` });
        await this.deps.store.put({ ...rec, agentId: undefined, runId: undefined });
      } catch (err) {
        this.deps.log.warn("could not end a stale run", { conversation: rec.conversationId, status: (err as { status?: number })?.status });
      }
    }
  }

  async stop(): Promise<void> {
    await Promise.all([...this.convs.values()].map((c) => c.stop()));
  }

  /** For tests and diagnostics. */
  get(conversationId: string): Conversation<TRef> | undefined {
    return this.convs.get(conversationId);
  }

  /** seen tells onSeen who is present, from every path the platform
   *  authenticated: a person who presses a card or sends a voice message is
   *  as present as one who types. */
  private seen(id: Ok, opts: MessageOptions): void {
    this.opts.onSeen?.({ speaker: id.speaker.subject, runAs: id.runAs.subject, scope: id.scope, sentAt: opts.sentAt });
  }

  private async refuse(id: Refused, ref: TRef): Promise<void> {
    // A message dropped without a notice (the bot's own, another bot's) is
    // routine and not logged.
    if (!id.notice || !id.conversationId) return;
    this.deps.log.warn("message refused", { reason: id.reason, conversation: id.conversationId });
    const cid = id.conversationId;
    if (this.refused.has(cid)) return;
    // Bounded: forgetting means one more polite refusal, never a leak.
    if (this.refused.size >= 10_000) this.refused.clear();
    this.refused.add(cid);
    await this.deps.port.send(ref, { type: "message", text: id.notice }).catch(() => undefined);
  }

  private conversation(id: Ok, ref: TRef): Promise<Conversation<TRef>> {
    const hit = this.convs.get(id.conversationId);
    if (hit) return Promise.resolve(hit);
    const pending = this.loading.get(id.conversationId);
    if (pending) return pending;
    const p = (async () => {
      const rec = (await this.deps.store.get(id.conversationId)) ?? {
        conversationId: id.conversationId,
        scope: id.scope,
        ref,
        // A guest opens a conversation without owning it; the first member
        // who speaks or presses a button there claims it.
        ownerSubject: id.speaker.guest ? "" : id.speaker.subject,
        ownerName: id.speaker.guest ? "" : id.speaker.name,
        runSubject: id.runAs.subject,
        runName: id.runAs.name,
        lastActivityAt: (this.deps.now ?? Date.now)(),
      };
      const conv = new Conversation(rec, this.deps);
      this.convs.set(id.conversationId, conv);
      return conv;
    })().finally(() => this.loading.delete(id.conversationId));
    this.loading.set(id.conversationId, p);
    return p;
  }
}
