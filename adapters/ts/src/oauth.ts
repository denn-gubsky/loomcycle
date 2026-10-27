// App sign-in (RFC FL) — the client side of loomcycle's OAuth 2.0
// authorization server for its own apps.
//
// The flow is the authorization code grant with PKCE S256 (RFC 6749, RFC 7636,
// RFC 8252 for native apps): the app sends the person to loomcycle's login
// page, they sign in through whatever gates the operator configured, loomcycle
// redirects back with a single-use code, and the app exchanges the code (with
// the PKCE verifier only it holds) for a short-lived access token and a
// rotating refresh token. The app never sees a password or the gate config.
//
// Everything here runs on Web Crypto + fetch, so the same code serves a
// browser SPA, a Chrome extension and a Tauri or Node app. Node 18 has no
// global `crypto`: pass `crypto: require("node:crypto").webcrypto`.
//
// What this module never does: put a token or the verifier in a URL, send a
// bearer or a cookie to the token endpoint, or log anything.

import { OAuthError } from "./errors.js";
import type {
  AuthorizeRequest,
  AuthorizeResponseParams,
  BeginAuthorizeOptions,
  CompleteAuthorizeOptions,
  OAuthServerMetadata,
  OAuthTokenResponse,
  OAuthTokenSet,
  RefreshTokenOptions,
  RevokeTokenOptions,
  TokenProvider,
} from "./types.js";

/** The server's bound on `state` (oauthMaxStateLen). */
const MAX_STATE_LEN = 1024;

/** Options for {@link LoomcycleOAuth}. */
export interface LoomcycleOAuthOptions {
  /** loomcycle's address as the person's browser reaches it, e.g.
   *  `https://loom.example.com`. Defaults to http://127.0.0.1:8787. */
  baseUrl?: string;
  /** Custom fetch (tests, Tauri's HTTP plugin); defaults to global `fetch`. */
  fetch?: typeof fetch;
  /** Web Crypto; defaults to `globalThis.crypto` (browsers, Node 19+). On
   *  Node 18 pass `require("node:crypto").webcrypto`. */
  crypto?: Crypto;
  /** The server's issuer identifier, when the app knows it. A redirect's
   *  `iss` is compared with it; without it, the issuer is discovered from
   *  the server's metadata the first time a redirect carries `iss`. */
  issuer?: string;
}

/**
 * The app sign-in helper. Construct one per loomcycle server.
 *
 * ```ts
 * const oauth = new LoomcycleOAuth({ baseUrl: "https://loom.example.com" });
 * // 1. Start: keep verifier + state in session storage, then navigate.
 * const req = await oauth.beginAuthorize({ clientId: "loomboard", redirectUri });
 * sessionStorage.setItem("lc_pkce", JSON.stringify({ verifier: req.verifier, state: req.state }));
 * location.assign(req.url);
 * // 2. Back on redirectUri:
 * const saved = JSON.parse(sessionStorage.getItem("lc_pkce")!);
 * const tokens = await oauth.completeAuthorize({
 *   ...parseAuthorizeResponse(location.href),
 *   expectedState: saved.state, verifier: saved.verifier, clientId: "loomboard", redirectUri,
 * });
 * ```
 */
export class LoomcycleOAuth {
  private readonly baseUrl: string;
  private readonly fetchImpl: typeof fetch;
  private readonly cryptoImpl?: Crypto;
  private readonly issuer?: string;
  private metadata?: Promise<OAuthServerMetadata>;

  constructor(opts: LoomcycleOAuthOptions = {}) {
    this.baseUrl = (opts.baseUrl ?? "http://127.0.0.1:8787").replace(/\/+$/, "");
    // Called through a closure, never as this.fetchImpl(...): a browser's
    // fetch called with any other receiver than the window throws.
    const f = opts.fetch;
    this.fetchImpl = f ? (input, init) => f(input, init) : (input, init) => fetch(input, init);
    this.cryptoImpl = opts.crypto;
    this.issuer = opts.issuer?.replace(/\/+$/, "");
  }

  /** Start a sign-in: a fresh PKCE verifier, its S256 challenge, a state,
   *  and the authorize URL to send the person to. Nothing is sent to the
   *  server. */
  async beginAuthorize(opts: BeginAuthorizeOptions): Promise<AuthorizeRequest> {
    if (!opts.clientId) throw new TypeError("beginAuthorize: clientId is required");
    if (!opts.redirectUri) throw new TypeError("beginAuthorize: redirectUri is required");
    const state = opts.state ?? randomToken(this.webCrypto());
    if (state.length < 1 || state.length > MAX_STATE_LEN) {
      throw new TypeError(`beginAuthorize: state must be 1 to ${MAX_STATE_LEN} characters`);
    }
    const verifier = randomToken(this.webCrypto());
    const challenge = await pkceChallenge(verifier, this.webCrypto());
    const q = new URLSearchParams({
      response_type: "code",
      client_id: opts.clientId,
      redirect_uri: opts.redirectUri,
      code_challenge: challenge,
      code_challenge_method: "S256",
      state,
    });
    return { url: `${this.baseUrl}/v1/_oauth/authorize?${q.toString()}`, verifier, state };
  }

  /** Finish a sign-in from the redirect's parameters (see
   *  {@link parseAuthorizeResponse}). In order: the `state` must be the one
   *  this sign-in sent, an `iss` must name this server (RFC 9207), an
   *  `error` is thrown as an {@link OAuthError}, and only then is the code
   *  exchanged. */
  async completeAuthorize(opts: CompleteAuthorizeOptions): Promise<OAuthTokenSet> {
    if (!opts.expectedState || opts.state === undefined || !constantTimeEqual(opts.state, opts.expectedState)) {
      throw new OAuthError("state_mismatch", {
        errorDescription: "the redirect's state is not the one this sign-in sent",
      });
    }
    if (opts.iss !== undefined) {
      const issuer = this.issuer ?? (await this.discover({ signal: opts.signal })).issuer;
      if (opts.iss !== issuer) {
        throw new OAuthError("issuer_mismatch", {
          errorDescription: "the redirect names another authorization server",
        });
      }
    }
    if (opts.error) {
      throw new OAuthError(opts.error, { errorDescription: opts.error_description });
    }
    if (!opts.code) {
      throw new OAuthError("invalid_response", { errorDescription: "the redirect carries no code" });
    }
    return this.tokenRequest(
      {
        grant_type: "authorization_code",
        code: opts.code,
        redirect_uri: opts.redirectUri,
        code_verifier: opts.verifier,
        client_id: opts.clientId,
      },
      opts.signal,
    );
  }

  /** Trade a refresh token for a new token set. The refresh token is spent:
   *  persist the returned one before anything else uses the old. A spent
   *  token presented again ends the whole sign-in. */
  async refresh(opts: RefreshTokenOptions): Promise<OAuthTokenSet> {
    return this.tokenRequest(
      { grant_type: "refresh_token", refresh_token: opts.refreshToken, client_id: opts.clientId },
      opts.signal,
    );
  }

  /** Sign out (RFC 7009): a refresh token ends the whole sign-in, an access
   *  token ends that token. The server answers the same for a token it does
   *  not know, so this resolves unless the request itself failed. */
  async revoke(opts: RevokeTokenOptions): Promise<void> {
    const resp = await this.postForm("/v1/_oauth/revoke", { token: opts.token, client_id: opts.clientId }, opts.signal);
    if (!resp.ok) throw await oauthErrorFrom(resp);
    await resp.body?.cancel().catch(() => undefined);
  }

  /** The server's authorization server metadata (RFC 8414). Cached after the
   *  first success. Fails with a 404 when the server does not know its public
   *  address (`LOOMCYCLE_PUBLIC_URL` unset). */
  async discover(opts?: { signal?: AbortSignal }): Promise<OAuthServerMetadata> {
    if (!this.metadata) {
      const p = this.fetchMetadata(opts?.signal);
      this.metadata = p;
      p.catch(() => {
        if (this.metadata === p) this.metadata = undefined;
      });
    }
    return this.metadata;
  }

  private async fetchMetadata(signal?: AbortSignal): Promise<OAuthServerMetadata> {
    const resp = await this.fetchImpl(`${this.baseUrl}/.well-known/oauth-authorization-server`, {
      method: "GET",
      headers: { Accept: "application/json" },
      credentials: "omit",
      signal,
    });
    if (!resp.ok) throw await oauthErrorFrom(resp);
    const md = (await resp.json().catch(() => undefined)) as Partial<OAuthServerMetadata> | undefined;
    if (!md || typeof md.issuer !== "string" || !md.issuer) {
      throw new OAuthError("invalid_response", { status: resp.status, errorDescription: "metadata without an issuer" });
    }
    return md as OAuthServerMetadata;
  }

  private async tokenRequest(params: Record<string, string>, signal?: AbortSignal): Promise<OAuthTokenSet> {
    const resp = await this.postForm("/v1/_oauth/token", params, signal);
    if (!resp.ok) throw await oauthErrorFrom(resp);
    // The body holds tokens: on a malformed one, say so without echoing it.
    const t = (await resp.json().catch(() => undefined)) as Partial<OAuthTokenResponse> | undefined;
    if (
      !t ||
      typeof t.access_token !== "string" || !t.access_token ||
      typeof t.refresh_token !== "string" || !t.refresh_token ||
      typeof t.expires_in !== "number" || !Number.isFinite(t.expires_in) ||
      typeof t.token_type !== "string" || t.token_type.toLowerCase() !== "bearer"
    ) {
      throw new OAuthError("invalid_response", { status: resp.status, errorDescription: "not a bearer token response" });
    }
    return {
      access_token: t.access_token,
      token_type: t.token_type,
      expires_in: t.expires_in,
      refresh_token: t.refresh_token,
      expires_at: Date.now() + t.expires_in * 1000,
    };
  }

  /** POST a form: parameters in the body, never the URL; no bearer and no
   *  cookie (the code or the refresh token is the credential); only
   *  CORS-safelisted headers, so a browser sends it without a preflight. */
  private postForm(path: string, params: Record<string, string>, signal?: AbortSignal): Promise<Response> {
    return this.fetchImpl(this.baseUrl + path, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded", Accept: "application/json" },
      body: new URLSearchParams(params).toString(),
      credentials: "omit",
      signal,
    });
  }

  private webCrypto(): Crypto {
    const c = this.cryptoImpl ?? (globalThis as { crypto?: Crypto }).crypto;
    if (!c?.subtle || typeof c.getRandomValues !== "function") {
      throw new Error(
        "loomcycle sign-in: no Web Crypto implementation; pass opts.crypto (on Node 18: require('node:crypto').webcrypto)",
      );
    }
    return c;
  }
}

/** Read the authorization response parameters from the redirect URL (its
 *  query). Pass the result to {@link LoomcycleOAuth.completeAuthorize}. */
export function parseAuthorizeResponse(url: string | URL): AuthorizeResponseParams {
  const q = (typeof url === "string" ? new URL(url) : url).searchParams;
  const out: AuthorizeResponseParams = {};
  for (const k of ["code", "state", "iss", "error", "error_description"] as const) {
    const v = q.get(k);
    if (v !== null) out[k] = v;
  }
  return out;
}

/** The PKCE S256 code challenge of a verifier (RFC 7636 §4.2):
 *  BASE64URL(SHA-256(ASCII(verifier))). */
export async function pkceChallenge(verifier: string, cryptoImpl?: Crypto): Promise<string> {
  const c = cryptoImpl ?? (globalThis as { crypto?: Crypto }).crypto;
  if (!c?.subtle) {
    throw new Error("pkceChallenge: no Web Crypto implementation; pass cryptoImpl");
  }
  const digest = await c.subtle.digest("SHA-256", new TextEncoder().encode(verifier));
  return base64url(new Uint8Array(digest));
}

/** Options for {@link OAuthTokenProvider}. */
export interface OAuthTokenProviderOptions {
  /** The helper for the server that issued the tokens. */
  oauth: LoomcycleOAuth;
  clientId: string;
  /** The current token set (from completeAuthorize, or persisted). */
  tokens: OAuthTokenSet;
  /** Called with every new token set, before the token is used. Persist it:
   *  the refresh token it replaced is spent. A throw fails the request that
   *  caused the refresh; the new set is kept in memory regardless. */
  onTokens?: (tokens: OAuthTokenSet) => void | Promise<void>;
  /** Called once when the sign-in ends: the server refused the refresh token
   *  (revoked, expired, reused) or {@link OAuthTokenProvider.signOut} ran.
   *  Clear what you persisted and send the person to sign in again. */
  onSignedOut?: (reason: OAuthError) => void;
  /** Refresh this long before the access token expires (default 60 s). */
  refreshSkewMs?: number;
}

/**
 * A {@link TokenProvider} over an app sign-in: hands out the access token,
 * refreshes it before it expires and once after a 401, and shares one
 * refresh between concurrent requests (two refreshes of the same refresh
 * token would end the sign-in, since the server takes the second as a
 * replay).
 *
 * One provider per sign-in: two providers — or two browser tabs — refreshing
 * the same refresh token end it the same way.
 */
export class OAuthTokenProvider implements TokenProvider {
  private current?: OAuthTokenSet;
  private inflight?: Promise<OAuthTokenSet>;
  private ended?: OAuthError;
  private readonly skew: number;

  constructor(private readonly opts: OAuthTokenProviderOptions) {
    this.current = opts.tokens;
    this.skew = opts.refreshSkewMs ?? 60_000;
  }

  /** The current token set; undefined once signed out. */
  get tokens(): OAuthTokenSet | undefined {
    return this.current;
  }

  async getToken(): Promise<string | undefined> {
    const cur = this.current;
    if (!cur) throw this.ended ?? new OAuthError("signed_out");
    if (Date.now() < cur.expires_at - this.skew) return cur.access_token;
    try {
      return (await this.refreshOnce()).access_token;
    } catch (err) {
      // A refresh ahead of expiry that failed for a transient reason leaves
      // a token that still works; past expiry there is none to send.
      if (this.current === cur && Date.now() < cur.expires_at && !isSignInOver(err)) return cur.access_token;
      throw err;
    }
  }

  async onUnauthorized(rejected: string | undefined): Promise<string | undefined> {
    const cur = this.current;
    if (!cur) return undefined;
    // Another request already replaced the token this one was refused with.
    if (rejected !== cur.access_token) return cur.access_token;
    try {
      return (await this.refreshOnce()).access_token;
    } catch {
      return undefined;
    }
  }

  /** Refresh now, sharing a refresh already under way. */
  refresh(): Promise<OAuthTokenSet> {
    return this.refreshOnce();
  }

  /** End the sign-in: revoke the refresh token on the server (which also
   *  ends the access tokens it issued) and forget the tokens. Resolves even
   *  if the server could not be reached; the tokens are forgotten either
   *  way. */
  async signOut(opts?: { signal?: AbortSignal }): Promise<void> {
    const cur = this.current;
    this.end(new OAuthError("signed_out"));
    if (!cur) return;
    try {
      await this.opts.oauth.revoke({ token: cur.refresh_token, clientId: this.opts.clientId, signal: opts?.signal });
    } catch {
      // Forgotten locally; the refresh token lapses on its own.
    }
  }

  private refreshOnce(): Promise<OAuthTokenSet> {
    if (!this.inflight) {
      const p = this.doRefresh();
      this.inflight = p;
      const clear = () => {
        if (this.inflight === p) this.inflight = undefined;
      };
      p.then(clear, clear);
    }
    return this.inflight;
  }

  private async doRefresh(): Promise<OAuthTokenSet> {
    const cur = this.current;
    if (!cur) throw this.ended ?? new OAuthError("signed_out");
    let next: OAuthTokenSet;
    try {
      next = await this.opts.oauth.refresh({ refreshToken: cur.refresh_token, clientId: this.opts.clientId });
    } catch (err) {
      if (isSignInOver(err) && this.current === cur) this.end(err as OAuthError);
      throw err;
    }
    if (this.current !== cur) {
      // Signed out while the refresh was in flight: do not resurrect it.
      throw this.ended ?? new OAuthError("signed_out");
    }
    this.current = next;
    await this.opts.onTokens?.(next);
    return next;
  }

  private end(reason: OAuthError): void {
    const was = this.current;
    this.current = undefined;
    this.ended = reason;
    if (was) this.opts.onSignedOut?.(reason);
  }
}

/** A refresh refused for good: the token is spent, revoked, expired, or its
 *  client is gone. */
function isSignInOver(err: unknown): boolean {
  return err instanceof OAuthError && (err.code === "invalid_grant" || err.code === "invalid_client");
}

/** An OAuthError from a non-2xx answer: an RFC 6749 §5.2 body
 *  (`error`, `error_description`) or loomcycle's JSON error (`code`). */
async function oauthErrorFrom(resp: Response): Promise<OAuthError> {
  const text = await resp.text().catch(() => "");
  let code = "";
  let desc: string | undefined;
  try {
    const b = JSON.parse(text) as Record<string, unknown>;
    if (typeof b.error === "string" && /^[a-z_]+$/.test(b.error)) {
      code = b.error;
      if (typeof b.error_description === "string" && b.error_description) desc = b.error_description;
    } else if (typeof b.code === "string" && b.code) {
      code = b.code;
      if (typeof b.error === "string" && b.error) desc = b.error;
    }
  } catch {
    // not JSON
  }
  if (!code) code = resp.status === 429 ? "temporarily_unavailable" : "server_error";
  return new OAuthError(code, { status: resp.status, bodyText: text.slice(0, 1024), errorDescription: desc });
}

/** 32 random bytes, base64url: a 43-character PKCE verifier (RFC 7636 §4.1)
 *  or state. */
function randomToken(c: Crypto): string {
  const b = new Uint8Array(32);
  c.getRandomValues(b);
  return base64url(b);
}

function base64url(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** Compares two strings in time that depends only on their lengths. */
function constantTimeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}
