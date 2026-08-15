import { describe, expect, it } from "vitest";
import { hitText, judgeable, SEARCH_SOURCES, sourcesParam } from "./memorySearch";

describe("judgeable", () => {
  it("only a document-chunk hit can carry a verdict", () => {
    // Verification state lives on the entity tier. A k/v row has nowhere to hold a
    // verdict and no span to check one against, so offering the controls there would
    // produce a button that fails.
    expect(judgeable({ key: "doc.chunk:abc", score: 1, rank_score: 1, kind: "document", chunk_id: "abc" })).toBe(true);
    expect(judgeable({ key: "memory/fact/x", score: 1, rank_score: 1, kind: "fact" })).toBe(false);
    expect(judgeable({ key: "notes/y", score: 1, rank_score: 1, kind: "note" })).toBe(false);
  });

  it("refuses a document hit with no chunk id rather than guessing one from the key", () => {
    // The server supplies chunk_id; parsing it out of the key would be a second decoder
    // for a format the server already decoded.
    expect(judgeable({ key: "doc.chunk:abc", score: 1, rank_score: 1, kind: "document" })).toBe(false);
  });
});

describe("hitText", () => {
  it("renders a string value as itself", () => {
    expect(hitText("The user lives in Cluj-Napoca.")).toBe("The user lives in Cluj-Napoca.");
  });

  it("renders a non-string value as JSON rather than [object Object]", () => {
    expect(hitText({ a: 1 })).toBe('{"a":1}');
    expect(hitText(["x"])).toBe('["x"]');
  });

  it("renders nothing for an absent value", () => {
    expect(hitText(null)).toBe("");
    expect(hitText(undefined)).toBe("");
  });
});

describe("sourcesParam", () => {
  const all = SEARCH_SOURCES.map((s) => s.id);

  it("sends nothing when everything is selected", () => {
    // The endpoint reads an empty list as "every source". Enumerating today's three
    // would mean the same thing now and silently exclude a fourth added later.
    expect(sourcesParam(new Set(all), all)).toBeUndefined();
    expect(sourcesParam(new Set(), all)).toBeUndefined();
  });

  it("sends the selection when it is a subset", () => {
    expect(sourcesParam(new Set(["facts"]), all)).toEqual(["facts"]);
    expect(sourcesParam(new Set(["facts", "documents"]), all)).toEqual(["facts", "documents"]);
  });

  it("keeps the server's order rather than the click order", () => {
    // Selection is a set; the request should be stable so two identical selections
    // produce identical requests (and identical cache keys downstream).
    expect(sourcesParam(new Set(["documents", "facts"]), all)).toEqual(["facts", "documents"]);
  });
});
