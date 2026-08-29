// adapters/ts/tests/credentials.test.ts — RFC CN P4: the RFC AR credential store
// (POST /v1/_credentialdef) on the @loomcycle/client wire. create/list/delete post
// an op-discriminated body; get/list are metadata-only (no secret ever returned).

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient } from "./helpers.js";

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
