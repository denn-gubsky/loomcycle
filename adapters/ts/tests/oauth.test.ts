/**
 * App sign-in (RFC FL): the PKCE authorization-code flow, refresh, revoke,
 * discovery, the token provider, and the client's use of it.
 *
 * Every token here is a made-up fixture string; nothing is a real credential.
 */

import { afterEach, describe, expect, it, vi, type Mock } from "vitest";
import {
  LoomcycleClient,
  LoomcycleOAuth,
  OAuthError,
  OAuthTokenProvider,
  AuthError,
  parseAuthorizeResponse,
  pkceChallenge,
  type OAuthTokenSet,
  type WebSocketCtor,
  type WebSocketLike,
} from "../src/index.js";

const BASE = "https://loom.test";
const REDIRECT = "https://app.test/callback";

type Factory = (req: Request) => Response | Promise<Response>;

/** A fetch mock answering from a queue, recording each request. */
function mockFetch(responses: Factory[]): Mock {
  const queue = [...responses];
  return vi.fn(async (url: string, init?: RequestInit) => {
    const factory = queue.shift();
    if (!factory) throw new Error(`fetch mock: nothing queued for ${init?.method ?? "GET"} ${url}`);
    return factory(new Request(url, init));
  });
}

function json(body: unknown, status = 200): Factory {
  return () => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

function tokenBody(n: number, expiresIn = 3600) {
  return { access_token: `at-${n}`, token_type: "Bearer", expires_in: expiresIn, refresh_token: `lcrt_rt-${n}` };
}

const METADATA = {
  issuer: BASE,
  authorization_endpoint: `${BASE}/v1/_oauth/authorize`,
  token_endpoint: `${BASE}/v1/_oauth/token`,
  revocation_endpoint: `${BASE}/v1/_oauth/revoke`,
  response_types_supported: ["code"],
  grant_types_supported: ["authorization_code", "refresh_token"],
  code_challenge_methods_supported: ["S256"],
  token_endpoint_auth_methods_supported: ["none"],
  revocation_endpoint_auth_methods_supported: ["none"],
  authorization_response_iss_parameter_supported: true,
};

/** The method, URL, headers and form of one recorded call. */
function call(m: Mock, i: number) {
  const [url, init] = m.mock.calls[i]! as [string, RequestInit];
  const headers = new Headers(init.headers);
  const form = typeof init.body === "string" ? new URLSearchParams(init.body) : undefined;
  return { url, init, headers, form };
}

function formObject(form: URLSearchParams | undefined): Record<string, string> {
  return Object.fromEntries([...(form ?? new URLSearchParams()).entries()]);
}

afterEach(() => {
  vi.useRealTimers();
});

describe("pkceChallenge", () => {
  it("matches the RFC 7636 Appendix B test vector", async () => {
    await expect(pkceChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")).resolves.toBe(
      "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
    );
  });
});

describe("beginAuthorize", () => {
  it("builds the authorize URL with an S256 challenge of the returned verifier", async () => {
    const fetchMock = mockFetch([]);
    const oauth = new LoomcycleOAuth({ baseUrl: `${BASE}/`, fetch: fetchMock as unknown as typeof fetch });
    const req = await oauth.beginAuthorize({ clientId: "loomboard", redirectUri: REDIRECT });

    const u = new URL(req.url);
    expect(`${u.origin}${u.pathname}`).toBe(`${BASE}/v1/_oauth/authorize`);
    expect(Object.fromEntries(u.searchParams.entries())).toEqual({
      response_type: "code",
      client_id: "loomboard",
      redirect_uri: REDIRECT,
      code_challenge: await pkceChallenge(req.verifier),
      code_challenge_method: "S256",
      state: req.state,
    });
    // A 43-character verifier from the PKCE alphabet, kept out of the URL.
    expect(req.verifier).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(req.url).not.toContain(req.verifier);
    expect(req.state).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("gives every sign-in a fresh verifier and state", async () => {
    const oauth = new LoomcycleOAuth({ baseUrl: BASE });
    const a = await oauth.beginAuthorize({ clientId: "c", redirectUri: REDIRECT });
    const b = await oauth.beginAuthorize({ clientId: "c", redirectUri: REDIRECT });
    expect(a.verifier).not.toBe(b.verifier);
    expect(a.state).not.toBe(b.state);
  });

  it("round-trips a caller's state and refuses one the server would not take", async () => {
    const oauth = new LoomcycleOAuth({ baseUrl: BASE });
    const req = await oauth.beginAuthorize({ clientId: "c", redirectUri: REDIRECT, state: "s 1/2" });
    expect(req.state).toBe("s 1/2");
    expect(new URL(req.url).searchParams.get("state")).toBe("s 1/2");
    await expect(oauth.beginAuthorize({ clientId: "c", redirectUri: REDIRECT, state: "" })).rejects.toThrow(TypeError);
    await expect(
      oauth.beginAuthorize({ clientId: "c", redirectUri: REDIRECT, state: "x".repeat(1025) }),
    ).rejects.toThrow(TypeError);
  });
});

describe("completeAuthorize", () => {
  const base = { expectedState: "st-1", verifier: "v".repeat(43), clientId: "loomboard", redirectUri: REDIRECT };

  it("exchanges the code as a form, with no bearer, cookie or URL parameter", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(1_000_000);
    const fetchMock = mockFetch([json(tokenBody(1, 600))]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch, issuer: BASE });

    const tokens = await oauth.completeAuthorize({ ...base, code: "code-1", state: "st-1", iss: BASE });

    expect(tokens).toEqual({ ...tokenBody(1, 600), expires_at: 1_000_000 + 600_000 });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const c = call(fetchMock, 0);
    expect(c.url).toBe(`${BASE}/v1/_oauth/token`);
    expect(c.init.method).toBe("POST");
    expect(c.init.credentials).toBe("omit");
    expect(c.headers.get("content-type")).toBe("application/x-www-form-urlencoded");
    expect(c.headers.has("authorization")).toBe(false);
    expect(formObject(c.form)).toEqual({
      grant_type: "authorization_code",
      code: "code-1",
      redirect_uri: REDIRECT,
      code_verifier: base.verifier,
      client_id: "loomboard",
    });
  });

  it("refuses a redirect whose state is not this sign-in's, before using the code", async () => {
    const fetchMock = mockFetch([json(tokenBody(1))]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch });
    for (const state of ["st-2", "st-", undefined]) {
      const err = await oauth.completeAuthorize({ ...base, code: "code-1", state }).catch((e: unknown) => e);
      expect(err).toBeInstanceOf(OAuthError);
      expect((err as OAuthError).code).toBe("state_mismatch");
    }
    // An error response with a foreign state is refused as a foreign response.
    const err = await oauth
      .completeAuthorize({ ...base, error: "access_denied", state: "other" })
      .catch((e: unknown) => e);
    expect((err as OAuthError).code).toBe("state_mismatch");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("refuses an iss naming another server (RFC 9207)", async () => {
    const fetchMock = mockFetch([json(tokenBody(1))]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch, issuer: BASE });
    const err = await oauth
      .completeAuthorize({ ...base, code: "code-1", state: "st-1", iss: "https://evil.test" })
      .catch((e: unknown) => e);
    expect((err as OAuthError).code).toBe("issuer_mismatch");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("checks iss against the discovered issuer when none was configured", async () => {
    const fetchMock = mockFetch([json(METADATA), json(tokenBody(1))]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch });
    await oauth.completeAuthorize({ ...base, code: "code-1", state: "st-1", iss: BASE });
    expect(call(fetchMock, 0).url).toBe(`${BASE}/.well-known/oauth-authorization-server`);
    expect(call(fetchMock, 1).url).toBe(`${BASE}/v1/_oauth/token`);

    const other = mockFetch([json(METADATA)]);
    const oauth2 = new LoomcycleOAuth({ baseUrl: BASE, fetch: other as unknown as typeof fetch });
    const err = await oauth2
      .completeAuthorize({ ...base, code: "code-1", state: "st-1", iss: "https://evil.test" })
      .catch((e: unknown) => e);
    expect((err as OAuthError).code).toBe("issuer_mismatch");
    expect(other).toHaveBeenCalledTimes(1);
  });

  it("fails closed when a redirect carries iss but the issuer cannot be learned", async () => {
    const fetchMock = mockFetch([json({ code: "oauth_issuer_unset", error: "set LOOMCYCLE_PUBLIC_URL" }, 404)]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch });
    const err = await oauth
      .completeAuthorize({ ...base, code: "code-1", state: "st-1", iss: BASE })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(OAuthError);
    expect((err as OAuthError).code).toBe("oauth_issuer_unset");
    expect(fetchMock).toHaveBeenCalledTimes(1); // no token request
  });

  it("throws the redirect's error without a token request", async () => {
    const fetchMock = mockFetch([]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch });
    const err = await oauth
      .completeAuthorize({ ...base, state: "st-1", error: "access_denied", error_description: "pending" })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(OAuthError);
    expect((err as OAuthError).code).toBe("access_denied");
    expect((err as OAuthError).errorDescription).toBe("pending");
    const noCode = await oauth.completeAuthorize({ ...base, state: "st-1" }).catch((e: unknown) => e);
    expect((noCode as OAuthError).code).toBe("invalid_response");
  });

  it("maps a token error body to OAuthError and never echoes a malformed token body", async () => {
    const fetchMock = mockFetch([
      json({ error: "invalid_grant" }, 400),
      json({ access_token: "at-secret-looking", token_type: "mac", expires_in: 1, refresh_token: "r" }),
    ]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch });
    const bad = await oauth.completeAuthorize({ ...base, code: "c", state: "st-1" }).catch((e: unknown) => e);
    expect(bad).toBeInstanceOf(OAuthError);
    expect((bad as OAuthError).code).toBe("invalid_grant");
    expect((bad as OAuthError).status).toBe(400);
    expect((bad as OAuthError).retryable).toBe(false);

    const odd = await oauth.completeAuthorize({ ...base, code: "c", state: "st-1" }).catch((e: unknown) => e);
    expect((odd as OAuthError).code).toBe("invalid_response");
    expect((odd as OAuthError).bodyText).toBeUndefined();
    expect(String((odd as OAuthError).message)).not.toContain("at-secret-looking");
  });
});

describe("refresh, revoke, discover, parseAuthorizeResponse", () => {
  it("refreshes with the refresh_token grant", async () => {
    const fetchMock = mockFetch([json(tokenBody(2))]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch });
    const t = await oauth.refresh({ refreshToken: "lcrt_rt-1", clientId: "loomboard" });
    expect(t.access_token).toBe("at-2");
    expect(formObject(call(fetchMock, 0).form)).toEqual({
      grant_type: "refresh_token",
      refresh_token: "lcrt_rt-1",
      client_id: "loomboard",
    });
  });

  it("revokes with token and client_id, and reports a refused request", async () => {
    const fetchMock = mockFetch([() => new Response(null, { status: 200 }), json({ error: "temporarily_unavailable" }, 503)]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch });
    await oauth.revoke({ token: "lcrt_rt-1", clientId: "loomboard" });
    const c = call(fetchMock, 0);
    expect(c.url).toBe(`${BASE}/v1/_oauth/revoke`);
    expect(c.headers.has("authorization")).toBe(false);
    expect(formObject(c.form)).toEqual({ token: "lcrt_rt-1", client_id: "loomboard" });
    const err = await oauth.revoke({ token: "x", clientId: "loomboard" }).catch((e: unknown) => e);
    expect((err as OAuthError).code).toBe("temporarily_unavailable");
    expect((err as OAuthError).retryable).toBe(true);
  });

  it("caches discovered metadata but not a failure", async () => {
    const fetchMock = mockFetch([() => new Response("down", { status: 503 }), json(METADATA)]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch });
    await expect(oauth.discover()).rejects.toBeInstanceOf(OAuthError);
    await expect(oauth.discover()).resolves.toEqual(METADATA);
    await expect(oauth.discover()).resolves.toEqual(METADATA);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("reads the redirect parameters", () => {
    expect(parseAuthorizeResponse(`${REDIRECT}?code=c1&state=s1&iss=${encodeURIComponent(BASE)}`)).toEqual({
      code: "c1",
      state: "s1",
      iss: BASE,
    });
    expect(parseAuthorizeResponse(new URL(`${REDIRECT}?error=access_denied&error_description=pending&state=s`))).toEqual({
      error: "access_denied",
      error_description: "pending",
      state: "s",
    });
  });
});

function tokenSet(n: number, expiresAt: number): OAuthTokenSet {
  return { ...tokenBody(n), expires_at: expiresAt };
}

describe("OAuthTokenProvider", () => {
  it("hands out the current token without a request while it is fresh", async () => {
    const fetchMock = mockFetch([]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch });
    const p = new OAuthTokenProvider({ oauth, clientId: "c", tokens: tokenSet(1, Date.now() + 3_600_000) });
    await expect(p.getToken()).resolves.toBe("at-1");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("refreshes once ahead of expiry for concurrent callers and persists the new set", async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const fetchMock = mockFetch([
      async () => {
        await gate;
        return json(tokenBody(2))(new Request(BASE));
      },
    ]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch });
    const persisted: OAuthTokenSet[] = [];
    const p = new OAuthTokenProvider({
      oauth,
      clientId: "c",
      tokens: tokenSet(1, Date.now() + 30_000), // inside the 60 s skew
      onTokens: (t) => {
        persisted.push(t);
      },
    });
    const all = Promise.all([p.getToken(), p.getToken(), p.getToken()]);
    release();
    await expect(all).resolves.toEqual(["at-2", "at-2", "at-2"]);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(persisted.map((t) => t.refresh_token)).toEqual(["lcrt_rt-2"]);
    expect(formObject(call(fetchMock, 0).form).refresh_token).toBe("lcrt_rt-1");
  });

  it("keeps a still-valid token when an early refresh fails for a transient reason", async () => {
    const fetchMock = mockFetch([json({ error: "temporarily_unavailable" }, 503)]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch });
    const onSignedOut = vi.fn();
    const p = new OAuthTokenProvider({ oauth, clientId: "c", tokens: tokenSet(1, Date.now() + 30_000), onSignedOut });
    await expect(p.getToken()).resolves.toBe("at-1");
    expect(onSignedOut).not.toHaveBeenCalled();
    expect(p.tokens?.access_token).toBe("at-1");
  });

  it("ends the sign-in when the refresh token is refused", async () => {
    const fetchMock = mockFetch([json({ error: "invalid_grant" }, 400)]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch });
    const onSignedOut = vi.fn();
    const p = new OAuthTokenProvider({ oauth, clientId: "c", tokens: tokenSet(1, Date.now() - 1), onSignedOut });
    const err = await p.getToken().catch((e: unknown) => e);
    expect((err as OAuthError).code).toBe("invalid_grant");
    expect(onSignedOut).toHaveBeenCalledTimes(1);
    expect(p.tokens).toBeUndefined();
    // Later calls fail at once, with no further refresh.
    await expect(p.getToken()).rejects.toBeInstanceOf(OAuthError);
    await expect(p.onUnauthorized("at-1")).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(onSignedOut).toHaveBeenCalledTimes(1);
  });

  it("signs out by revoking the refresh token and forgetting the tokens", async () => {
    const fetchMock = mockFetch([() => new Response(null, { status: 200 })]);
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: fetchMock as unknown as typeof fetch });
    const onSignedOut = vi.fn();
    const p = new OAuthTokenProvider({ oauth, clientId: "c", tokens: tokenSet(1, Date.now() + 3_600_000), onSignedOut });
    await p.signOut();
    expect(formObject(call(fetchMock, 0).form)).toEqual({ token: "lcrt_rt-1", client_id: "c" });
    expect(onSignedOut).toHaveBeenCalledTimes(1);
    expect((onSignedOut.mock.calls[0]![0] as OAuthError).code).toBe("signed_out");
    await expect(p.getToken()).rejects.toMatchObject({ code: "signed_out" });
  });
});

describe("LoomcycleClient with a token provider", () => {
  /** A client whose data calls and token calls share one fetch mock. */
  function setup(responses: Factory[], expiresAt = Date.now() + 3_600_000) {
    const fetchMock = mockFetch(responses);
    const f = fetchMock as unknown as typeof fetch;
    const oauth = new LoomcycleOAuth({ baseUrl: BASE, fetch: f });
    const provider = new OAuthTokenProvider({ oauth, clientId: "c", tokens: tokenSet(1, expiresAt) });
    const client = new LoomcycleClient({ baseUrl: BASE, fetch: f, tokenProvider: provider });
    return { fetchMock, client, provider };
  }

  const health = { status: "ok" };

  it("sends the provider's token as the bearer", async () => {
    const { fetchMock, client } = setup([json(health)]);
    await client.health();
    expect(call(fetchMock, 0).headers.get("authorization")).toBe("Bearer at-1");
  });

  it("refreshes after a 401 and retries the request once with the new token", async () => {
    const { fetchMock, client, provider } = setup([
      json({ code: "unauthorized", error: "token expired" }, 401),
      json(tokenBody(2)),
      json(health),
    ]);
    await expect(client.health()).resolves.toEqual(health);
    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(call(fetchMock, 1).url).toBe(`${BASE}/v1/_oauth/token`);
    expect(call(fetchMock, 1).headers.has("authorization")).toBe(false);
    expect(call(fetchMock, 2).headers.get("authorization")).toBe("Bearer at-2");
    expect(provider.tokens?.access_token).toBe("at-2");
  });

  it("lets a second 401 stand instead of refreshing again", async () => {
    const { fetchMock, client } = setup([
      json({ code: "unauthorized", error: "no" }, 401),
      json(tokenBody(2)),
      json({ code: "unauthorized", error: "still no" }, 401),
    ]);
    await expect(client.health()).rejects.toBeInstanceOf(AuthError);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("shares one refresh between concurrent 401s", async () => {
    const { fetchMock, client } = setup([
      json({ code: "unauthorized", error: "a" }, 401),
      json({ code: "unauthorized", error: "b" }, 401),
      json(tokenBody(2)),
      json(health),
      json(health),
    ]);
    await expect(Promise.all([client.health(), client.health()])).resolves.toEqual([health, health]);
    const tokenCalls = fetchMock.mock.calls.filter(([u]) => String(u).endsWith("/v1/_oauth/token"));
    expect(tokenCalls).toHaveLength(1);
  });

  it("does not retry when the sign-in has ended", async () => {
    const { fetchMock, client } = setup([json({ code: "unauthorized", error: "revoked" }, 401), json({ error: "invalid_grant" }, 400)]);
    await expect(client.health()).rejects.toBeInstanceOf(AuthError);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("refuses an authToken and a tokenProvider together", () => {
    const oauth = new LoomcycleOAuth({ baseUrl: BASE });
    const provider = new OAuthTokenProvider({ oauth, clientId: "c", tokens: tokenSet(1, Date.now() + 1e6) });
    expect(() => new LoomcycleClient({ authToken: "t", tokenProvider: provider })).toThrow(TypeError);
  });

  it("keeps a static authToken client unchanged and its oauth helper bearer-free", async () => {
    const fetchMock = mockFetch([json(health), json(tokenBody(3))]);
    const client = new LoomcycleClient({ baseUrl: BASE, authToken: "static", fetch: fetchMock as unknown as typeof fetch });
    await client.health();
    expect((call(fetchMock, 0).init.headers as Record<string, string>).Authorization).toBe("Bearer static");
    await client.oauth.refresh({ refreshToken: "lcrt_rt-2", clientId: "c" });
    expect(call(fetchMock, 1).headers.has("authorization")).toBe(false);
  });

  it("opens a client-tool socket with the provider's token", async () => {
    const opened: string[][] = [];
    class FakeWS implements WebSocketLike {
      onopen = null;
      onclose = null;
      onerror = null;
      onmessage = null;
      constructor(_url: string, protocols?: string | string[]) {
        opened.push([protocols ?? []].flat());
      }
      send(): void {}
      close(): void {}
    }
    const { client } = setup([]);
    const host = client.connectClientTools({
      tools: [],
      onInvoke: () => null,
      WebSocketImpl: FakeWS as unknown as WebSocketCtor,
      reconnect: false,
    });
    await vi.waitFor(() => expect(opened).toHaveLength(1));
    expect(opened[0]).toContain("bearer.at-1");
    host.close();
  });
});
