// Who sent an inbound message, as a bridge package tells the core.
//
// The platform package reads only the fields its platform authenticates, and
// applies its allow-list (an Entra tenant, a Slack workspace, a Telegram chat)
// before anything here touches loomcycle.

import { createHash } from "node:crypto";

/** A 1:1 chat, a group chat, or a channel. */
export type Scope = "personal" | "group" | "channel";

/** How the agent receives what a speaker writes. `trusted` text is part of the
 *  prompt. `untrusted` text is sent as an untrusted block: the agent reads it
 *  as data, and it never reaches memory. */
export type Trust = "trusted" | "untrusted";

export interface Speaker {
  /** The loomcycle subject: "<platform>-" + the platform's stable user id. */
  subject: string;
  /** Display name, for messages that need to say who spoke. */
  name: string;
  trust: Trust;
  /** From outside the platform's fence (a Slack Connect guest): admitted as
   *  untrusted, but never the owner of a conversation, so never the one who
   *  answers its cards or stops it. */
  guest?: boolean;
}

/** Who a conversation's runs act as: the person in a 1:1 chat, the
 *  conversation itself in a shared one. */
export interface RunAs {
  subject: string;
  name: string;
}

export type Identified =
  | {
      ok: true;
      speaker: Speaker;
      scope: Scope;
      /** The conversation's key in the store: one session, one run at a time. */
      conversationId: string;
      runAs: RunAs;
    }
  | {
      ok: false;
      /** Why the message is dropped. Logged, never shown. */
      reason: string;
      /** When set with conversationId, sent to that conversation once, so a
       *  refused user hears why. */
      notice?: string;
      conversationId?: string;
    };

/** sharedSubject is the loomcycle user a shared conversation runs as. A shared
 *  chat runs as itself, never as one of its members: anyone in it may talk to
 *  the agent, so the agent must not hold any one person's data or tools.
 *  `parts` name the conversation's memory, not its session: a channel's
 *  threads pass the channel alone, so the channel's memory spans its threads
 *  while each thread is its own conversation. */
export function sharedSubject(platform: string, ...parts: string[]): string {
  return `${platform}-conv-${convHash(parts)}`;
}

/** gateSharedSubject is sharedSubject for a bridge that is a loomcycle gate:
 *  "<gate>:conv:<hash>", the subject loomcycle gives the platform id
 *  "conv:<hash>" (RFC FA §3.7d). */
export function gateSharedSubject(gate: string, ...parts: string[]): string {
  return `${gate}:conv:${convHash(parts)}`;
}

function convHash(parts: string[]): string {
  return createHash("sha256").update(parts.join("\n")).digest("hex").slice(0, 32);
}
