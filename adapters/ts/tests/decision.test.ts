/**
 * Tests for the decision-model client surface:
 *   - decide()              → POST /v1/_decide
 *   - listDecisionModels()  → GET  /v1/_decide/models
 */

import { describe, it, expect } from "vitest";
import { makeClient, jsonResponse } from "./helpers.js";
import { LoomcycleError } from "../src/errors.js";
import type { DecideRequest, DecisionAnswer } from "../src/types.js";

const state = {
  ticket: "My invoice for March was charged twice and I want my money back.",
  customer_tier: "gold",
};

// The three question types in one request, as the server documents them.
const request: DecideRequest = {
  state,
  questions: {
    route: {
      type: "choice",
      instructions: "Which team should handle this ticket?",
      criteria: { billing: "invoices, refunds, charges", support: null },
    },
    urgent: { type: "noul", instructions: "Does this ticket need a reply within the hour?" },
    detail: {
      type: "score",
      instructions: "How complete is the problem report?",
      criteria: ["no detail", "some detail", "everything needed"],
    },
  },
};

describe("decide", () => {
  it("POSTs /v1/_decide with state + questions, and no model unless one is named", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ model: "decide", provider: "p", served_model: "m", answers: {}, usage: { input_tokens: 0, output_tokens: 0 } }),
      jsonResponse({ model: "decide-deep", provider: "p", served_model: "m", answers: {}, usage: { input_tokens: 0, output_tokens: 0 } }),
    ]);

    await client.decide(request);
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_decide");
    expect(call[1]!.method).toBe("POST");
    expect(call[1]!.headers.Authorization).toBe("Bearer test-bearer");
    // The body is the caller's, key for key: a `null` criteria value and a
    // question without criteria reach the server as written.
    expect(JSON.parse(call[1]!.body as string)).toEqual({
      state,
      questions: {
        route: {
          type: "choice",
          instructions: "Which team should handle this ticket?",
          criteria: { billing: "invoices, refunds, charges", support: null },
        },
        urgent: { type: "noul", instructions: "Does this ticket need a reply within the hour?" },
        detail: {
          type: "score",
          instructions: "How complete is the problem report?",
          criteria: ["no detail", "some detail", "everything needed"],
        },
      },
    });

    await client.decide({ ...request, model: "decide-deep" });
    expect(JSON.parse(fetchMock.mock.calls[1]![1]!.body as string).model).toBe("decide-deep");
  });

  it("returns each answer under its question's name, narrowed by type", async () => {
    const { client } = makeClient([
      jsonResponse({
        model: "decide",
        provider: "ollama-local",
        served_model: "nimble",
        answers: {
          detail: {
            type: "score",
            score: 1.874,
            legend: { "0": "no detail", "1": "some detail", "2": "everything needed" },
            probabilities: { "0": 0.039, "1": 0.048, "2": 0.913 },
            confidence: 0.677,
          },
          route: {
            type: "choice",
            choice: "billing",
            probabilities: { billing: 0.983, support: 0.017 },
            confidence: 0.911,
          },
          urgent: { type: "noul", noul: 0.316 },
        },
        usage: { input_tokens: 1059, output_tokens: 4 },
      }),
    ]);

    const res = await client.decide(request);
    expect(res.model).toBe("decide");
    expect(res.provider).toBe("ollama-local");
    expect(res.served_model).toBe("nimble");
    expect(res.usage).toEqual({ input_tokens: 1059, output_tokens: 4 });
    expect(Object.keys(res.answers).sort()).toEqual(["detail", "route", "urgent"]);

    // Each branch reads fields only its own answer type has: this compiles
    // only while the union discriminates on `type`.
    const seen: string[] = [];
    for (const a of Object.values(res.answers)) {
      switch (a.type) {
        case "choice":
          expect(a.choice).toBe("billing");
          expect(a.probabilities.billing).toBe(0.983);
          expect(a.confidence).toBe(0.911);
          break;
        case "noul":
          expect(a.noul).toBe(0.316);
          break;
        case "score":
          expect(a.score).toBe(1.874);
          expect(a.legend["2"]).toBe("everything needed");
          expect(a.probabilities["2"]).toBe(0.913);
          expect(a.confidence).toBe(0.677);
          break;
      }
      seen.push(a.type);
    }
    expect(seen.sort()).toEqual(["choice", "noul", "score"]);
  });

  it("keeps a noul of 0 as the number 0, and a field the types do not name", async () => {
    const { client } = makeClient([
      jsonResponse({
        model: "decide",
        provider: "p",
        served_model: "m",
        answers: { urgent: { type: "noul", noul: 0, margin: 0.25 } },
        usage: { input_tokens: 9, output_tokens: 1 },
      }),
    ]);

    const res = await client.decide({
      state: {},
      questions: { urgent: { type: "noul", instructions: "Urgent?", criteria: { true: "needs a reply now" } } },
    });
    const a: DecisionAnswer = res.answers.urgent!;
    expect(a.type).toBe("noul");
    if (a.type !== "noul") throw new Error("unreachable");
    expect(a.noul).toBe(0);
    expect(a.noul).not.toBeUndefined();
    expect(a.margin).toBe(0.25);
  });

  it("types criteria per question type", () => {
    const bad: DecideRequest = {
      state: {},
      questions: {
        // @ts-expect-error a score's criteria is an array of level descriptions
        a: { type: "score", instructions: "q", criteria: { low: "x" } },
        // @ts-expect-error a choice's criteria is an object of options
        b: { type: "choice", instructions: "q", criteria: ["x", "y"] },
        // @ts-expect-error a choice needs criteria
        c: { type: "choice", instructions: "q" },
        // @ts-expect-error a noul's criteria describes only true and false
        d: { type: "noul", instructions: "q", criteria: { maybe: "x" } },
      },
    };
    expect(Object.keys(bad.questions)).toHaveLength(4);
  });

  // Every status the endpoint documents, with a code it carries there.
  const refusals: Array<[number, string]> = [
    [400, "invalid_input"],
    [400, "bad_question"],
    [400, "model_not_allowed"],
    [403, "operator_key_restricted"],
    [413, "prompt_too_large"],
    [429, "token_limit_exceeded"],
    [502, "model_not_found"],
    [502, "call_failed"],
    [503, "decision_not_configured"],
    [504, "timeout"],
  ];
  for (const [status, code] of refusals) {
    it(`surfaces a ${status} ${code} as a thrown error carrying the code and status`, async () => {
      const { client } = makeClient([
        jsonResponse(
          { code, error: `Decision: ${code}: refused`, errorCategory: "validation", isRetryable: false },
          status,
        ),
      ]);
      const err = await client.decide(request).then(
        () => undefined,
        (e: unknown) => e,
      );
      expect(err).toBeInstanceOf(LoomcycleError);
      expect((err as LoomcycleError).code).toBe(code);
      expect((err as LoomcycleError).status).toBe(status);
      expect((err as LoomcycleError).message).toContain(`Decision: ${code}: refused`);
    });
  }
});

describe("listDecisionModels", () => {
  it("GETs /v1/_decide/models and returns the default + each model's limits", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        default: "decide",
        models: [
          { name: "decide", provider: "ollama-local", model: "nimble", limits: { max_questions: 64, min_options: 2, max_options: 26 } },
          { name: "decide-deep", provider: "ollama-local", model: "nimble-xl", limits: { max_questions: 64, min_options: 2, max_options: 26 } },
        ],
      }),
    ]);

    const res = await client.listDecisionModels();
    expect(res.default).toBe("decide");
    expect(res.models.map((m) => m.name)).toEqual(["decide", "decide-deep"]);
    expect(res.models[1]!.model).toBe("nimble-xl");
    expect(res.models[0]!.limits).toEqual({ max_questions: 64, min_options: 2, max_options: 26 });

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_decide/models");
    expect(call[1]!.method).toBe("GET");
  });

  it("throws decision_not_configured (503) on a deployment that declares none", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "decision_not_configured", error: "this deployment declares no decision models" }, 503),
    ]);
    const err = await client.listDecisionModels().then(
      () => undefined,
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).code).toBe("decision_not_configured");
    expect((err as LoomcycleError).status).toBe(503);
  });
});
