import { describe, expect, it } from "vitest";
import { jsonResponse, makeClient } from "./helpers.js";

// RFC BY (loomcycle v1.51.0) — the user-facing runnable-agent catalog. Thin GET
// wrapper over /v1/_runnable-agents; the tiering is server-side, so the client
// just relays the lean {name, source} entries.

describe("runnableAgents", () => {
  it("GETs /v1/_runnable-agents and returns the tiered entries", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        agents: [
          { name: "chat", source: "bundled" },
          { name: "tenant-bot", source: "tenant" },
        ],
      }),
    ]);

    const resp = await client.runnableAgents();

    expect(resp.agents).toHaveLength(2);
    expect(resp.agents[0]!.name).toBe("chat");
    expect(resp.agents[0]!.source).toBe("bundled");
    expect(resp.agents[1]!.source).toBe("tenant");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_runnable-agents");
    expect(init.method).toBe("GET");
  });
});
