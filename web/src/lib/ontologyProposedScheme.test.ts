import { describe, expect, it } from "vitest";
import { proposedOutlineText } from "./ontologyProposedScheme";

describe("proposedOutlineText", () => {
  it("shows each concept's directives under it, indented by level", () => {
    expect(
      proposedOutlineText([
        { name: "Hydraulics", level: 1, notation: "29", alt: ["Hydraulic power"] },
        { name: "Pumps", level: 2, exact_match: "http://example.org/pumps" },
        { name: "Avionics", level: 1 },
      ]),
    ).toBe(
      [
        "- Hydraulics",
        "    @notation 29",
        "    @alt Hydraulic power",
        "  - Pumps",
        "      @exact_match http://example.org/pumps",
        "- Avionics",
      ].join("\n"),
    );
  });
  it("keeps markup as text", () => {
    expect(proposedOutlineText([{ name: "<b>x</b>", level: 1, alt: ["<img src=x>"] }])).toBe(
      "- <b>x</b>\n    @alt <img src=x>",
    );
  });
});
