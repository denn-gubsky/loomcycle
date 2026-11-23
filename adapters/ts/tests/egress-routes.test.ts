// adapters/ts/tests/egress-routes.test.ts — RFC GW P4 egressRouteDef.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient } from "./helpers.js";
import { SubstrateToolRefusedError, type EgressRouteView } from "../src/index.js";

describe("egressRouteDef", () => {
  it("posts the route to /v1/_egressroutedef and returns the envelope", async () => {
    const view: EgressRouteView = {
      name: "crm",
      url: "https://api.crm.example/v2/",
      methods: ["GET", "POST"],
      writes: true,
      credential: "$cred:crm",
      auth: "bearer",
      created_at: "2026-11-23T00:00:00Z",
    };
    const { client, fetchMock } = makeClient([jsonResponse({ route: view })]);

    const result = (await client.egressRouteDef({
      op: "create",
      name: "crm",
      url: "https://api.crm.example/v2/",
      methods: ["GET", "POST"],
      credential: "$cred:crm",
    })) as { route: EgressRouteView };

    expect(result.route.writes).toBe(true);
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_egressroutedef");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body).toEqual({
      op: "create",
      name: "crm",
      url: "https://api.crm.example/v2/",
      methods: ["GET", "POST"],
      credential: "$cred:crm",
    });
  });

  it("surfaces a tool refusal as SubstrateToolRefusedError", async () => {
    const refusalBody = JSON.stringify({
      code: "tool_refused",
      tool: "EgressRouteDef",
      error: "EgressRouteDef create: tenant routes are off",
    });
    const { client } = makeClient([
      () => new Response(refusalBody, { status: 422, headers: { "Content-Type": "application/json" } }),
    ]);
    await expect(client.egressRouteDef({ op: "create", name: "crm", url: "https://x.example/" })).rejects.toBeInstanceOf(
      SubstrateToolRefusedError,
    );
  });
});
