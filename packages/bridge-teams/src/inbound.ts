// Where an inbound Teams message goes: the router's text path, or, for an
// audio file attached in a 1:1 chat with voice on, its voice path (RFC GA
// §5.4). Kept apart from the SDK so it is tested without one.

import { sentAtOf, type ConversationRouter, type Identified, type MessageOptions } from "@loomcycle/bridge-core";

import { audioFileOf, type TeamsAttachment } from "./voice.js";

/** The subset of a message activity this module reads. */
export interface MessageFields {
  attachments?: readonly TeamsAttachment[];
  locale?: string;
  /** When Teams says the activity was sent: a Date from the SDK's types, an
   *  ISO string on the wire. */
  timestamp?: Date | string;
}

/** seenOf is an activity's date for the router, from its timestamp: what
 *  lets a person who writes again drop a revoke kept for them. */
export function seenOf(activity: { timestamp?: Date | string }): MessageOptions {
  const t = activity.timestamp;
  return { sentAt: sentAtOf(t instanceof Date ? t.getTime() : typeof t === "string" ? Date.parse(t) : undefined) };
}

export type MessageRouter<TRef> = Pick<ConversationRouter<TRef>, "onMessage" | "onVoice">;

/** routeMessage hands one message to the router. `id` comes from identify,
 *  so the tenant fence and the user checks have already run, and a refused
 *  activity takes the text path, which answers it as it always has.
 *
 *  An audio file is taken only in a personal chat (Teams drops files sent to
 *  a bot anywhere else) and only with BRIDGE_VOICE=1: with voice off the
 *  message goes the text path exactly as before. Text typed with the file
 *  follows the voice message as a message of its own, so a caption ("answer
 *  in French") is not lost; the conversation takes the two in order. */
export async function routeMessage<TRef>(
  router: MessageRouter<TRef>,
  id: Identified,
  ref: TRef,
  text: string,
  activity: MessageFields,
  voiceOn: boolean,
): Promise<void> {
  const audio = voiceOn && id.ok && id.scope === "personal" ? audioFileOf(activity.attachments) : undefined;
  const seen = seenOf(activity);
  if (!audio) {
    await router.onMessage(id, ref, text, seen);
    return;
  }
  await router.onVoice(id, ref, audio, activity.locale, seen);
  if (text.trim()) await router.onMessage(id, ref, text, seen);
}
