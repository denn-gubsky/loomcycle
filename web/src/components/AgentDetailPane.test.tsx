import { createElement, type ReactElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import AgentDetailPane, { ChildrenAwaitOrChip, RunClockFacts, RunTeamBadge, stopRun } from "./AgentDetailPane";
import { AgentTabStrip } from "./AgentDetailTabs";

// The web tests render to static markup (no DOM), so effects never run. The
// pane's state is checked through what each selection mounts, and the cancel
// route through the fetch it issues.

function okFetch() {
  const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => new Response("{}", { status: 200 }));
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("AgentDetailPane selection", () => {
  // React remounts a component whose key changes, discarding all its state:
  // the previous run's header, events, error and poll timer.
  it("mounts fresh state for each new run selection", () => {
    const first = AgentDetailPane({ runId: "r_one" }) as ReactElement;
    const second = AgentDetailPane({ runId: "r_two" }) as ReactElement;
    expect(first.key).toBeTruthy();
    expect(second.key).toBeTruthy();
    expect(second.key).not.toBe(first.key);
    // Same run again keeps its state (no remount on an unrelated re-render).
    expect((AgentDetailPane({ runId: "r_one" }) as ReactElement).key).toBe(first.key);
  });

  it("mounts fresh state when an agent selection becomes a run selection", () => {
    const byAgent = AgentDetailPane({ agentId: "a_1" }) as ReactElement;
    const byRun = AgentDetailPane({ agentId: "a_1", runId: "r_1" }) as ReactElement;
    expect(byRun.key).not.toBe(byAgent.key);
  });

  it("asks for a specific walk for a team walk id with no run and reads nothing", () => {
    const fetchMock = okFetch();
    const html = renderToStaticMarkup(
      createElement(MemoryRouter, null, createElement(AgentDetailPane, { agentId: "team:x" })),
    );
    expect(html).toContain("is a team walk");
    expect(html).toContain("Pick a specific walk run");
    // The reading body (its loading placeholder and tab strip) is not mounted.
    expect(html).not.toContain("loading…");
    expect(html).not.toContain("agent-tabs");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("reads a team walk selected by run id", () => {
    const html = renderToStaticMarkup(
      createElement(MemoryRouter, null, createElement(AgentDetailPane, { agentId: "team:x", runId: "r_w" })),
    );
    expect(html).not.toContain("is a team walk");
    expect(html).toContain("loading…");
  });

  it("hides the memory and channels tabs for a team walk", () => {
    const html = renderToStaticMarkup(
      createElement(
        MemoryRouter,
        { initialEntries: ["/agents?run=r_w&tab=memory"] },
        createElement(AgentDetailPane, { agentId: "team:x", runId: "r_w" }),
      ),
    );
    expect(html).toContain(">transcript<");
    expect(html).toContain(">interrupts<");
    expect(html).not.toContain(">memory<");
    expect(html).not.toContain(">channels<");
    // A ?tab=memory link falls back to the transcript tab.
    expect(html).toMatch(/aria-selected="true"[^>]*>transcript</);
  });

  it("keeps every tab for an ordinary run", () => {
    const html = renderToStaticMarkup(
      createElement(MemoryRouter, null, createElement(AgentDetailPane, { agentId: "a_1", runId: "r_1" })),
    );
    for (const t of ["transcript", "memory", "interrupts", "channels"]) expect(html).toContain(`>${t}<`);
  });
});

describe("AgentTabStrip", () => {
  it("leaves hidden tabs out of the strip", () => {
    const html = renderToStaticMarkup(
      createElement(AgentTabStrip, { tab: "transcript", onChange: () => {}, hidden: ["memory", "channels"] }),
    );
    expect(html).toContain(">transcript<");
    expect(html).toContain(">interrupts<");
    expect(html).not.toContain(">memory<");
    expect(html).not.toContain(">channels<");
  });
});

describe("stopRun", () => {
  it("cancels a team walk by its run id", async () => {
    const fetchMock = okFetch();
    await stopRun({ agent_id: "team:triage", run_id: "r_walk" }, "why");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/v1/runs/r_walk/cancel");
    expect(init?.method).toBe("POST");
    expect(JSON.parse(String(init?.body))).toEqual({ reason: "why" });
  });

  it("cancels an ordinary run by its agent id", async () => {
    const fetchMock = okFetch();
    await stopRun({ agent_id: "a_123", run_id: "r_123" }, "why");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0]).toBe("/v1/agents/a_123/cancel");
  });

  it("refuses a team walk with no run id without calling the agent route", async () => {
    const fetchMock = okFetch();
    await expect(stopRun({ agent_id: "team:triage", run_id: "" }, "why")).rejects.toThrow(/no run id/);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe("RunTeamBadge", () => {
  const html = (spec?: Record<string, unknown>) => renderToStaticMarkup(createElement(RunTeamBadge, { run: { spec } }));

  it("names the team of a run of one of its own agents, saying it runs only inside a walk", () => {
    const out = html({ team_scope: { team: "sdlc", def_id: "tdf_1" }, agent_version: { team_def_id: "tdf_1" } });
    expect(out).toContain(">team: sdlc</span>");
    expect(out).toContain("cannot be started on its own");
  });

  it("names the team of a global agent run inside a walk, without calling it the team's own", () => {
    const out = html({ team_scope: { team: "sdlc", def_id: "tdf_1" }, agent_version: { def_id: "adf_9" } });
    expect(out).toContain(">team: sdlc</span>");
    expect(out).toContain("Runs inside a walk of team sdlc.");
  });

  it("renders nothing for a run outside any team, a walk's own run included", () => {
    expect(html(undefined)).toBe("");
    expect(html({ agent_version: { def_id: "adf_9" } })).toBe("");
    expect(html({ team: { name: "sdlc", def_id: "tdf_1", version: 1 } })).toBe("");
  });
});

describe("ChildrenAwaitOrChip", () => {
  const html = (agent: Parameters<typeof ChildrenAwaitOrChip>[0]["agent"]) =>
    renderToStaticMarkup(
      createElement(MemoryRouter, null, createElement(ChildrenAwaitOrChip, { agent, state: { kind: "running" } })),
    );

  it("names each background child a parked run waits for, linking to its run", () => {
    const out = html({ status: "running", awaited_state: "children", awaited_on: "r_c1, r_c2" });
    expect(out).toContain("waiting for background children");
    expect(out).toContain('href="/agents?run=r_c1"');
    expect(out).toContain('href="/agents?run=r_c2"');
    expect(out).not.toContain("more");
  });

  it("says how many more the server left out of a bounded list", () => {
    const out = html({ status: "running", awaited_state: "children", awaited_on: "r_c1, +3 more" });
    expect(out).toContain('href="/agents?run=r_c1"');
    expect(out).toContain("+3 more");
  });

  it("shows the transcript-derived chip for any other running run", () => {
    const out = html({ status: "running", awaited_state: "input" });
    expect(out).toContain("await-chip-running");
    expect(out).not.toContain("background children");
  });
});

describe("RunClockFacts", () => {
  const html = (run: Parameters<typeof RunClockFacts>[0]["run"]) => renderToStaticMarkup(createElement(RunClockFacts, { run }));

  it("shows a code agent's budget and its clock at its last pause", () => {
    const out = html({
      usage: { provider: "code-js" },
      spec: { run_timeout_seconds: 300, run_clock: { active_ms: 12_500, waited_ms: 95_000, wall_ms: 107_500 } },
    });
    expect(out).toContain("code budget: 5m 0s");
    expect(out).toContain("used 12.5s, waited 1m 35s, lived 1m 48s");
  });

  it("says the operator default applies when the run set no budget, and shows no clock before a pause", () => {
    const out = html({ usage: { provider: "code-js" }, spec: {} });
    expect(out).toContain("code budget: operator default");
    expect(out).not.toContain("at last pause");
  });

  it("shows nothing for a model-driven run", () => {
    expect(html({ usage: { provider: "anthropic" }, spec: { run_timeout_seconds: 300 } })).toBe("");
  });
});
