// Who sent an activity, taken only from fields the Bot Connector signs.

export type Scope = "personal" | "groupChat" | "channel";

export interface Speaker {
  /** The loomcycle subject: "teams-" + the Entra object id. */
  subject: string;
  /** Display name, for messages that need to say who spoke. */
  name: string;
}

export type Identified =
  | { ok: true; speaker: Speaker; tenantId: string; scope: Scope; conversationId: string }
  | { ok: false; reason: "no_user" | "no_tenant" | "tenant_not_allowed" | "no_conversation" };

/** The subset of a Teams activity this module reads. */
export interface ActivityIdentityFields {
  from?: { aadObjectId?: string; name?: string };
  conversation?: { id?: string; tenantId?: string; conversationType?: string };
  channelData?: { tenant?: { id?: string } };
}

const GUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** subjectFor maps an Entra object id to the loomcycle subject the bridge uses
 *  for that person. Lower-cased, so the same person always maps to one user. */
export function subjectFor(aadObjectId: string): string | undefined {
  const id = aadObjectId.trim().toLowerCase();
  return GUID.test(id) ? `teams-${id}` : undefined;
}

export function identify(activity: ActivityIdentityFields, allowedTenants: ReadonlySet<string>): Identified {
  const conversationId = activity.conversation?.id;
  if (!conversationId) return { ok: false, reason: "no_conversation" };
  const tenantId = (activity.conversation?.tenantId ?? activity.channelData?.tenant?.id ?? "").toLowerCase();
  if (!tenantId) return { ok: false, reason: "no_tenant" };
  // The fence comes before anything else touches loomcycle.
  if (!allowedTenants.has(tenantId)) return { ok: false, reason: "tenant_not_allowed" };
  const subject = subjectFor(activity.from?.aadObjectId ?? "");
  if (!subject) return { ok: false, reason: "no_user" };
  const t = activity.conversation?.conversationType;
  const scope: Scope = t === "groupChat" || t === "channel" ? t : "personal";
  return {
    ok: true,
    speaker: { subject, name: activity.from?.name?.trim() || "Someone" },
    tenantId,
    scope,
    conversationId,
  };
}
