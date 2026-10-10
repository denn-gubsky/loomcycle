// adapters/ts/tests/team_decision.test.ts — a team walk's decision is read
// from the walk's own run. A client that starts a walk with mode "detach"
// gets no steps, so this stream is where it finds what a decision answered.

import { describe, expect, it } from "vitest";

import { makeClient, sseResponse } from "./helpers.js";
import type { AgentEvent, EventType, TeamDecisionInfo } from "../src/index.js";

describe("team_decision on a walk's run stream", () => {
  it("is delivered by streamRunByID with the edge taken and the model's answer", async () => {
    const decision = {
      state: "triage",
      visit: 2,
      edge: "conditional:billing",
      next: "billing-desk",
      answer: {
        model: "decide",
        answers: {
          route: { type: "choice", choice: "billing", probabilities: { billing: 0.91, support: 0.09 }, confidence: 0.82 },
          urgent: { type: "noul", noul: 0.4 },
        },
      },
    };
    const { client, fetchMock } = makeClient([
      sseResponse([
        'event: agent\ndata: {"agent_id":"team:triage","run_id":"r_walk","session_id":"s1"}\n\n',
        `event: team_decision\ndata: ${JSON.stringify({ type: "team_decision", team_decision: decision })}\n\n`,
      ]),
    ]);
    const seen: AgentEvent[] = [];
    for await (const ev of client.streamRunByID("r_walk")) {
      seen.push(ev);
    }
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/runs/r_walk/stream");

    // Narrowed the way a consumer does it: by the union member, then the
    // payload's own type.
    const name: EventType = "team_decision";
    const got: TeamDecisionInfo | undefined = seen.find((ev) => ev.type === name)?.team_decision;
    expect(got).toEqual(decision);
    expect(got?.answer.answers.route).toMatchObject({ choice: "billing" });
  });
});
