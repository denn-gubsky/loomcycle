// adapters/ts/tests/oauth-credentials.test.ts — OAuth credentials on the
// @loomcycle/client wire: the typed create and the three connect ops post an
// op-discriminated body to POST /v1/_credentialdef and hand back the server's
// answer, which never carries a token.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient } from "./helpers.js";
import { SubstrateToolRefusedError } from "../src/index.js";
import type { CredentialMeta, OAuthConnectStart } from "../src/index.js";

function sentBody(fetchMock: { mock: { calls: unknown[][] } }): unknown {
  const call = fetchMock.mock.calls[0]!;
  expect(call[0]).toBe("http://test-loomcycle:8787/v1/_credentialdef");
  expect((call[1] as RequestInit).method).toBe("POST");
  return JSON.parse((call[1] as RequestInit).body as string);
}

describe("OAuth credentials", () => {
  it("createOAuthCredential posts op=create with usage oauth and the oauth object", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        name: "docs-server",
        scope: "user",
        backend: "inline",
        usage: "oauth",
        allowed_urls: ["https://docs.example.com/mcp"],
        oauth: { resource: "https://docs.example.com/mcp", registration: "dynamic", state: "pending_connect" },
        status: "stored",
      }),
    ]);

    const meta: CredentialMeta = await client.createOAuthCredential({
      scope: "user",
      name: "docs-server",
      oauth: {
        resource: "https://docs.example.com/mcp",
        scopes: ["openid", "offline_access"],
        client: { token_endpoint_auth: "none" },
      },
    });
    expect(meta.usage).toBe("oauth");
    expect(meta.oauth?.state).toBe("pending_connect");

    expect(sentBody(fetchMock)).toEqual({
      op: "create",
      usage: "oauth",
      scope: "user",
      name: "docs-server",
      oauth: {
        resource: "https://docs.example.com/mcp",
        scopes: ["openid", "offline_access"],
        client: { token_endpoint_auth: "none" },
      },
    });
  });

  it("oauthConnectStart posts op=oauth_connect_start and returns the code to approve", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        name: "docs-server",
        scope: "user",
        status: "pending_connect",
        user_code: "ABCD-EFGH",
        verification_uri: "https://auth.example.com/device",
        verification_uri_complete: "https://auth.example.com/device?user_code=ABCD-EFGH",
        expires_in: 299,
        interval: 5,
        next: "Open verification_uri, enter user_code and approve; then check with oauth_connect_status.",
      }),
    ]);

    const start: OAuthConnectStart = await client.oauthConnectStart({ scope: "user", name: "docs-server" });
    expect(start.user_code).toBe("ABCD-EFGH");
    expect(start.verification_uri).toBe("https://auth.example.com/device");
    expect(start.verification_uri_complete).toBe("https://auth.example.com/device?user_code=ABCD-EFGH");
    expect(start.expires_in).toBe(299);
    expect(start.interval).toBe(5);

    expect(sentBody(fetchMock)).toEqual({ op: "oauth_connect_start", scope: "user", name: "docs-server" });
  });

  it("oauthConnectStatus posts op=oauth_connect_status and returns the connection state", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        name: "docs-server",
        scope: "tenant",
        usage: "oauth",
        oauth: {
          resource: "https://docs.example.com/mcp",
          registration: "dynamic",
          state: "connected",
          connected_as: "owner@example.com",
          access_expires_at: "2026-11-07T10:05:00Z",
        },
      }),
    ]);

    const meta = await client.oauthConnectStatus({ scope: "tenant", name: "docs-server" });
    expect(meta.oauth?.state).toBe("connected");
    expect(meta.oauth?.connected_as).toBe("owner@example.com");

    expect(sentBody(fetchMock)).toEqual({ op: "oauth_connect_status", scope: "tenant", name: "docs-server" });
  });

  it("oauthDisconnect posts op=oauth_disconnect and returns the revoked state", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        name: "docs-server",
        scope: "user",
        usage: "oauth",
        oauth: { resource: "https://docs.example.com/mcp", registration: "dynamic", state: "revoked" },
      }),
    ]);

    const meta = await client.oauthDisconnect({ scope: "user", name: "docs-server" });
    expect(meta.oauth?.state).toBe("revoked");

    expect(sentBody(fetchMock)).toEqual({ op: "oauth_disconnect", scope: "user", name: "docs-server" });
  });

  it("oauthConnectStart raises SubstrateToolRefusedError when the server refuses", async () => {
    const { client } = makeClient([
      jsonResponse(
        {
          code: "tool_refused",
          tool: "CredentialDef",
          error: "oauth_connect_start: a tenant OAuth credential is managed by a tenant operator",
        },
        422,
      ),
    ]);

    await expect(client.oauthConnectStart({ scope: "tenant", name: "docs-server" })).rejects.toBeInstanceOf(
      SubstrateToolRefusedError,
    );
  });
});
