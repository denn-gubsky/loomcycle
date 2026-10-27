// Who sent a Slack event or pressed a button, and which conversation it
// belongs to. Only fields of a request whose signature was checked are read.
//
// The identity of a shared conversation is the channel, and its session is
// the thread: a channel's memory spans its threads while each thread is its
// own run. A DM is one conversation, thread or not.

import { gateSharedSubject, sharedSubject, type Identified, type Scope, type Trust } from "@loomcycle/bridge-core";

/** A DM, a group DM, or a channel (public or private). */
export type ChannelType = "im" | "mpim" | "channel";

/** Where a reply goes. Plain JSON: it is kept in the conversation store. */
export interface SlackRef {
  team: string;
  channel: string;
  channelType: ChannelType;
  /** The thread replies go in: always set in a channel; in a DM only when the
   *  person wrote inside a thread. */
  threadTs?: string;
  /** The message that was answered. A streamed reply must be a thread reply,
   *  so in a DM it threads under this message. */
  messageTs?: string;
}

/** What identify reads, from an event or a button press. */
export interface InboundFields {
  /** The workspace the request was delivered for. */
  team: string;
  /** The speaker's own team; undefined when it is unknown (a button press
   *  that names none, an externally shared channel's event that names none).
   *  Such a speaker is never taken for a member. */
  speakerTeam: string | undefined;
  /** Any other team id the request carries (a shared channel's source). Each
   *  must be allowed too. */
  otherTeams: string[];
  user: string;
  channel: string;
  channelType: ChannelType;
  /** The thread's root, when the message is in one. */
  threadTs?: string;
  ts: string;
}

export interface TrustPolicy {
  allowedTeams: ReadonlySet<string>;
  sharedTrust: Trust;
  /** The bridge's gate name in loomcycle (RFC FA §3.7d): a person is
   *  "<gate>:<team>:<user>", a shared conversation "<gate>:conv:<hash>".
   *  Unset for a bridge with a loomcycle token: "slack-<team>-<user>" and
   *  "slack-conv-<hash>", the subjects a gate's legacy_subject_prefix
   *  ("slack-", ':' read as '-') links to on a first login. */
  gate?: string;
}

export const NOT_ALLOWED_NOTICE = "This bot isn't enabled for this Slack workspace. Ask your administrator to allow it.";

const SLACK_ID = /^[A-Z0-9]{2,32}$/;

/** conversationId names the session: the DM, or one thread of a channel. */
export function conversationIdOf(f: Pick<InboundFields, "team" | "channel" | "channelType" | "threadTs" | "ts">): string {
  if (f.channelType === "im") return `slack:${f.team}:${f.channel}`;
  return `slack:${f.team}:${f.channel}:${f.threadTs ?? f.ts}`;
}

export function refOf(f: InboundFields): SlackRef {
  return {
    team: f.team,
    channel: f.channel,
    channelType: f.channelType,
    ...(f.channelType === "im" ? (f.threadTs ? { threadTs: f.threadTs } : {}) : { threadTs: f.threadTs ?? f.ts }),
    messageTs: f.ts,
  };
}

/** subjectFor is a person's loomcycle subject: their own team and user id,
 *  lower-cased, so one person always maps to one user. */
export function subjectFor(team: string, user: string): string {
  return `slack-${team}-${user}`.toLowerCase();
}

/** personSub is the platform id a person is asserted under: their own team
 *  and user id, lower-cased like subjectFor. loomcycle makes it the subject
 *  "<gate>:<team>:<user>". */
export function personSub(team: string, user: string): string {
  return `${team}:${user}`.toLowerCase();
}

/** teamsAllowed says whether every team id the request carries is allowed.
 *  In a channel shared between organisations (Slack Connect) the workspace
 *  may be ours while the speaker's is another: one allowed id must never
 *  vouch for a disallowed one, so such a speaker is a guest. So is one
 *  whose own team the request does not name. */
export function teamsAllowed(f: InboundFields, allowed: ReadonlySet<string>): boolean {
  if (f.speakerTeam === undefined) return false;
  return [f.team, f.speakerTeam, ...f.otherTeams].every((t) => allowed.has(t.toUpperCase()));
}

/** cleanName makes a display name safe to put in a bracketed label: it is
 *  whatever the person typed into their profile. */
export function cleanName(name: string | undefined, fallback: string): string {
  const n = (name ?? "").replace(/[\r\n[\]<>&]+/g, " ").replace(/\s+/g, " ").trim().slice(0, 64);
  return n || fallback;
}

export function identify(f: InboundFields, policy: TrustPolicy, displayName: string): Identified {
  const conversationId = conversationIdOf(f);
  const scope: Scope = f.channelType === "im" ? "personal" : f.channelType === "mpim" ? "group" : "channel";
  const refused = { ok: false, reason: "team_not_allowed", notice: NOT_ALLOWED_NOTICE, conversationId } as const;
  if (!policy.allowedTeams.has(f.team.toUpperCase())) return refused;
  // A guest from another organisation (Slack Connect) is admitted only where
  // the conversation's own identity runs, and only as untrusted: their text is
  // data, never part of the prompt, and they never own the conversation. In a
  // DM they would run as themselves, so there they are refused. A mention
  // carries no channel_type, so a DM is also known by its id.
  const guest = !teamsAllowed(f, policy.allowedTeams);
  if (guest && (scope === "personal" || f.channel.toUpperCase().startsWith("D"))) return refused;
  if (!SLACK_ID.test(f.user.toUpperCase())) return { ok: false, reason: "no_user" };
  // A speaker whose team is unknown has no subject that is safe to give: the
  // workspace's team would make it a member's, and the core takes whoever
  // holds the owner's subject for the owner, guest or not. So they are
  // refused here as well as in a DM.
  if (f.speakerTeam === undefined) return { ok: false, reason: "no_user" };
  // The speaker's own team names them, so a Slack Connect guest keeps one
  // identity whichever workspace they write in.
  const subject = policy.gate ? `${policy.gate}:${personSub(f.speakerTeam, f.user)}` : subjectFor(f.speakerTeam, f.user);
  const trust: Trust = guest ? "untrusted" : scope === "personal" ? "trusted" : policy.sharedTrust;
  const speaker = { subject, name: displayName, trust, ...(guest ? { guest: true } : {}) };
  if (scope === "personal") return { ok: true, speaker, scope, conversationId, runAs: { subject, name: displayName } };
  return {
    ok: true,
    speaker,
    scope,
    conversationId,
    runAs: {
      subject: policy.gate
        ? gateSharedSubject(policy.gate, f.team.toLowerCase(), f.channel.toLowerCase())
        : sharedSubject("slack", f.team.toLowerCase(), f.channel.toLowerCase()),
      name: scope === "group" ? `Slack group DM ${f.channel}` : `Slack channel ${f.channel}`,
    },
  };
}
