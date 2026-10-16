import { describe, expect, it } from "vitest";
import { promptSegments } from "./transcriptPayload";

describe("promptSegments", () => {
  it("returns the segments of a stored prompt", () => {
    const segs = [
      { role: "system", content: [{ type: "text", text: "be brief" }] },
      { role: "user", content: [{ type: "trusted-text", text: "hello" }] },
    ];
    expect(promptSegments(segs)).toEqual(segs);
  });

  it("reads a no-trace marker as no segments", () => {
    // What the transcript holds for a no-trace run's user_input: the content
    // was not stored, only its size.
    expect(promptSegments({ type: "user_input", retained: false, bytes: 201 })).toEqual([]);
  });

  it("reads a missing payload as no segments", () => {
    expect(promptSegments(undefined)).toEqual([]);
    expect(promptSegments(null)).toEqual([]);
  });

  it("skips entries that are not segments", () => {
    const user = { role: "user", content: [] };
    expect(promptSegments([null, "x", 3, user])).toEqual([user]);
  });
});
