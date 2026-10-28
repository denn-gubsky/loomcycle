// Passkeys (WebAuthn) in the browser (RFC FA P6). The server hands a page an
// offer — {ceremony, publicKey} — whose binary fields are base64url strings;
// this module turns it into what navigator.credentials expects, runs the
// ceremony, and turns the browser's PublicKeyCredential back into JSON the
// server's WebAuthn library parses. The ceremony id goes back with it: the
// server answers each ceremony once.

import { type Refusal, type Result, call } from "./passwordGate";

/** A ceremony the server began. */
export interface PasskeyOffer {
  ceremony: string;
  publicKey: Record<string, unknown>;
}

/** What a page posts back for an offer. */
export interface PasskeyAnswer {
  ceremony: string;
  credential: unknown;
}

export function b64urlToBytes(s: string): Uint8Array {
  const pad = "=".repeat((4 - (s.length % 4)) % 4);
  const bin = atob(s.replace(/-/g, "+").replace(/_/g, "/") + pad);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

export function bytesToB64url(b: ArrayBuffer | Uint8Array): string {
  const bytes = b instanceof Uint8Array ? b : new Uint8Array(b);
  let bin = "";
  for (const x of bytes) bin += String.fromCharCode(x);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

type Descriptor = { id: string; type: string; transports?: string[] };

function descriptors(list: unknown): PublicKeyCredentialDescriptor[] | undefined {
  if (!Array.isArray(list)) return undefined;
  return (list as Descriptor[]).map((d) => ({
    type: "public-key",
    id: b64urlToBytes(d.id) as BufferSource,
    transports: d.transports as AuthenticatorTransport[] | undefined,
  }));
}

/** The creation options of an offer, binary fields decoded. */
export function creationOptions(o: PasskeyOffer): PublicKeyCredentialCreationOptions {
  const pk = o.publicKey as Record<string, unknown> & { user: { id: string; name: string; displayName: string } };
  return {
    ...(pk as unknown as PublicKeyCredentialCreationOptions),
    challenge: b64urlToBytes(pk.challenge as string) as BufferSource,
    user: { ...pk.user, id: b64urlToBytes(pk.user.id) as BufferSource },
    excludeCredentials: descriptors(pk.excludeCredentials),
  };
}

/** The request options of an offer, binary fields decoded. */
export function requestOptions(o: PasskeyOffer): PublicKeyCredentialRequestOptions {
  const pk = o.publicKey;
  return {
    ...(pk as unknown as PublicKeyCredentialRequestOptions),
    challenge: b64urlToBytes(pk.challenge as string) as BufferSource,
    allowCredentials: descriptors(pk.allowCredentials),
  };
}

/** The JSON form of a PublicKeyCredential the server parses. */
export function credentialJSON(c: PublicKeyCredential): Record<string, unknown> {
  const r = c.response as AuthenticatorAttestationResponse & AuthenticatorAssertionResponse;
  const response: Record<string, unknown> = { clientDataJSON: bytesToB64url(r.clientDataJSON) };
  if ("attestationObject" in r && r.attestationObject) {
    response.attestationObject = bytesToB64url(r.attestationObject);
    if (typeof r.getTransports === "function") response.transports = r.getTransports();
  } else {
    response.authenticatorData = bytesToB64url(r.authenticatorData);
    response.signature = bytesToB64url(r.signature);
    if (r.userHandle) response.userHandle = bytesToB64url(r.userHandle);
  }
  return {
    id: c.id,
    rawId: bytesToB64url(c.rawId),
    type: c.type,
    authenticatorAttachment: c.authenticatorAttachment ?? undefined,
    response,
    clientExtensionResults: c.getClientExtensionResults(),
  };
}

/** Whether this browser can use passkeys at all. */
export function passkeysSupported(win: Window = window): boolean {
  return "PublicKeyCredential" in win && !!win.navigator.credentials;
}

/** Runs a registration offer; null when the person cancelled. */
export async function createPasskey(o: PasskeyOffer): Promise<PasskeyAnswer | null> {
  const c = (await navigator.credentials.create({ publicKey: creationOptions(o) })) as PublicKeyCredential | null;
  return c ? { ceremony: o.ceremony, credential: credentialJSON(c) } : null;
}

/** Runs an assertion offer; null when the person cancelled. */
export async function assertPasskey(o: PasskeyOffer): Promise<PasskeyAnswer | null> {
  const c = (await navigator.credentials.get({ publicKey: requestOptions(o) })) as PublicKeyCredential | null;
  return c ? { ceremony: o.ceremony, credential: credentialJSON(c) } : null;
}

export interface PasskeyInfo {
  id: string;
  name: string;
  created_at: string;
  last_used_at?: string;
  discoverable: boolean;
  flagged: boolean;
  flag_reason?: string;
}

/** The re-authentication a passkey change carries: the current password (and
 *  the code or passkey a sign-in would ask for) on a password account; a
 *  passkey answer on a passkey account. */
export interface StepUp {
  current_password?: string;
  otp?: string;
  webauthn?: PasskeyAnswer;
}

export const listPasskeys = (f: typeof fetch = fetch) =>
  call<{ available: boolean; passwordless?: boolean; passkeys: PasskeyInfo[] }>(f, "GET", "/v1/_auth/passkeys");

export const registerBegin = (req: StepUp, f: typeof fetch = fetch) =>
  call<PasskeyOffer>(f, "POST", "/v1/_auth/passkeys/register/begin", req);

export const registerFinish = (req: PasskeyAnswer & { name: string }, f: typeof fetch = fetch) =>
  call<{ id: string; name: string }>(f, "POST", "/v1/_auth/passkeys/register/finish", req);

export const renamePasskey = (id: string, name: string, f: typeof fetch = fetch) =>
  call<object>(f, "PATCH", "/v1/_auth/passkeys/" + encodeURIComponent(id), { name });

export const removePasskey = (id: string, req: StepUp, f: typeof fetch = fetch) =>
  call<object>(f, "DELETE", "/v1/_auth/passkeys/" + encodeURIComponent(id), req);

export const passkeyLoginBegin = (gate: string, f: typeof fetch = fetch) =>
  call<PasskeyOffer>(f, "POST", "/v1/_auth/passkey/" + encodeURIComponent(gate) + "/begin", {});

export const passkeyLoginComplete = (gate: string, a: PasskeyAnswer, f: typeof fetch = fetch) =>
  call<object>(f, "POST", "/v1/_auth/complete/" + encodeURIComponent(gate), a);

/** Runs a request that may come back "second_factor_required" with a passkey
 *  offer: the person confirms with the passkey and the request is sent again
 *  with the answer. Other results are returned as they are; a cancelled
 *  passkey is the refusal "passkey_cancelled". `assert` is for tests. */
export async function withPasskeyStepUp<T>(
  send: (webauthn?: PasskeyAnswer) => Promise<Result<T>>,
  assert: (o: PasskeyOffer) => Promise<PasskeyAnswer | null> = assertPasskey,
): Promise<Result<T>> {
  const first = await send();
  if (first.ok || first.refusal.code !== "second_factor_required" || !first.refusal.webauthn) return first;
  let answer: PasskeyAnswer | null = null;
  try {
    answer = await assert(first.refusal.webauthn);
  } catch {
    answer = null;
  }
  if (!answer) return { ok: false, refusal: { code: "passkey_cancelled" } as Refusal };
  return send(answer);
}
