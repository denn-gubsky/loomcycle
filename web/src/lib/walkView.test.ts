import { describe, expect, it } from "vitest";
import type { Agent, InterruptRow, RunStateEvent, WalkRunsPage } from "../api";
import {
  boardDocuments,
  countdownLabel,
  currentState,
  emptyWalk,
  fetchAllWalkPages,
  foldWalk,
  holdSecondsLeft,
  httpStatusOf,
  parseBreakpointList,
  parseBreakpointPause,
  parseReviewTTL,
  rowFromAgent,
  rowFromEvent,
  runPaneFor,
  visitGroups,
  watchWalk,
  type WalkRunRow,
} from "./walkView";
import { paneRunId, selectedRowKey } from "./runLineage";

const WALK = "run_walk";

function row(p: Partial<WalkRunRow> & { runId: string }): WalkRunRow {
  return { agentId: "a_" + p.runId, agent: "writer", status: "running", ts: "2026-10-01T10:00:00Z", ...p };
}

function agent(p: Partial<Agent> & { run_id: string }): Agent {
  return {
    agent_id: "a_" + p.run_id,
    session_id: "s",
    agent: "writer",
    parent_agent_id: null,
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

function event(p: Partial<RunStateEvent> & { run_id: string }): RunStateEvent {
  return {
    agent_id: "a_" + p.run_id,
    agent: "writer",
    user_id: "alice",
    status: "running",
    ts: "2026-10-01T10:00:00Z",
    parent_context: { walk_id: WALK },
    ...p,
  };
}

function pauseQuestion(state: string, pending: number, size: number): string {
  return (
    `Team "triage": state ${JSON.stringify(state)} paused BEFORE dispatching wave wv_1 (${size} runs in the wave, ${pending} pending).\n` +
    `[0] writer ← hello\n` +
    `Reply \`continue\` to dispatch all ${pending}, \`release:<n>\` to dispatch the first n and pause again, or \`abort\` to stop the walk.`
  );
}

function interrupt(p: Partial<InterruptRow>): InterruptRow {
  return {
    interrupt_id: "ir_1",
    run_id: WALK,
    kind: "question",
    status: "pending",
    priority: "normal",
    created_at: "2026-10-01T10:00:00Z",
    ...p,
  };
}

describe("foldWalk", () => {
  it("keeps the newer row when an older listing page lands after a stream frame", () => {
    let v = foldWalk(emptyWalk(WALK), [row({ runId: "m1", status: "running", awaited: "review", ts: "2026-10-01T10:00:05Z" })]);
    v = foldWalk(v, [row({ runId: "m1", status: "running", ts: "2026-10-01T10:00:01Z" })]);
    expect(v.members.get("m1")?.awaited).toBe("review");
  });

  it("takes a newer row, and a cleared hold arrives as the field's absence", () => {
    let v = foldWalk(emptyWalk(WALK), [row({ runId: "m1", awaited: "review", holdExpiresAt: "2026-10-01T10:05:00Z" })]);
    v = foldWalk(v, [row({ runId: "m1", ts: "2026-10-01T10:00:09Z" })]);
    expect(v.members.get("m1")?.awaited).toBeUndefined();
    expect(v.members.get("m1")?.holdExpiresAt).toBeUndefined();
  });

  it("keeps a terminal row against any later non-terminal one", () => {
    let v = foldWalk(emptyWalk(WALK), [row({ runId: "m1", status: "completed", ts: "2026-10-01T10:00:05Z" })]);
    v = foldWalk(v, [row({ runId: "m1", status: "running", ts: "2026-10-01T10:09:00Z" })]);
    expect(v.members.get("m1")?.status).toBe("completed");
    // A terminal row can still replace a terminal one that is not newer.
    v = foldWalk(v, [row({ runId: "m1", status: "failed", ts: "2026-10-01T10:00:05Z" })]);
    expect(v.members.get("m1")?.status).toBe("failed");
  });

  it("compares instants as times, not strings", () => {
    // As strings "…:05Z" > "…:05.500Z", which would keep the older row.
    let v = foldWalk(emptyWalk(WALK), [row({ runId: "m1", status: "running", ts: "2026-10-01T10:00:05Z" })]);
    v = foldWalk(v, [row({ runId: "m1", status: "completed", ts: "2026-10-01T10:00:05.500Z" })]);
    expect(v.members.get("m1")?.status).toBe("completed");
  });

  it("orders a listing's local-offset instant against the stream's UTC one", () => {
    // As served: the listing writes "+03:00" with microseconds, the stream
    // UTC whole seconds. As strings the 12:… listing row always looks newer.
    let v = foldWalk(emptyWalk(WALK), [
      rowFromAgent(agent({ run_id: "m1", awaited_state: "review", last_heartbeat_at: "2026-10-01T12:37:14.827396+03:00" })),
    ]);
    v = foldWalk(v, [rowFromEvent(event({ run_id: "m1", ts: "2026-10-01T09:37:48Z" }))]);
    expect(v.members.get("m1")?.awaited).toBeUndefined();
  });

  it("carries the graph place and listing-only facts a stream frame omits", () => {
    const listed = rowFromAgent(
      agent({
        run_id: "m1",
        usage: { input_tokens: 10, output_tokens: 4 },
        parent_context: { walk_id: WALK, state: "draft", state_visit: 2, board_document_id: "doc_1", board_scope: "tenant" },
      }),
    );
    let v = foldWalk(emptyWalk(WALK), [listed]);
    v = foldWalk(v, [rowFromEvent(event({ run_id: "m1", status: "completed", ts: "2026-10-01T10:01:00Z", parent_context: { walk_id: WALK } }))]);
    const m = v.members.get("m1")!;
    expect(m).toMatchObject({
      status: "completed",
      state: "draft",
      stateVisit: 2,
      inputTokens: 10,
      outputTokens: 4,
      startedAt: "2026-10-01T10:00:00Z",
      completedAt: "2026-10-01T10:01:00Z",
      boardDocumentId: "doc_1",
    });
  });

  it("keeps a hold's deadline while the run stays held, though listing rows never carry it", () => {
    let v = foldWalk(emptyWalk(WALK), [
      rowFromEvent(event({ run_id: "m1", awaited_state: "review", hold_expires_at: "2026-10-01T10:05:00Z", ts: "2026-10-01T10:00:01Z" })),
    ]);
    v = foldWalk(v, [rowFromAgent(agent({ run_id: "m1", awaited_state: "review", last_heartbeat_at: "2026-10-01T10:00:30Z" }))]);
    expect(v.members.get("m1")?.holdExpiresAt).toBe("2026-10-01T10:05:00Z");
  });

  it("files the walk's own run as the walk, not a member", () => {
    const v = foldWalk(emptyWalk(WALK), [row({ runId: WALK, agentId: "team:triage" }), row({ runId: "m1" })]);
    expect(v.walk?.runId).toBe(WALK);
    expect([...v.members.keys()]).toEqual(["m1"]);
  });

  it("returns the same view when nothing changes", () => {
    const v = foldWalk(emptyWalk(WALK), [row({ runId: "m1", status: "completed" })]);
    expect(foldWalk(v, [row({ runId: "m1", status: "running", ts: "2026-10-01T11:00:00Z" })])).toBe(v);
  });
});

describe("visitGroups", () => {
  it("groups by visit, then state, each group in wave order", () => {
    const v = foldWalk(emptyWalk(WALK), [
      row({ runId: "c", state: "review", stateVisit: 2 }),
      row({ runId: "b2", state: "draft", stateVisit: 1, waveId: "w", waveIndex: 1 }),
      row({ runId: "b1", state: "draft", stateVisit: 1, waveId: "w", waveIndex: 0 }),
      row({ runId: "d", state: "draft", stateVisit: 3 }),
      row({ runId: "e", state: "alpha", stateVisit: 2 }),
    ]);
    const g = visitGroups(v);
    expect(g.map((x) => `${x.visit}:${x.state}`)).toEqual(["1:draft", "2:alpha", "2:review", "3:draft"]);
    expect(g[0].members.map((m) => m.runId)).toEqual(["b1", "b2"]);
  });

  it("keeps a member with no recorded visit, ahead of the counted ones", () => {
    const g = visitGroups(foldWalk(emptyWalk(WALK), [row({ runId: "x", state: "s", stateVisit: 1 }), row({ runId: "y" })]));
    expect(g.map((x) => x.visit)).toEqual([0, 1]);
  });
});

describe("currentState", () => {
  const view = foldWalk(emptyWalk(WALK), [
    row({ runId: "m1", state: "draft", stateVisit: 1, status: "completed" }),
    row({ runId: "m2", state: "review", stateVisit: 2, awaited: "review" }),
    row({ runId: "m3", state: "draft", stateVisit: 3, status: "failed" }),
  ]);

  it("is the highest visit among members still running or waiting", () => {
    expect(currentState(view, [])).toBe("review");
  });

  it("is the pending breakpoint pause's state when the walk is paused", () => {
    expect(currentState(view, [interrupt({ question: pauseQuestion("publish", 3, 3) })])).toBe("publish");
  });

  it("ignores a question that is not a breakpoint pause", () => {
    const cap = 'Team "triage": state "draft" hit its iteration cap (4 entries > max 3). Reply `continue` …';
    expect(currentState(view, [interrupt({ question: cap })])).toBe("review");
  });

  it("is undefined when nothing runs and nothing is paused", () => {
    expect(currentState(foldWalk(emptyWalk(WALK), [row({ runId: "m", state: "s", status: "completed" })]), [])).toBeUndefined();
  });
});

describe("parseBreakpointPause", () => {
  it("reads the state, wave and counts from the walk's question", () => {
    expect(parseBreakpointPause(pauseQuestion("draft", 2, 5))).toEqual({ state: "draft", wave: "wv_1", waveSize: 5, pending: 2 });
  });

  it("reads the question exactly as a live walk asked it", () => {
    const live =
      'Team "uiwalk": state "fan" paused BEFORE dispatching wave wav_0bb3a3aae3a6b6c1 (1 runs in the wave, 1 pending).\n' +
      '[0] worker ← {"task":"one"}\n' +
      "Reply `continue` to dispatch all 1, `release:<n>` to dispatch the first n and pause again, or `abort` to stop the walk.";
    expect(parseBreakpointPause(live)).toEqual({ state: "fan", wave: "wav_0bb3a3aae3a6b6c1", waveSize: 1, pending: 1 });
  });

  it("unquotes a state id with an escaped quote", () => {
    expect(parseBreakpointPause(pauseQuestion('say "hi"', 1, 1))?.state).toBe('say "hi"');
  });

  it("is undefined for any other text", () => {
    expect(parseBreakpointPause("Should I proceed?")).toBeUndefined();
    expect(parseBreakpointPause(undefined)).toBeUndefined();
  });
});

describe("fetchAllWalkPages", () => {
  it("follows next_cursor through every page", async () => {
    const pages: Record<string, WalkRunsPage> = {
      "": { agents: [agent({ run_id: "a" })], next_cursor: "c1" },
      c1: { agents: [agent({ run_id: "b" })], next_cursor: "c2" },
      c2: { agents: [agent({ run_id: "c" })], next_cursor: "" },
    };
    const asked: (string | undefined)[] = [];
    const all = await fetchAllWalkPages(async (c) => {
      asked.push(c);
      return pages[c ?? ""];
    });
    expect(all.map((a) => a.run_id)).toEqual(["a", "b", "c"]);
    expect(asked).toEqual([undefined, "c1", "c2"]);
  });

  it("stops instead of looping when a cursor repeats", async () => {
    let calls = 0;
    const all = await fetchAllWalkPages(async () => {
      calls++;
      return { agents: [agent({ run_id: `r${calls}` })], next_cursor: "same" };
    });
    expect(calls).toBe(2);
    expect(all).toHaveLength(2);
  });
});

describe("watchWalk", () => {
  const tick = (ms = 0) => new Promise((r) => setTimeout(r, ms));

  it("hydrates, streams, and re-hydrates after the stream's cap ends it", async () => {
    let lists = 0;
    let streams = 0;
    const got: WalkRunRow[] = [];
    const stop = watchWalk(
      {
        listPage: async () => {
          lists++;
          return { agents: [agent({ run_id: WALK, agent_id: "team:triage" }), agent({ run_id: "m1" })], next_cursor: "" };
        },
        stream: async (userId, onEvent) => {
          streams++;
          expect(userId).toBe("alice");
          onEvent(event({ run_id: "m1", awaited_state: "review", ts: "2026-10-01T10:00:02Z" }));
          // Another walk's run must not reach the view.
          onEvent(event({ run_id: "other", parent_context: { walk_id: "run_else" } }));
          // Returning = the server closed the stream at its lifetime cap.
        },
        readWalk: async () => agent({ run_id: WALK, agent_id: "team:triage" }),
      },
      WALK,
      { onRows: (rows) => got.push(...rows), onWalk: () => {} },
      { reconnectMs: 5, pollMs: 10_000 },
    );
    await tick(40);
    stop();
    expect(streams).toBeGreaterThanOrEqual(2);
    expect(lists).toBeGreaterThanOrEqual(2);
    expect(got.some((r) => r.runId === "m1" && r.awaited === "review")).toBe(true);
    expect(got.some((r) => r.runId === "other")).toBe(false);
  });

  it("folds one last listing and stops once the walk's own run has ended", async () => {
    let lists = 0;
    let streams = 0;
    let walks = 0;
    const stop = watchWalk(
      {
        listPage: async () => {
          lists++;
          return { agents: [agent({ run_id: "m1", status: "completed" })], next_cursor: "" };
        },
        stream: async () => {
          streams++;
        },
        readWalk: async () => {
          walks++;
          return agent({ run_id: WALK, agent_id: "team:triage", status: "completed", completed_at: "2026-10-01T10:02:00Z" });
        },
      },
      WALK,
      { onRows: () => {}, onWalk: () => {} },
      { reconnectMs: 5, pollMs: 5 },
    );
    await tick(40);
    stop();
    expect(walks).toBe(1);
    expect(lists).toBe(1);
    expect(streams).toBe(0);
  });
});

describe("runPaneFor", () => {
  const walkRow = agent({ run_id: WALK, agent_id: "team:triage", agent: "team:triage" });
  const plain = agent({ run_id: "run_plain" });

  it("opens the walk pane for a listed team: row, by its run id", () => {
    expect(runPaneFor([walkRow, plain], WALK, WALK)).toEqual({ kind: "walk", runId: WALK });
  });

  it("opens the agent pane for an ordinary row", () => {
    expect(runPaneFor([walkRow, plain], "run_plain", "run_plain")).toEqual({ kind: "agent" });
  });

  it("asks the caller to read a run the listing does not hold", () => {
    expect(runPaneFor([plain], "run_x", "run_x")).toEqual({ kind: "unknown", runId: "run_x" });
  });

  it("opens the newest listed walk for a legacy ?agent=team: link", () => {
    const older = agent({ run_id: "run_old", agent_id: "team:triage", started_at: "2026-10-01T09:00:00Z" });
    const agents = [older, walkRow];
    expect(
      runPaneFor(agents, selectedRowKey(agents, undefined, "team:triage"), paneRunId(agents, undefined, "team:triage")),
    ).toEqual({ kind: "walk", runId: WALK });
  });

  it("leaves an agent-only selection with no listed row to the agent pane", () => {
    expect(runPaneFor([], "team:triage", undefined)).toEqual({ kind: "agent" });
  });
});

describe("small helpers", () => {
  it("parses the breakpoint list as the whole set, de-duplicated", () => {
    expect(parseBreakpointList(" draft, review:review\nreview:review  publish ")).toEqual(["draft", "review:review", "publish"]);
    expect(parseBreakpointList("  ")).toEqual([]);
  });

  it("parses the review deadline as whole seconds", () => {
    expect(parseReviewTTL("")).toEqual({ ok: true, seconds: 0 });
    expect(parseReviewTTL(" 300 ")).toEqual({ ok: true, seconds: 300 });
    expect(parseReviewTTL("-1").ok).toBe(false);
    expect(parseReviewTTL("1.5").ok).toBe(false);
  });

  it("reads the status an api call failed with", () => {
    expect(httpStatusOf(new Error("403 Forbidden: missing scope"))).toBe(403);
    expect(httpStatusOf(new Error("network down"))).toBeUndefined();
  });

  it("counts a hold down and floors it at zero", () => {
    const now = Date.parse("2026-10-01T10:00:00Z");
    expect(holdSecondsLeft("2026-10-01T10:01:05Z", now)).toBe(65);
    expect(holdSecondsLeft("2026-10-01T09:00:00Z", now)).toBe(0);
    expect(holdSecondsLeft(undefined, now)).toBeUndefined();
    expect(countdownLabel(65)).toBe("1m 05s");
    expect(countdownLabel(9)).toBe("9s");
    expect(countdownLabel(0)).toBe("expiring");
  });

  it("lists each board document once", () => {
    const v = foldWalk(emptyWalk(WALK), [
      row({ runId: "a", boardDocumentId: "doc_1", boardScope: "tenant" }),
      row({ runId: "b", boardDocumentId: "doc_1" }),
      row({ runId: "c" }),
    ]);
    expect(boardDocuments(v)).toEqual([{ documentId: "doc_1", scope: "tenant" }]);
  });
});
