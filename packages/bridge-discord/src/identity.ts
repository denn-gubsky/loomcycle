// Who used a command, and where, as the core needs it. The fences (allowed
// servers, DMs on or off) are applied here, before anything touches loomcycle.

import { gateSharedSubject, sharedSubject, type Identified, type Speaker } from "@loomcycle/bridge-core";

import { isSnowflake, type DiscordConfig } from "./config.js";
import { Context, type Interaction } from "./interaction.js";

export type Fences = Pick<DiscordConfig, "applicationId" | "allowedGuilds" | "allowDms" | "trustedGuilds"> & {
  /** The bridge's gate name in loomcycle (RFC FA §3.7d): a person is
   *  "<gate>:<user id>", a channel "<gate>:conv:<hash>". Unset for a bridge
   *  with a loomcycle token: "discord-<id>" and "discord-conv-<hash>", the
   *  subjects a gate's legacy_subject_prefix links to on a first login. */
  gate?: string;
};

/** Where an interaction happened. */
export interface Place {
  channelId: string;
  /** Absent in a DM. */
  guildId?: string;
}

const THREAD_TYPES = new Set([10, 11, 12]);

/** personSub is the platform id a person is asserted under: loomcycle makes
 *  it the subject "<gate>:<id>". */
export function personSub(userId: string): string {
  return userId;
}

/** guildGroup names the platform group of a server's members, and roleGroup
 *  that of a role's holders. loomcycle keeps them as "<gate>:dc-guild-<id>"
 *  external groups, which an operator maps (group_map) and binds ACLs to. */
export function guildGroup(guildId: string): string {
  return `dc-guild-${guildId}`;
}

export function roleGroup(roleId: string): string {
  return `dc-role-${roleId}`;
}

export const NOT_IN_SERVER = "This bot isn't available in this server.";
export const NOT_IN_DMS = "This bot isn't available in direct messages.";
export const NOT_HERE = "This bot isn't available here.";

/** placeOf reads the channel and server, or undefined when they are missing. */
export function placeOf(ix: Interaction): Place | undefined {
  const channelId = ix.channel?.id ?? ix.channel_id;
  if (!isSnowflake(channelId)) return undefined;
  if (ix.guild_id === undefined) return { channelId };
  return isSnowflake(ix.guild_id) ? { channelId, guildId: ix.guild_id } : undefined;
}

export function conversationIdOf(place: Place): string {
  return place.guildId ? `dc:${place.guildId}:${place.channelId}` : `dc:dm:${place.channelId}`;
}

/** displayName is untrusted: it is shown to the agent as "[name] text", so it
 *  is kept to one short line without brackets. */
function displayName(ix: Interaction, id: string): string {
  const u = ix.member?.user ?? ix.user;
  const raw = ix.member?.nick ?? u?.global_name ?? u?.username ?? id;
  const clean = String(raw)
    .replace(/[\u0000-\u001f\u007f\u2028\u2029[\]]/g, " ")
    .replace(/\s+/g, " ")
    .trim()
    .slice(0, 64);
  return clean || id;
}

export function identify(ix: Interaction, fences: Fences): Identified {
  const user = ix.member?.user ?? ix.user;
  if (!user || !isSnowflake(user.id)) return { ok: false, reason: "no_user" };
  const subject = fences.gate ? `${fences.gate}:${personSub(user.id)}` : `discord-${user.id}`;
  // Bots cannot use slash commands today; this keeps that true if they ever can.
  if (user.bot || user.id === fences.applicationId) return { ok: false, reason: "self_or_bot" };
  const place = placeOf(ix);
  if (!place) return { ok: false, reason: "no_channel" };
  const conversationId = conversationIdOf(place);

  if (!place.guildId) {
    // A group DM, or a DM between two other people (a user-installed app):
    // several people, none of them the bot, so neither a 1:1 nor a server.
    if (ix.context === Context.PRIVATE_CHANNEL) return { ok: false, reason: "private_channel", notice: NOT_HERE, conversationId };
    if (!fences.allowDms) return { ok: false, reason: "dms_closed", notice: NOT_IN_DMS, conversationId };
    const speaker: Speaker = { subject, name: displayName(ix, user.id), trust: "trusted" };
    return { ok: true, speaker, scope: "personal", conversationId, runAs: { subject: speaker.subject, name: speaker.name } };
  }

  if (!fences.allowedGuilds.has(place.guildId)) return { ok: false, reason: "guild_not_allowed", notice: NOT_IN_SERVER, conversationId };
  const speaker: Speaker = {
    subject,
    name: displayName(ix, user.id),
    trust: fences.trustedGuilds.has(place.guildId) ? "trusted" : "untrusted",
  };
  // A thread is its own conversation, but remembers as its parent channel,
  // so a channel's memory spans its threads.
  const parent = ix.channel?.parent_id;
  const memoryChannel = THREAD_TYPES.has(ix.channel?.type ?? -1) && isSnowflake(parent) ? parent : place.channelId;
  return {
    ok: true,
    speaker,
    scope: "channel",
    conversationId,
    runAs: {
      subject: fences.gate ? gateSharedSubject(fences.gate, place.guildId, memoryChannel) : sharedSubject("discord", place.guildId, memoryChannel),
      name: "Discord channel",
    },
  };
}
