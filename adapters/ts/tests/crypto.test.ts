// adapters/ts/tests/crypto.test.ts — RFC EP: the Crypto tool outside a run
// (POST /v1/_crypto) and the crypto-key ops of the credential store
// (POST /v1/_credentialdef). Each helper must post its op-discriminated body
// verbatim; crypto() must pass the admin-only `tenant` through; refusals and a
// spent run rate must surface as the adapter's typed errors.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient } from "./helpers.js";
import {
  BackpressureError,
  RateLimitedError,
  SubstrateToolRefusedError,
  type CryptoDecryptResult,
  type CryptoEncryptResult,
} from "../src/index.js";

const CREDDEF = "http://test-loomcycle:8787/v1/_credentialdef";
const CRYPTO = "http://test-loomcycle:8787/v1/_crypto";

function sent(fetchMock: { mock: { calls: unknown[][] } }, i = 0): { url: string; method: string; body: unknown } {
  const call = fetchMock.mock.calls[i]!;
  const init = call[1] as RequestInit;
  return { url: call[0] as string, method: init.method ?? "", body: JSON.parse(init.body as string) };
}

const keyMeta = {
  name: "customer-fields",
  scope: "tenant",
  backend: "inline",
  usage: "crypto",
  key_type: "aes-256-gcm",
  current_version: 2,
  min_version: 1,
  versions: [
    { version: 1, created_at: "2026-10-01T00:00:00Z", retired: false },
    { version: 2, created_at: "2026-10-17T00:00:00Z", retired: false },
  ],
  status: "rotated",
};

describe("crypto keys (CredentialDef)", () => {
  it("generateCryptoKey posts op=generate with scope/name/key_type", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ...keyMeta, current_version: 1, status: "stored" })]);

    const meta = await client.generateCryptoKey({ scope: "tenant", name: "customer-fields", key_type: "aes-256-gcm" });
    expect(meta.usage).toBe("crypto");
    expect(meta.key_type).toBe("aes-256-gcm");

    const s = sent(fetchMock);
    expect(s.url).toBe(CREDDEF);
    expect(s.method).toBe("POST");
    expect(s.body).toEqual({ op: "generate", scope: "tenant", name: "customer-fields", key_type: "aes-256-gcm" });
  });

  it("createCryptoKey posts op=create with usage crypto and the material", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ...keyMeta, status: "stored" })]);

    await client.createCryptoKey({
      scope: "user",
      name: "partner-hmac",
      key_type: "hmac-sha256",
      value: "c2VjcmV0",
      key_encoding: "base64",
    });

    const s = sent(fetchMock);
    expect(s.url).toBe(CREDDEF);
    expect(s.body).toEqual({
      op: "create",
      usage: "crypto",
      scope: "user",
      name: "partner-hmac",
      key_type: "hmac-sha256",
      value: "c2VjcmV0",
      key_encoding: "base64",
    });
  });

  it("createKmsCryptoKey posts the backend and the external reference", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ...keyMeta, key_backend: "vault", status: "stored" })]);

    const meta = await client.createKmsCryptoKey({
      scope: "tenant",
      name: "orders",
      key_type: "aes-256-gcm",
      external: { backend: "vault", address: "https://vault.example:8200", key: "orders", token: "$cred:vault-token" },
    });
    expect(meta.key_backend).toBe("vault");

    const s = sent(fetchMock);
    expect(s.url).toBe(CREDDEF);
    expect(s.body).toEqual({
      op: "create",
      usage: "crypto",
      scope: "tenant",
      name: "orders",
      key_type: "aes-256-gcm",
      backend: "vault",
      external: { address: "https://vault.example:8200", key: "orders", token: "$cred:vault-token" },
    });
  });

  it("rotateCryptoKey posts op=rotate and returns the versions metadata", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(keyMeta)]);

    const meta = await client.rotateCryptoKey({ scope: "tenant", name: "customer-fields" });
    expect(meta.current_version).toBe(2);
    expect(meta.min_version).toBe(1);
    expect(meta.versions).toHaveLength(2);
    expect(meta.versions![1]!.retired).toBe(false);

    expect(sent(fetchMock).body).toEqual({ op: "rotate", scope: "tenant", name: "customer-fields" });
  });

  it("setCryptoKeyMinVersion posts op=set_min_version with min_version", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ...keyMeta, min_version: 2, status: "min_version set" })]);

    const meta = await client.setCryptoKeyMinVersion({ scope: "tenant", name: "customer-fields", min_version: 2 });
    expect(meta.min_version).toBe(2);

    expect(sent(fetchMock).body).toEqual({
      op: "set_min_version",
      scope: "tenant",
      name: "customer-fields",
      min_version: 2,
    });
  });

  it("deleteCryptoKeyVersion posts op=delete_version with version", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ...keyMeta, status: "version deleted" })]);

    await client.deleteCryptoKeyVersion({ scope: "tenant", name: "customer-fields", version: 1 });

    expect(sent(fetchMock).body).toEqual({ op: "delete_version", scope: "tenant", name: "customer-fields", version: 1 });
  });

  it("exportCryptoKey posts op=export_key with the passphrase and returns the blob", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ name: "customer-fields", scope: "tenant", blob: "eyJmb3JtYXQiOiJ4In0" }),
    ]);

    const out = await client.exportCryptoKey({
      scope: "tenant",
      name: "customer-fields",
      passphrase: "correct horse battery staple",
    });
    expect(out.blob).toBe("eyJmb3JtYXQiOiJ4In0");

    expect(sent(fetchMock).body).toEqual({
      op: "export_key",
      scope: "tenant",
      name: "customer-fields",
      passphrase: "correct horse battery staple",
    });
  });

  it("importCryptoKey posts op=import_key with blob and passphrase", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ...keyMeta, status: "imported" })]);

    const meta = await client.importCryptoKey({
      scope: "tenant",
      name: "customer-fields",
      blob: "eyJmb3JtYXQiOiJ4In0",
      passphrase: "correct horse battery staple",
    });
    expect(meta.status).toBe("imported");

    expect(sent(fetchMock).body).toEqual({
      op: "import_key",
      scope: "tenant",
      name: "customer-fields",
      blob: "eyJmb3JtYXQiOiJ4In0",
      passphrase: "correct horse battery staple",
    });
  });

  it("exportCryptoKey raises SubstrateToolRefusedError for a non-admin token", async () => {
    const { client } = makeClient([
      jsonResponse(
        {
          code: "tool_refused",
          tool: "CredentialDef",
          error: "export_key: only an operator, outside a run, moves a crypto key between deployments",
        },
        422,
      ),
    ]);

    await expect(
      client.exportCryptoKey({ scope: "tenant", name: "k", passphrase: "0123456789abcdef" }),
    ).rejects.toBeInstanceOf(SubstrateToolRefusedError);
  });
});

describe("crypto()", () => {
  it("posts the op body verbatim to /v1/_crypto and types the result by op", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ ciphertext: "lcx1:kt:customer-fields:2:QUJD", key_version: 2 }),
    ]);

    const res: CryptoEncryptResult = await client.crypto({
      op: "encrypt",
      key: "customer-fields",
      plaintext: "4111 1111 1111 1111",
      context: "customer:42/card",
    });
    expect(res.ciphertext).toBe("lcx1:kt:customer-fields:2:QUJD");
    expect(res.key_version).toBe(2);

    const s = sent(fetchMock);
    expect(s.url).toBe(CRYPTO);
    expect(s.method).toBe("POST");
    expect(s.body).toEqual({
      op: "encrypt",
      key: "customer-fields",
      plaintext: "4111 1111 1111 1111",
      context: "customer:42/card",
    });
  });

  it("passes the admin-only tenant through as a top-level field", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ plaintext: "hello", encoding: "text", key_version: 1 }),
    ]);

    const res: CryptoDecryptResult = await client.crypto({
      op: "decrypt",
      ciphertext: "lcx1:kt:customer-fields:1:QUJD",
      sensitive: true,
      tenant: "acme",
    });
    expect(res.plaintext).toBe("hello");
    expect(res.encoding).toBe("text");

    expect(sent(fetchMock).body).toEqual({
      op: "decrypt",
      ciphertext: "lcx1:kt:customer-fields:1:QUJD",
      sensitive: true,
      tenant: "acme",
    });
  });

  it("sends a keyless op (random) with only its own fields", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ kind: "token", value: "abc" })]);

    const res = await client.crypto({ op: "random", kind: "token", length: 24 });
    expect(res.value).toBe("abc");

    expect(sent(fetchMock).body).toEqual({ op: "random", kind: "token", length: 24 });
  });

  it("raises SubstrateToolRefusedError on a 422 tool refusal", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "tool_refused", tool: "Crypto", error: "did not decrypt" }, 422),
    ]);

    const err = await client
      .crypto({ op: "decrypt", ciphertext: "lcx1:kt:k:1:QUJD" })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(SubstrateToolRefusedError);
    expect((err as SubstrateToolRefusedError).tool).toBe("Crypto");
    expect((err as SubstrateToolRefusedError).message).toBe("did not decrypt");
  });

  it("raises RateLimitedError with Retry-After on a 429 rate_limited", async () => {
    const { client } = makeClient([
      () =>
        new Response(JSON.stringify({ code: "rate_limited", error: "run-start rate limit reached" }), {
          status: 429,
          headers: { "Content-Type": "application/json", "Retry-After": "4" },
        }),
    ]);

    const err = await client.crypto({ op: "hash", data: "x" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(RateLimitedError);
    expect(err).toBeInstanceOf(BackpressureError);
    expect((err as RateLimitedError).retryAfterMs).toBe(4000);
    expect((err as RateLimitedError).retryable).toBe(true);
  });
});
