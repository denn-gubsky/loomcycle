import { describe, expect, it } from "vitest";
import { chunkColor, chunkInheritsDocColor, chunkRowStyle, docColor, effectiveScheme, tintStyle, washStyle } from "./colorScheme";

// A chunk without a status colour of its own takes a lighter wash of its
// document's colour (a corpus of documents coloured by kind, whose thousands of
// sections carry no status).

const LAW = { "doc.Law": "#2f6fd6" };
const PASSPORT = { "doc.Passport": "#3aa06a", "chunk.pass": "#2fa84f", "chunk.fail": "#d64545" };

describe("chunk colour inherits the document's", () => {
  it("TestChunkRowStyle_AChunkWithoutStatusWearsTheDocumentsWash", () => {
    const docHex = docColor("Law", undefined, effectiveScheme(LAW));
    expect(docHex).toBe("#2f6fd6");
    const style = chunkRowStyle({ status: undefined, isRoot: false, documentColor: docHex, scheme: effectiveScheme(LAW) });
    expect(style).toEqual(washStyle("#2f6fd6"));
    // Lighter than a status tint: half the alpha, and no left accent.
    expect(style?.background).toBe("rgba(47, 111, 214, 0.08)");
    expect(style?.borderLeft).toBeUndefined();
  });

  it("TestChunkRowStyle_AStatusColourStillWinsInAPassport", () => {
    const scheme = effectiveScheme(PASSPORT);
    const docHex = docColor("Passport", undefined, scheme);
    expect(chunkRowStyle({ status: "fail", isRoot: false, documentColor: docHex, scheme })).toEqual(tintStyle("#d64545"));
    expect(chunkRowStyle({ status: undefined, isRoot: false, documentColor: docHex, scheme })).toEqual(washStyle("#3aa06a"));
    // A status the scheme has no colour for falls back to the wash too.
    expect(chunkRowStyle({ status: "review", isRoot: false, documentColor: docHex, scheme })).toEqual(washStyle("#3aa06a"));
  });

  it("TestChunkRowStyle_TheDocumentsOwnRowWearsItsFullColour", () => {
    const scheme = effectiveScheme(PASSPORT);
    // The root chunk is the document: its row is tinted like its Path-tree row.
    expect(chunkRowStyle({ status: "pass", isRoot: true, documentColor: "#3aa06a", scheme })).toEqual(tintStyle("#3aa06a"));
    // A document with no colour of its own: the root falls back to its status.
    expect(chunkRowStyle({ status: "pass", isRoot: true, documentColor: undefined, scheme })).toEqual(tintStyle("#2fa84f"));
  });

  it("TestChunkRowStyle_ColouringOffTintsNothing", () => {
    for (const isRoot of [true, false]) {
      expect(chunkRowStyle({ status: "fail", isRoot, documentColor: "#2f6fd6", scheme: null })).toBeUndefined();
    }
  });

  it("TestChunkInheritsDocColor_TheSchemeCanTurnItOff", () => {
    expect(chunkInheritsDocColor(LAW)).toBe(true);
    for (const off of ["false", "none", "off", "0", " FALSE "]) {
      const scheme = { ...LAW, "chunk.inherit": off };
      expect(chunkInheritsDocColor(scheme)).toBe(false);
      expect(chunkRowStyle({ status: undefined, isRoot: false, documentColor: "#2f6fd6", scheme })).toBeUndefined();
    }
    expect(chunkInheritsDocColor({ ...LAW, "chunk.inherit": "true" })).toBe(true);
  });
});

// An ontology retypes documents to class names (`rfc` → `Rfc`, `document` →
// `Guide`); a document's colour must survive that.
describe("a retyped document keeps its colour", () => {
  const STORED = { "doc.rfc.done": "#2f8fbf", "doc.research": "#8a6ad6", "chunk.done": "#3aa06a" };

  it("TestDocColor_AClassTypeMatchesTheLowercaseKey", () => {
    expect(docColor("Rfc", "done", STORED)).toBe("#2f8fbf");
    expect(docColor("RESEARCH", undefined, STORED)).toBe("#8a6ad6");
    expect(chunkColor("Done", STORED)).toBe("#3aa06a");
  });

  it("TestDocColor_AnExactKeyWinsOverACaseInsensitiveOne", () => {
    expect(docColor("Rfc", "done", { ...STORED, "doc.Rfc.done": "#111111" })).toBe("#111111");
  });

  it("TestDocColor_ATypeTheDefaultSchemeDoesNotNameIsColouredByStatus", () => {
    const scheme = effectiveScheme(undefined);
    expect(docColor("Guide", "done", scheme)).toBe("#3aa06a");
    expect(docColor("Benchmark", "draft", scheme)).toBe("#c99a3a");
    // A named type still takes its own key before the any-type one.
    expect(docColor("research", "done", scheme)).toBe("#8a6ad6");
    // No status and no doc.* key: neutral.
    expect(docColor("Guide", undefined, scheme)).toBeUndefined();
  });

  it("TestDocColor_AnAnyTypeKeyColoursEveryType", () => {
    expect(docColor("Anything", undefined, { "doc.*": "#123456" })).toBe("#123456");
  });
});
