// Delegated Graph mode's sign-in (RFC HF §3.5, D4): the person signs in once,
// through the Azure Bot's OAuth connection, the first time a lookup needs
// their token, and the message that needed it waits for the sign-in.
//
// The bridge stores no user token. The Bot Framework token service keeps it,
// and the bridge asks it for one each time a lookup or a download needs it.
// What this module holds is the waiting message: in memory, per person and
// conversation, bounded in number and in time.

import type { FileIn, Identified } from "@loomcycle/bridge-core";

import type { GraphLookup } from "./graph.js";
import type { MessageFields } from "./inbound.js";

/** The sign-in card's text. */
export const SIGN_IN_TEXT = "To take files you attach, sign in once.";

/** The card's button. */
export const SIGN_IN_BUTTON = "Sign in";

/** The reply to a sign-in that came after the message it was for expired. */
export const ATTACH_AGAIN_TEXT = "Please attach the file again.";

/** The reply to /signout. */
export const SIGNED_OUT_TEXT = "You're signed out. I'll ask you to sign in again when I next need to.";

/** The reply to a /signout the token service refused. */
export const SIGN_OUT_FAILED_TEXT = "I couldn't sign you out. Please try again.";

/** How long a message waits for its sign-in (D4). */
export const PENDING_TTL_MS = 10 * 60 * 1000;

/** How many people may be waiting to sign in at once. */
export const DEFAULT_MAX_PENDING = 1_000;

/** isSignOutCommand says a message is /signout: the whole message, any case,
 *  as the core matches its own commands. */
export function isSignOutCommand(text: string): boolean {
  return text.trim().toLowerCase() === "/signout";
}

/** One message held until its sender signs in. Never a token. */
export interface PendingMessage<TRef> {
  id: Identified;
  ref: TRef;
  text: string;
  activity: MessageFields;
  lookup: GraphLookup;
}

/** pendingKey is who a held message is for: the person (their Bot Framework
 *  id, which a sign-in invoke carries too) in one conversation. */
export function pendingKey(conversationId: string, botUserId: string): string {
  return JSON.stringify([conversationId, botUserId]);
}

interface Entry<TRef> {
  msg: PendingMessage<TRef>;
  /** When the card was sent: the message waits until this plus the TTL. */
  since: number;
}

export interface PendingOptions {
  ttlMs?: number;
  max?: number;
  now?: () => number;
}

/** PendingSignIns holds at most one message per person and conversation, for
 *  at most ttlMs after its card was sent, and at most `max` in all (the
 *  oldest goes first). A bridge restart forgets them: the person is then
 *  asked to attach the file again after signing in. */
export class PendingSignIns<TRef> {
  private readonly entries = new Map<string, Entry<TRef>>();
  private readonly ttlMs: number;
  private readonly max: number;
  private readonly now: () => number;

  constructor(o: PendingOptions = {}) {
    this.ttlMs = o.ttlMs ?? PENDING_TTL_MS;
    this.max = o.max ?? DEFAULT_MAX_PENDING;
    this.now = o.now ?? Date.now;
  }

  /** hold keeps msg for key. It is true when nobody was waiting there, so a
   *  card should be sent; false when it replaced a message still waiting for
   *  the card already sent, which keeps its time. */
  hold(key: string, msg: PendingMessage<TRef>): boolean {
    const now = this.now();
    this.prune(now);
    const waiting = this.entries.get(key);
    if (waiting) {
      waiting.msg = msg;
      return false;
    }
    if (this.entries.size >= this.max) {
      const oldest = this.entries.keys().next().value;
      if (oldest !== undefined) this.entries.delete(oldest);
    }
    this.entries.set(key, { msg, since: now });
    return true;
  }

  /** take removes and returns the message waiting for key, or undefined when
   *  none is, or it waited too long. */
  take(key: string): PendingMessage<TRef> | undefined {
    const e = this.entries.get(key);
    this.entries.delete(key);
    if (!e || this.now() - e.since >= this.ttlMs) return undefined;
    return e.msg;
  }

  drop(key: string): void {
    this.entries.delete(key);
  }

  get size(): number {
    return this.entries.size;
  }

  /** prune drops what waited too long. Entries are in the order their cards
   *  were sent, so the expired ones are at the front. */
  private prune(now: number): void {
    for (const [key, e] of this.entries) {
      if (now - e.since < this.ttlMs) break;
      this.entries.delete(key);
    }
  }
}

/** userTokenStatus is the HTTP status of a token service error (an
 *  AxiosError from the SDK), or undefined. A 404 means the person has no
 *  token. Read without importing axios. */
export function userTokenStatus(err: unknown): number | undefined {
  const e = err as { status?: unknown; response?: { status?: unknown } } | null;
  const s = e?.status ?? e?.response?.status;
  return typeof s === "number" ? s : undefined;
}

/** What the bridge does with a message once its files are known: the same
 *  for a message looked up at once and one that waited for a sign-in. */
export type Deliver<TRef> = (id: Identified, ref: TRef, text: string, activity: MessageFields, found: readonly FileIn[]) => Promise<void>;
