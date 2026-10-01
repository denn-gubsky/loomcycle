import { describe, expect, it } from "vitest";
import { isHeldBack, summarizeRestore } from "./restoreSummary";

describe("summarizeRestore", () => {
  it("keeps refused and credential-disabled counts off the restored line", () => {
    // Under "restored" a refused MCP server read as one more success.
    const s = summarizeRestore({
      memory: 3,
      mcp_server_defs: 1,
      mcp_server_defs_refused: 2,
      active_pointers_refused: 1,
      defs_disabled_for_credentials: 1,
    });
    expect(s.restored).toBe("restored: mcp_server_defs=1, memory=3");
    expect(s.notRestored).toBe(
      "not restored: active_pointers_refused=1, defs_disabled_for_credentials=1, mcp_server_defs_refused=2",
    );
  });

  it("has no not-restored line when every held-back count is zero", () => {
    const s = summarizeRestore({ memory: 3, hook_defs_refused: 0, defs_disabled_for_credentials: 0 });
    expect(s.restored).toBe("restored: memory=3");
    expect(s.notRestored).toBeUndefined();
  });

  it("does not claim everything was already there when rows were held back", () => {
    const s = summarizeRestore({ memory: 0, mcp_server_defs_refused: 1 });
    expect(s.restored).not.toContain("already in the store");
    expect(s.notRestored).toBe("not restored: mcp_server_defs_refused=1");
  });

  it("says so when a re-restore wrote nothing", () => {
    expect(summarizeRestore({ memory: 0, agent_defs: 0 })).toEqual({
      restored: "restored (0 new rows — every section was already in the store)",
    });
  });
});

describe("isHeldBack", () => {
  it("counts only refusals and credential-disabled triggers as held back", () => {
    expect(isHeldBack("paused_runs_resumed")).toBe(false);
    expect(isHeldBack("mcp_server_defs_activated")).toBe(false);
    expect(isHeldBack("paused_runs_already_live")).toBe(false);
    expect(isHeldBack("active_pointers_refused")).toBe(true);
  });
});
