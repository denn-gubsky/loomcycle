// The bot's file token (RFC HF §3.7, D5): a Teams file link without tempauth
// is downloaded with the bot's own token for that link's host, acquired with
// a certificate or the SDK's non-secret credential, never sent past the first
// request and never logged.

import { execFileSync } from "node:child_process";
import { constants, verify, X509Certificate } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import net from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { MemoryStore, type FileIn, type VoiceIn } from "@loomcycle/bridge-core";

import { createTeamsBridge, type ConversationRef } from "../src/bridge.js";
import { loadConfig } from "../src/config.js";
import {
  certificateTokenSource,
  chooseFileToken,
  credentialKind,
  FileTokenError,
  parseCertificate,
  SECRET_ONLY_WARNING,
  type FileCertificate,
  type FileTokenSource,
} from "../src/filetoken.js";
import { TeamsFileError, teamsFilePort, teamsVoicePort } from "../src/voice.js";
import { FakeLoomcycle, RecordingLogger } from "./fakes.js";

const TENANT = "11111111-1111-1111-1111-111111111111";
const BOT = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
const HOSTS = [".sharepoint.com"];
const TEMPAUTH = "https://contoso-my.sharepoint.com/personal/alice/_layouts/15/download.aspx?UniqueId=1&tempauth=secret-tempauth";
const BARE = "https://contoso-my.sharepoint.com/personal/alice/_layouts/15/download.aspx?UniqueId=1";
const BEARER = "file-token-value";
const PDF = new TextEncoder().encode("%PDF-1.7\n1 0 obj\n");

type Route = { status?: number; headers?: Record<string, string>; body?: Uint8Array };

/** A scripted SharePoint that records each request's Authorization header. */
function fakeHost(routes: Record<string, Route>) {
  const requests: Array<{ url: string; authorization: string | null }> = [];
  const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    requests.push({ url, authorization: new Headers(init?.headers).get("authorization") });
    const r = routes[url];
    if (!r) return new Response("not found", { status: 404 });
    const redirect = r.status !== undefined && r.status >= 300 && r.status < 400;
    return new Response(redirect ? null : (r.body ?? PDF), { status: r.status ?? 200, headers: r.headers ?? { "content-type": "application/pdf" } });
  }) as typeof fetch;
  return { requests, fetchFn };
}

/** A token source that records the scopes it was asked for. */
function recordingToken(result: () => Promise<string> = async () => BEARER) {
  const scopes: string[] = [];
  const source: FileTokenSource = async (scope) => {
    scopes.push(scope);
    return result();
  };
  return { scopes, source };
}

const signal = () => AbortSignal.timeout(5_000);
const file = (url: string): FileIn => ({ ref: { url, name: "a.pdf" }, name: "a.pdf", type: "pdf" });
const voice = (url: string): VoiceIn => ({ kind: "audio_file", ref: { url, name: "memo.m4a" }, mime: "audio/mp4" });

describe("the file port with a file token", () => {
  it("fetches a tempauth link with no Authorization header and acquires no token", async () => {
    const host = fakeHost({ [TEMPAUTH]: {} });
    const tok = recordingToken();
    expect(await teamsFilePort({ hosts: HOSTS, fetch: host.fetchFn, token: tok.source }).download(file(TEMPAUTH), 1000, signal())).toEqual({ bytes: PDF });
    expect(host.requests).toEqual([{ url: TEMPAUTH, authorization: null }]);
    expect(tok.scopes).toEqual([]);
  });

  it("fetches a link without tempauth with the bot's bearer for that link's host", async () => {
    const host = fakeHost({ [BARE]: {} });
    const tok = recordingToken();
    expect(await teamsFilePort({ hosts: HOSTS, fetch: host.fetchFn, token: tok.source }).download(file(BARE), 1000, signal())).toEqual({ bytes: PDF });
    expect(tok.scopes).toEqual(["https://contoso-my.sharepoint.com/.default"]);
    expect(host.requests).toEqual([{ url: BARE, authorization: `Bearer ${BEARER}` }]);
  });

  it("sends the voice port's download of a link without tempauth with the bearer too", async () => {
    const host = fakeHost({ [BARE]: { headers: { "content-type": "audio/mp4" } } });
    const tok = recordingToken();
    const port = teamsVoicePort<ConversationRef>({ hosts: HOSTS, fetch: host.fetchFn, token: tok.source });
    expect((await port.download(voice(BARE), 1000, signal())).bytes).toEqual(PDF);
    expect(host.requests).toEqual([{ url: BARE, authorization: `Bearer ${BEARER}` }]);
  });

  it("drops the token on a redirect, to another allowed host or the same one", async () => {
    const other = "https://contoso.sharepoint.com/files/a.pdf";
    const same = "https://contoso-my.sharepoint.com/again";
    const host = fakeHost({ [BARE]: { status: 302, headers: { location: other } }, [other]: { status: 302, headers: { location: same } }, [same]: {} });
    const tok = recordingToken();
    expect(await teamsFilePort({ hosts: HOSTS, fetch: host.fetchFn, token: tok.source }).download(file(BARE), 1000, signal())).toEqual({ bytes: PDF });
    expect(host.requests).toEqual([
      { url: BARE, authorization: `Bearer ${BEARER}` },
      { url: other, authorization: null },
      { url: same, authorization: null },
    ]);
  });

  it("still refuses a redirect off the allowed hosts after a bearer request", async () => {
    const host = fakeHost({ [BARE]: { status: 302, headers: { location: "https://evil.example/steal" } } });
    const err = await teamsFilePort({ hosts: HOSTS, fetch: host.fetchFn, token: recordingToken().source })
      .download(file(BARE), 1000, signal())
      .catch((e: unknown) => e);
    expect(err).toMatchObject({ name: "TeamsFileError", reason: "redirect" });
    expect(host.requests.map((r) => r.url)).toEqual([BARE]);
  });

  it("refuses a link off the allowed hosts before any token is acquired", async () => {
    const host = fakeHost({});
    const tok = recordingToken();
    const port = teamsFilePort({ hosts: HOSTS, fetch: host.fetchFn, token: tok.source });
    for (const url of ["https://evil.example/a.pdf", "http://contoso.sharepoint.com/a.pdf", "https://contoso.sharepoint.com.evil.example/a.pdf"]) {
      await expect(port.download(file(url), 1000, signal())).rejects.toMatchObject({ reason: "host" });
    }
    expect(tok.scopes).toEqual([]);
    expect(host.requests).toEqual([]);
  });

  it("sends the bearer only to a OneDrive personal site, refusing a team site or any other path before acquiring a token", async () => {
    const host = fakeHost({});
    const tok = recordingToken();
    const log = new RecordingLogger();
    const port = teamsFilePort({ hosts: HOSTS, fetch: host.fetchFn, token: tok.source, log });
    for (const url of [
      "https://contoso.sharepoint.com/sites/finance/Shared%20Documents/a.pdf",
      "https://contoso-my.sharepoint.com/sites/x/a.pdf",
      "https://contoso-my.sharepoint.com/personalx/alice/a.pdf",
      "https://contoso-my.sharepoint.com/_layouts/15/download.aspx?UniqueId=1",
      "https://-my.sharepoint.com/personal/alice/a.pdf",
      "https://a.contoso-my.sharepoint.com/personal/alice/a.pdf",
    ]) {
      await expect(port.download(file(url), 1000, signal())).rejects.toMatchObject({ name: "TeamsFileError", reason: "host" });
    }
    expect(tok.scopes).toEqual([]);
    expect(host.requests).toEqual([]);
    expect(log.lines[0]).toEqual({ level: "warn", msg: "teams file download refused", attrs: { reason: "host", host: "contoso.sharepoint.com" } });

    // A personal site under a sovereign suffix the operator allows gets it.
    const gov = "https://contoso-my.sharepoint.us/personal/alice_contoso_us/Documents/a.pdf";
    const govHost = fakeHost({ [gov]: {} });
    const port2 = teamsFilePort({ hosts: [".sharepoint.us"], fetch: govHost.fetchFn, token: tok.source });
    expect(await port2.download(file(gov), 1000, signal())).toEqual({ bytes: PDF });
    expect(govHost.requests).toEqual([{ url: gov, authorization: `Bearer ${BEARER}` }]);
    expect(tok.scopes).toEqual(["https://contoso-my.sharepoint.us/.default"]);
  });

  it("drops the bearer on a redirect from a personal site to a team site, which must still be an allowed host", async () => {
    const team = "https://contoso.sharepoint.com/sites/finance/a.pdf";
    const host = fakeHost({ [BARE]: { status: 302, headers: { location: team } }, [team]: {} });
    expect(await teamsFilePort({ hosts: HOSTS, fetch: host.fetchFn, token: recordingToken().source }).download(file(BARE), 1000, signal())).toEqual({
      bytes: PDF,
    });
    expect(host.requests).toEqual([
      { url: BARE, authorization: `Bearer ${BEARER}` },
      { url: team, authorization: null },
    ]);
    const off = fakeHost({ [BARE]: { status: 302, headers: { location: "https://contoso.sharepoint.example/sites/x" } } });
    await expect(teamsFilePort({ hosts: HOSTS, fetch: off.fetchFn, token: recordingToken().source }).download(file(BARE), 1000, signal())).rejects.toMatchObject({
      reason: "redirect",
    });
    expect(off.requests.map((r) => r.url)).toEqual([BARE]);
  });

  it("refuses a link without tempauth when there is no file token, fetching nothing", async () => {
    const host = fakeHost({ [BARE]: {} });
    const log = new RecordingLogger();
    await expect(teamsFilePort({ hosts: HOSTS, fetch: host.fetchFn, log }).download(file(BARE), 1000, signal())).rejects.toMatchObject({
      reason: "token",
      detail: { cause: "none" },
    });
    expect(host.requests).toEqual([]);
    expect(log.lines).toEqual([{ level: "warn", msg: "teams file download refused", attrs: { reason: "token", cause: "none" } }]);
  });

  it("turns a failed acquisition into a refused download, logging why and no secret", async () => {
    const host = fakeHost({ [BARE]: {} });
    const log = new RecordingLogger();
    const failing = recordingToken(async () => {
      throw new FileTokenError("status", { status: 401 });
    });
    const err = await teamsFilePort({ hosts: HOSTS, fetch: host.fetchFn, token: failing.source, log })
      .download(file(BARE), 1000, signal())
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(TeamsFileError);
    expect(err).toMatchObject({ reason: "token", detail: { cause: "status", status: 401 } });
    expect(host.requests).toEqual([]);
    expect(log.lines).toEqual([{ level: "warn", msg: "teams file download refused", attrs: { reason: "token", cause: "status", status: 401 } }]);

    // An error of unknown shape (the SDK's MSAL, say) is "failed", and its
    // message, whatever it holds, reaches neither the error nor the log.
    const leaky = recordingToken(async () => {
      throw new Error(`boom ${BEARER} client_secret=abc`);
    });
    const err2 = await teamsFilePort({ hosts: HOSTS, fetch: host.fetchFn, token: leaky.source, log })
      .download(file(BARE), 1000, signal())
      .catch((e: unknown) => e);
    expect(err2).toMatchObject({ reason: "token", detail: { cause: "failed" } });
    expect(String((err2 as Error).message)).not.toContain(BEARER);
    expect(JSON.stringify(log.lines)).not.toContain(BEARER);
    expect(JSON.stringify(log.lines)).not.toContain("client_secret");
  });

  it("never logs the bearer when the host answers 401", async () => {
    const host = fakeHost({ [BARE]: { status: 401 } });
    const log = new RecordingLogger();
    await expect(teamsFilePort({ hosts: HOSTS, fetch: host.fetchFn, token: recordingToken().source, log }).download(file(BARE), 1000, signal())).rejects.toMatchObject({
      reason: "status",
    });
    expect(JSON.stringify(log.lines)).not.toContain(BEARER);
  });
});

// A real certificate and key, made by openssl for this run only.
let dir: string;
let pemPath: string;
let pem: string;
let certificate: FileCertificate;

beforeAll(() => {
  dir = mkdtempSync(join(tmpdir(), "bridge-teams-cert-"));
  const key = join(dir, "key.pem");
  const crt = join(dir, "cert.pem");
  execFileSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", key, "-out", crt, "-days", "1", "-subj", "/CN=bridge-teams-test"], { stdio: "ignore" });
  pem = readFileSync(key, "utf8") + readFileSync(crt, "utf8");
  pemPath = join(dir, "bot.pem");
  writeFileSync(pemPath, pem, { mode: 0o600 });
  certificate = parseCertificate(pem);
});

afterAll(() => {
  rmSync(dir, { recursive: true, force: true });
});

/** A fake Entra token endpoint: checks the client assertion against the
 *  certificate and answers with a token per request. */
function fakeEntra(opts: { status?: number; expiresIn?: number } = {}) {
  const forms: URLSearchParams[] = [];
  let n = 0;
  const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    expect(url).toBe(`https://login.example/${TENANT}/oauth2/v2.0/token`);
    expect(init?.method).toBe("POST");
    const form = new URLSearchParams(String(init?.body));
    forms.push(form);
    if (opts.status) return new Response(JSON.stringify({ error: "invalid_client", error_description: "secret-ish detail" }), { status: opts.status });
    return new Response(JSON.stringify({ access_token: `tok-${++n}`, expires_in: opts.expiresIn ?? 3600, token_type: "Bearer" }), {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  }) as typeof fetch;
  return { forms, fetchFn };
}

function decode(part: string): Record<string, unknown> {
  return JSON.parse(Buffer.from(part, "base64url").toString("utf8")) as Record<string, unknown>;
}

describe("the certificate token source", () => {
  it("requests a client-credentials token with a PS256 assertion the certificate verifies", async () => {
    const entra = fakeEntra();
    const source = certificateTokenSource({ clientId: BOT, tenantId: TENANT, certificate, loginEndpoint: "https://login.example/", fetch: entra.fetchFn });
    expect(await source("https://contoso-my.sharepoint.com/.default")).toBe("tok-1");

    const form = entra.forms[0]!;
    expect(form.get("grant_type")).toBe("client_credentials");
    expect(form.get("client_id")).toBe(BOT);
    expect(form.get("scope")).toBe("https://contoso-my.sharepoint.com/.default");
    expect(form.get("client_assertion_type")).toBe("urn:ietf:params:oauth:client-assertion-type:jwt-bearer");
    expect(form.has("client_secret")).toBe(false);

    const [h, c, s] = form.get("client_assertion")!.split(".");
    const cert = new X509Certificate(pem);
    const header = decode(h!);
    expect(header).toEqual({ alg: "PS256", typ: "JWT", "x5t#S256": Buffer.from(cert.fingerprint256.replaceAll(":", ""), "hex").toString("base64url") });
    const claims = decode(c!);
    expect(claims).toMatchObject({ aud: `https://login.example/${TENANT}/oauth2/v2.0/token`, iss: BOT, sub: BOT });
    expect(typeof claims.jti).toBe("string");
    expect((claims.exp as number) - (claims.iat as number)).toBe(600);
    const ok = verify("sha256", Buffer.from(`${h}.${c}`), { key: cert.publicKey, padding: constants.RSA_PKCS1_PSS_PADDING, saltLength: 32 }, Buffer.from(s!, "base64url"));
    expect(ok).toBe(true);
  });

  it("holds a token per scope until shortly before it expires, sharing one request", async () => {
    let t = 1_000_000;
    const entra = fakeEntra({ expiresIn: 3600 });
    const source = certificateTokenSource({ clientId: BOT, tenantId: TENANT, certificate, loginEndpoint: "https://login.example", fetch: entra.fetchFn, now: () => t });
    const a = "https://a.sharepoint.com/.default";
    expect(await Promise.all([source(a), source(a)])).toEqual(["tok-1", "tok-1"]);
    expect(await source("https://b.sharepoint.com/.default")).toBe("tok-2");
    t += 50 * 60_000;
    expect(await source(a)).toBe("tok-1");
    t += 6 * 60_000;
    expect(await source(a)).toBe("tok-3");
    expect(entra.forms).toHaveLength(3);
  });

  it("fails with the endpoint's status only, never its answer", async () => {
    const source = certificateTokenSource({ clientId: BOT, tenantId: TENANT, certificate, loginEndpoint: "https://login.example", fetch: fakeEntra({ status: 401 }).fetchFn });
    const err = await source("https://a.sharepoint.com/.default").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(FileTokenError);
    expect(err).toMatchObject({ reason: "status", detail: { status: 401 } });
    expect(String((err as Error).message)).not.toContain("secret-ish");
  });

  it("refuses a PEM without a key, without a certificate, or with a key of another certificate, quoting none of it", () => {
    const keyOnly = pem.slice(0, pem.indexOf("-----BEGIN CERTIFICATE-----"));
    const certOnly = pem.slice(pem.indexOf("-----BEGIN CERTIFICATE-----"));
    expect(() => parseCertificate(certOnly)).toThrow(/no readable unencrypted private key/);
    expect(() => parseCertificate(keyOnly)).toThrow(/no readable certificate/);
    const other = join(dir, "other.pem");
    execFileSync("openssl", ["genrsa", "-out", other, "2048"], { stdio: "ignore" });
    expect(() => parseCertificate(readFileSync(other, "utf8") + certOnly)).toThrow(/does not belong/);
    try {
      parseCertificate("-----BEGIN PRIVATE KEY-----\nSECRETSECRET\n-----END PRIVATE KEY-----");
    } catch (e) {
      expect(String((e as Error).message)).not.toContain("SECRET");
    }
  });
});

describe("choosing the file token", () => {
  const app = { getAppToken: async (scope?: string) => ({ toString: () => `sdk:${scope}` }) };

  it("names the SDK's credential kinds", () => {
    expect(credentialKind(undefined)).toBe("none");
    expect(credentialKind({ clientId: BOT, clientSecret: "s" })).toBe("secret");
    expect(credentialKind({ clientId: BOT, token: () => "t" })).toBe("token");
    expect(credentialKind({ clientId: BOT, managedIdentityType: "system" })).toBe("federated");
    expect(credentialKind({ clientId: BOT })).toBe("managed-identity");
  });

  it("has no source with only a client secret, and takes the SDK's managed identity or FIC", async () => {
    expect(chooseFileToken({ credentials: { clientId: BOT, clientSecret: "s" }, app })).toEqual({ kind: "secret" });
    expect(chooseFileToken({ credentials: undefined, app })).toEqual({ kind: "none" });
    const fic = chooseFileToken({ credentials: { clientId: BOT, managedIdentityClientId: "mi", managedIdentityType: "user" }, app });
    expect(fic.kind).toBe("federated");
    expect(await fic.source!("https://a.sharepoint.com/.default")).toBe("sdk:https://a.sharepoint.com/.default");
    const none = chooseFileToken({ credentials: { clientId: BOT }, app: { getAppToken: async () => null } });
    await expect(none.source!("https://a.sharepoint.com/.default")).rejects.toMatchObject({ reason: "none" });
  });

  it("takes the certificate over a client secret", () => {
    expect(chooseFileToken({ certificate: { clientId: BOT, tenantId: TENANT, certificate }, credentials: { clientId: BOT, clientSecret: "s" }, app }).kind).toBe("certificate");
  });
});

const baseEnv = {
  LOOMCYCLE_BASE_URL: "https://loomcycle.test",
  LOOMCYCLE_TOKEN: "admin-token",
  BRIDGE_AGENT: "helper",
  TEAMS_BRIDGE_ALLOWED_TENANTS: TENANT,
  BRIDGE_DEV: "1",
};

describe("CLIENT_CERTIFICATE_PATH", () => {
  it("loads the certificate with the bot's app id and tenant", () => {
    const cfg = loadConfig({ ...baseEnv, CLIENT_CERTIFICATE_PATH: pemPath, CLIENT_ID: BOT, TENANT_ID: TENANT });
    expect(cfg.fileCertificate).toMatchObject({ clientId: BOT, tenantId: TENANT, certificate: { x5tS256: certificate.x5tS256 } });
    expect(loadConfig(baseEnv).fileCertificate).toBeUndefined();
  });

  it("refuses to start without TENANT_ID, or with a file it cannot use, naming the file only", () => {
    expect(() => loadConfig({ ...baseEnv, CLIENT_CERTIFICATE_PATH: pemPath, CLIENT_ID: BOT })).toThrow(/TENANT_ID/);
    expect(() => loadConfig({ ...baseEnv, CLIENT_CERTIFICATE_PATH: join(dir, "missing.pem"), CLIENT_ID: BOT, TENANT_ID: TENANT })).toThrow(/cannot read/);
    const bad = join(dir, "bad.pem");
    writeFileSync(bad, "-----BEGIN PRIVATE KEY-----\nSECRETSECRET\n-----END PRIVATE KEY-----\n");
    let message = "";
    try {
      loadConfig({ ...baseEnv, CLIENT_CERTIFICATE_PATH: bad, CLIENT_ID: BOT, TENANT_ID: TENANT });
    } catch (e) {
      message = String((e as Error).message);
    }
    expect(message).toMatch(/private key/);
    expect(message).not.toContain("SECRET");
  });
});

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.once("error", reject);
    s.listen(0, "127.0.0.1", () => {
      const p = (s.address() as net.AddressInfo).port;
      s.close(() => resolve(p));
    });
  });
}

describe("the start-up warning", () => {
  async function warnings(env: Record<string, string>, app: { clientId: string; clientSecret?: string; tenantId?: string }): Promise<string[]> {
    const log = new RecordingLogger();
    const bridge = createTeamsBridge(loadConfig({ ...baseEnv, ...env, PORT: String(await freePort()) }), {
      clients: new FakeLoomcycle().client,
      logger: log,
      store: new MemoryStore(),
      app,
    });
    await bridge.start();
    await bridge.stop();
    return log.lines.filter((l) => l.level === "warn").map((l) => l.msg);
  }

  it("warns when files or voice are on and only a client secret is configured", async () => {
    expect(await warnings({ BRIDGE_FILES: "1" }, { clientId: BOT, clientSecret: "s" })).toContain(SECRET_ONLY_WARNING);
    expect(await warnings({ BRIDGE_VOICE: "1" }, { clientId: BOT, clientSecret: "s" })).toContain(SECRET_ONLY_WARNING);
  });

  it("does not warn with a certificate, a managed identity, or files and voice off", async () => {
    const cert = { CLIENT_CERTIFICATE_PATH: pemPath, CLIENT_ID: BOT, TENANT_ID: TENANT };
    expect(await warnings({ BRIDGE_FILES: "1", ...cert }, { clientId: BOT, clientSecret: "s" })).not.toContain(SECRET_ONLY_WARNING);
    expect(await warnings({ BRIDGE_FILES: "1" }, { clientId: BOT, tenantId: TENANT })).not.toContain(SECRET_ONLY_WARNING);
    expect(await warnings({}, { clientId: BOT, clientSecret: "s" })).not.toContain(SECRET_ONLY_WARNING);
  });
});
