import { createHmac, generateKeyPairSync, verify as cryptoVerify, type KeyObject } from "node:crypto";
import fs from "node:fs";
import { mkdtemp, readFile, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { afterEach, describe, expect, it, vi } from "vitest";

import { EnvReader, loadSharedConfig } from "../src/config.js";
import { GateBroker, GateClient, GateError, keyFromPrivate, keyFromSecret, loadGateKey, type GateKey } from "../src/gate.js";

const AUD = "https://lc.example";

function edKey(): { key: GateKey; pub: KeyObject; jwk: string } {
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  const jwk = JSON.stringify(privateKey.export({ format: "jwk" }));
  return { key: keyFromPrivate(jwk), pub: publicKey, jwk };
}

function decode(jwt: string): { header: Record<string, unknown>; payload: Record<string, any>; input: string; sig: Buffer } {
  const [h, p, s] = jwt.split(".");
  return {
    header: JSON.parse(Buffer.from(h!, "base64url").toString()),
    payload: JSON.parse(Buffer.from(p!, "base64url").toString()),
    input: `${h}.${p}`,
    sig: Buffer.from(s!, "base64url"),
  };
}

/** A fake loomcycle: records each posted body and answers an issued token. */
function fakeLoomcycle(opts: { ttlMs?: number; status?: number; now?: () => number } = {}) {
  const calls: { path: string; body: Record<string, any> }[] = [];
  let n = 0;
  const fetchFn = (async (url: string, init: RequestInit) => {
    const body = JSON.parse(String(init.body));
    calls.push({ path: new URL(url).pathname, body });
    if (opts.status) return new Response(JSON.stringify({ code: "invalid_assertion", error: "x" }), { status: opts.status });
    const exp = new Date((opts.now ?? Date.now)() + (opts.ttlMs ?? 12 * 3600_000)).toISOString();
    n++;
    return new Response(JSON.stringify({ token: `lct_issued_${n}`, expires_at: exp, subject: "tg:1" }), { status: 200 });
  }) as unknown as typeof fetch;
  return { calls, fetchFn };
}

describe("gate client", () => {
  afterEach(() => vi.restoreAllMocks());

  it("signs an assertion loomcycle can verify under the pinned public key", async () => {
    const { key, pub } = edKey();
    const lc = fakeLoomcycle();
    const gc = new GateClient({ baseUrl: "http://lc:8080/", gate: "tg", audience: AUD, key, fetch: lc.fetchFn });
    await gc.tokenFor({ sub: "1", name: "Ann", groups: ["tg-chat-5"] });
    expect(lc.calls[0]!.path).toBe("/v1/_auth/assert");
    const jwt = decode(lc.calls[0]!.body.assertion);
    expect(jwt.header).toMatchObject({ alg: "EdDSA", typ: "JWT" });
    expect(cryptoVerify(null, Buffer.from(jwt.input), pub, jwt.sig)).toBe(true);
    expect(jwt.payload).toMatchObject({ iss: "tg", aud: AUD, sub: "1", lc_use: "assert", name: "Ann", groups: ["tg-chat-5"] });
    expect(jwt.payload.exp - jwt.payload.iat).toBeLessThanOrEqual(300);
    expect(typeof jwt.payload.jti).toBe("string");
  });

  it("signs ES256 in the JOSE (r||s) form and HS256 with the shared secret", () => {
    const { privateKey, publicKey } = generateKeyPairSync("ec", { namedCurve: "P-256" });
    const es = new GateClient({ baseUrl: "http://lc", gate: "g", audience: AUD, key: keyFromPrivate(privateKey.export({ format: "pem", type: "pkcs8" }).toString()) });
    const a = decode(es.sign("assert", { sub: "1" }));
    expect(a.header.alg).toBe("ES256");
    expect(a.sig.length).toBe(64);
    expect(cryptoVerify("sha256", Buffer.from(a.input), { key: publicKey, dsaEncoding: "ieee-p1363" }, a.sig)).toBe(true);

    const secret = "s".repeat(40);
    const hs = new GateClient({ baseUrl: "http://lc", gate: "g", audience: AUD, key: keyFromSecret(secret) });
    const b = decode(hs.sign("revoke", { sub: "1" }));
    expect(b.header.alg).toBe("HS256");
    expect(b.sig.equals(createHmac("sha256", secret).update(b.input).digest())).toBe(true);
    expect(() => keyFromSecret("short")).toThrow();
  });

  it("asks once for a burst, caches in memory and renews before expiry", async () => {
    let now = 1_000_000;
    const lc = fakeLoomcycle({ ttlMs: 60 * 60_000, now: () => now });
    const gc = new GateClient({ baseUrl: "http://lc", gate: "tg", audience: AUD, key: edKey().key, fetch: lc.fetchFn, now: () => now });
    const tokens = await Promise.all(Array.from({ length: 10 }, () => gc.tokenFor({ sub: "1" })));
    expect(new Set(tokens).size).toBe(1);
    expect(lc.calls).toHaveLength(1);
    now += 45 * 60_000;
    expect(await gc.tokenFor({ sub: "1" })).toBe(tokens[0]);
    now += 6 * 60_000; // inside the 10-minute renewal window
    expect(await gc.tokenFor({ sub: "1" })).not.toBe(tokens[0]);
    expect(lc.calls).toHaveLength(2);
    // Each assertion is a fresh JWT: a new jti every time.
    expect(decode(lc.calls[0]!.body.assertion).payload.jti).not.toBe(decode(lc.calls[1]!.body.assertion).payload.jti);
  });

  it("omits groups it was not told, so loomcycle syncs nothing then", async () => {
    const lc = fakeLoomcycle();
    const gc = new GateClient({ baseUrl: "http://lc", gate: "tg", audience: AUD, key: edKey().key, fetch: lc.fetchFn });
    await gc.tokenFor({ sub: "1" });
    expect("groups" in decode(lc.calls[0]!.body.assertion).payload).toBe(false);
  });

  it("revokes: drops the cached token and posts a signed revoke", async () => {
    const lc = fakeLoomcycle();
    const gc = new GateClient({ baseUrl: "http://lc", gate: "tg", audience: AUD, key: edKey().key, fetch: lc.fetchFn });
    await gc.tokenFor({ sub: "1" });
    await gc.revoke("1");
    expect(lc.calls[1]!.path).toBe("/v1/_auth/revoke");
    expect(decode(lc.calls[1]!.body.request).payload).toMatchObject({ lc_use: "revoke", sub: "1" });
    await gc.tokenFor({ sub: "1" });
    expect(lc.calls).toHaveLength(3);
  });

  it("reports a refusal by status and code only", async () => {
    const lc = fakeLoomcycle({ status: 401 });
    const gc = new GateClient({ baseUrl: "http://lc", gate: "tg", audience: AUD, key: edKey().key, fetch: lc.fetchFn });
    const err = await gc.tokenFor({ sub: "1" }).catch((e) => e);
    expect(err).toBeInstanceOf(GateError);
    expect(err.status).toBe(401);
    expect(err.code).toBe("invalid_assertion");
  });

  // RFC FA §8 (bridges): bridge-core never writes a token to disk.
  it("never writes an issued token anywhere on disk", async () => {
    const writes: string[] = [];
    for (const m of ["writeFile", "writeFileSync", "appendFile", "appendFileSync", "createWriteStream", "openSync", "open"] as const) {
      vi.spyOn(fs, m as any).mockImplementation(((...args: unknown[]) => {
        writes.push(`${m}:${String(args[0])}`);
        throw new Error("no disk writes");
      }) as any);
    }
    for (const m of ["writeFile", "appendFile", "open"] as const) {
      vi.spyOn(fs.promises, m as any).mockImplementation(((...args: unknown[]) => {
        writes.push(`promises.${m}:${String(args[0])}`);
        return Promise.reject(new Error("no disk writes"));
      }) as any);
    }
    const lc = fakeLoomcycle();
    const gc = new GateClient({ baseUrl: "http://lc", gate: "tg", audience: AUD, key: edKey().key, fetch: lc.fetchFn });
    const tok = await gc.tokenFor({ sub: "1" });
    expect(tok).toMatch(/^lct_issued_/);
    await gc.revoke("1");
    expect(writes).toEqual([]);
    // And by construction: the module imports no write API at all.
    const src = await readFile(path.join(path.dirname(fileURLToPath(import.meta.url)), "../src/gate.ts"), "utf8");
    const fsImports = [...src.matchAll(/import\s*\{([^}]*)\}\s*from\s*"node:fs(?:\/promises)?"/g)].flatMap((m) => m[1]!.split(",").map((s) => s.trim()));
    expect(fsImports).toEqual(["readFile"]);
    expect(src).not.toMatch(/from\s*"node:fs"\s*;?\s*$/m);
  });
});

describe("gate broker", () => {
  it("maps <gate>:<sub> subjects, asks with the conversation's groups, and refuses another gate's subject", async () => {
    const lc = fakeLoomcycle();
    const seen: unknown[] = [];
    const b = new GateBroker({ baseUrl: "http://lc", gate: "telegram", audience: AUD, fetch: lc.fetchFn }, async (subject, ref) => {
      seen.push([subject, ref]);
      return ["tg-chat--100"];
    });
    await expect(b.tokenFor("telegram:1")).rejects.toThrow(/not loaded/);
    b.ready(edKey().key);
    await b.tokenFor("telegram:conv:abc", "Group", { chatId: -100 });
    const p = decode(lc.calls[0]!.body.assertion).payload;
    expect(p).toMatchObject({ sub: "conv:abc", name: "Group", groups: ["tg-chat--100"] });
    expect(seen).toEqual([["telegram:conv:abc", { chatId: -100 }]]);
    await expect(b.tokenFor("slack:1")).rejects.toThrow(/not one of gate telegram/);
    await expect(b.tokenFor("telegram:")).rejects.toThrow();
    expect(b.subjectOf("7")).toBe("telegram:7");
  });

  it("asserts no groups when the lookup fails, so nothing is removed", async () => {
    const lc = fakeLoomcycle();
    const b = new GateBroker({ baseUrl: "http://lc", gate: "telegram", audience: AUD, fetch: lc.fetchFn }, async () => {
      throw new Error("Bot API down");
    });
    b.ready(edKey().key);
    await b.tokenFor("telegram:1", "Ann", { chatId: 1 });
    expect("groups" in decode(lc.calls[0]!.body.assertion).payload).toBe(false);
  });
});

describe("gate configuration", () => {
  const base = { LOOMCYCLE_BASE_URL: "http://lc:8080", BRIDGE_AGENT: "chat", BRIDGE_GATE: "telegram", BRIDGE_GATE_AUDIENCE: "https://lc.example/", BRIDGE_GATE_KEY: "{}" };

  it("reads the gate and holds no loomcycle token", () => {
    const r = new EnvReader(base);
    const cfg = loadSharedConfig(r, { port: 1, gate: true });
    r.check("t");
    expect(cfg.gate).toMatchObject({ name: "telegram", audience: "https://lc.example" });
    expect(cfg.loomcycleToken).toBe("");
  });

  it("refuses a leftover operator token, a bridge-side access mode, no key, or a plain-http audience", () => {
    for (const [env, want] of [
      [{ ...base, LOOMCYCLE_TOKEN: "lct_x" }, /LOOMCYCLE_TOKEN must not be set/],
      [{ ...base, BRIDGE_USER_ACCESS_MODE: "tenant" }, /identity_map/],
      [{ ...base, BRIDGE_GATE_KEY: undefined }, /BRIDGE_GATE_KEY/],
      [{ ...base, BRIDGE_GATE_AUDIENCE: "http://lc.example" }, /https/],
      [{ ...base, BRIDGE_GATE: "Tele Gram" }, /gate name/],
    ] as const) {
      const r = new EnvReader(env);
      loadSharedConfig(r, { port: 1, gate: true });
      expect(() => r.check("t")).toThrow(want);
    }
  });

  it("leaves a bridge that is not a gate exactly as it was", () => {
    const r = new EnvReader({ LOOMCYCLE_BASE_URL: "http://lc", LOOMCYCLE_TOKEN: "lct_x", BRIDGE_AGENT: "chat" });
    const cfg = loadSharedConfig(r, { port: 1 });
    r.check("t");
    expect(cfg.gate).toBeUndefined();
    expect(cfg.loomcycleToken).toBe("lct_x");
  });
});

describe("gate key loading", () => {
  it("reads a JWK or PEM from the environment, a file, or the OS keychain", async () => {
    const { jwk } = edKey();
    expect((await loadGateKey({ BRIDGE_GATE_KEY: jwk })).alg).toBe("EdDSA");
    const dir = await mkdtemp(path.join(os.tmpdir(), "gate-key-"));
    const file = path.join(dir, "k.jwk");
    await writeFile(file, jwk, { mode: 0o600 });
    expect((await loadGateKey({ BRIDGE_GATE_KEY: `file:${file}` })).alg).toBe("EdDSA");

    const seen: string[][] = [];
    const exec = async (cmd: string, args: string[]) => {
      seen.push([cmd, ...args]);
      return jwk;
    };
    await loadGateKey({ BRIDGE_GATE_KEY: "keychain:loomcycle-gate/telegram" }, { exec, platform: "darwin" });
    await loadGateKey({ BRIDGE_GATE_KEY: "keychain:loomcycle-gate/telegram" }, { exec, platform: "linux" });
    expect(seen).toEqual([
      ["security", "find-generic-password", "-s", "loomcycle-gate", "-a", "telegram", "-w"],
      ["secret-tool", "lookup", "service", "loomcycle-gate", "account", "telegram"],
    ]);
    expect((await loadGateKey({ BRIDGE_GATE_SECRET: "x".repeat(32) })).alg).toBe("HS256");
  });

  it("refuses a public key, both kinds at once, or none", async () => {
    const { publicKey } = generateKeyPairSync("ed25519");
    await expect(loadGateKey({ BRIDGE_GATE_KEY: JSON.stringify(publicKey.export({ format: "jwk" })) })).rejects.toThrow(/private/);
    await expect(loadGateKey({ BRIDGE_GATE_KEY: "{}", BRIDGE_GATE_SECRET: "x".repeat(32) })).rejects.toThrow(/not both/);
    await expect(loadGateKey({})).rejects.toThrow(/required/);
  });
});

describe("keyFromPrivate with malformed key material", () => {
  it("never echoes the key into the error a start-up crash would log", () => {
    const secretish = "SECRETSECRETSECRET";
    for (const material of [`{"kty":"OKP","d":${secretish}`, `-----BEGIN PRIVATE KEY-----\n${secretish}\n-----END PRIVATE KEY-----`]) {
      let message = "";
      try {
        keyFromPrivate(material);
      } catch (err) {
        message = String((err as Error).message);
      }
      expect(message).not.toBe("");
      // Node quotes only a prefix of the input ("SECRETSECR…"), so look for
      // any piece of it.
      expect(message).not.toContain("SECRET");
    }
  });
});
