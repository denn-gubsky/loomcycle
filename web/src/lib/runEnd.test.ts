import { describe, expect, it } from "vitest";
import { errorFrameStatus } from "./runEnd";

describe("errorFrameStatus", () => {
  it("shows a run stopped mid-turn as cancelled, not failed", () => {
    // The row is cancelled; the terminal used to call it failed because a
    // mid-turn cancel ends the stream on an error frame.
    expect(
      errorFrameStatus({
        type: "error",
        error: "context canceled",
        error_info: { category: "cancelled", is_retryable: false },
      }),
    ).toBe("cancelled");
  });

  it("keeps every other error a failure", () => {
    expect(errorFrameStatus({ type: "error", error: "provider error: boom" })).toBe("failed");
    expect(
      errorFrameStatus({
        type: "error",
        error: "budget",
        error_info: { category: "business", is_retryable: false },
      }),
    ).toBe("failed");
    // An unclassified "context canceled" is not read from its text.
    expect(errorFrameStatus({ type: "error", error: "context canceled" })).toBe("failed");
  });
});
