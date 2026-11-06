// Reading Slack's two inbound shapes, after their signature is checked: an
// Events API envelope, and a block_actions interaction payload.

import type { VoiceIn } from "@loomcycle/bridge-core";

import type { ChannelType, InboundFields } from "./identity.js";
import type { SlackFileRef } from "./voice.js";

/** A shared file, as a `file_share` message carries it. */
export interface SlackFile {
  id?: string;
  /** `slack_audio` for an audio clip recorded in Slack. */
  subtype?: string;
  mimetype?: string;
  size?: number;
  duration_ms?: number;
  url_private_download?: string;
}

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
  files?: SlackFile[];
}

export interface EventEnvelope {
  type?: string;
  team_id?: string;
  event_id?: string;
  /** Slack: "whether the event occurred in an externally shared channel". */
  is_ext_shared_channel?: boolean;
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
  | { kind: "message"; fields: InboundFields; text: string }
  /** A voice clip or an audio file (RFC GA), read only with voice on. */
  | { kind: "voice"; fields: InboundFields; voice: VoiceIn };

export interface ReadOptions {
  /** Take a message carrying one audio file as a voice message. Off, such a
   *  message is read exactly as before voice existed. */
  voice?: boolean;
}

function str(v: unknown): string | undefined {
  return typeof v === "string" && v ? v : undefined;
}

function channelTypeOf(t: string | undefined): ChannelType {
  return t === "im" ? "im" : t === "mpim" ? "mpim" : "channel";
}

/** readEvent takes a DM (`message` with channel_type "im") or a mention in a
 *  channel (`app_mention`). Every other event is ignored. With voice on, a
 *  DM or a mention that carries exactly one audio file (a clip, or an audio
 *  file shared) is a voice message: in a channel only a mention, as for
 *  text, so a clip there must name the bot. */
export function readEvent(env: EventEnvelope, self: SelfIds, opts: ReadOptions = {}): ReadEvent {
  const ev = env.event;
  if (!ev || env.type !== "event_callback") return { kind: "ignore" };
  const isDm = ev.type === "message" && ev.channel_type === "im";
  // A mention is delivered as app_mention, so a channel's plain `message`
  // (if the app also subscribes to message.channels) would answer it twice.
  if (!isDm && ev.type !== "app_mention") return { kind: "ignore" };
  // Bots and the bot itself first, before any subtype is let through: the
  // bridge's own voice reply is a file_share of an audio file, and taking it
  // as input would loop on the operator's engines.
  if (ev.bot_id !== undefined) return { kind: "self_or_bot" };
  const user = str(ev.user);
  if (!user || user === self.userId) return { kind: "self_or_bot" };
  const voice = opts.voice ? audioOf(ev.files) : undefined;
  // Any other subtype is an edit, a deletion, a join, a bot_message…: never
  // a person writing to the agent. file_share passes only as a voice message.
  if (ev.subtype !== undefined && !(ev.subtype === "file_share" && voice)) return { kind: "self_or_bot" };
  const team = str(env.team_id);
  const channel = str(ev.channel);
  const ts = str(ev.ts);
  if (!team || !channel || !ts) return { kind: "ignore" };
  const fields: InboundFields = {
    team,
    speakerTeam: speakerTeamOf(env, ev, team),
    otherTeams: [str(ev.source_team)].filter((t): t is string => !!t),
    user,
    channel,
    channelType: isDm ? "im" : channelTypeOf(ev.channel_type),
    threadTs: str(ev.thread_ts),
    ts,
  };
  // Text typed beside a clip is not read: the recording is the message.
  if (voice) return { kind: "voice", fields, voice };
  return {
    kind: "message",
    fields,
    text: cleanInbound(str(ev.text) ?? "", self.userId),
  };
}

/** audioOf reads a message's files as a voice message: exactly one, either
 *  a Slack audio clip or a file whose type is audio. Length and size come
 *  from Slack's metadata, so the core can refuse before any download. */
export function audioOf(files: unknown): VoiceIn | undefined {
  if (!Array.isArray(files) || files.length !== 1) return undefined;
  const f = files[0] as SlackFile | null;
  if (typeof f !== "object" || f === null) return undefined;
  const id = str(f.id);
  const mime = str(f.mimetype)?.toLowerCase();
  const isClip = f.subtype === "slack_audio";
  if (!id || (!isClip && !mime?.startsWith("audio/"))) return undefined;
  const url = str(f.url_private_download);
  const ref: SlackFileRef = { id, ...(url ? { url } : {}) };
  const seconds = count(f.duration_ms);
  const bytes = count(f.size);
  return {
    kind: isClip ? "clip" : "audio_file",
    ref,
    ...(mime ? { mime } : {}),
    ...(seconds !== undefined ? { seconds: seconds / 1000 } : {}),
    ...(bytes !== undefined ? { bytes } : {}),
  };
}

function count(v: unknown): number | undefined {
  return typeof v === "number" && Number.isFinite(v) && v >= 0 ? v : undefined;
}

/** A person deactivated in their workspace: their team and user id. */
export interface DeletedUser {
  team: string;
  user: string;
}

/** readDeletedUser reads a `user_change` event whose user is deleted
 *  (deactivated), the only one the bridge acts on: as a gate it tells
 *  loomcycle the person is gone. Every team id it carries must be allowed:
 *  the envelope's and the user's own. Anything else is undefined. */
export function readDeletedUser(env: EventEnvelope, allowedTeams: ReadonlySet<string>): DeletedUser | undefined {
  if (env.type !== "event_callback") return undefined;
  // user_change carries a user object where a message carries an id.
  const ev = env.event as { type?: unknown; user?: unknown } | undefined;
  if (ev?.type !== "user_change" || typeof ev.user !== "object" || ev.user === null) return undefined;
  const u = ev.user as { id?: unknown; team_id?: unknown; deleted?: unknown };
  if (u.deleted !== true) return undefined;
  const user = str(u.id);
  const team = str(u.team_id);
  const envTeam = str(env.team_id);
  if (!user || !team || !envTeam) return undefined;
  if (!allowedTeams.has(team.toUpperCase()) || !allowedTeams.has(envTeam.toUpperCase())) return undefined;
  return { team, user };
}

/** speakerTeamOf is the speaker's own team. Slack's reference examples show
 *  neither `user_team` nor `team` on these events, so one that names none is
 *  taken for the delivered-for workspace's, except in a channel shared with
 *  another organisation: there the speaker may be the other one's, and is
 *  left unknown, which identify never takes for a member. */
function speakerTeamOf(env: EventEnvelope, ev: SlackMessageEvent, team: string): string | undefined {
  // Absent (or null) is not shared; any other value but false fails closed.
  const extShared = env.is_ext_shared_channel != null && env.is_ext_shared_channel !== false;
  return str(ev.user_team) ?? str(ev.team) ?? (extShared ? undefined : team);
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
      speakerTeam: str(p.user?.team_id),
      otherTeams: [],
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
