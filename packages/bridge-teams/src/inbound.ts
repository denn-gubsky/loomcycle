// Where an inbound Teams message goes: the router's text path, or, for an
// audio file attached in a 1:1 chat with voice on, its voice path (RFC GA
// §5.4). Kept apart from the SDK so it is tested without one.

import type { ConversationRouter, Identified } from "@loomcycle/bridge-core";

import { audioFileOf, type TeamsAttachment } from "./voice.js";

/** The subset of a message activity this module reads. */
export interface MessageFields {
  attachments?: readonly TeamsAttachment[];
  locale?: string;
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
  if (!audio) {
    await router.onMessage(id, ref, text);
    return;
  }
  await router.onVoice(id, ref, audio, activity.locale);
  if (text.trim()) await router.onMessage(id, ref, text);
}
