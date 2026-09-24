// RFC DI: a run created now and started later.
import { describe, expect, it } from "vitest";

import { errorResponse, jsonResponse, makeClient, noContentResponse, sseResponse } from "./helpers.js";

const draftReply = {
  run_id: "r_1", agent_id: "a_1", session_id: "s_1", status: "configured",
  draft: { agent: "qa", segments: [] },
};

function sentBody(call: unknown[]): Record<string, unknown> {
  return JSON.parse((call[1] as RequestInit).body as string);
}

describe("configured runs", () => {
  it("createConfiguredRun posts the run with start:false", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(draftReply, 201)]);
    const r = await client.createConfiguredRun({
      agent: "qa",
      segments: [{ role: "user", content: [{ type: "trusted-text", text: "hi" }] }],
      toolChoice: { mode: "none" },
    });
    expect(r.status).toBe("configured");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/runs");
    expect((init as RequestInit).method).toBe("POST");
    const body = sentBody(fetchMock.mock.calls[0]!);
    expect(body.start).toBe(false);
    expect(body.agent).toBe("qa");
    expect(body.tool_choice).toEqual({ mode: "none" });
  });

  it("updateConfiguredRun PATCHes only the set fields, and null for a removal", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(draftReply)]);
    await client.updateConfiguredRun("r_1", { sampling: { temperature: 0.2 }, maxTokens: 100 }, { remove: ["tool_choice"] });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/runs/r_1");
    expect((init as RequestInit).method).toBe("PATCH");
    const body = sentBody(fetchMock.mock.calls[0]!);
    expect(body).toEqual({ sampling: { temperature: 0.2 }, max_tokens: 100, tool_choice: null });
  });

  it("startConfiguredRun streams the run and passes the secrets to start", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse([
        'event: agent\ndata: {"agent_id":"a_1","run_id":"r_1","session_id":"s_1"}\n\n',
        'event: done\ndata: {"type":"done","stop_reason":"end_turn"}\n\n',
      ]),
    ]);
    const types: string[] = [];
    for await (const ev of client.startConfiguredRun("r_1", { userBearer: "b".repeat(20) })) {
      types.push((ev as { type: string }).type);
    }
    expect(types).toContain("done");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/runs/r_1/start");
    expect((init as RequestInit).method).toBe("POST");
    expect(sentBody(fetchMock.mock.calls[0]!)).toEqual({ user_bearer: "b".repeat(20) });
  });

  it("a start refused at admission throws before any event", async () => {
    const { client } = makeClient([errorResponse(429, '{"code":"backpressure","error":"busy"}')]);
    await expect(async () => {
      for await (const _ of client.startConfiguredRun("r_1")) {
        void _;
      }
    }).rejects.toThrow();
  });

  it("deleteConfiguredRun sends DELETE", async () => {
    const { client, fetchMock } = makeClient([noContentResponse()]);
    await client.deleteConfiguredRun("r_1");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/runs/r_1");
    expect((init as RequestInit).method).toBe("DELETE");
  });
});
