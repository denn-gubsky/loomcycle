// RFC DI: a finished run's answer and the prompt its first model call received.
import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient } from "./helpers.js";

describe("a run's result and prompt", () => {
  it("getAgent surfaces the run's result", async () => {
    const { client } = makeClient([
      jsonResponse({
        agent_id: "a_1", run_id: "r_1", session_id: "s_1", agent: "chat",
        parent_agent_id: null, user_id: "u", status: "completed",
        started_at: "2026-09-23T00:00:00Z", completed_at: "2026-09-23T00:00:05Z",
        stop_reason: "end_turn", error: null, usage: { input_tokens: 1, output_tokens: 2 },
        last_heartbeat_at: null, live: false,
        result: { final_text: "the answer", state: { k: 1 } },
      }),
    ]);
    const a = await client.getAgent("a_1");
    expect(a.result?.final_text).toBe("the answer");
    expect(a.result?.state).toEqual({ k: 1 });
  });

  it("getRunPrompt GETs /prompt and returns the recorded system and input", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        run_id: "r_1",
        system: [{ type: "text", text: "You are a reviewer." }],
        input: [{ type: "text", text: "Review PR 7." }, { type: "image", media_type: "image/png" }],
      }),
    ]);
    const out = await client.getRunPrompt("r_1");

    const call = fetchMock.mock.calls[0]!;
    expect(String(call[0])).toContain("/v1/runs/r_1/prompt");
    expect(call[1]?.method ?? "GET").toBe("GET");
    expect(out.system[0]?.text).toBe("You are a reviewer.");
    expect(out.input[1]?.media_type).toBe("image/png");
  });
});
