// adapters/ts/tests/credentials.test.ts — RFC CN P4: the RFC AR credential store
// (POST /v1/_credentialdef) on the @loomcycle/client wire. create/get/list/delete
// post an op-discriminated body; get/list are metadata-only (no secret ever returned).

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient } from "./helpers.js";
import { SubstrateToolRefusedError } from "../src/index.js";

describe("credentials", () => {
  it("createCredential posts op=create with scope/name/value", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ name: "TELEGRAM_BOT_TOKEN", scope: "user", status: "stored" }),
    ]);

    const meta = await client.createCredential({
      scope: "user",
      name: "TELEGRAM_BOT_TOKEN",
      value: "secret-123",
    });
    expect(meta.name).toBe("TELEGRAM_BOT_TOKEN");
    expect(meta.scope).toBe("user");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_credentialdef");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body).toEqual({
      op: "create",
      scope: "user",
      name: "TELEGRAM_BOT_TOKEN",
      value: "secret-123",
    });
  });

  it("listCredentials posts op=list for the scope and returns metadata", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ scope: "user", credentials: [{ name: "slack", scope: "user" }] }),
    ]);

    const resp = await client.listCredentials("user");
    expect(resp.credentials).toHaveLength(1);
    expect(resp.credentials[0]!.name).toBe("slack");

    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body).toEqual({ op: "list", scope: "user" });
  });

  it("createCredential sends expires_at and backend verbatim", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ name: "slack", scope: "tenant", backend: "inline", expires_at: "2026-12-31T00:00:00Z" }),
    ]);

    await client.createCredential({
      scope: "tenant",
      name: "slack",
      value: "xoxb-1",
      expires_at: "2026-12-31T00:00:00Z",
      backend: "inline",
    });

    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body).toEqual({
      op: "create",
      scope: "tenant",
      name: "slack",
      value: "xoxb-1",
      expires_at: "2026-12-31T00:00:00Z",
      backend: "inline",
    });
  });

  it("getCredential posts op=get with scope/name and returns one row's metadata", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        name: "git",
        scope: "user",
        backend: "inline",
        allowed_urls: ["https://git.example.com/"],
        username: "bot",
      }),
    ]);

    const meta = await client.getCredential({ scope: "user", name: "git" });
    expect(meta.name).toBe("git");
    expect(meta.allowed_urls).toEqual(["https://git.example.com/"]);
    expect(meta.username).toBe("bot");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_credentialdef");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body).toEqual({ op: "get", scope: "user", name: "git" });
  });

  it("getCredential raises SubstrateToolRefusedError for an absent name", async () => {
    const { client } = makeClient([
      jsonResponse(
        { code: "tool_refused", tool: "CredentialDef", error: 'get: no credential "nope" in user scope' },
        422,
      ),
    ]);

    await expect(client.getCredential({ scope: "user", name: "nope" })).rejects.toBeInstanceOf(
      SubstrateToolRefusedError,
    );
  });

  it("deleteCredential posts op=delete with scope/name", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ name: "slack", scope: "user", deleted: true }),
    ]);

    const resp = await client.deleteCredential({ scope: "user", name: "slack" });
    expect(resp.deleted).toBe(true);

    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body).toEqual({ op: "delete", scope: "user", name: "slack" });
  });
});
