import { describe, expect, it } from "vitest";
import { draftPromptText, draftSettings } from "./draft";

const simple = {
  agent: "qa",
  segments: [{ role: "user", content: [{ type: "trusted-text", text: "review PR 7" }] }],
  sampling: { temperature: 0.2 },
};

describe("draftPromptText", () => {
  it("reads the prompt of the simple one-user-segment shape the run form creates", () => {
    expect(draftPromptText(simple)).toBe("review PR 7");
  });

  it("is null for any richer shape, so the editor never flattens it", () => {
    expect(draftPromptText(null)).toBeNull();
    expect(draftPromptText({ segments: [] })).toBeNull();
    expect(
      draftPromptText({
        segments: [
          { role: "user", content: [{ type: "trusted-text", text: "a" }] },
          { role: "user", content: [{ type: "trusted-text", text: "b" }] },
        ],
      }),
    ).toBeNull();
    expect(
      draftPromptText({ segments: [{ role: "user", content: [{ type: "image", media_type: "image/png" }] }] }),
    ).toBeNull();
    expect(
      draftPromptText({ segments: [{ role: "user", content: [{ type: "untrusted-block", text: "x" }] }] }),
    ).toBeNull();
    expect(draftPromptText({ segments: [{ role: "system", content: [{ type: "trusted-text", text: "x" }] }] })).toBeNull();
  });
});

describe("draftSettings", () => {
  it("leaves out the agent and the prompt and keeps every override", () => {
    expect(draftSettings(simple)).toEqual({ sampling: { temperature: 0.2 } });
    expect(draftSettings(undefined)).toEqual({});
  });
});
