// RFC DJ: a run held for an operator's verdict.
import { describe, expect, it } from "vitest";

import { LoomcycleError } from "../src/errors.js";
import { errorResponse, jsonResponse, makeClient, sseResponse } from "./helpers.js";

function sentBody(call: unknown[]): Record<string, unknown> {
  return JSON.parse((call[1] as RequestInit).body as string);
}

describe("reviewRun", () => {
  it("POSTs the decision and the feedback", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ run_id: "r_1", decision: "reject", delivered: true }),
    ]);
    const res = await client.reviewRun("r_1", "reject", { feedback: "redo section 3" });
    expect(res.delivered).toBe(true);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/runs/r_1/review");
    expect((init as RequestInit).method).toBe("POST");
    expect(sentBody(fetchMock.mock.calls[0]!)).toEqual({ decision: "reject", feedback: "redo section 3" });
  });

  it("sends an approval with no feedback key", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ run_id: "a/b", decision: "approve", delivered: true }),
    ]);
    await client.reviewRun("a/b", "approve");
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/runs/a%2Fb/review");
    expect(sentBody(fetchMock.mock.calls[0]!)).toEqual({ decision: "approve" });
  });

  it("surfaces a run that is not held as a 409", async () => {
    const { client } = makeClient([
      errorResponse(409, JSON.stringify({ code: "not_held", error: "the run is not held for review" })),
    ]);
    const err = await client.reviewRun("r_1", "approve").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(409);
  });
});

describe("the review option", () => {
  it("rides on a run start and on a retune", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse([`event: done\ndata: {"type":"done"}\n\n`]),
      jsonResponse({ run_id: "r_1" }),
    ]);
    for await (const _ of client.runStreaming({
      agent: "qa",
      segments: [{ role: "user", content: [{ type: "trusted-text", text: "hi" }] }],
      review: true,
    })) {
      // drain
    }
    expect(sentBody(fetchMock.mock.calls[0]!).review).toBe(true);
    await client.retuneRun("r_1", { review: false });
    expect(sentBody(fetchMock.mock.calls[1]!).review).toBe(false);
  });
});
