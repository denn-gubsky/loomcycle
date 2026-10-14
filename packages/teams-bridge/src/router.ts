// Maps Teams activities to conversations. Teams-free: the bridge passes in
// the activity fields it needs, so this is tested without the SDK.

import { parseCardAction } from "./cards.js";
import { Conversation, type CardResult, type ConversationDeps } from "./conversation.js";
import { conversationSubject, identify, type ActivityIdentityFields, type Identified } from "./identity.js";
import type { ConversationRef } from "./ports.js";

export interface RouterOptions {
  allowedTenants: ReadonlySet<string>;
  idleMinutes: number;
}

type Ok = Extract<Identified, { ok: true }>;

export const NOT_AVAILABLE_TEXT = "This bot isn't available to your organization.";

export class ConversationRouter {
  private readonly convs = new Map<string, Conversation>();
  private readonly loading = new Map<string, Promise<Conversation>>();
  /** Conversations already told the bot is not for them, so they hear it once. */
  private readonly refused = new Set<string>();

  constructor(
    private readonly opts: RouterOptions,
    private readonly deps: ConversationDeps,
  ) {}

  /** onMessage routes one message and returns once it is queued; the reply is
   *  rendered in the background. */
  async onMessage(activity: ActivityIdentityFields, ref: ConversationRef, text: string): Promise<void> {
    const id = identify(activity, this.opts.allowedTenants);
    if (!id.ok) {
      await this.refuse(id, activity, ref);
      return;
    }
    const conv = await this.conversation(id, ref);
    conv.updateRef(ref);
    conv.onMessage(id.speaker, text).catch((err: unknown) => {
      this.deps.log.error("message failed", { conversation: id.conversationId, error: (err as Error)?.name });
      this.deps.port
        .send(ref, { type: "message", text: "Something went wrong. Please try again." })
        .catch(() => undefined);
    });
  }

  /** onCardAction answers a card button. undefined = not one of ours. */
  async onCardAction(activity: ActivityIdentityFields & { value?: unknown }, ref: ConversationRef): Promise<CardResult | undefined> {
    const action = parseCardAction(activity.value);
    if (!action) return undefined;
    const id = identify(activity, this.opts.allowedTenants);
    if (!id.ok) return { message: NOT_AVAILABLE_TEXT };
    const conv = await this.conversation(id, ref);
    return conv.onCardAction(id.speaker, action);
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
        const token = await this.deps.broker.tokenFor(rec.runSubject, rec.runName);
        await this.deps.clients(token).cancelAgent(rec.agentId, { reason: "teams-bridge: idle" });
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
  get(conversationId: string): Conversation | undefined {
    return this.convs.get(conversationId);
  }

  private async refuse(id: Extract<Identified, { ok: false }>, activity: ActivityIdentityFields, ref: ConversationRef): Promise<void> {
    if (id.reason !== "tenant_not_allowed") return;
    const cid = activity.conversation?.id ?? "";
    this.deps.log.warn("activity from a tenant not allowed", { conversation: cid });
    if (this.refused.has(cid)) return;
    // Bounded: forgetting means one more polite refusal, never a leak.
    if (this.refused.size >= 10_000) this.refused.clear();
    this.refused.add(cid);
    await this.deps.port.send(ref, { type: "message", text: NOT_AVAILABLE_TEXT }).catch(() => undefined);
  }

  private conversation(id: Ok, ref: ConversationRef): Promise<Conversation> {
    const hit = this.convs.get(id.conversationId);
    if (hit) return Promise.resolve(hit);
    const pending = this.loading.get(id.conversationId);
    if (pending) return pending;
    const p = (async () => {
      const rec = (await this.deps.store.get(id.conversationId)) ?? {
        conversationId: id.conversationId,
        scope: id.scope,
        ref,
        ownerSubject: id.speaker.subject,
        ownerName: id.speaker.name,
        ...runIdentity(id),
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

/** runIdentity is who a new conversation's runs act as (identity.ts). */
function runIdentity(id: Ok): { runSubject: string; runName: string } {
  if (id.scope === "personal") return { runSubject: id.speaker.subject, runName: id.speaker.name };
  return {
    runSubject: conversationSubject(id.tenantId, id.conversationId),
    runName: id.scope === "channel" ? "Teams channel" : "Teams group chat",
  };
}
