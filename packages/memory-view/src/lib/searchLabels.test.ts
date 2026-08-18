import { describe, expect, it } from "vitest";

import { searchHitLabel, type LabelledSearchEntry } from "./searchLabels";

const hit = (over: Partial<LabelledSearchEntry>): LabelledSearchEntry =>
  ({
    key: "doc.chunk:8f1eb1e1",
    value: null,
    score: 0.7,
    rank_score: 0.7,
    embedded_with: { provider: "ollama-local", model: "bge-m3:latest" },
    kind: "document",
    chunk_id: "8f1eb1e1",
    ...over,
  }) as LabelledSearchEntry;

describe("searchHitLabel", () => {
  it("names a document hit by its document and heading", () => {
    expect(
      searchHitLabel(
        hit({ document: "Verified writes", title: "Reading your coverage" }),
      ),
    ).toBe("Verified writes › Reading your coverage");
  });

  it("does not repeat the title for a document's root chunk", () => {
    // The root chunk carries the document's own title, so both fields are equal —
    // "Verified writes › Verified writes" is noise, not information.
    expect(
      searchHitLabel(
        hit({ document: "Verified writes", title: "Verified writes" }),
      ),
    ).toBe("Verified writes");
  });

  it("falls back through heading, document, chunk id, key", () => {
    expect(searchHitLabel(hit({ title: "Reading your coverage" }))).toBe(
      "Reading your coverage",
    );
    expect(searchHitLabel(hit({ document: "Verified writes" }))).toBe(
      "Verified writes",
    );
    expect(searchHitLabel(hit({}))).toBe("8f1eb1e1");
    expect(searchHitLabel(hit({ chunk_id: undefined }))).toBe(
      "doc.chunk:8f1eb1e1",
    );
  });

  it("treats a blank label as absent", () => {
    // A chunk created with no heading stores "", which must not render as an empty
    // identity — the row would look nameless rather than unlabelled.
    expect(searchHitLabel(hit({ document: "   ", title: "\n" }))).toBe(
      "8f1eb1e1",
    );
  });

  it("leaves a fact or note keyed by its key", () => {
    expect(
      searchHitLabel(hit({ kind: "fact", key: "voice", chunk_id: undefined })),
    ).toBe("voice");
    expect(
      searchHitLabel(
        hit({ kind: "note", key: "prefs.tone", chunk_id: undefined }),
      ),
    ).toBe("prefs.tone");
  });
});
