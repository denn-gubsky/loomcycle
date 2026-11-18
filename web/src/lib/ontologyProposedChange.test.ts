import { describe, expect, it } from "vitest";
import { proposedChangeText } from "./ontologyProposedChange";

const base = {
  target: "c1",
  target_name: "supervises",
  section: "relations",
  kind: "relation",
};

describe("proposedChangeText", () => {
  it("lists an edit's changes, directives one per line", () => {
    expect(
      proposedChangeText({
        ...base,
        op: "edit",
        name: "oversees",
        parent: "c2",
        parent_name: "manages",
        sets_directives: true,
        directives: [
          { name: "domain", value: "Person" },
          { name: "functional" },
        ],
      }),
    ).toBe(
      [
        "rename supervises to oversees",
        "move under manages",
        "directives:",
        "    @domain Person",
        "    @functional",
      ].join("\n"),
    );
  });
  it("says what a retire keeps", () => {
    expect(proposedChangeText({ ...base, op: "retire" })).toBe(
      "retire the relation supervises: nothing new may use it; what uses it stays",
    );
  });
  it("names an emptied directive set", () => {
    expect(
      proposedChangeText({
        ...base,
        op: "edit",
        sets_directives: true,
        directives: [],
      }),
    ).toBe("directives: none (clears them)");
  });
});
