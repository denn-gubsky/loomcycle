// adapters/ts/tests/evaluation.test.ts — the Evaluation tool outside a run
// (POST /v1/_evaluation) on the wire: evaluation(input) passes the
// op-discriminated input through verbatim, and the positional helpers build
// the same input.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient, errorResponse } from "./helpers.js";
import {
  AuthError,
  InvalidArgumentError,
  SubstrateToolRefusedError,
} from "../src/index.js";
import type {
  EvaluationAggregate,
  EvaluationListForRunResult,
  EvaluationRecord,
  EvaluationSubmitResult,
} from "../src/index.js";

function sentBody(fetchMock: { mock: { calls: unknown[][] } }, i = 0) {
  const call = fetchMock.mock.calls[i]!;
  expect(call[0]).toBe("http://test-loomcycle:8787/v1/_evaluation");
  expect((call[1] as RequestInit).method).toBe("POST");
  return JSON.parse((call[1] as RequestInit).body as string);
}

const record: EvaluationRecord = {
  eval_id: "eval_1",
  run_id: "r_1",
  def_id: "def_1",
  score: 0.8,
  dimensions: { correctness: 0.9 },
  judgement: null,
  rationale: "good",
  emitter_role: "unrelated",
  emitter_agent_id: "a_http-admin",
  emitter_run_id: "",
  created_at: "2026-10-14T00:00:00.000000000Z",
};

describe("evaluation", () => {
  it("posts a submit to /v1/_evaluation and returns the stored row's identity", async () => {
    const submitted: EvaluationSubmitResult = {
      eval_id: "eval_1",
      run_id: "r_1",
      def_id: "def_1",
      emitter_role: "unrelated",
      score: 0.8,
      created_at: "2026-10-14T00:00:00.000000000Z",
    };
    const { client, fetchMock } = makeClient([jsonResponse(submitted)]);
    const res = await client.evaluation({
      op: "submit",
      run_id: "r_1",
      score: 0.8,
      dimensions: { correctness: 0.9 },
      rationale: "good",
    });
    expect(res.eval_id).toBe("eval_1");
    expect(res.emitter_role).toBe("unrelated");
    expect(sentBody(fetchMock)).toEqual({
      op: "submit",
      run_id: "r_1",
      score: 0.8,
      dimensions: { correctness: 0.9 },
      rationale: "good",
    });
  });

  it("keeps a score of 0, which is a score and not an absent field", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ eval_id: "e" })]);
    await client.submitEvaluation("r_1", 0);
    expect(sentBody(fetchMock)).toEqual({ op: "submit", run_id: "r_1", score: 0 });
  });

  it("submitEvaluation sends only the options it was given", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ eval_id: "e" })]);
    await client.submitEvaluation("r_1", -1, {
      judgement: { verdict: "wrong" },
      rationale: "missed the point",
    });
    expect(sentBody(fetchMock)).toEqual({
      op: "submit",
      run_id: "r_1",
      score: -1,
      judgement: { verdict: "wrong" },
      rationale: "missed the point",
    });
  });

  it("getEvaluation sends op get with the eval_id", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(record)]);
    const got = await client.getEvaluation("eval_1");
    expect(got.rationale).toBe("good");
    expect(sentBody(fetchMock)).toEqual({ op: "get", eval_id: "eval_1" });
  });

  it("listEvaluationsForRun sends the run and the limit", async () => {
    const listed: EvaluationListForRunResult = { run_id: "r_1", evaluations: [record] };
    const { client, fetchMock } = makeClient([jsonResponse(listed)]);
    const res = await client.listEvaluationsForRun("r_1", { limit: 5 });
    expect(res.evaluations).toHaveLength(1);
    expect(sentBody(fetchMock)).toEqual({ op: "list_for_run", run_id: "r_1", limit: 5 });
  });

  it("listEvaluationsForDef omits an unset limit", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ def_id: "def_1", evaluations: [] }),
    ]);
    const res = await client.listEvaluationsForDef("def_1");
    expect(res.evaluations).toEqual([]);
    expect(sentBody(fetchMock)).toEqual({ op: "list_for_def", def_id: "def_1" });
  });

  it("aggregateEvaluations maps includeLineage to include_lineage", async () => {
    const stats = { mean: 0.5, median: 0.5, min: 0, max: 1, latest: 1, count: 2 };
    const agg: EvaluationAggregate = {
      def_id: "def_1",
      count: 2,
      score: stats,
      by_emitter_role: { unrelated: stats },
      lineage_included: true,
    };
    const { client, fetchMock } = makeClient([jsonResponse(agg)]);
    const res = await client.aggregateEvaluations("def_1", { includeLineage: true });
    expect(res.score.mean).toBe(0.5);
    expect(sentBody(fetchMock)).toEqual({
      op: "aggregate",
      def_id: "def_1",
      include_lineage: true,
    });
  });

  it("raises SubstrateToolRefusedError when the token may not use the op", async () => {
    const refusal = JSON.stringify({
      code: "tool_refused",
      tool: "Evaluation",
      error: 'get: requires evaluation_scopes containing "read_any"',
    });
    const { client } = makeClient([
      () =>
        new Response(refusal, {
          status: 422,
          headers: { "Content-Type": "application/json" },
        }),
    ]);
    await expect(client.getEvaluation("eval_1")).rejects.toBeInstanceOf(
      SubstrateToolRefusedError,
    );
  });

  it("raises InvalidArgumentError on 400", async () => {
    const { client } = makeClient([errorResponse(400, "body is not valid JSON")]);
    await expect(client.getEvaluation("eval_1")).rejects.toBeInstanceOf(
      InvalidArgumentError,
    );
  });

  it("raises AuthError on 401", async () => {
    const { client } = makeClient([errorResponse(401, "unauthorized")]);
    await expect(client.getEvaluation("eval_1")).rejects.toBeInstanceOf(AuthError);
  });
});
