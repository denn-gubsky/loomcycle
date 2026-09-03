import { describe, expect, it } from "vitest";
import { LoomcycleError } from "../src/errors.js";
import {
  errorResponse,
  jsonResponse,
  makeClient,
  noContentResponse,
} from "./helpers.js";

// v0.11.5 — Memory entry admin CRUD (setMemoryEntry /
// deleteMemoryEntry). Same pattern as channels-admin: thin REST
// wrappers, tests cover the wire shape + typed error surface.

describe("setMemoryEntry", () => {
  it("PUTs the value to the (scope,scope_id,key) URL", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        scope: "user",
        scope_id: "alice",
        key: "tone",
        embedded: false,
      }),
    ]);

    const resp = await client.setMemoryEntry("user", "alice", "tone", {
      value: { polite: true },
    });

    expect(resp.scope).toBe("user");
    expect(resp.embedded).toBe(false);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_memory/scopes/user/alice/keys/tone");
    expect(init.method).toBe("PUT");
    const body = JSON.parse(init.body as string);
    expect(body).toEqual({ value: { polite: true } });
  });

  it("passes embed + ttl_seconds when provided", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ scope: "user", scope_id: "alice", key: "v", embedded: true }),
    ]);
    await client.setMemoryEntry("user", "alice", "v", {
      value: "hello",
      embed: true,
      ttl_seconds: 600,
    });
    const body = JSON.parse(fetchMock.mock.calls[0]![1].body as string);
    expect(body).toEqual({ value: "hello", embed: true, ttl_seconds: 600 });
  });

  it("URL-encodes scope_id and key", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ scope: "user", scope_id: "id/x", key: "k/1", embedded: false }),
    ]);
    await client.setMemoryEntry("user", "id/x", "k/1", { value: 1 });
    const [url] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_memory/scopes/user/id%2Fx/keys/k%2F1");
  });

  it("surfaces 413 quota errors as typed", async () => {
    const { client } = makeClient([
      errorResponse(413, `{"code":"memory_quota_exceeded","error":"too big"}`),
    ]);
    await expect(
      client.setMemoryEntry("user", "alice", "bulky", { value: "x".repeat(10) }),
    ).rejects.toBeInstanceOf(LoomcycleError);
  });

  it("surfaces embed_warning in response when embedding fails", async () => {
    const { client } = makeClient([
      jsonResponse({
        scope: "user",
        scope_id: "alice",
        key: "v",
        embedded: false,
        embed_warning: "embedder transient: 503",
      }),
    ]);
    const resp = await client.setMemoryEntry("user", "alice", "v", {
      value: "x",
      embed: true,
    });
    expect(resp.embedded).toBe(false);
    expect(resp.embed_warning).toContain("embedder transient");
  });
});

describe("deleteMemoryEntry", () => {
  it("DELETEs the (scope,scope_id,key) URL", async () => {
    const { client, fetchMock } = makeClient([noContentResponse()]);
    await client.deleteMemoryEntry("user", "alice", "tone");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_memory/scopes/user/alice/keys/tone");
    expect(init.method).toBe("DELETE");
  });
});

// The super-admin tenant focus on the memory browse routes. A substrate:admin
// token satisfies every scope gate and then, before v1.72.0, could not read the
// data the gate admits: those five handlers resolved the tenant from the bearer
// and took nothing from the URL, so an admin whose own tenant is empty saw an
// empty console with no parameter available to look elsewhere.
//
// Sending it is safe from ANY principal: the server ignores the value for a
// caller that cannot widen its own scope, rather than honouring-then-checking
// it. Omitting it resolves to the caller's own tenant, which is what every one
// of these calls did before, so the option adds a capability and moves no default.
describe("memory admin tenant focus", () => {
  it("omits ?tenant= entirely when no focus is given", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ scope: "user", scope_id: "alice", entries: [], truncated: false }),
    ]);
    await client.listMemoryEntries("user", "alice");
    const [url] = fetchMock.mock.calls[0]!;
    expect(String(url)).toBe("http://test-loomcycle:8787/v1/_memory/scopes/user/alice/keys");
  });

  it("sends ?tenant= on every browse method", async () => {
    const cases: Array<[string, (c: ReturnType<typeof makeClient>["client"]) => Promise<unknown>, string]> = [
      [
        "listMemoryScopeIDs",
        (c) => c.listMemoryScopeIDs("user", { tenant: "acme" }),
        "http://test-loomcycle:8787/v1/_memory/scopes/user?tenant=acme",
      ],
      [
        "listMemoryEntries",
        (c) => c.listMemoryEntries("user", "alice", { tenant: "acme", prefix: "memory/" }),
        "http://test-loomcycle:8787/v1/_memory/scopes/user/alice/keys?tenant=acme&prefix=memory%2F",
      ],
      [
        "getMemoryEntry",
        (c) => c.getMemoryEntry("user", "alice", "tone", { tenant: "acme" }),
        "http://test-loomcycle:8787/v1/_memory/scopes/user/alice/keys/tone?tenant=acme",
      ],
      [
        "setMemoryEntry",
        (c) => c.setMemoryEntry("user", "alice", "tone", { value: "warm", tenant: "acme" }),
        "http://test-loomcycle:8787/v1/_memory/scopes/user/alice/keys/tone?tenant=acme",
      ],
      [
        "deleteMemoryEntry",
        (c) => c.deleteMemoryEntry("user", "alice", "tone", { tenant: "acme" }),
        "http://test-loomcycle:8787/v1/_memory/scopes/user/alice/keys/tone?tenant=acme",
      ],
    ];
    for (const [name, call, want] of cases) {
      const { client, fetchMock } = makeClient([
        jsonResponse({ scope: "user", scope_id: "alice", entries: [], truncated: false, scope_ids: [], key: "tone", value: "warm" }),
        noContentResponse(),
      ]);
      await call(client);
      const [url] = fetchMock.mock.calls[0]!;
      expect(String(url), `${name} must carry the tenant focus`).toBe(want);
    }
  });

  it("treats a blank focus as no focus", async () => {
    // The Web UI's tenant switcher is empty until an admin types one, and "" there
    // means "my own tenant" — not a tenant literally named "". Sending tenant=
    // would be a different request, so it is dropped.
    const { client, fetchMock } = makeClient([
      jsonResponse({ scope: "user", scope_id: "alice", entries: [], truncated: false }),
    ]);
    await client.listMemoryEntries("user", "alice", { tenant: "   " });
    const [url] = fetchMock.mock.calls[0]!;
    expect(String(url)).toBe("http://test-loomcycle:8787/v1/_memory/scopes/user/alice/keys");
  });
});
