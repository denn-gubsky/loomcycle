/**
 * The app sign-in and ClientDef types and parameters against the Go server
 * that produces or reads them (RFC FL).
 *
 * WHY. The types are hand-written and the mocked tests only echo what each
 * test put in the mock: a field the server added, or a parameter it renamed,
 * passes all of them. So each is compared both ways with the server source —
 * a struct's json tags, a map literal's keys, the parameters a handler reads.
 *
 * HOW. Text, like users-drift.test.ts: the Go source is read, never a copy of
 * its field list. A standalone checkout of the adapter (no Go tree) has
 * nothing to compare against and the suite is skipped.
 */

import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { LoomcycleClient, LoomcycleOAuth } from "../src/index.js";

const root = (rel: string) => fileURLToPath(new URL(`../../../${rel}`, import.meta.url));
const SERVER = "internal/api/http/oauth_server.go";
const CLIENTDEF = "internal/api/http/oauth_clientdef.go";
const CONFIG = "internal/config/auth_clients.go";
const haveGo = existsSync(root("go.mod")) && existsSync(root(SERVER));

const TYPES = fileURLToPath(new URL("../src/types.ts", import.meta.url));

function read(rel: string): string {
  return readFileSync(root(rel), "utf8");
}

/** The json keys of `type <name> struct { … }`. Fails when the struct is
 *  gone, so a rename cannot turn a check vacuous. */
function goKeys(rel: string, name: string): string[] {
  const src = read(rel);
  const start = src.indexOf(`type ${name} struct {`);
  expect(start, `${rel}: type ${name} struct not found — renamed or moved?`).toBeGreaterThan(-1);
  const body = src.slice(start, src.indexOf("\n}", start));
  const keys = [...body.matchAll(/json:"([^",]*)/g)].map((m) => m[1]!).filter((k) => k !== "-");
  expect(keys.length, `${rel}: ${name} has no json tags`).toBeGreaterThan(0);
  return keys;
}

/** The body of a func or method `<name>(`, up to its closing brace. */
function goFunc(rel: string, name: string): string {
  const src = read(rel);
  const m = new RegExp(String.raw`^func (?:\([^)]*\) )?${name}\(`, "m").exec(src);
  expect(m, `${rel}: func ${name} not found — renamed or moved?`).not.toBeNull();
  return src.slice(m!.index, src.indexOf("\n}", m!.index));
}

/** The string keys (`"key":`) of the map literals in a Go snippet. */
function mapKeys(snippet: string): string[] {
  const keys = [...new Set([...snippet.matchAll(/"([a-z0-9_]+)":/g)].map((m) => m[1]!))];
  expect(keys.length, "no map keys found").toBeGreaterThan(0);
  return keys.sort();
}

/** The top-level fields of `export interface <name> { … }` in types.ts. */
function tsFields(name: string): string[] {
  const src = readFileSync(TYPES, "utf8");
  const start = src.indexOf(`export interface ${name} `);
  expect(start, `types.ts: interface ${name} not found`).toBeGreaterThan(-1);
  const body = src.slice(start, src.indexOf("\n}", start));
  const fields = [...body.matchAll(/^ {2}([a-z_][a-z0-9_]*)\??:/gm)].map((m) => m[1]!);
  expect(fields.length, `types.ts: ${name} has no fields`).toBeGreaterThan(0);
  return fields.sort();
}

function bothWays(ts: string, goKeyList: string[]) {
  const fields = new Set(tsFields(ts));
  const keys = new Set(goKeyList);
  expect([...keys].filter((k) => !fields.has(k)).sort(), `Go sends/reads keys ${ts} lacks`).toEqual([]);
  expect([...fields].filter((f) => !keys.has(f)).sort(), `${ts} fields the Go side has not`).toEqual([]);
}

/** The parameter names a handler reads from its query or form: `q["x"]`,
 *  `f["x"]`. */
function paramsRead(snippet: string, v: string): string[] {
  return [...new Set([...snippet.matchAll(new RegExp(String.raw`\b${v}\["([a-z_]+)"\]`, "g"))].map((m) => m[1]!))];
}

/** Captures the requests a call makes, answering each with `answer`. */
function capture(answer: () => Response) {
  const calls: Array<{ url: string; init: RequestInit }> = [];
  const fetchImpl = (async (url: string, init: RequestInit) => {
    calls.push({ url, init });
    return answer();
  }) as unknown as typeof fetch;
  return { calls, fetchImpl };
}

const tokenAnswer = () =>
  new Response(JSON.stringify({ access_token: "a", token_type: "Bearer", expires_in: 60, refresh_token: "r" }), {
    status: 200,
  });

function formKeys(init: RequestInit): string[] {
  return [...new URLSearchParams(init.body as string).keys()].sort();
}

describe.skipIf(!haveGo)("app sign-in types and parameters match the Go server", () => {
  it("OAuthTokenResponse ⇔ oauthTokenResponse", () => {
    bothWays("OAuthTokenResponse", goKeys(SERVER, "oauthTokenResponse"));
  });

  it("OAuthServerMetadata ⇔ the metadata handler's keys", () => {
    bothWays("OAuthServerMetadata", mapKeys(goFunc(SERVER, "handleOAuthMetadata")));
  });

  it("AuthorizeResponseParams ⇔ the parameters the redirect carries", () => {
    const authorize = goFunc(SERVER, "handleOAuthAuthorize");
    // The success redirect's own parameters (not the login page hand-off,
    // which also builds a url.Values).
    const code = [...authorize.matchAll(/oauthRedirect\([^)]*url\.Values\{"([a-z_]+)"/g)].map((m) => m[1]!);
    expect(code, "handleOAuthAuthorize: no oauthRedirect(…url.Values{…}) found").not.toEqual([]);
    const redirect = goFunc(SERVER, "oauthRedirect");
    const errRedirect = goFunc(SERVER, "oauthRedirectError");
    const set = (s: string) => [...s.matchAll(/\.Set\("([a-z_]+)"/g)].map((m) => m[1]!);
    const errKeys = [...errRedirect.matchAll(/url\.Values\{"([a-z_]+)"/g)].map((m) => m[1]!);
    bothWays("AuthorizeResponseParams", [...new Set([...code, ...set(redirect), ...set(errRedirect), ...errKeys])]);
  });

  it("beginAuthorize sends exactly the parameters authorize reads", async () => {
    const req = await new LoomcycleOAuth({ baseUrl: "https://x.test" }).beginAuthorize({
      clientId: "c",
      redirectUri: "https://app.test/cb",
    });
    const sent = [...new URL(req.url).searchParams.keys()].sort();
    expect(sent).toEqual(paramsRead(goFunc(SERVER, "handleOAuthAuthorize"), "q").sort());
  });

  it("the token and revoke forms carry exactly the parameters their handlers read", async () => {
    const shared = paramsRead(goFunc(SERVER, "oauthForm"), "f"); // client_id
    const grant = paramsRead(goFunc(SERVER, "handleOAuthToken"), "f"); // grant_type
    const { calls, fetchImpl } = capture(tokenAnswer);
    const oauth = new LoomcycleOAuth({ baseUrl: "https://x.test", fetch: fetchImpl });

    await oauth.completeAuthorize({
      code: "c",
      state: "s",
      expectedState: "s",
      verifier: "v".repeat(43),
      clientId: "c",
      redirectUri: "https://app.test/cb",
    });
    expect(formKeys(calls[0]!.init)).toEqual(
      [...shared, ...grant, ...paramsRead(goFunc(SERVER, "oauthCodeGrant"), "f")].sort(),
    );

    await oauth.refresh({ refreshToken: "r", clientId: "c" });
    expect(formKeys(calls[1]!.init)).toEqual(
      [...shared, ...grant, ...paramsRead(goFunc(SERVER, "oauthRefreshGrant"), "f")].sort(),
    );

    await oauth.revoke({ token: "t", clientId: "c" });
    expect(formKeys(calls[2]!.init)).toEqual([...shared, ...paramsRead(goFunc(SERVER, "handleOAuthRevoke"), "f")].sort());
  });
});

describe.skipIf(!haveGo)("ClientDef types match the Go server", () => {
  it("AppClientConfig ⇔ config.AuthClientConfig", () => {
    bothWays("AppClientConfig", goKeys(CONFIG, "AuthClientConfig"));
  });

  it("AppClientRefresh ⇔ config.AuthClientRefresh", () => {
    bothWays("AppClientRefresh", goKeys(CONFIG, "AuthClientRefresh"));
  });

  it("ClientDefInput ⇔ clientDefInput", () => {
    bothWays("ClientDefInput", goKeys(CLIENTDEF, "clientDefInput"));
  });

  it("ClientDefInput.op names every op the dispatch serves", () => {
    const src = readFileSync(TYPES, "utf8");
    const start = src.indexOf("export interface ClientDefInput ");
    const opLine = /^ {2}op: ([^;]+);/m.exec(src.slice(start))!;
    const tsOps = [...opLine[1]!.matchAll(/"([a-z_]+)"/g)].map((m) => m[1]!).sort();
    const body = goFunc(CLIENTDEF, "ClientDef");
    const goOps = [...body.matchAll(/case ([^:]+):/g)]
      .flatMap((m) => [...m[1]!.matchAll(/"([a-z_]+)"/g)].map((x) => x[1]!))
      .sort();
    expect(tsOps).toEqual(goOps);
  });

  it("ClientDefRecord ⇔ clientDefView", () => {
    bothWays("ClientDefRecord", mapKeys(goFunc(CLIENTDEF, "clientDefView")));
  });

  it("ConfigAppClient ⇔ configClientView", () => {
    bothWays("ConfigAppClient", mapKeys(goFunc(CLIENTDEF, "configClientView")));
  });

  it("ClientDefVerifyResult ⇔ the verify op's answers", () => {
    bothWays("ClientDefVerifyResult", mapKeys(goFunc(CLIENTDEF, "clientDefVerify")));
  });

  it("ClientDefRetireResult ⇔ the retire op's answer", () => {
    const body = goFunc(CLIENTDEF, "clientDefRetire");
    const ok = body.slice(body.indexOf("clientDefOK("));
    bothWays("ClientDefRetireResult", mapKeys(ok));
  });

  it("ClientDefNameSummary ⇔ one row of the names route", () => {
    const body = goFunc(CLIENTDEF, "handleListClientDefNames");
    const row = body.slice(body.indexOf("append(names,"));
    bothWays("ClientDefNameSummary", mapKeys(row.slice(0, row.indexOf("})"))));
  });

  it("the client methods reach the routes the server mounts", async () => {
    const server = read("internal/api/http/server.go");
    expect(server).toContain(`"POST /v1/_clientdef"`);
    expect(server).toContain(`"GET /v1/_clientdef/names"`);
    const { calls, fetchImpl } = capture(() => new Response("{}", { status: 200 }));
    const client = new LoomcycleClient({ baseUrl: "https://x.test", fetch: fetchImpl });
    await client.listClientDefs();
    await client.listClientDefNames();
    expect(calls.map((c) => `${c.init.method} ${new URL(c.url).pathname}`)).toEqual([
      "POST /v1/_clientdef",
      "GET /v1/_clientdef/names",
    ]);
  });
});
