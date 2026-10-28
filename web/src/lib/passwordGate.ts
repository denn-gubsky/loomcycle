// The password gate's browser side (RFC FA P2). loomcycle's own pages are the
// only place a password is typed: the login form, an invite's page, a reset's
// page and the account page. Every call is a same-origin JSON POST; the invite
// and reset codes travel in the body, never in a URL.

import { UI_REQUEST_HEADER } from "./uiSession";

/** A refusal from the server: its code, message and any extra fields. */
export interface Refusal {
  code: string;
  error?: string;
  min_length?: number;
  /** For "second_factor_required" (RFC FA P6): the passkey offer to answer,
   *  and whether an authenticator code would do instead. */
  webauthn?: { ceremony: string; publicKey: Record<string, unknown> };
  otp?: boolean;
}

export type Result<T> = { ok: true; data: T } | { ok: false; refusal: Refusal };

/** A passkey's answer to an offer, as a request carries it (passkeys.ts). */
export type PasskeyAnswerJSON = { ceremony: string; credential: unknown };

export async function call<T>(fetchImpl: typeof fetch, method: string, path: string, body?: unknown): Promise<Result<T>> {
  let r: Response;
  try {
    r = await fetchImpl(path, {
      method,
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", [UI_REQUEST_HEADER]: "1" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    return { ok: false, refusal: { code: "network" } };
  }
  if (r.status === 204) return { ok: true, data: {} as T };
  let parsed: unknown = null;
  try {
    parsed = await r.json();
  } catch {
    parsed = null;
  }
  if (r.ok) return { ok: true, data: (parsed ?? {}) as T };
  const p = (parsed ?? {}) as Partial<Refusal>;
  return {
    ok: false,
    refusal: {
      code: typeof p.code === "string" ? p.code : "refused",
      error: p.error,
      min_length: p.min_length,
      webauthn: p.webauthn,
      otp: p.otp,
    },
  };
}

/** The code a link carries in its fragment ("#lci_…"). The fragment never
 *  reaches the server's logs; the page sends it in a POST body. */
export function codeFromHash(hash: string): string {
  const raw = hash.startsWith("#") ? hash.slice(1) : hash;
  return /^lc[ir]_[A-Za-z0-9_-]{20,60}$/.test(raw) ? raw : "";
}

/** Removes the code from the address bar and history once read. */
export function forgetHash(win: Window = window): void {
  try {
    win.history.replaceState(null, "", win.location.pathname + win.location.search);
  } catch {
    /* best effort */
  }
}

export interface TotpEnrolment {
  secret: string;
  otpauth_uri: string;
  enrolment: string;
}

export interface InviteInfo {
  gate: string;
  login: string;
  email: string;
  require_mfa: boolean;
  min_length: number;
  min_length_with_mfa: number;
  expires_at: string;
  totp?: TotpEnrolment;
}

export interface ResetInfo {
  gate: string;
  login: string;
  has_totp: boolean;
  require_mfa: boolean;
  min_length: number;
  min_length_with_mfa: number;
  expires_at: string;
  totp?: TotpEnrolment;
}

export interface Redeemed {
  redirect: string;
  login: string;
  recovery_codes: string[] | null;
}

export interface AccountInfo {
  gate: string;
  login: string;
  has_totp: boolean;
  recovery_codes_left: number;
  require_mfa: boolean;
}

export const signInWithPassword = (
  gate: string,
  login: string,
  password: string,
  otp: string,
  f: typeof fetch = fetch,
  webauthn?: PasskeyAnswerJSON,
) => call<object>(f, "POST", "/v1/_auth/complete/" + encodeURIComponent(gate), { login, password, otp, webauthn });

export const inviteInfo = (code: string, f: typeof fetch = fetch) => call<InviteInfo>(f, "POST", "/v1/_auth/invite/info", { code });

export const redeemInvite = (
  req: { code: string; login: string; password: string; totp_enrolment?: string; totp_code?: string },
  f: typeof fetch = fetch,
) => call<Redeemed>(f, "POST", "/v1/_auth/invite", req);

export const resetInfo = (code: string, f: typeof fetch = fetch) => call<ResetInfo>(f, "POST", "/v1/_auth/reset/info", { code });

export const completeReset = (
  req: { code: string; password: string; otp?: string; totp_enrolment?: string; totp_code?: string },
  f: typeof fetch = fetch,
) => call<Redeemed>(f, "POST", "/v1/_auth/reset", req);

export const accountInfo = (f: typeof fetch = fetch) => call<AccountInfo>(f, "GET", "/v1/_auth/account");

export const changePassword = (
  current_password: string,
  new_password: string,
  otp: string,
  f: typeof fetch = fetch,
  webauthn?: PasskeyAnswerJSON,
) => call<object>(f, "POST", "/v1/_auth/password", { current_password, new_password, otp, webauthn });

export const totpBegin = (f: typeof fetch = fetch) => call<TotpEnrolment>(f, "POST", "/v1/_auth/totp/begin", {});

export const totpConfirm = (
  req: { current_password: string; otp: string; enrolment: string; code: string; webauthn?: PasskeyAnswerJSON },
  f: typeof fetch = fetch,
) => call<{ recovery_codes: string[] }>(f, "POST", "/v1/_auth/totp/confirm", req);

export const totpRemove = (current_password: string, otp: string, f: typeof fetch = fetch, webauthn?: PasskeyAnswerJSON) =>
  call<object>(f, "DELETE", "/v1/_auth/totp", { current_password, otp, webauthn });

const messages: Record<string, string> = {
  invalid_credentials: "That login and password were not accepted.",
  otp_required: "Enter the 6-digit code from your authenticator app, or a recovery code.",
  invalid_otp: "That code was not accepted.",
  too_many_attempts: "Too many attempts. Wait a few minutes and try again.",
  mfa_enrolment_required: "This sign-in needs an authenticator app and your account has none. Ask your administrator for a reset link.",
  link_invalid: "This link is not valid. It may have been used or have expired; ask for a new one.",
  login_taken: "That login is taken. Choose another.",
  login_fixed: "This invitation is for a specific login.",
  invalid_login: "A login must be 2 to 254 characters, with no spaces.",
  password_too_short: "That password is too short.",
  password_too_long: "That password is too long.",
  password_breached: "That password appears in a list of breached or common passwords. Choose another.",
  password_is_login: "Your password must not be your login or e-mail.",
  totp_required: "This sign-in needs an authenticator app. Add it below.",
  invalid_totp: "The authenticator code was not accepted. Check the app's clock and try the next code.",
  reauth_failed: "Your current password or code was not accepted.",
  insecure_transport: "This server only signs you in over HTTPS. Open it through its https:// address.",
  session_required: "Sign in with your login and password to manage this account.",
  mfa_required: "This sign-in requires an authenticator app; it cannot be removed.",
  network: "The server could not be reached.",
  second_factor_required: "Confirm with your passkey.",
  second_factor_unavailable:
    "Your account signs in with a passkey, which this server cannot use right now, and has no authenticator app. Ask your administrator.",
  passkey_cancelled: "The passkey was not used. Try again.",
  passkey_not_accepted: "The passkey was not accepted. Try again.",
  passkey_exists: "That passkey is already registered.",
  passkeys_unavailable: "Passkeys are not available on this server.",
  passkey_needs_user_session: "Sign in with your password, a passkey or your identity provider to manage passkeys.",
  password_required: "Enter your current password above to confirm it is you.",
  too_many_passkeys: "This account has as many passkeys as it can keep. Remove one first.",
  last_sign_in_method: "This passkey is the only way to sign in to this account. Add another first.",
  last_second_factor: "This sign-in needs a second factor. Add an authenticator app or another passkey first.",
  reauth_required: "Sign in again to make this change.",
  password_too_weak_alone: "Without a second factor your password must stand alone: set a longer one.",
};

/** The text a refusal shows the person. */
export function refusalText(r: Refusal): string {
  if (r.code === "password_too_short" && r.min_length) {
    return `That password is too short: use at least ${r.min_length} characters.`;
  }
  return messages[r.code] ?? "That did not work. Try again.";
}

/** The minimum password length the policy applies (NIST SP 800-63B-4): 15 as
 *  the only factor, 8 with an authenticator. */
export function minLength(info: { min_length: number; min_length_with_mfa: number }, withMFA: boolean): number {
  return withMFA ? info.min_length_with_mfa : info.min_length;
}
