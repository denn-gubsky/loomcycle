import { describe, expect, it } from "vitest";
import { InvalidArgumentError, LoomcycleError } from "../src/errors.js";
import { errorResponse, jsonResponse, makeClient, noContentResponse } from "./helpers.js";

// The sign-in gate listing and the password gate's management API
// (/v1/_authgate). These pin the wire shape — verb, URL, body — and that the
// tenant goes where each route reads it: in the body for the POST/PATCH
// routes, as ?tenant= for the rest, and nowhere when not named.

const BASE = "http://test-loomcycle:8787";

describe("listAuthGates", () => {
  it("GETs /v1/_authgate and returns the gates", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        gates: [
          { name: "token", kind: "token", label: "Token", interactive: true },
          {
            name: "corp", kind: "external", label: "Corp", interactive: false,
            key: { alg: "EdDSA", kid: "k1", fingerprint: "sha256:ab" },
            on_removed: { unbind_after: "168h0m0s", erase_after: "never" },
          },
        ],
      }),
    ]);
    const { gates } = await client.listAuthGates();
    expect(gates.map((g) => g.kind)).toEqual(["token", "external"]);
    expect(gates[1]!.on_removed!.erase_after).toBe("never");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${BASE}/v1/_authgate`);
    expect(init.method).toBe("GET");
  });

  it("asks for the discovery probe with probe=1", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ gates: [] })]);
    await client.listAuthGates({ probe: true });
    expect(fetchMock.mock.calls[0]![0]).toBe(`${BASE}/v1/_authgate?probe=1`);
  });
});

describe("createInvite", () => {
  it("POSTs the body with the tenant in it and returns the one-time link", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse(
        { invite_id: "inv_1", url: "https://lc.example/ui/invite#code", expires_at: "2026-10-30T00:00:00Z" },
        201,
      ),
    ]);
    const inv = await client.createInvite(
      "password",
      { email: "carol@example.com", ttl: "72h", groups: ["support"] },
      { tenant: "acme" },
    );
    expect(inv.invite_id).toBe("inv_1");
    expect(inv.url).toContain("/ui/invite#");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${BASE}/v1/_authgate/password/invites`);
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({
      email: "carol@example.com",
      ttl: "72h",
      groups: ["support"],
      tenant: "acme",
    });
  });

  it("sends no tenant key when none is named", async () => {
    // The route refuses unknown fields and reads an empty tenant as "your
    // own"; nothing but the caller's fields goes out.
    const { client, fetchMock } = makeClient([
      jsonResponse({ invite_id: "inv_2", url: "u", expires_at: "t" }, 201),
    ]);
    await client.createInvite("pw gate", { subject: "bob" });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${BASE}/v1/_authgate/pw%20gate/invites`);
    expect(JSON.parse(init.body as string)).toEqual({ subject: "bob" });
  });

  it("surfaces a grant beyond the gate's floor as a 403 with code beyond_floor", async () => {
    const { client } = makeClient([
      errorResponse(403, `{"code":"beyond_floor","error":"an invite through this gate may grant only the floor"}`),
    ]);
    const err = await client.createInvite("password", { access_mode: "tenant" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).code).toBe("beyond_floor");
  });
});

describe("listInvites", () => {
  it("GETs the gate's invites with ?tenant=", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        invites: [
          {
            invite_id: "inv_1", gate: "password", tenant_id: "acme", groups: [],
            created_at: "2026-10-27T00:00:00Z", expires_at: "2026-10-30T00:00:00Z", state: "live",
          },
        ],
      }),
    ]);
    const { invites } = await client.listInvites("password", { tenant: "acme" });
    expect(invites[0]!.state).toBe("live");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${BASE}/v1/_authgate/password/invites?tenant=acme`);
    expect(init.method).toBe("GET");
  });
});

describe("revokeInvite", () => {
  it("DELETEs the invite by id", async () => {
    const { client, fetchMock } = makeClient([noContentResponse()]);
    await client.revokeInvite("password", "inv_1");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${BASE}/v1/_authgate/password/invites/inv_1`);
    expect(init.method).toBe("DELETE");
  });

  it("surfaces an invite that is no longer live as a 409 invite_not_live", async () => {
    const { client } = makeClient([
      errorResponse(409, `{"code":"invite_not_live","error":"the invite was already redeemed, revoked or expired"}`),
    ]);
    const err = await client.revokeInvite("password", "inv_1").catch((e: unknown) => e);
    expect((err as LoomcycleError).status).toBe(409);
    expect((err as LoomcycleError).code).toBe("invite_not_live");
  });
});

describe("createPasswordReset", () => {
  it("POSTs {subject, tenant} to /resets and returns the one-time link", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ reset_id: "rst_1", url: "https://lc.example/ui/reset#code", expires_at: "2026-10-28T00:00:00Z" }, 201),
    ]);
    const rst = await client.createPasswordReset("password", "bob", { tenant: "acme" });
    expect(rst.reset_id).toBe("rst_1");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${BASE}/v1/_authgate/password/resets`);
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({ subject: "bob", tenant: "acme" });
  });
});

describe("removeGateCredentials", () => {
  it("DELETEs the subject's credentials with ?tenant=", async () => {
    const { client, fetchMock } = makeClient([noContentResponse()]);
    await client.removeGateCredentials("password", "a/b", { tenant: "acme" });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${BASE}/v1/_authgate/password/credentials/a%2Fb?tenant=acme`);
    expect(init.method).toBe("DELETE");
  });
});

describe("renameGateLogin", () => {
  it("PATCHes {login, tenant} and returns the normalised login", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ subject: "bob", login: "bobby" })]);
    const out = await client.renameGateLogin("password", "bob", "Bobby", { tenant: "acme" });
    expect(out).toEqual({ subject: "bob", login: "bobby" });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${BASE}/v1/_authgate/password/logins/bob`);
    expect(init.method).toBe("PATCH");
    expect(JSON.parse(init.body as string)).toEqual({ login: "Bobby", tenant: "acme" });
  });

  it("surfaces an invalid login as InvalidArgumentError", async () => {
    const { client } = makeClient([errorResponse(400, `{"code":"invalid_login","error":"bad login"}`)]);
    await expect(client.renameGateLogin("password", "bob", " ")).rejects.toBeInstanceOf(
      InvalidArgumentError,
    );
  });
});

describe("listGateIdentities", () => {
  it("GETs /identities with the subject and tenant as query parameters", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        tenant_id: "acme",
        subject: "bob",
        identities: [
          {
            gate: "password", login: "bobby", email_verified: false,
            created_at: "2026-10-27T00:00:00Z", has_password: true, has_totp: false,
          },
        ],
      }),
    ]);
    const resp = await client.listGateIdentities("password", "bob", { tenant: "acme" });
    expect(resp.identities[0]!.has_password).toBe(true);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${BASE}/v1/_authgate/password/identities?subject=bob&tenant=acme`);
    expect(init.method).toBe("GET");
  });
});

describe("getGateTenantStatus", () => {
  it("GETs /tenant-status and returns the bootstrap facts", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ tenant_id: "acme", has_users: false, bootstrapped: false, bootstrap_invite_live: true }),
    ]);
    const st = await client.getGateTenantStatus("password", { tenant: "acme" });
    expect(st.bootstrap_invite_live).toBe(true);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${BASE}/v1/_authgate/password/tenant-status?tenant=acme`);
    expect(init.method).toBe("GET");
  });
});
