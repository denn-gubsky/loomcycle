import { describe, expect, it } from "vitest";
import { jsonResponse, makeClient } from "./helpers.js";
import { AgentNotFoundError, LoomcycleError } from "../src/index.js";
import type { AgentOverridableResponse } from "../src/index.js";

// GET /v1/_agentdef/{name}/overridable — which per-run overrides a run of one
// agent may set, for the calling token. A thin GET: the folding of the
// agent's allow_overrides with the token's policy happens server-side.

describe("getAgentOverridable", () => {
  it("GETs the agent's overridable report and returns it as sent", async () => {
    const report: AgentOverridableResponse = {
      agent: "researcher",
      def_id: "def_abc",
      allow_overrides: true,
      restricted_by_token: true,
      fields: {
        model: { overridable: false, reason: "token_locked" },
        effort: { overridable: true, values: ["high", "low", "medium"] },
        max_concurrent_children: { overridable: true, narrow_only: true, max: 4 },
      },
    };
    const { client, fetchMock } = makeClient([jsonResponse(report)]);

    const out = await client.getAgentOverridable("researcher");

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_agentdef/researcher/overridable");
    expect(init.method).toBe("GET");
    expect(out).toEqual(report);
    // A refused field says which lock refuses it, so a form can explain why
    // the control is disabled.
    expect(out.fields.model!.reason).toBe("token_locked");
    expect(out.fields.max_concurrent_children!.narrow_only).toBe(true);
  });

  it("escapes the name and sends an admin tenant focus as ?tenant=", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ agent: "a/b", allow_overrides: false, restricted_by_token: false, fields: {} }),
    ]);

    await client.getAgentOverridable("a/b", { tenant: "globex" });

    expect(String(fetchMock.mock.calls[0]![0])).toBe(
      "http://test-loomcycle:8787/v1/_agentdef/a%2Fb/overridable?tenant=globex",
    );
  });

  it("raises AgentNotFoundError for an agent the caller cannot resolve", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "agent_not_found", error: 'no agent named "ghost"' }, 404),
    ]);

    const err = await client.getAgentOverridable("ghost").catch((e: unknown) => e);

    expect(err).toBeInstanceOf(AgentNotFoundError);
    expect((err as LoomcycleError).code).toBe("agent_not_found");
  });

  it("carries agent_not_allowed for an agent outside the token's allowlist", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "agent_not_allowed", error: 'agent not allowed: "other"' }, 403),
    ]);

    const err = await client.getAgentOverridable("other").catch((e: unknown) => e);

    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(403);
    expect((err as LoomcycleError).code).toBe("agent_not_allowed");
  });
});
