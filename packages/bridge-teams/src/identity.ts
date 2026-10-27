// Who sent an activity, taken only from fields the Bot Connector signs.

import { gateSharedSubject, sharedSubject, type Identified, type Scope } from "@loomcycle/bridge-core";

/** The subset of a Teams activity this module reads. */
export interface ActivityIdentityFields {
  from?: { aadObjectId?: string; name?: string };
  conversation?: { id?: string; tenantId?: string; conversationType?: string };
  channelData?: { tenant?: { id?: string } };
}

/** The subset of a stored conversation reference groupsOf reads. */
export interface RefGroupFields {
  conversation?: { tenantId?: string };
}

export const NOT_AVAILABLE_TEXT = "This bot isn't available to your organization.";

const GUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** personSub is the platform id a person is asserted under as a gate: their
 *  Entra object id, lower-cased, so the same person always maps to one user.
 *  loomcycle makes it the subject "<gate>:<object id>". undefined for an id
 *  that is not a GUID. */
export function personSub(aadObjectId: string): string | undefined {
  const id = aadObjectId.trim().toLowerCase();
  return GUID.test(id) ? id : undefined;
}

/** subjectFor maps an Entra object id to the loomcycle subject a bridge with
 *  a loomcycle token uses for that person: "teams-<object id>", which is also
 *  the user a gate's legacy_subject_prefix links a first login to. */
export function subjectFor(aadObjectId: string): string | undefined {
  const id = personSub(aadObjectId);
  return id ? `teams-${id}` : undefined;
}

/** conversationSubject is the loomcycle user a group chat or channel runs as.
 *  A channel's threads share one identity (the thread suffix is dropped), so
 *  its memory spans the channel; each thread is still its own run. With a
 *  gate it is "<gate>:conv:<hash>", the same hash as "teams-conv-<hash>". */
export function conversationSubject(tenantId: string, conversationId: string, gate?: string): string {
  const base = conversationId.split(";messageid=")[0];
  const tenant = tenantId.toLowerCase();
  return gate ? gateSharedSubject(gate, tenant, base) : sharedSubject("teams", tenant, base);
}

/** tenantGroup names the platform group of everyone in an Entra tenant.
 *  loomcycle keeps it as the "<gate>:teams-tenant-<id>" external group, which
 *  an operator maps (group_map) and binds ACLs to. */
export function tenantGroup(tenantId: string): string {
  return `teams-tenant-${tenantId.trim().toLowerCase()}`;
}

/** groupsOf is what a gate tells loomcycle about a run identity's groups
 *  (RFC FA §3.7d), from the conversation's stored reference alone: a person,
 *  a group chat and a channel are each in their tenant's group. A reference
 *  with no tenant asserts nothing. The reference carries no team id (that is
 *  in the activity's channelData, which is not kept), so a channel gets no
 *  team group. */
export function groupsOf(ref: RefGroupFields): string[] | undefined {
  const tenantId = ref.conversation?.tenantId?.trim();
  return tenantId ? [tenantGroup(tenantId)] : undefined;
}

/** identify reads who sent an activity. With a gate (the bridge's gate name in
 *  loomcycle, RFC FA §3.7d) a person is "<gate>:<object id>" and a shared
 *  conversation "<gate>:conv:<hash>". Without one the bridge holds a loomcycle
 *  token, and they are "teams-<object id>" and "teams-conv-<hash>": the
 *  subjects a gate's legacy_subject_prefix links to on a first login. */
export function identify(activity: ActivityIdentityFields, allowedTenants: ReadonlySet<string>, gate?: string): Identified {
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
  const sub = personSub(activity.from?.aadObjectId ?? "");
  if (!sub) return { ok: false, reason: "no_user" };
  const subject = gate ? `${gate}:${sub}` : `teams-${sub}`;
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
        : { subject: conversationSubject(tenantId, conversationId, gate), name: scope === "channel" ? "Teams channel" : "Teams group chat" },
  };
}

/** uninstalledSub is the person an installationUpdate "remove" says has left
 *  the bot: one who uninstalled it from their own 1:1 chat, in an allowed
 *  tenant. undefined for anything else (an install, a group chat, a team, a
 *  tenant outside the fence, no valid user), which the gate ignores. */
export function uninstalledSub(
  activity: ActivityIdentityFields & { action?: string },
  allowedTenants: ReadonlySet<string>,
): string | undefined {
  if (activity.action !== "remove") return undefined;
  // identify applies the same fence and user checks a message gets.
  const id = identify(activity, allowedTenants);
  if (!id.ok || id.scope !== "personal") return undefined;
  return personSub(activity.from?.aadObjectId ?? "");
}
