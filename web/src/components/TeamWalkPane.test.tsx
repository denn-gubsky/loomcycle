import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";
import type { Agent, InterruptRow } from "../api";
import { TeamWalkView, type GraphState, type TeamWalkViewProps, type WalkActions } from "./TeamWalkPane";
import RunDetail from "./RunDetail";
import { emptyWalk, foldWalk, type WalkRunRow } from "../lib/walkView";

const WALK = "run_walk";
const noop = async () => {};
const actions: WalkActions = { review: noop, resolvePause: noop, saveBreakpoints: noop, cancelWalk: noop };

function walkRun(p: Partial<Agent> = {}): Agent {
  return {
    agent_id: "team:triage",
    run_id: WALK,
    session_id: "s",
    agent: "team:triage",
    parent_agent_id: "a_parent",
    parent_run_id: "run_parent",
    user_id: "alice",
    status: "running",
    started_at: "2026-10-01T10:00:00Z",
    completed_at: null,
    stop_reason: null,
    error: null,
    usage: {},
    last_heartbeat_at: null,
    live: true,
    ...p,
  };
}

function member(p: Partial<WalkRunRow> & { runId: string }): WalkRunRow {
  return {
    agentId: "a_" + p.runId,
    agent: "writer",
    status: "running",
    ts: "2026-10-01T10:00:00Z",
    state: "draft",
    stateVisit: 1,
    startedAt: "2026-10-01T10:00:00Z",
    inputTokens: 120,
    outputTokens: 30,
    ...p,
  };
}

function render(p: Partial<TeamWalkViewProps>): string {
  const props: TeamWalkViewProps = {
    runId: WALK,
    walk: walkRun(),
    view: emptyWalk(WALK),
    interrupts: [],
    breakpoints: { kind: "ready", data: { run_id: WALK, armed: ["draft:review"], review_ttl_seconds: 300 } },
    graph: { kind: "ready", source: "stateDiagram-v2\n  draft --> review", svg: "<svg id=\"g\"></svg>", renderErr: "" },
    now: Date.parse("2026-10-01T10:00:00Z"),
    actions,
    ...p,
  };
  return renderToStaticMarkup(createElement(MemoryRouter, null, createElement(TeamWalkView, props)));
}

const pause: InterruptRow = {
  interrupt_id: "ir_1",
  run_id: WALK,
  kind: "question",
  status: "pending",
  priority: "normal",
  created_at: "2026-10-01T10:00:00Z",
  question:
    'Team "triage": state "publish" paused BEFORE dispatching wave wv_1 (3 runs in the wave, 3 pending).\n[0] writer ← x\nReply `continue` …',
};

describe("TeamWalkView", () => {
  it("renders the header: team, status, user, parent run and result", () => {
    const html = render({
      walk: walkRun({ status: "completed", live: false, completed_at: "2026-10-01T10:02:30Z", result: { final_text: "shipped it" } }),
    });
    expect(html).toContain("team triage");
    expect(html).toContain('class="pill completed"');
    expect(html).toContain("user: alice");
    expect(html).toContain(`href="/agents?run=run_parent"`);
    expect(html).toContain("2m 30s");
    expect(html).toContain("shipped it");
  });

  it("lists members by visit, each linking to its own run", () => {
    const view = foldWalk(emptyWalk(WALK), [
      member({ runId: "m1", state: "draft", stateVisit: 1, status: "completed", completedAt: "2026-10-01T10:00:05Z" }),
      member({ runId: "m2", state: "review", stateVisit: 2, agent: "critic" }),
    ]);
    const html = render({ view });
    expect(html).toContain('href="/agents?run=m1"');
    expect(html).toContain('href="/agents?run=m2"');
    expect(html.indexOf("#1")).toBeLessThan(html.indexOf("#2"));
    expect(html).toContain("tokens 120 in / 30 out");
    expect(html).toContain("5.0s");
  });

  it("shows review controls only on a member held for review, with its countdown", () => {
    const view = foldWalk(emptyWalk(WALK), [
      member({ runId: "held", awaited: "review", holdExpiresAt: "2026-10-01T10:01:05Z" }),
      member({ runId: "busy", awaited: "channel", awaitedOn: "inbox" }),
    ]);
    const html = render({ view });
    expect(html.match(/Approve/g)).toHaveLength(1);
    expect(html).toContain("Reject…");
    expect(html).toContain("awaiting review");
    expect(html).toContain("1m 05s left");
    expect(html).toContain("awaiting channel");
  });

  it("hides the graph entirely when the principal may not read the team", () => {
    const hidden: GraphState = { kind: "hidden" };
    const html = render({ graph: hidden, view: foldWalk(emptyWalk(WALK), [member({ runId: "m1" })]) });
    expect(html).not.toContain("team-walk-graph");
    expect(html).not.toContain("current definition");
    // Everything else stays.
    expect(html).toContain("team triage");
    expect(html).toContain('href="/agents?run=m1"');
  });

  it("draws the graph with the current state, saying it is the current definition for a walk that recorded no version", () => {
    const html = render({ highlight: "draft" });
    expect(html).toContain('<svg id="g"></svg>');
    expect(html).toContain("<code>draft</code>");
    expect(html).toContain("current definition");
    expect(html).not.toContain("the version this walk ran");
  });

  it("says the graph is the version the walk ran when its run recorded one", () => {
    const html = render({ walk: walkRun({ spec: { team: { name: "triage", def_id: "tdf_ran", version: 4 } } }) });
    expect(html).toContain("Drawn from version 4 of the team&#x27;s definition, the version this walk ran.");
    expect(html).not.toContain("current definition");
  });

  it("offers continue, release and abort for a pending breakpoint pause", () => {
    const html = render({ interrupts: [pause] });
    expect(html).toContain("Paused before dispatching <code>publish</code>");
    expect(html).toContain("Continue (dispatch 3)");
    expect(html).toContain("Release 1");
    expect(html).toContain("Abort walk");
  });

  it("shows the breakpoints editor seeded from the walk's armed set", () => {
    const html = render({});
    expect(html).toContain('value="draft:review"');
    expect(html).toContain('value="300"');
  });

  it("renders a live walk with nothing armed, which the server reports as armed: null", () => {
    const html = render({ breakpoints: { kind: "ready", data: { run_id: WALK, armed: null, review_ttl_seconds: 0 } } });
    expect(html).toContain('placeholder="none armed"');
    expect(html).toContain('aria-label="armed breakpoints" value=""');
  });

  it("says why breakpoints are unavailable instead of failing", () => {
    const html = render({ breakpoints: { kind: "unavailable", reason: "not live on this one" } });
    expect(html).toContain("not live on this one");
  });

  it("hides every control once the walk is no longer live", () => {
    const view = foldWalk(emptyWalk(WALK), [member({ runId: "held", awaited: "review", holdExpiresAt: "2026-10-01T10:01:05Z" })]);
    const html = render({
      walk: walkRun({ status: "cancelled", live: false, completed_at: "2026-10-01T10:01:00Z" }),
      view,
      interrupts: [pause],
    });
    expect(html).not.toContain("cancel walk");
    expect(html).not.toContain("Approve");
    expect(html).not.toContain("Continue (dispatch");
    expect(html).not.toContain("Breakpoints");
  });

  it("offers cancel while live", () => {
    expect(render({})).toContain("cancel walk");
  });

  it("links the board document the members write to", () => {
    const view = foldWalk(emptyWalk(WALK), [member({ runId: "m1", boardDocumentId: "doc_42", boardScope: "tenant" })]);
    expect(render({ view })).toContain('href="/documents/doc_42?scope=tenant"');
  });
});

describe("RunDetail", () => {
  const html = (agents: Agent[], key: string) =>
    renderToStaticMarkup(
      createElement(MemoryRouter, null, createElement(RunDetail, { agents, selectedKey: key, runId: key })),
    );

  it("routes a listed team: row to the walk pane", () => {
    const out = html([walkRun()], WALK);
    expect(out).toContain("team-walk");
  });

  it("routes an ordinary row to the agent pane", () => {
    const plain = walkRun({ agent_id: "a_plain", agent: "writer", run_id: "run_plain" });
    const out = html([plain], "run_plain");
    expect(out).toContain("agent-detail");
    expect(out).not.toContain("team-walk");
  });
});
