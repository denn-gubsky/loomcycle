import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { TeamRunFields } from "./TeamRunModal";
import { fieldsFromSchema, initialValues, type FormValues } from "../lib/teamStart";

// Static renders: effects (the picker list reads) do not run, so a picker shows
// its pre-load state — enough to see which controls render and which wait.

const fields = fieldsFromSchema({
  type: "object",
  required: ["document_id", "chunk_id"],
  properties: {
    document_id: { type: "string", title: "Document", "x-loomcycle-picker": { kind: "document", under_path: "/parts" } },
    chunk_id: { type: "string", title: "Part", "x-loomcycle-picker": { kind: "chunk", document: "document_id", depth: 1 } },
    notes: { type: "string", title: "Notes", description: "Anything else" },
    urgent: { type: "boolean" },
    count: { type: "integer" },
    tone: { enum: ["formal", "casual"] },
    meta: { type: "object" },
  },
});

function render(values: FormValues) {
  return renderToStaticMarkup(createElement(TeamRunFields, { fields, values, disabled: false, onChange: () => {} }));
}

describe("TeamRunFields", () => {
  it("renders one control per field type, marking only the required fields", () => {
    const html = render(initialValues(fields));
    expect(html.match(/class="team-run-required"/g)).toHaveLength(2);
    expect(html).toContain('type="checkbox"');
    expect(html).toContain('type="number"');
    expect(html).toContain('<option value="0">formal</option>');
    expect(html).toContain('placeholder="JSON value"');
    expect(html).toContain("Anything else");
    expect(html).toContain("From user documents under /parts.");
  });

  it("holds the chunk picker disabled, asking for the document, until one is chosen", () => {
    const empty = render(initialValues(fields));
    expect(empty).toContain('<select disabled=""><option value="" selected="">choose Document first</option>');

    // With a document chosen it moves on to reading that document's chunks.
    const chosen = render({ ...initialValues(fields), document_id: "doc1" });
    expect(chosen).not.toContain("choose Document first");
    expect(chosen).toContain('Part<span class="team-run-required" title="required" aria-label="required"> *</span></span><select disabled=""><option value="" selected="">loading…</option>');
  });
});
