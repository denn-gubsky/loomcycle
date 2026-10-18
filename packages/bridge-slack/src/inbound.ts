// Reading Slack's two inbound shapes, after their signature is checked: an
// Events API envelope, and a block_actions interaction payload.

import type { ChannelType, InboundFields } from "./identity.js";

export interface SlackMessageEvent {
  type?: string;
  subtype?: string;
  user?: string;
  bot_id?: string;
  text?: string;
  ts?: string;
  thread_ts?: string;
  channel?: string;
  channel_type?: string;
  team?: string;
  user_team?: string;
  source_team?: string;
}

export interface EventEnvelope {
  type?: string;
  team_id?: string;
  event_id?: string;
  challenge?: string;
  event?: SlackMessageEvent;
}

/** The bot's own ids, learned from auth.test at start. */
export interface SelfIds {
  userId: string;
}

export type ReadEvent =
  /** Not an event the bridge handles (another type, a channel message that is not a mention). */
  | { kind: "ignore" }
  /** An edit, a join, a bot's message, our own. Dropped before anything else. */
  | { kind: "self_or_bot" }
  | { kind: "message"; fields: InboundFields; text: string };

function str(v: unknown): string | undefined {
  return typeof v === "string" && v ? v : undefined;
}

function channelTypeOf(t: string | undefined): ChannelType {
  return t === "im" ? "im" : t === "mpim" ? "mpim" : "channel";
}

/** readEvent takes a DM (`message` with channel_type "im") or a mention in a
 *  channel (`app_mention`). Every other event is ignored. */
export function readEvent(env: EventEnvelope, self: SelfIds): ReadEvent {
  const ev = env.event;
  if (!ev || env.type !== "event_callback") return { kind: "ignore" };
  const isDm = ev.type === "message" && ev.channel_type === "im";
  // A mention is delivered as app_mention, so a channel's plain `message`
  // (if the app also subscribes to message.channels) would answer it twice.
  if (!isDm && ev.type !== "app_mention") return { kind: "ignore" };
  // Any subtype is an edit, a deletion, a join, a bot_message…: never a
  // person writing to the agent.
  if (ev.subtype !== undefined || ev.bot_id !== undefined) return { kind: "self_or_bot" };
  const user = str(ev.user);
  if (!user || user === self.userId) return { kind: "self_or_bot" };
  const team = str(env.team_id);
  const channel = str(ev.channel);
  const ts = str(ev.ts);
  if (!team || !channel || !ts) return { kind: "ignore" };
  // The speaker's team comes first: identify names the person by it.
  const speakerTeam = str(ev.user_team) ?? str(ev.team) ?? team;
  const otherTeams = [speakerTeam, str(ev.source_team)].filter((t): t is string => !!t);
  return {
    kind: "message",
    fields: {
      team,
      otherTeams,
      user,
      channel,
      channelType: isDm ? "im" : channelTypeOf(ev.channel_type),
      threadTs: str(ev.thread_ts),
      ts,
    },
    text: cleanInbound(str(ev.text) ?? "", self.userId),
  };
}

/** cleanInbound turns Slack's message markup into plain text for the agent:
 *  the bot's own mention removed, links unwrapped, entities decoded. */
export function cleanInbound(text: string, botUserId: string): string {
  return text
    .replace(new RegExp(`<@${botUserId}(\\|[^>]*)?>`, "g"), "")
    .replace(/<((?:https?|mailto):[^|>]+)\|([^>]+)>/g, "$2 ($1)")
    .replace(/<((?:https?|mailto):[^>]+)>/g, "$1")
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&amp;/g, "&")
    .replace(/[ \t]{2,}/g, " ")
    .trim();
}

export interface BlockActionsPayload {
  type?: string;
  team?: { id?: string };
  user?: { id?: string; team_id?: string };
  channel?: { id?: string };
  container?: { channel_id?: string; message_ts?: string; thread_ts?: string };
  message?: { ts?: string; thread_ts?: string };
  response_url?: string;
  actions?: Array<{ type?: string; action_id?: string; block_id?: string; value?: string }>;
}

/** What a pressed card button names: the conversation and the button id. */
export interface ReadAction {
  fields: InboundFields;
  buttonId: string;
  responseUrl?: string;
}

/** The actions block of every card the bridge draws carries one of these as
 *  its block_id, so a press tells which kind of conversation drew it. The
 *  payload is signed, and only the bot can edit its own message. */
export const CARD_BLOCK_ID: Record<ChannelType, string> = { im: "lc.card.im", mpim: "lc.card.mpim", channel: "lc.card.channel" };
export const ACTION_ID_PREFIX = "lc.";
const BUTTON_ID = /^[A-Za-z0-9_-]{1,32}$/;

/** readAction reads a press of a button the bridge drew. Anything else (a
 *  button of another app's message, a malformed payload) is undefined. */
export function readAction(p: BlockActionsPayload): ReadAction | undefined {
  if (p.type !== "block_actions") return undefined;
  const a = p.actions?.[0];
  if (!a || a.type !== "button" || !a.action_id?.startsWith(ACTION_ID_PREFIX)) return undefined;
  const value = a.value;
  if (!value || !BUTTON_ID.test(value)) return undefined;
  const channelType = (Object.keys(CARD_BLOCK_ID) as ChannelType[]).find((k) => CARD_BLOCK_ID[k] === a.block_id);
  if (!channelType) return undefined;
  const team = str(p.team?.id);
  const user = str(p.user?.id);
  const channel = str(p.channel?.id) ?? str(p.container?.channel_id);
  const ts = str(p.message?.ts) ?? str(p.container?.message_ts);
  if (!team || !user || !channel || !ts) return undefined;
  return {
    fields: {
      team,
      otherTeams: [str(p.user?.team_id) ?? team],
      user,
      channel,
      channelType,
      // A channel's card is posted in the thread it answers, so its thread is
      // the conversation's.
      threadTs: str(p.message?.thread_ts) ?? str(p.container?.thread_ts),
      ts,
    },
    buttonId: value,
    responseUrl: str(p.response_url),
  };
}
