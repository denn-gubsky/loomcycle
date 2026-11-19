import { describe, expect, it } from "vitest";
import { changeConfirmText, proposedChangeText } from "./ontologyProposedChange";

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

describe("changeConfirmText", () => {
  it("names the change and what rests on it, summed over the planned changes", () => {
    const text = changeConfirmText(
      { ...base, op: "edit", name: "oversees" },
      {
        fingerprint: "f",
        complete: true,
        changes: [
          {
            op: "rename",
            entry: { chunk_id: "c1", name: "supervises", section: "relations", kind: "relation" },
            risk: "stales",
            counts: {
              statements_of_kind: { n: 2, effect: "stale", tenant_scope: 2 },
              review_rules: { n: 1, effect: "kept", tenant_scope: 1 },
            },
          },
        ],
      },
    );
    expect(text).toContain('Accept this change to the relation "supervises"?');
    expect(text).toContain("rename supervises to oversees");
    expect(text).toContain("What rests on it: statements of kind: 2.");
    expect(text).not.toContain("review rules");
  });
});
