import { describe, expect, it } from "vitest";
import { jsonResponse, makeClient, noContentResponse } from "./helpers.js";

// The timed override of an agent's no-trace mode. These tests pin the wire
// shape (verb, URL, body) of the three thin wrappers.

describe("retention overrides", () => {
  it("lists them, with the admin-only tenant focus", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ overrides: [{ tenant: "acme", agent: "judge", until: "2026-10-12T18:00:00Z", set_by: "ops" }] }),
      jsonResponse({ overrides: [] }),
    ]);
    const res = await client.listRetentionOverrides();
    expect(res.overrides[0]!.agent).toBe("judge");
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/_retention/overrides");
    await client.listRetentionOverrides({ tenant: "globex" });
    expect(fetchMock.mock.calls[1]![0]).toBe("http://test-loomcycle:8787/v1/_retention/overrides?tenant=globex");
  });

  it("PUTs agent and until, and sends tenant only when given", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ tenant: "acme", agent: "judge", until: "2026-10-12T18:00:00Z" }),
      jsonResponse({ tenant: "globex", agent: "judge", until: "2026-10-12T18:00:00Z" }),
    ]);
    await client.setRetentionOverride({ agent: "judge", until: new Date("2026-10-12T18:00:00Z") });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_retention/overrides");
    expect(init.method).toBe("PUT");
    expect(JSON.parse(init.body as string)).toEqual({ agent: "judge", until: "2026-10-12T18:00:00.000Z" });

    await client.setRetentionOverride({ agent: "judge", until: "2026-10-12T18:00:00Z", tenant: "globex" });
    expect(JSON.parse(fetchMock.mock.calls[1]![1]!.body as string)).toEqual({
      agent: "judge",
      until: "2026-10-12T18:00:00Z",
      tenant: "globex",
    });
  });

  it("DELETEs by agent", async () => {
    const { client, fetchMock } = makeClient([noContentResponse(), noContentResponse()]);
    await client.clearRetentionOverride("judge");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_retention/overrides?agent=judge");
    expect(init.method).toBe("DELETE");
    await client.clearRetentionOverride("judge", { tenant: "globex" });
    expect(fetchMock.mock.calls[1]![0]).toBe("http://test-loomcycle:8787/v1/_retention/overrides?agent=judge&tenant=globex");
  });
});
