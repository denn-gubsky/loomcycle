// A bridge as an external gate of loomcycle (RFC FA §3.5, §3.7d).
//
// The bridge already knows who is talking: the platform authenticated them.
// Instead of holding a loomcycle operator token and minting user tokens on
// its own, it tells loomcycle who the person is in a short-lived JWT signed
// with the gate's own key, and loomcycle decides the rest (which tenant, which
// subject, whether to create the user, which groups) and issues the token.
// loomcycle trusts only the signature: the key's public half is pinned in its
// `auth.gates.<name>` config, and nothing else the bridge says is believed.
//
// Issued tokens live in this process's memory only. They are never written
// anywhere — not to the conversation store, not to a file, not to a log — and
// a restart simply asks again. Each is renewed shortly before it expires.

import { createHmac, createPrivateKey, randomUUID, sign as cryptoSign, type KeyObject } from "node:crypto";
import { execFile } from "node:child_process";
import { readFile } from "node:fs/promises";

import type { TokenSource } from "./tokens.js";

/** The JWS algorithms a gate key signs with; loomcycle derives the same one
 *  from the public key it pinned. */
export type GateAlg = "EdDSA" | "ES256" | "HS256";

export interface GateKey {
  alg: GateAlg;
  kid?: string;
  sign(data: Buffer): Buffer;
}

/** keyFromPrivate builds a signing key from an Ed25519 (OKP) or P-256 (EC)
 *  private JWK, or a PKCS#8 PEM. */
export function keyFromPrivate(material: string, kid?: string): GateKey {
  const text = material.trim();
  let key: KeyObject;
  let jwkKid: string | undefined;
  if (text.startsWith("{")) {
    const jwk = JSON.parse(text) as Record<string, string>;
    if (!jwk.d) throw new Error("the gate key JWK has no private part (d)");
    jwkKid = jwk.kid;
    key = createPrivateKey({ key: jwk, format: "jwk" });
  } else {
    key = createPrivateKey(text);
  }
  const type = key.asymmetricKeyType;
  const id = kid ?? jwkKid;
  if (type === "ed25519") {
    return { alg: "EdDSA", kid: id, sign: (data) => cryptoSign(null, data, key) };
  }
  if (type === "ec" && key.asymmetricKeyDetails?.namedCurve === "prime256v1") {
    return { alg: "ES256", kid: id, sign: (data) => cryptoSign("sha256", data, { key, dsaEncoding: "ieee-p1363" }) };
  }
  throw new Error("the gate key must be Ed25519 or P-256");
}

/** keyFromSecret builds an HS256 key for a gate that shares a `$cred:` secret
 *  with a co-located loomcycle. At least 32 bytes, as loomcycle requires. */
export function keyFromSecret(secret: string): GateKey {
  const buf = Buffer.from(secret, "utf8");
  if (buf.length < 32) throw new Error("the gate secret must be at least 32 bytes");
  return { alg: "HS256", sign: (data) => createHmac("sha256", buf).update(data).digest() };
}

export type Exec = (cmd: string, args: string[]) => Promise<string>;

const defaultExec: Exec = (cmd, args) =>
  new Promise((resolve, reject) => {
    execFile(cmd, args, { timeout: 10_000, maxBuffer: 64 * 1024 }, (err, stdout) => (err ? reject(err) : resolve(stdout)));
  });

/** loadGateKey reads the gate's signing key from where the operator keeps it:
 *
 *  - `BRIDGE_GATE_KEY`: the private JWK or PEM itself (from the container's
 *    secret store), or `keychain:<service>/<account>` for the OS keychain
 *    (macOS `security`, Linux `secret-tool`), or `file:<path>`;
 *  - `BRIDGE_GATE_SECRET`: an HMAC secret instead (a co-located gate).
 *
 *  The key is read once, at start, and held in memory. */
export async function loadGateKey(
  env: Record<string, string | undefined>,
  opts: { exec?: Exec; platform?: NodeJS.Platform } = {},
): Promise<GateKey> {
  const spec = env.BRIDGE_GATE_KEY?.trim();
  const secret = env.BRIDGE_GATE_SECRET?.trim();
  if (spec && secret) throw new Error("set BRIDGE_GATE_KEY or BRIDGE_GATE_SECRET, not both");
  if (secret) return keyFromSecret(secret);
  if (!spec) throw new Error("BRIDGE_GATE_KEY (or BRIDGE_GATE_SECRET) is required for a gate");
  const kid = env.BRIDGE_GATE_KEY_ID?.trim() || undefined;
  if (spec.startsWith("file:")) return keyFromPrivate(await readFile(spec.slice(5), "utf8"), kid);
  if (spec.startsWith("keychain:")) {
    const [service, account] = spec.slice(9).split("/", 2);
    if (!service || !account) throw new Error("BRIDGE_GATE_KEY=keychain:<service>/<account>");
    const exec = opts.exec ?? defaultExec;
    const platform = opts.platform ?? process.platform;
    const out =
      platform === "darwin"
        ? await exec("security", ["find-generic-password", "-s", service, "-a", account, "-w"])
        : await exec("secret-tool", ["lookup", "service", service, "account", account]);
    return keyFromPrivate(out, kid);
  }
  return keyFromPrivate(spec, kid);
}

/** What the platform says about one person (or one shared conversation). */
export interface GateIdentity {
  /** The stable id at the platform: the JWT's `sub`. loomcycle's subject is
   *  "<gate>:<sub>". */
  sub: string;
  name?: string;
  email?: string;
  /** The platform groups the identity is in. undefined: not asserted (a
   *  group sync would otherwise remove what the bridge did not mention). */
  groups?: string[];
  /** Honoured by loomcycle only when the gate is `trusted: true`. */
  accessMode?: "isolated" | "tenant";
}

/** What the issued token is held to; loomcycle can only narrow it further. */
export interface TokenRequest {
  agents?: string[];
  overrides?: string[];
  ratePerMinute?: number;
  rateBurst?: number;
  ttlSeconds?: number;
}

export interface GateClientOptions {
  /** loomcycle's base URL, as the bridge reaches it. */
  baseUrl: string;
  /** The gate's name in loomcycle's `auth.gates` (the JWT's `iss`). */
  gate: string;
  /** loomcycle's issuer URL (its LOOMCYCLE_PUBLIC_URL): the JWT's `aud`. */
  audience: string;
  key: GateKey;
  token?: TokenRequest;
  /** Renew this long before an issued token expires. */
  renewBeforeMs?: number;
  /** An assertion's lifetime; loomcycle accepts at most 300 s. */
  assertionSeconds?: number;
  fetch?: typeof fetch;
  now?: () => number;
}

interface Issued {
  token: string;
  subject: string;
  expiresAt: number;
}

export class GateError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
  ) {
    super(`loomcycle refused the gate request: ${status} ${code}`);
    this.name = "GateError";
  }
}

const DEFAULT_RENEW_BEFORE_MS = 10 * 60 * 1000;

function b64url(buf: Buffer | string): string {
  return Buffer.from(buf).toString("base64url");
}

export class GateClient {
  private readonly cache = new Map<string, Issued>();
  private readonly inflight = new Map<string, Promise<Issued>>();
  private readonly fetchFn: typeof fetch;
  private readonly now: () => number;
  private readonly renewBeforeMs: number;
  private readonly assertionSeconds: number;

  constructor(private readonly opts: GateClientOptions) {
    this.fetchFn = opts.fetch ?? fetch;
    this.now = opts.now ?? Date.now;
    this.renewBeforeMs = opts.renewBeforeMs ?? DEFAULT_RENEW_BEFORE_MS;
    this.assertionSeconds = Math.min(Math.max(opts.assertionSeconds ?? 60, 10), 300);
  }

  /** The loomcycle subject an identity of this gate maps to. */
  subjectOf(sub: string): string {
    return `${this.opts.gate}:${sub}`;
  }

  /** sign makes one assertion JWT for use. */
  sign(use: "assert" | "revoke" | "sync", claims: Record<string, unknown>): string {
    const iat = Math.floor(this.now() / 1000);
    const header: Record<string, string> = { alg: this.opts.key.alg, typ: "JWT" };
    if (this.opts.key.kid) header.kid = this.opts.key.kid;
    const payload = {
      ...claims,
      iss: this.opts.gate,
      aud: this.opts.audience,
      jti: randomUUID(),
      iat,
      exp: iat + this.assertionSeconds,
      lc_use: use,
    };
    const input = `${b64url(JSON.stringify(header))}.${b64url(JSON.stringify(payload))}`;
    return `${input}.${b64url(this.opts.key.sign(Buffer.from(input)))}`;
  }

  /** tokenFor returns a valid issued token for the identity, asking loomcycle
   *  when there is none or it is about to expire. Concurrent calls for one
   *  identity share one request. */
  async tokenFor(id: GateIdentity): Promise<string> {
    const hit = this.cache.get(id.sub);
    if (hit && hit.expiresAt - this.renewBeforeMs > this.now()) return hit.token;
    const pending = this.inflight.get(id.sub);
    if (pending) return (await pending).token;
    const p = this.assert(id).finally(() => this.inflight.delete(id.sub));
    this.inflight.set(id.sub, p);
    return (await p).token;
  }

  /** forget drops a cached token, so the next call asks again (after a 401). */
  forget(sub: string): void {
    this.cache.delete(sub);
  }

  private async assert(id: GateIdentity): Promise<Issued> {
    const claims: Record<string, unknown> = { sub: id.sub };
    if (id.name) claims.name = id.name;
    if (id.email) claims.email = id.email;
    if (id.groups) claims.groups = id.groups;
    if (id.accessMode) claims.access_mode = id.accessMode;
    const t = this.opts.token;
    if (t) {
      claims.token = {
        ...(t.agents ? { agents: t.agents } : {}),
        ...(t.overrides ? { overrides: t.overrides } : {}),
        ...(t.ratePerMinute ? { rate_per_minute: t.ratePerMinute } : {}),
        ...(t.rateBurst ? { rate_burst: t.rateBurst } : {}),
        ...(t.ttlSeconds ? { ttl_seconds: t.ttlSeconds } : {}),
      };
    }
    const body = await this.post("/v1/_auth/assert", { assertion: this.sign("assert", claims) });
    const token = typeof body.token === "string" ? body.token : "";
    if (!token) throw new GateError(502, "no_token");
    const parsed = typeof body.expires_at === "string" ? Date.parse(body.expires_at) : NaN;
    const issued: Issued = {
      token,
      subject: typeof body.subject === "string" ? body.subject : this.subjectOf(id.sub),
      // No expiry reported: treat it as short-lived, so it is renewed soon.
      expiresAt: Number.isFinite(parsed) ? parsed : this.now() + this.renewBeforeMs * 2,
    };
    this.cache.set(id.sub, issued);
    return issued;
  }

  /** revoke tells loomcycle the person is gone: their sessions and issued
   *  tokens end and the removal policy takes over. The cached token goes too. */
  async revoke(sub: string): Promise<void> {
    this.cache.delete(sub);
    await this.post("/v1/_auth/revoke", { request: this.sign("revoke", { sub }) });
  }

  /** sync reconciles users and groups in bulk. With complete, users is every
   *  identity the gate knows: on an authoritative gate one missing from it is
   *  removed per loomcycle's policy. */
  async sync(users: GateIdentity[], opts: { complete?: boolean } = {}): Promise<Record<string, unknown>> {
    const list = users.map((u) => ({
      sub: u.sub,
      ...(u.name ? { name: u.name } : {}),
      ...(u.email ? { email: u.email } : {}),
      ...(u.groups ? { groups: u.groups } : {}),
      ...(u.accessMode ? { access_mode: u.accessMode } : {}),
    }));
    return this.post("/v1/_auth/sync", { request: this.sign("sync", { users: list, ...(opts.complete ? { complete: true } : {}) }) });
  }

  private async post(path: string, payload: Record<string, unknown>): Promise<Record<string, unknown>> {
    const res = await this.fetchFn(`${this.opts.baseUrl.replace(/\/+$/, "")}${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    let body: Record<string, unknown> = {};
    try {
      body = (await res.json()) as Record<string, unknown>;
    } catch {
      body = {};
    }
    if (!res.ok) {
      // loomcycle's error body is {code, error}; only the code is kept, so a
      // message can never carry anything into a log line.
      throw new GateError(res.status, typeof body.code === "string" ? body.code : "error");
    }
    return body;
  }
}

/** GroupsFor says which platform groups a conversation's run identity is in,
 *  from where the conversation is (its reply reference). undefined: unknown,
 *  so nothing is asserted and loomcycle syncs nothing. */
export type GroupsFor = (subject: string, ref: unknown) => Promise<string[] | undefined>;

/** GateBroker is the TokenSource of a bridge that is an external gate: a run
 *  identity "<gate>:<sub>" gets the token loomcycle issues for `sub`. The key
 *  is read at start (ready), so building a bridge stays synchronous. */
export class GateBroker implements TokenSource {
  private client?: GateClient;

  constructor(
    private readonly opts: Omit<GateClientOptions, "key">,
    private readonly groupsFor?: GroupsFor,
  ) {}

  ready(key: GateKey): void {
    this.client = new GateClient({ ...this.opts, key });
  }

  /** subjectOf is the loomcycle subject of a platform id. */
  subjectOf(sub: string): string {
    return `${this.opts.gate}:${sub}`;
  }

  private started(): GateClient {
    if (!this.client) throw new Error("the gate key is not loaded yet (the bridge has not started)");
    return this.client;
  }

  private subOf(subject: string): string {
    const prefix = `${this.opts.gate}:`;
    if (!subject.startsWith(prefix) || subject.length === prefix.length) {
      throw new Error(`subject ${JSON.stringify(subject)} is not one of gate ${this.opts.gate}'s`);
    }
    return subject.slice(prefix.length);
  }

  async tokenFor(subject: string, displayName?: string, ref?: unknown): Promise<string> {
    const client = this.started();
    const sub = this.subOf(subject);
    // A failed lookup asserts no groups rather than an empty list: an empty
    // list would remove every group the gate gave the user.
    const groups = this.groupsFor && ref !== undefined ? await this.groupsFor(subject, ref).catch(() => undefined) : undefined;
    return client.tokenFor({ sub, name: displayName, groups });
  }

  forget(subject: string): void {
    if (this.client && subject.startsWith(`${this.opts.gate}:`)) this.client.forget(subject.slice(this.opts.gate.length + 1));
  }

  /** revoke tells loomcycle the identity is gone (§3.7c removal). */
  async revoke(subject: string): Promise<void> {
    await this.started().revoke(this.subOf(subject));
  }
}
