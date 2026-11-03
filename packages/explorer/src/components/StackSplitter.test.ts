import { describe, expect, it } from "vitest";
import { clampBottom } from "./StackSplitter";

// The links pane can be dragged anywhere between its minimum and the height
// that still leaves the text its minimum; never off either end.
describe("StackSplitter", () => {
  it("TestClampBottom_KeepsBothPanesUsable", () => {
    // 600 px column, text ≥ 96, links ≥ 48, 6 px handle → links in [48, 498].
    expect(clampBottom(240, 600, 96, 48)).toBe(240);
    expect(clampBottom(10, 600, 96, 48)).toBe(48);
    expect(clampBottom(590, 600, 96, 48)).toBe(498);
  });

  it("TestClampBottom_ATinyColumnKeepsTheLinksMinimum", () => {
    // Too small for both minimums: the links keep theirs (CSS shrinks the rest).
    expect(clampBottom(300, 100, 96, 48)).toBe(48);
  });
});
