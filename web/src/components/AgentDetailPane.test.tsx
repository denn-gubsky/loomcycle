import { createElement, type ReactElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import AgentDetailPane, { stopRun } from "./AgentDetailPane";
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
