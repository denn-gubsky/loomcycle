import { describe, expect, it } from "vitest";
import type { Agent } from "../api";
import { breadcrumbAncestors, parentRunHref, runRowHref, selectedRowKey } from "./runLineage";

const run = (over: Partial<Agent> & Pick<Agent, "agent_id" | "run_id">): Agent => ({
  session_id: "s",
  agent: "a",
  parent_agent_id: null,
  user_id: "u",
  status: "completed",
  started_at: "2026-09-29T00:00:00Z",
  completed_at: null,
  stop_reason: null,
  error: null,
  usage: {},
  last_heartbeat_at: null,
  live: false,
  ...over,
});

// Two walks of one team share agent_id "team:triage". The older walk has a
// child. It is listed first, so a lookup by agent id (last one wins) lands on
// the newer walk.
const twoWalks = [
  run({ agent_id: "team:triage", run_id: "r_old", agent: "triage", started_at: "2026-09-29T00:01:00Z" }),
  run({ agent_id: "team:triage", run_id: "r_new", agent: "triage", started_at: "2026-09-29T00:02:00Z" }),
  run({ agent_id: "a_co", run_id: "r_co", parent_agent_id: "team:triage", parent_run_id: "r_old" }),
];

describe("parentRunHref", () => {
  it("opens the parent run itself by run id in the run terminal", () => {
    expect(parentRunHref("r_5e8b82599098c19f")).toBe("/run?attach=r_5e8b82599098c19f");
  });

  it("escapes the run id into the query", () => {
    expect(parentRunHref("r a&b")).toBe("/run?attach=r%20a%26b");
  });
});

describe("selectedRowKey", () => {
  it("selects the named run for ?run=", () => {
    expect(selectedRowKey(twoWalks, "r_old", undefined)).toBe("r_old");
  });

  it("maps an older ?agent= link to that agent's newest listed run", () => {
    expect(selectedRowKey(twoWalks, undefined, "team:triage")).toBe("r_new");
  });

  it("keeps the agent id when no listed row carries it", () => {
    expect(selectedRowKey(twoWalks, undefined, "a_gone")).toBe("a_gone");
  });
});

describe("breadcrumbAncestors", () => {
  it("names the run that spawned the selected run, not another run of its agent", () => {
    expect(breadcrumbAncestors(twoWalks, "r_co")).toEqual([
      { agent_id: "team:triage", run_id: "r_old", agent: "triage", status: "completed", inResultSet: true },
    ]);
  });

  it("stubs a parent run that is not listed", () => {
    const rows = [run({ agent_id: "a_c", run_id: "r_c", parent_agent_id: "a_p", parent_run_id: "r_gone" })];
    expect(breadcrumbAncestors(rows, "r_c")).toEqual([{ agent_id: "a_p", run_id: "r_gone", inResultSet: false }]);
  });

  it("stops when a parent_agent_id lookup comes back to a row already on the chain", () => {
    const rows = [run({ agent_id: "a_self", run_id: "", parent_agent_id: "a_self" })];
    expect(breadcrumbAncestors(rows, "a_self")).toEqual([]);
  });
});

describe("runRowHref", () => {
  it("links a run by run id and a row with no run id by agent id", () => {
    expect(runRowHref({ runId: "r a", agentId: "team:triage" })).toBe("/agents?run=r%20a");
    expect(runRowHref({ agentId: "team:triage" })).toBe("/agents?agent=team%3Atriage");
  });
});
