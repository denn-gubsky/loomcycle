// The bot's own token for a Teams file link (RFC HF §3.7, decision D5).
//
// Until 2027-03-01 a downloadUrl carries a `tempauth` query and needs no
// credential. From then on Teams drops tempauth and the bot sends its own
// access token, for `https://<the link's host>/.default`, with each download.
// Microsoft refuses one acquired with a client secret (HTTP 401): it must come
// from a certificate, a federated identity credential (FIC) or a managed
// identity. The Teams SDK acquires tokens from a managed identity and a FIC
// for any scope, but has no certificate credential, so the certificate grant
// (a client assertion, RFC 7523) is made here with node:crypto.
//
// The token is never logged and never in an error: errors carry a reason and,
// for an answer from the token endpoint, its status.

import { constants, createPrivateKey, randomUUID, sign, X509Certificate, type KeyObject } from "node:crypto";

import type { Credentials } from "@microsoft/teams.api";

/** FileTokenSource acquires a token for scope (`https://<host>/.default`). */
export type FileTokenSource = (scope: string) => Promise<string>;

/** Which credential the file token comes from. "secret" and "none" mean
 *  there is no file token: a link without tempauth is refused. */
export type FileCredentialKind = "certificate" | "managed-identity" | "federated" | "token" | "secret" | "none";

/** A token that could not be acquired. Never carries the token, the key or
 *  the endpoint's answer: only why, and the endpoint's status. */
export class FileTokenError extends Error {
  constructor(
    readonly reason: "none" | "status" | "response" | "failed",
    readonly detail: { status?: number } = {},
  ) {
    super(`teams file token not acquired: ${reason}`);
    this.name = "FileTokenError";
  }
}

/** A certificate credential: the RSA private key it signs with and the
 *  certificate's SHA-256 thumbprint, base64url, as Entra matches it. */
export interface FileCertificate {
  key: KeyObject;
  x5tS256: string;
}

/** parseCertificate reads a PEM holding an unencrypted RSA private key and
 *  the certificate uploaded to the bot's Entra app (the leaf first when a
 *  chain follows). Its errors are fixed text: Node quotes a prefix of bad
 *  input in its own messages, and the input here is a private key. */
export function parseCertificate(pem: string): FileCertificate {
  let key: KeyObject;
  let cert: X509Certificate;
  try {
    key = createPrivateKey(pem);
  } catch {
    throw new Error("holds no readable unencrypted private key");
  }
  try {
    cert = new X509Certificate(pem);
  } catch {
    throw new Error("holds no readable certificate");
  }
  if (key.asymmetricKeyType !== "rsa") throw new Error("the private key is not RSA (Entra takes RSA certificates)");
  if (!cert.checkPrivateKey(key)) throw new Error("the private key does not belong to the certificate");
  const x5tS256 = Buffer.from(cert.fingerprint256.replaceAll(":", ""), "hex").toString("base64url");
  return { key, x5tS256 };
}

export interface CertificateTokenOptions {
  clientId: string;
  tenantId: string;
  certificate: FileCertificate;
  /** The cloud's login endpoint (the SDK's cloud environment). */
  loginEndpoint?: string;
  fetch?: typeof fetch;
  /** Clock, milliseconds (tests). */
  now?: () => number;
}

const ASSERTION_TYPE = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer";
/** An assertion lives ten minutes, as MSAL's does. */
const ASSERTION_SECONDS = 600;
/** A held token is renewed this long before it expires. */
const RENEW_MS = 5 * 60_000;
const ACQUIRE_MS = 10_000;
/** Scopes held at once. Each allowed host is one scope, and a tenant has few;
 *  the bound keeps a stream of odd hosts from growing the map. */
const MAX_HELD = 32;

/** certificateTokenSource acquires tokens with the client-credentials grant,
 *  authenticated by a JWT the certificate signs (PS256, x5t#S256 header: what
 *  MSAL sends). Tokens are held per scope until shortly before they expire,
 *  and concurrent requests for one scope share one acquisition. */
export function certificateTokenSource(o: CertificateTokenOptions): FileTokenSource {
  const doFetch = o.fetch ?? globalThis.fetch;
  const now = o.now ?? Date.now;
  const endpoint = `${(o.loginEndpoint ?? "https://login.microsoftonline.com").replace(/\/+$/, "")}/${encodeURIComponent(o.tenantId)}/oauth2/v2.0/token`;
  const held = new Map<string, { value: string; expiresAt: number }>();
  const pending = new Map<string, Promise<{ value: string; expiresAt: number }>>();

  async function acquire(scope: string): Promise<{ value: string; expiresAt: number }> {
    const body = new URLSearchParams({
      client_id: o.clientId,
      scope,
      grant_type: "client_credentials",
      client_assertion_type: ASSERTION_TYPE,
      client_assertion: assertion(o.clientId, endpoint, o.certificate, Math.floor(now() / 1000)),
    });
    let res: Response;
    try {
      res = await doFetch(endpoint, {
        method: "POST",
        headers: { "content-type": "application/x-www-form-urlencoded" },
        body,
        redirect: "error",
        signal: AbortSignal.timeout(ACQUIRE_MS),
      });
    } catch {
      throw new FileTokenError("failed");
    }
    if (!res.ok) {
      await res.body?.cancel().catch(() => undefined);
      throw new FileTokenError("status", { status: res.status });
    }
    const got = (await res.json().catch(() => undefined)) as { access_token?: unknown; expires_in?: unknown } | undefined;
    if (!got || typeof got.access_token !== "string" || !got.access_token) throw new FileTokenError("response");
    const seconds = typeof got.expires_in === "number" ? got.expires_in : Number(got.expires_in);
    return { value: got.access_token, expiresAt: now() + (Number.isFinite(seconds) && seconds > 0 ? seconds * 1000 : 0) };
  }

  return async (scope) => {
    const h = held.get(scope);
    if (h && h.expiresAt - RENEW_MS > now()) return h.value;
    let p = pending.get(scope);
    if (!p) {
      p = acquire(scope).finally(() => pending.delete(scope));
      pending.set(scope, p);
    }
    const got = await p;
    if (!held.has(scope) && held.size >= MAX_HELD) held.delete(held.keys().next().value as string);
    held.set(scope, got);
    return got.value;
  };
}

/** assertion is the signed client assertion for one token request. */
function assertion(clientId: string, audience: string, cert: FileCertificate, iat: number): string {
  const b64 = (v: unknown) => Buffer.from(JSON.stringify(v)).toString("base64url");
  const head = b64({ alg: "PS256", typ: "JWT", "x5t#S256": cert.x5tS256 });
  const claims = b64({ aud: audience, iss: clientId, sub: clientId, jti: randomUUID(), nbf: iat, iat, exp: iat + ASSERTION_SECONDS });
  const input = `${head}.${claims}`;
  const sig = sign("sha256", Buffer.from(input), {
    key: cert.key,
    padding: constants.RSA_PKCS1_PSS_PADDING,
    saltLength: constants.RSA_PSS_SALTLEN_DIGEST,
  });
  return `${input}.${sig.toString("base64url")}`;
}

/** The part of the SDK app's token source the file token uses. */
export interface AppTokenSource {
  getAppToken(scope?: string, tenantId?: string): Promise<{ toString(): string } | null>;
}

/** credentialKind names the SDK's credential, as the app resolved it from its
 *  options and CLIENT_ID / CLIENT_SECRET / MANAGED_IDENTITY_CLIENT_ID. */
export function credentialKind(c: Credentials | undefined): FileCredentialKind {
  if (!c) return "none";
  if ("clientSecret" in c) return "secret";
  if ("token" in c) return "token";
  if ("managedIdentityType" in c) return "federated";
  return "managed-identity";
}

/** chooseFileToken picks where file tokens come from: the certificate when
 *  one is configured (whatever the bot's own traffic uses), else the SDK's
 *  credential when it is not a client secret. With only a secret, or none,
 *  there is no source. */
export function chooseFileToken(o: {
  certificate?: CertificateTokenOptions;
  credentials: Credentials | undefined;
  app: AppTokenSource;
}): { kind: FileCredentialKind; source?: FileTokenSource } {
  if (o.certificate) return { kind: "certificate", source: certificateTokenSource(o.certificate) };
  const kind = credentialKind(o.credentials);
  if (kind === "secret" || kind === "none") return { kind };
  return {
    kind,
    source: async (scope) => {
      let t: { toString(): string } | null;
      try {
        t = await o.app.getAppToken(scope);
      } catch {
        throw new FileTokenError("failed");
      }
      const value = t?.toString();
      if (!value) throw new FileTokenError("none");
      return value;
    },
  };
}

/** The start-up warning for files or recordings on with only a client secret. */
export const SECRET_ONLY_WARNING = "files and recordings will fail from 2027-03-01: configure a certificate or federated credential (RFC HF)";
