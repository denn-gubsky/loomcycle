import { describe, expect, it } from "vitest";
import {
  agentSummary,
  channelSummary,
  countOwnDefinitions,
  keepOwnDefinitionRemovals,
  readLocalAgent,
  scheduleSummary,
  skillSummary,
  teamOwnDefinitions,
  webhookRoute,
  webhookSummary,
  writeLocalAgent,
} from "./teamLocal";

// The help article's examples, folded into one definition.
const def = {
  entry: "wave",
  vars: { tone: "formal", audience: "" },
  local: {
    agents: {
      reviewer: { tier: "middle", tools: ["Read", "Agent"], system_prompt: "You review diffs." },
      pinned: { provider: "anthropic", model: "claude-x", tools: [] },
    },
    skills: { "house-style": { description: "How this team writes code", body: "Prefer short functions.", tools: ["Read"] } },
    channels: { events: { scope: "tenant", default_ttl: 3600 } },
    schedules: { minute: { schedule: "@every 1m", channel: "./ticks" } },
    webhooks: {
      github: {
        channel: "./events",
        auth: { kind: "hmac", header: "X-Hub-Signature-256", signing_secret_env: "LOOMCYCLE_GH_TEAM_SECRET" },
      },
    },
  },
  states: [],
};

describe("teamOwnDefinitions", () => {
  it("reads every variable and every local kind, one line per entry", () => {
    const own = teamOwnDefinitions(def, "sdlc", "acme");
    expect(own.vars).toEqual([
      { name: "tone", defaultValue: "formal" },
      { name: "audience", defaultValue: "" },
    ]);
    expect(own.local.agents).toEqual([
      { name: "reviewer", summary: "tier middle · tools: Read, Agent" },
      { name: "pinned", summary: "anthropic/claude-x · no tools" },
    ]);
    expect(own.local.skills).toEqual([{ name: "house-style", summary: "How this team writes code" }]);
    expect(own.local.channels).toEqual([{ name: "events", summary: "scope tenant" }]);
    expect(own.local.schedules).toEqual([{ name: "minute", summary: "@every 1m → ./ticks" }]);
    expect(own.local.webhooks).toEqual([
      {
        name: "github",
        summary: "POST /v1/_teams/acme/sdlc/webhooks/github · hmac, secret in LOOMCYCLE_GH_TEAM_SECRET → ./events",
      },
    ]);
    expect(countOwnDefinitions(own)).toBe(8);
  });

  it("is empty for a definition that declares nothing of its own, or is not an object", () => {
    for (const d of [{ entry: "x", states: [] }, null, "nope", { local: "x", vars: [] }]) {
      expect(countOwnDefinitions(teamOwnDefinitions(d, "t", ""))).toBe(0);
    }
  });
});

describe("agentSummary", () => {
  it("names the routing it has, most specific first", () => {
    expect(agentSummary({ provider: "openai", model: "gpt-x", tier: "top" })).toBe("openai/gpt-x · no tools");
    expect(agentSummary({ model: "local-medium" })).toBe("model local-medium · no tools");
    expect(agentSummary({ provider: "ollama", tier: "small" })).toBe("ollama, tier small · no tools");
    expect(agentSummary({ provider: "code-js", code_body: "x" })).toBe("code-js · no tools");
    expect(agentSummary({})).toBe("default routing · no tools");
  });
});

describe("skillSummary / channelSummary / scheduleSummary", () => {
  it("falls back to what is there when a field is missing", () => {
    expect(skillSummary({ body: "x", tools: ["Read"] })).toBe("(no description) · tools: Read");
    expect(skillSummary({ body: "x" })).toBe("(no description)");
    expect(channelSummary({ scope: "user", max_messages: 5 })).toBe("scope user");
    expect(channelSummary({})).toBe("no scope set");
    expect(scheduleSummary({ schedule: "*/5 * * * *" })).toBe("*/5 * * * * → (no channel)");
  });
});

describe("webhookRoute", () => {
  it("carries the tenant segment only for a team in a tenant", () => {
    expect(webhookRoute("acme", "sdlc", "github")).toBe("/v1/_teams/acme/sdlc/webhooks/github");
    expect(webhookRoute("", "sdlc", "github")).toBe("/v1/_teams/sdlc/webhooks/github");
  });

  it("escapes a segment rather than letting it add one", () => {
    expect(webhookRoute("a/b", "t", "n")).toBe("/v1/_teams/a%2Fb/t/webhooks/n");
  });
});

describe("webhookSummary", () => {
  const route = "/v1/_teams/t/webhooks/n";

  it("names the env var holding a bearer token, defaulting the kind to hmac", () => {
    expect(webhookSummary({ channel: "./e", auth: { kind: "bearer", bearer_token_env: "LOOMCYCLE_T" } }, route)).toBe(
      "POST /v1/_teams/t/webhooks/n · bearer, token in LOOMCYCLE_T → ./e",
    );
    expect(webhookSummary({ channel: "./e", auth: { signing_secret_env: "LOOMCYCLE_S" } }, route)).toBe(
      "POST /v1/_teams/t/webhooks/n · hmac, secret in LOOMCYCLE_S → ./e",
    );
    expect(webhookSummary({ channel: "./e", auth: { kind: "none" } }, route)).toBe("POST /v1/_teams/t/webhooks/n · no auth → ./e");
  });

  it("reads only the env-var name fields, never another auth field", () => {
    // A field the server would refuse, holding something secret-looking: the
    // summary must not repeat it.
    const s = webhookSummary({ channel: "./e", auth: { kind: "hmac", signing_secret_env: "LOOMCYCLE_S", secret: "s3cr3t-value" } }, route);
    expect(s).not.toContain("s3cr3t-value");
  });
});

describe("readLocalAgent / writeLocalAgent", () => {
  it("replaces one agent's body and leaves the rest of the definition as it was", () => {
    const next = writeLocalAgent(def, "reviewer", { tier: "top", tools: ["Read"] }) as typeof def;
    expect(readLocalAgent(next, "reviewer")).toEqual({ tier: "top", tools: ["Read"] });
    expect(next.local.agents.pinned).toBe(def.local.agents.pinned);
    expect(next.local.skills).toBe(def.local.skills);
    expect(next.local.webhooks).toBe(def.local.webhooks);
    expect(next.vars).toBe(def.vars);
    expect(next.entry).toBe("wave");
    // The input is not modified.
    expect(def.local.agents.reviewer.tier).toBe("middle");
  });

  it("drops a field the editor cleared rather than keeping the old value", () => {
    const next = writeLocalAgent(def, "reviewer", { tools: ["Read", "Agent"] });
    expect(readLocalAgent(next, "reviewer")).toEqual({ tools: ["Read", "Agent"] });
  });

  it("leaves the definition unchanged for an agent it does not declare", () => {
    expect(writeLocalAgent(def, "ghost", { tier: "top" })).toBe(def);
    expect(writeLocalAgent({ entry: "x" }, "reviewer", {})).toEqual({ entry: "x" });
    expect(readLocalAgent(def, "ghost")).toBeUndefined();
  });
});

describe("keepOwnDefinitionRemovals", () => {
  const { vars: _v, local: _l, ...bare } = def;

  it("sends vars: {} when the edit drops the parent's variables", () => {
    const { vars: _drop, ...edited } = def;
    expect((keepOwnDefinitionRemovals(def, edited) as { vars: unknown }).vars).toEqual({});
    expect((keepOwnDefinitionRemovals(def, { ...edited, vars: null }) as { vars: unknown }).vars).toEqual({});
  });

  it("sends {} for each local kind the edit drops, keeping the kinds it still has", () => {
    const { agents: _a, webhooks: _w, ...rest } = def.local;
    const out = keepOwnDefinitionRemovals(def, { ...def, local: rest }) as typeof def;
    expect(out.local.agents).toEqual({});
    expect(out.local.webhooks).toEqual({});
    expect(out.local.skills).toBe(def.local.skills);
    expect(out.local.channels).toBe(def.local.channels);
  });

  it("sends {} for every kind the parent had when the edit drops the whole local block", () => {
    const out = keepOwnDefinitionRemovals(def, { ...def, local: undefined }) as { local: unknown };
    expect(out.local).toEqual({ agents: {}, skills: {}, channels: {}, schedules: {}, webhooks: {} });
    const both = keepOwnDefinitionRemovals(def, bare) as { local: unknown; vars: unknown };
    expect(both.vars).toEqual({});
    expect(both.local).toEqual({ agents: {}, skills: {}, channels: {}, schedules: {}, webhooks: {} });
  });

  it("clears only the kinds the parent declared", () => {
    const parent = { ...bare, local: { channels: { events: { scope: "tenant" } }, agents: {} } };
    expect(keepOwnDefinitionRemovals(parent, bare)).toEqual({ ...bare, local: { channels: {} } });
  });

  it("returns the edit unchanged when it removes nothing or the parent declared nothing", () => {
    expect(keepOwnDefinitionRemovals(def, def)).toBe(def);
    expect(keepOwnDefinitionRemovals(bare, bare)).toBe(bare);
    const emptied = { ...def, vars: {}, local: { ...def.local, agents: {} } };
    expect(keepOwnDefinitionRemovals(def, emptied)).toBe(emptied);
    expect(keepOwnDefinitionRemovals(undefined, bare)).toBe(bare);
  });

  it("sends max_iterations: 0 when the edit drops the parent's cap", () => {
    const capped = { ...bare, max_iterations: 12 };
    expect((keepOwnDefinitionRemovals(capped, bare) as { max_iterations: unknown }).max_iterations).toBe(0);
    expect((keepOwnDefinitionRemovals(capped, { ...bare, max_iterations: null }) as { max_iterations: unknown }).max_iterations).toBe(0);
    // A cap the edit still sets, and a parent with none, are left as written.
    const recapped = { ...bare, max_iterations: 3 };
    expect(keepOwnDefinitionRemovals(capped, recapped)).toBe(recapped);
    expect(keepOwnDefinitionRemovals(bare, bare)).toBe(bare);
  });

  it("leaves a local that is not an object for the server to refuse", () => {
    const edited = { ...bare, local: "oops" };
    expect((keepOwnDefinitionRemovals(def, edited) as { local: unknown }).local).toBe("oops");
  });
});
