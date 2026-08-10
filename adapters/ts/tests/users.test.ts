import { describe, expect, it } from "vitest";
import { LoomcycleError } from "../src/errors.js";
import {
  errorResponse,
  jsonResponse,
  makeClient,
  noContentResponse,
} from "./helpers.js";

// RFC BX Phase 2 (loomcycle v1.50.0) — tenant-owned users + delegated per-user
// token minting. Thin REST wrappers over /v1/_users; these tests pin the wire
// shape (verb, URL, body) + the typed error surface. The tenant is always
// server-derived from the bearer, so it never appears on the wire here.

describe("createUser", () => {
  it("POSTs the body to /v1/_users", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        tenant_id: "acme",
        subject: "alice",
        display_name: "Alice",
        access_mode: "isolated",
        status: "active",
        created_at: "2026-08-10T00:00:00Z",
        created_by: "root",
      }),
    ]);

    const rec = await client.createUser({
      subject: "alice",
      display_name: "Alice",
      access_mode: "isolated",
    });

    expect(rec.subject).toBe("alice");
    expect(rec.access_mode).toBe("isolated");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_users");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({
      subject: "alice",
      display_name: "Alice",
      access_mode: "isolated",
    });
  });

  it("surfaces a 409 duplicate as a typed error", async () => {
    const { client } = makeClient([
      errorResponse(409, `{"code":"user_exists","error":"already exists"}`),
    ]);
    await expect(client.createUser({ subject: "alice" })).rejects.toBeInstanceOf(
      LoomcycleError,
    );
  });
});

describe("updateUser", () => {
  it("PATCHes the (subject) URL with only the provided keys", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        tenant_id: "acme",
        subject: "alice",
        display_name: "Alice",
        access_mode: "tenant",
        status: "disabled",
        created_at: "2026-08-10T00:00:00Z",
        created_by: "root",
      }),
    ]);

    const rec = await client.updateUser("alice", { status: "disabled" });

    expect(rec.status).toBe("disabled");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_users/alice");
    expect(init.method).toBe("PATCH");
    expect(JSON.parse(init.body as string)).toEqual({ status: "disabled" });
  });

  it("URL-encodes the subject", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        tenant_id: "acme",
        subject: "a/b",
        display_name: "",
        access_mode: "tenant",
        status: "active",
        created_at: "2026-08-10T00:00:00Z",
        created_by: "root",
      }),
    ]);
    await client.updateUser("a/b", { display_name: "x" });
    const [url] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_users/a%2Fb");
  });
});

describe("deleteUser", () => {
  it("DELETEs the (subject) URL", async () => {
    const { client, fetchMock } = makeClient([noContentResponse()]);
    await client.deleteUser("alice");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_users/alice");
    expect(init.method).toBe("DELETE");
  });
});

describe("mintUserToken", () => {
  it("POSTs to /v1/_users/{subject}/tokens with NO body and returns the plaintext once", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "def_abc",
        token: "lc_secret_plaintext",
        token_suffix: "aintext",
        name: "u-alice",
        scopes: ["substrate:user"],
        created_at: "2026-08-10T00:00:00Z",
        warning: "store this token now",
      }),
    ]);

    const minted = await client.mintUserToken("alice");

    expect(minted.token).toBe("lc_secret_plaintext");
    expect(minted.scopes).toEqual(["substrate:user"]);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_users/alice/tokens");
    expect(init.method).toBe("POST");
    // Scopes are DERIVED server-side, so the mint call sends no body.
    expect(init.body).toBeUndefined();
  });

  it("surfaces a 409 disabled-user as typed", async () => {
    const { client } = makeClient([
      errorResponse(409, `{"code":"user_disabled","error":"disabled"}`),
    ]);
    await expect(client.mintUserToken("alice")).rejects.toBeInstanceOf(
      LoomcycleError,
    );
  });
});

describe("listUserTokens", () => {
  it("GETs metadata for the (subject) tokens", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        subject: "alice",
        tokens: [
          {
            def_id: "def_abc",
            name: "u-alice",
            scopes: ["substrate:user"],
            created_at: "2026-08-10T00:00:00Z",
            active: true,
          },
        ],
      }),
    ]);

    const resp = await client.listUserTokens("alice");

    expect(resp.subject).toBe("alice");
    expect(resp.tokens[0]!.active).toBe(true);
    // Metadata only — never the plaintext.
    expect((resp.tokens[0] as Record<string, unknown>).token).toBeUndefined();
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_users/alice/tokens");
    expect(init.method).toBe("GET");
  });
});

describe("revokeUserToken", () => {
  it("DELETEs the (subject, def_id) URL and returns the echo", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ def_id: "def_abc", retired_at: "2026-08-10T01:00:00Z" }),
    ]);

    const resp = await client.revokeUserToken("alice", "def_abc");

    expect(resp.def_id).toBe("def_abc");
    expect(resp.retired_at).toBe("2026-08-10T01:00:00Z");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_users/alice/tokens/def_abc");
    expect(init.method).toBe("DELETE");
  });

  it("surfaces an opaque 404 as typed", async () => {
    const { client } = makeClient([
      errorResponse(404, `{"code":"token_not_found","error":"no such token"}`),
    ]);
    await expect(
      client.revokeUserToken("alice", "def_xyz"),
    ).rejects.toBeInstanceOf(LoomcycleError);
  });
});
