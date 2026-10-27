import { describe, expect, it } from "vitest";
import { LoomcycleError } from "../src/errors.js";
import type { UserStatus } from "../src/index.js";
import {
  errorResponse,
  jsonResponse,
  makeClient,
  noContentResponse,
} from "./helpers.js";

// RFC BX Phase 2 (loomcycle v1.50.0) — tenant-owned users + delegated per-user
// token minting. Thin REST wrappers over /v1/_users; these tests pin the wire
// shape (verb, URL, body) + the typed error surface. The tenant is derived
// from the bearer; only `opts.tenant` (an admin's or an auth:manage
// credential's choice) puts it on the wire, as ?tenant=, never in a body.

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

  it("sends ttl_seconds / name / max_active when asked, and returns expires_at", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "def_abc",
        token: "lc_secret_plaintext",
        token_suffix: "aintext",
        name: "laptop-1",
        scopes: ["substrate:user"],
        created_at: "2026-10-12T00:00:00Z",
        expires_at: "2026-10-13T00:00:00Z",
        retired: ["def_old"],
        warning: "store this token now",
      }),
    ]);

    const minted = await client.mintUserToken("alice", {
      ttlSeconds: 86400,
      name: "laptop-1",
      maxActive: 2,
      agents: ["score"],
      overrides: "none",
      rate: { perMinute: 6, burst: 20 },
    });

    expect(minted.expires_at).toBe("2026-10-13T00:00:00Z");
    expect(minted.retired).toEqual(["def_old"]);
    const [, init] = fetchMock.mock.calls[0]!;
    expect(JSON.parse(init.body as string)).toEqual({
      ttl_seconds: 86400,
      name: "laptop-1",
      max_active: 2,
      agents: ["score"],
      overrides: "none",
      rate: { per_minute: 6, burst: 20 },
    });
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
    expect((resp.tokens[0] as unknown as Record<string, unknown>).token).toBeUndefined();
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

const BOB = {
  tenant_id: "acme",
  subject: "bob",
  display_name: "Bob",
  access_mode: "isolated",
  status: "active",
  created_at: "2026-10-27T00:00:00Z",
  created_by: "gate:sso",
};

describe("user CRUD with opts.tenant", () => {
  it("createUser sends the tenant as ?tenant=, not in the body", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(BOB, 201)]);
    await client.createUser({ subject: "bob" }, { tenant: "acme" });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_users?tenant=acme");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({ subject: "bob" });
  });

  it("updateUser and deleteUser send the tenant as ?tenant=", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(BOB), noContentResponse()]);
    await client.updateUser("bob", { status: "disabled" }, { tenant: "acme" });
    await client.deleteUser("bob", { tenant: "acme" });
    const [patchURL, patchInit] = fetchMock.mock.calls[0]!;
    expect(patchURL).toBe("http://test-loomcycle:8787/v1/_users/bob?tenant=acme");
    expect(patchInit.method).toBe("PATCH");
    expect(JSON.parse(patchInit.body as string)).toEqual({ status: "disabled" });
    const [delURL, delInit] = fetchMock.mock.calls[1]!;
    expect(delURL).toBe("http://test-loomcycle:8787/v1/_users/bob?tenant=acme");
    expect(delInit.method).toBe("DELETE");
  });

  it("leaves the URL bare without a tenant", async () => {
    const { client, fetchMock } = makeClient([noContentResponse()]);
    await client.deleteUser("bob");
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/_users/bob");
  });
});

describe("pending users", () => {
  it("listUsers surfaces a user awaiting approval as status pending", async () => {
    const { client } = makeClient([
      jsonResponse({
        users: [
          { user_id: "bob", running_count: 0, total_count: 0, last_started_at: "0001-01-01T00:00:00Z",
            registered: true, display_name: "Bob", access_mode: "isolated", status: "pending" },
        ],
      }),
    ]);
    const { users } = await client.listUsers();
    const awaiting = users.filter((u) => u.status === "pending").map((u) => u.user_id);
    expect(awaiting).toEqual(["bob"]);
  });

  it("UserStatus names all three statuses", () => {
    // Compile-time: a status missing from the union fails typecheck:tests.
    const all: UserStatus[] = ["active", "disabled", "pending"];
    expect(all).toHaveLength(3);
  });
});

describe("approveUser", () => {
  it("POSTs to /v1/_users/{subject}/approve with no body and returns the active record", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(BOB)]);
    const rec = await client.approveUser("bob", { tenant: "acme" });
    expect(rec.status).toBe("active");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_users/bob/approve?tenant=acme");
    expect(init.method).toBe("POST");
    expect(init.body).toBeUndefined();
  });

  it("surfaces a user that is not pending as a 409 with code not_pending", async () => {
    const { client } = makeClient([
      errorResponse(409, `{"code":"not_pending","error":"this user is not awaiting approval"}`),
    ]);
    const err = await client.approveUser("bob").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(409);
    expect((err as LoomcycleError).code).toBe("not_pending");
  });
});

describe("listUserIdentities", () => {
  it("GETs /v1/_users/{subject}/identities and returns the linked accounts", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        subject: "bob",
        identities: [
          { gate: "sso", issuer: "https://idp.example", external_subject: "00u1", email: "bob@example.com",
            email_verified: true, created_at: "2026-10-27T00:00:00Z" },
        ],
      }),
    ]);
    const resp = await client.listUserIdentities("bob");
    expect(resp.identities[0]!.gate).toBe("sso");
    expect(resp.identities[0]!.last_login_at).toBeUndefined();
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_users/bob/identities");
    expect(init.method).toBe("GET");
  });
});

describe("unlinkUserIdentity", () => {
  it("DELETEs /v1/_users/{subject}/identities/{gate}, encoding both", async () => {
    const { client, fetchMock } = makeClient([noContentResponse()]);
    await client.unlinkUserIdentity("a/b", "sso one", { tenant: "acme" });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_users/a%2Fb/identities/sso%20one?tenant=acme");
    expect(init.method).toBe("DELETE");
  });
});
