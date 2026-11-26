// Where an inbound Teams message goes: the router's text path; for an audio
// file attached in a 1:1 chat with voice on, its voice path (RFC GA §5.4);
// for other attached files with files on, its file path (RFC HE). Kept
// apart from the SDK so it is tested without one.

import { sentAtOf, type ConversationRouter, type FileIn, type Identified, type MessageOptions } from "@loomcycle/bridge-core";

import { audioFileOf, FILE_DOWNLOAD_INFO, filesOf, type TeamsAttachment } from "./voice.js";

/** The subset of a message activity this module reads. */
export interface MessageFields {
  attachments?: readonly TeamsAttachment[];
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

export type MessageRouter<TRef> = Pick<ConversationRouter<TRef>, "onMessage" | "onVoice" | "onFile">;

/** routeMessage hands one message to the router. `id` comes from identify,
 *  so the tenant fence and the user checks have already run, and a refused
 *  activity takes the text path, which answers it as it always has.
 *
 *  An audio file is taken only in a personal chat (Teams drops files sent to
 *  a bot anywhere else) and only with BRIDGE_VOICE=1: with voice off the
 *  message goes the text path exactly as before. Text typed with the file
 *  follows the voice message as a message of its own, so a caption ("answer
 *  in French") is not lost; the conversation takes the two in order.
 *
 *  Any other attached file goes the file path with BRIDGE_FILES=1 (RFC
 *  HE), and so does an audio file with voice off (decision D4), with the
 *  text typed beside it, so the agent gets both in one message. Files from
 *  a group chat or a channel go there too: the core takes files only in a
 *  1:1 chat, and says so in plain words rather than drop them unseen. With
 *  files off nothing changes.
 *
 *  graphFiles are the files the Graph fallback found for the message (RFC
 *  HF), taken like attached ones with files on. */
export async function routeMessage<TRef>(
  router: MessageRouter<TRef>,
  id: Identified,
  ref: TRef,
  text: string,
  activity: MessageFields,
  voiceOn: boolean,
  filesOn = false,
  graphFiles: readonly FileIn[] = [],
): Promise<void> {
  const audio = voiceOn && id.ok && id.scope === "personal" ? audioFileOf(activity.attachments) : undefined;
  const files = filesOn && id.ok ? [...filesOf(activity.attachments, voiceOn), ...graphFiles] : [];
  const seen = seenOf(activity);
  if (audio) await router.onVoice(id, ref, audio, seen);
  if (files.length > 0) {
    await router.onFile(id, ref, files, { ...seen, text });
    return;
  }
  if (!audio) {
    await router.onMessage(id, ref, text, seen);
    return;
  }
  if (text.trim()) await router.onMessage(id, ref, text, seen);
}

/** What the bridge answers a message that looked like a file and brought
 *  none (RFC HF D6). */
export const NO_FILE_TEXT = "I didn't receive a file. Teams didn't pass it to me.";

/** noFileText is that answer, with the operator's hint
 *  (TEAMS_BRIDGE_NO_FILE_HINT) after it when one is set. */
export function noFileText(hint?: string): string {
  return hint ? `${NO_FILE_TEXT} ${hint}` : NO_FILE_TEXT;
}

/** hasFileDownloadInfo says Teams sent a file the bot downloads itself, so
 *  there is nothing for Graph to find. */
export function hasFileDownloadInfo(attachments: readonly TeamsAttachment[] | undefined): boolean {
  return (attachments ?? []).some((a) => a.contentType === FILE_DOWNLOAD_INFO);
}

/** looksLikeFile is the shape a file attached in a 1:1 chat reached the bot
 *  as on a live tenant (RFC HF §1): no text, and only text/html attachments
 *  that hold no text either. A caption leaves the same trace as any typed
 *  message, so a captioned file does not look like one. */
export function looksLikeFile(text: string, attachments: readonly TeamsAttachment[] | undefined): boolean {
  const atts = attachments ?? [];
  if (text.trim() || atts.length === 0) return false;
  return atts.every((a) => (a.contentType ?? "").toLowerCase().startsWith("text/html") && htmlIsEmpty(a.content));
}

function htmlIsEmpty(content: unknown): boolean {
  if (content === undefined || content === null) return true;
  if (typeof content !== "string") return false;
  return !content
    .replace(/<[^>]*>/g, "")
    .replace(/&nbsp;|&#160;/gi, " ")
    .trim();
}
