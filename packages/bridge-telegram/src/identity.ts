// Who sent a Telegram update, whether they may use the bot, and how far their
// words are trusted. Pure functions over the Bot API's own shapes.

import { gateSharedSubject, sharedSubject, type Identified, type Trust } from "@loomcycle/bridge-core";

/** The few Bot API fields the bridge reads. */
export interface TgUser {
  id: number;
  is_bot?: boolean;
  first_name?: string;
  last_name?: string;
  username?: string;
}

export interface TgChat {
  id: number;
  type: string;
  title?: string;
}

export interface TgEntity {
  type: string;
  offset: number;
  length: number;
  user?: TgUser;
}

export interface TgMessage {
  message_id: number;
  /** 0 for a message the bot can no longer read (an InaccessibleMessage). */
  date?: number;
  chat: TgChat;
  from?: TgUser;
  text?: string;
  entities?: TgEntity[];
  message_thread_id?: number;
  is_topic_message?: boolean;
  reply_to_message?: { from?: TgUser };
  /** Telegram's service message in a group that became a supergroup. */
  migrate_to_chat_id?: number;
}

export interface TgCallbackQuery {
  id: string;
  from: TgUser;
  message?: TgMessage;
  data?: string;
}

/** The bot's own membership changing in a chat (Bot API ChatMemberUpdated):
 *  in a private chat, "kicked" means the person blocked the bot. */
export interface TgChatMemberUpdated {
  chat: TgChat;
  from: TgUser;
  new_chat_member?: { status?: string };
}

export interface TgUpdate {
  update_id: number;
  message?: TgMessage;
  callback_query?: TgCallbackQuery;
  my_chat_member?: TgChatMemberUpdated;
}

/** Where a reply goes. Plain JSON: it is kept in the conversation store. */
export interface TelegramRef {
  chatId: number;
  /** A forum topic. */
  threadId?: number;
}

export interface AccessPolicy {
  allowedUsers: ReadonlySet<string>;
  allowedChats: ReadonlySet<string>;
  allowAnyone: boolean;
  trustedChats: ReadonlySet<string>;
  /** A group member must also be in allowedUsers, even with allowAnyone. */
  groupRequireUsers: boolean;
  /** The bridge's gate name in loomcycle (RFC FA §3.7d): a person is
   *  "<gate>:<telegram id>", a group "<gate>:conv:<hash>". Unset for a bridge
   *  with a loomcycle token: "telegram-<id>" and "telegram-conv-<hash>", the
   *  subjects a gate's legacy_subject_prefix links to on a first login. */
  gate?: string;
}

/** personSub is the platform id a person is asserted under: loomcycle makes
 *  it the subject "<gate>:<id>". */
export function personSub(userId: number): string {
  return String(userId);
}

/** chatGroup names the platform group of a chat's members, and adminGroup
 *  that of its administrators. loomcycle keeps them as "<gate>:tg-chat-<id>"
 *  external groups, which an operator maps (group_map) and binds ACLs to. */
export function chatGroup(chatId: number | string): string {
  return `tg-chat-${chatId}`;
}

export function adminGroup(chatId: number | string): string {
  return `tg-chat-${chatId}-admins`;
}

export const NOT_ALLOWED_NOTICE = "Sorry, this bot isn't available here. Ask its operator to allow this chat.";
/** For a member of an allowed group who is not on the user allow-list: the
 *  chat is allowed, so "allow this chat" would send them the wrong way. */
export const USER_NOT_ALLOWED_NOTICE = "Sorry, this bot isn't available to you. Ask its operator to allow you.";

/** topicOf is a message's forum topic. In a plain supergroup
 *  message_thread_id names a reply thread, which is not a conversation. */
export function topicOf(msg: Pick<TgMessage, "is_topic_message" | "message_thread_id">): number | undefined {
  return msg.is_topic_message && typeof msg.message_thread_id === "number" ? msg.message_thread_id : undefined;
}

/** conversationIdOf names the session: a chat, or a forum topic in it. */
export function conversationIdOf(chatId: number, threadId: number | undefined): string {
  return threadId === undefined ? `tg:${chatId}` : `tg:${chatId}:${threadId}`;
}

export function refFor(chat: TgChat, threadId: number | undefined): TelegramRef {
  return threadId === undefined ? { chatId: chat.id } : { chatId: chat.id, threadId };
}

/** displayName is a Telegram name made safe to put before a message: shared
 *  chats reach the agent as "[name] text", so a name holding a newline or a
 *  bracket could fake a line from someone else. Still an untrusted string. */
export function displayName(u: TgUser): string {
  const raw = [u.first_name, u.last_name].filter(Boolean).join(" ") || u.username || `user ${u.id}`;
  const clean = raw.replace(/[\u0000-\u001f\u007f[\]]/g, " ").replace(/\s+/g, " ").trim();
  return (clean || `user ${u.id}`).slice(0, 64);
}

/** identify maps a message's (or a button press's) chat and sender to the
 *  core's identity, applying the allow-list first. */
export function identify(chat: TgChat, from: TgUser | undefined, threadId: number | undefined, policy: AccessPolicy): Identified {
  if (!from) return { ok: false, reason: "no_user" };
  if (from.is_bot) return { ok: false, reason: "self_or_bot" };
  if (!Number.isSafeInteger(from.id) || !Number.isSafeInteger(chat.id)) return { ok: false, reason: "bad_id" };

  const chatKey = String(chat.id);
  const conversationId = conversationIdOf(chat.id, threadId);
  const subject = policy.gate ? `${policy.gate}:${personSub(from.id)}` : `telegram-${from.id}`;
  const name = displayName(from);

  if (chat.type === "private") {
    // A private chat's id is the user's.
    if (chat.id !== from.id) return { ok: false, reason: "chat_user_mismatch" };
    if (!policy.allowAnyone && !policy.allowedUsers.has(String(from.id))) {
      return { ok: false, reason: "user_not_allowed", notice: NOT_ALLOWED_NOTICE, conversationId };
    }
    return { ok: true, speaker: { subject, name, trust: "trusted" }, scope: "personal", conversationId, runAs: { subject, name } };
  }

  if (chat.type === "group" || chat.type === "supergroup") {
    if (!policy.allowAnyone && !policy.allowedChats.has(chatKey)) {
      return { ok: false, reason: "chat_not_allowed", notice: NOT_ALLOWED_NOTICE, conversationId };
    }
    if (policy.groupRequireUsers && !policy.allowedUsers.has(String(from.id))) {
      return { ok: false, reason: "user_not_allowed", notice: USER_NOT_ALLOWED_NOTICE, conversationId };
    }
    // A Telegram group is open to whoever its admins let in: its speakers
    // are untrusted unless the operator vouches for the chat.
    const trust: Trust = policy.trustedChats.has(chatKey) ? "trusted" : "untrusted";
    return {
      ok: true,
      speaker: { subject, name, trust },
      scope: "group",
      conversationId,
      // The chat's memory spans its topics; each topic is its own session.
      runAs: {
        subject: policy.gate ? gateSharedSubject(policy.gate, chatKey) : sharedSubject("telegram", chatKey),
        name: `Telegram group ${chatKey}`,
      },
    };
  }

  // A channel has no senders to answer.
  return { ok: false, reason: "unsupported_chat" };
}

/** chatIsUntrusted reports, from a reply reference's chat id alone, whether
 *  the chat's speakers are untrusted, matching identify. Telegram gives users
 *  positive ids and groups negative ones, and a private chat's id is its
 *  user's (identify refuses one that is not), so a positive id is a private,
 *  trusted chat. Read from config, not stored in the ref, so a change to
 *  TELEGRAM_BRIDGE_TRUSTED_CHATS applies to conversations already stored. */
export function chatIsUntrusted(chatId: number, trustedChats: ReadonlySet<string>): boolean {
  return chatId < 0 && !trustedChats.has(String(chatId));
}

/** What a private chat hears for a photo, a voice note, a sticker, a file. */
export const TEXT_ONLY_NOTICE = "I can only read text for now.";

/** The Message fields that carry content other than text. Service messages
 *  (a pin, a member joining) carry none of them and stay unanswered. */
const NON_TEXT_CONTENT = [
  "animation",
  "audio",
  "checklist",
  "contact",
  "dice",
  "document",
  "game",
  "location",
  "paid_media",
  "photo",
  "poll",
  "sticker",
  "story",
  "venue",
  "video",
  "video_note",
  "voice",
] as const;

/** hasNonTextContent reports a message the bridge cannot read: no text, but
 *  media or another kind of content. */
export function hasNonTextContent(msg: TgMessage): boolean {
  if (msg.text !== undefined) return false;
  const fields = msg as unknown as Record<string, unknown>;
  return NON_TEXT_CONTENT.some((k) => fields[k] !== undefined && fields[k] !== null);
}

const COMMAND = /^\/([A-Za-z0-9_]{1,32})(?:@([A-Za-z0-9_]{1,64}))?(?=\s|$)/;
/** The bridge's own commands, plus /start, which Telegram sends first. */
const OWN_COMMANDS = new Set(["new", "stop", "help", "start"]);

export interface BotInfo {
  id: number;
  username: string;
}

/** addressedText returns the text to route, or undefined when the message is
 *  not for this bot. In a group only a command, a mention of the bot or a
 *  reply to it counts, the same rule as Telegram's privacy mode, kept here
 *  too so a bot whose privacy mode was turned off, or that was made an admin,
 *  does not answer every message on the operator's keys.
 *
 *  A command addressed to the bot ("/new@MyBot") loses its suffix, so the
 *  core sees "/new"; one addressed to another bot is not ours. /start, which
 *  Telegram sends when someone first opens the bot, shows the help. */
export function addressedText(msg: TgMessage, bot: BotInfo): string | undefined {
  const text = msg.text;
  if (!text || !text.trim()) return undefined;
  const isGroup = msg.chat.type !== "private";
  const me = bot.username.toLowerCase();

  const cmd = COMMAND.exec(text);
  if (cmd) {
    const target = cmd[2]?.toLowerCase();
    if (target !== undefined && target !== me) return undefined;
    const name = cmd[1]!.toLowerCase();
    // An unaddressed command in a group may be meant for another bot there.
    if (isGroup && target === undefined && !OWN_COMMANDS.has(name)) return undefined;
    const rest = text.slice(cmd[0].length);
    return name === "start" ? "/help" : `/${cmd[1]}${rest}`;
  }

  if (!isGroup) return text;
  const mention = `@${me}`;
  const mentioned = (msg.entities ?? []).some(
    (e) =>
      (e.type === "mention" && text.slice(e.offset, e.offset + e.length).toLowerCase() === mention) ||
      (e.type === "text_mention" && e.user?.id === bot.id),
  );
  const repliedTo = msg.reply_to_message?.from?.id === bot.id;
  if (!mentioned && !repliedTo) return undefined;
  // "@MyBot what's up" reaches the agent as "what's up".
  const stripped = text.replace(new RegExp(`^\\s*@${escapeRegExp(bot.username)}\\b[\\s,:]*`, "i"), "");
  return stripped.trim() ? stripped : text;
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}
