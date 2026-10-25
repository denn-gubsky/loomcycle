// A user's sign-in identities and explicit account linking (RFC FA D14), and
// the approval of a pending user (RFC FA §3.7a). Linking is never inferred
// from an e-mail: the person, signed in recently, starts a fresh sign-in at
// the other gate's identity provider, and the server attaches that account.

export interface UserIdentity {
  gate: string;
  issuer?: string;
  external_subject: string;
  email?: string;
  email_verified: boolean;
  created_at: string;
  last_login_at?: string;
}

export type Result<T> = { ok: true; data: T } | { ok: false; code: string; message: string };

async function call<T>(fetchImpl: typeof fetch, url: string, init?: RequestInit): Promise<Result<T>> {
  try {
    const r = await fetchImpl(url, { credentials: "same-origin", ...init });
    if (r.status === 204) return { ok: true, data: undefined as T };
    const body = (await r.json().catch(() => ({}))) as Record<string, unknown>;
    if (r.ok) return { ok: true, data: body as T };
    return {
      ok: false,
      code: typeof body.code === "string" ? body.code : "error",
      message: typeof body.error === "string" ? body.error : r.statusText,
    };
  } catch (e) {
    return { ok: false, code: "network", message: e instanceof Error ? e.message : String(e) };
  }
}

const userPath = (subject: string) => "/v1/_users/" + encodeURIComponent(subject);

/** GET /v1/_users/{subject}/identities */
export function listIdentities(subject: string, fetchImpl: typeof fetch = fetch) {
  return call<{ subject: string; identities: UserIdentity[] }>(fetchImpl, userPath(subject) + "/identities");
}

/** POST /v1/_users/{subject}/identities — answers the provider URL to go to. */
export function startLink(subject: string, gate: string, fetchImpl: typeof fetch = fetch) {
  return call<{ redirect: string }>(fetchImpl, userPath(subject) + "/identities", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ gate }),
  });
}

/** DELETE /v1/_users/{subject}/identities/{gate} */
export function unlink(subject: string, gate: string, fetchImpl: typeof fetch = fetch) {
  return call<void>(fetchImpl, userPath(subject) + "/identities/" + encodeURIComponent(gate), { method: "DELETE" });
}

/** POST /v1/_users/{subject}/approve (a tenant operator's action). */
export function approveUser(subject: string, fetchImpl: typeof fetch = fetch) {
  return call<unknown>(fetchImpl, userPath(subject) + "/approve", { method: "POST" });
}

const linkErrorText: Record<string, string> = {
  reauth_required: "Sign in again first — linking an account needs a recent sign-in.",
  already_linked: "An account of that sign-in method is linked already; unlink it first.",
  same_gate: "You are signed in through that method already.",
  link_needs_own_session: "Link accounts from your own signed-in browser session.",
  link_failed: "The account could not be linked. It may belong to someone else, or the sign-in was not fresh.",
  too_many_attempts: "Too many sign-in attempts. Wait a few minutes and try again.",
};

/** The message for a link refusal code. */
export function linkErrorMessage(code: string): string {
  return linkErrorText[code] ?? "The account could not be linked — try again.";
}
