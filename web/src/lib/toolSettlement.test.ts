import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import type { EventPayload } from "../api";
import { settledToolIds, toolResultId } from "./toolSettlement";

// Produced by internal/providers/tool_event_wire_fixture_test.go from the real
// providers.Event struct, so this test cannot share a wrong belief about the
// wire with the code it checks.
const frames: EventPayload[] = JSON.parse(
  readFileSync(new URL("./__fixtures__/tool-events.json", import.meta.url), "utf8"),
);

describe("tool-call settlement", () => {
  it("matches a tool_result to its call by the id the server actually sends", () => {
    const [call, result] = frames;
    expect(call.type).toBe("tool_call");
    expect(result.type).toBe("tool_result");
    expect(toolResultId(result)).toBe(call.tool_use?.id);
    expect(settledToolIds(frames.map((event) => ({ event }))).has(call.tool_use!.id)).toBe(true);
  });

  it("does not settle a call from a frame that is not a result", () => {
    const [call] = frames;
    expect(toolResultId(call)).toBe("");
    expect(settledToolIds([{ event: call }]).size).toBe(0);
  });
});
