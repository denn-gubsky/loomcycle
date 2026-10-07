import { describe, expect, it } from "vitest";
import { applyRunPolicy, CATCH_UP_MAX_LIMIT } from "./ScheduleRunPolicyFields";

describe("applyRunPolicy", () => {
  it("leaves blank fields out, so create gets the default and fork inherits", () => {
    const overlay: Record<string, unknown> = {};
    expect(applyRunPolicy(overlay, "", "  ")).toBeNull();
    expect(overlay).toEqual({});
  });

  it("writes a chosen policy and catch_up_max, including an explicit 0", () => {
    const overlay: Record<string, unknown> = {};
    expect(applyRunPolicy(overlay, "replace", "0")).toBeNull();
    expect(overlay).toEqual({ concurrency_policy: "replace", catch_up_max: 0 });
  });

  it("refuses a catch_up_max the server would refuse", () => {
    for (const bad of ["-1", "2.5", "abc", String(CATCH_UP_MAX_LIMIT + 1)]) {
      const overlay: Record<string, unknown> = {};
      expect(applyRunPolicy(overlay, "", bad)).toMatch(/catch_up_max/);
      expect(overlay).not.toHaveProperty("catch_up_max");
    }
  });
});
