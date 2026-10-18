// Who sent an activity, taken only from fields the Bot Connector signs.

import { sharedSubject, type Identified, type Scope } from "@loomcycle/bridge-core";

/** The subset of a Teams activity this module reads. */
export interface ActivityIdentityFields {
  from?: { aadObjectId?: string; name?: string };
  conversation?: { id?: string; tenantId?: string; conversationType?: string };
  channelData?: { tenant?: { id?: string } };
}

export const NOT_AVAILABLE_TEXT = "This bot isn't available to your organization.";

const GUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** subjectFor maps an Entra object id to the loomcycle subject the bridge uses
 *  for that person. Lower-cased, so the same person always maps to one user. */
export function subjectFor(aadObjectId: string): string | undefined {
  const id = aadObjectId.trim().toLowerCase();
  return GUID.test(id) ? `teams-${id}` : undefined;
}

/** conversationSubject is the loomcycle user a group chat or channel runs as.
 *  A channel's threads share one identity (the thread suffix is dropped), so
 *  its memory spans the channel; each thread is still its own run. */
export function conversationSubject(tenantId: string, conversationId: string): string {
  const base = conversationId.split(";messageid=")[0];
  return sharedSubject("teams", tenantId.toLowerCase(), base);
}

export function identify(activity: ActivityIdentityFields, allowedTenants: ReadonlySet<string>): Identified {
  const conversationId = activity.conversation?.id;
  if (!conversationId) return { ok: false, reason: "no_conversation" };
  // Every tenant id the activity carries must be allowed, not just the first
  // one found: in a cross-organisation (shared) channel the conversation's
  // tenant can be the host's while the sender's is another. One allowed id
  // must never vouch for a disallowed one.
  const ids = [activity.conversation?.tenantId, activity.channelData?.tenant?.id]
    .map((t) => t?.trim().toLowerCase())
    .filter((t): t is string => !!t);
  if (ids.length === 0) return { ok: false, reason: "no_tenant" };
  // The fence comes before anything else touches loomcycle.
  if (!ids.every((t) => allowedTenants.has(t))) {
    return { ok: false, reason: "tenant_not_allowed", notice: NOT_AVAILABLE_TEXT, conversationId };
  }
  const tenantId = ids[0];
  const subject = subjectFor(activity.from?.aadObjectId ?? "");
  if (!subject) return { ok: false, reason: "no_user" };
  const t = activity.conversation?.conversationType;
  const scope: Scope = t === "groupChat" ? "group" : t === "channel" ? "channel" : "personal";
  const name = activity.from?.name?.trim() || "Someone";
  return {
    ok: true,
    // Everyone the fence lets in is in an allow-listed organisation, so what
    // they write is part of the prompt, in a shared chat too.
    speaker: { subject, name, trust: "trusted" },
    scope,
    conversationId,
    runAs:
      scope === "personal"
        ? { subject, name }
        : { subject: conversationSubject(tenantId, conversationId), name: scope === "channel" ? "Teams channel" : "Teams group chat" },
  };
}
