import { describe, expect, it } from "vitest";
import { addPage, emptyModel } from "./model";
import { syntheticSnapshot } from "./synthetic";
import { chunkRgb, hexToRgb } from "./colors";

// A chunk with no status colour takes its document's colour, lighter; a status
// colour still wins; `chunk.inherit: false` keeps such a chunk neutral.
function law(scheme: Record<string, string>) {
  const m = emptyModel();
  addPage(m, syntheticSnapshot({ docs: 1, chunksPerDoc: 4, idPrefix: "l" }));
  m.doc.type[0] = "Law";
  m.doc.status[0] = "";
  m.doc.colorEnabled[0] = true;
  m.doc.scheme[0] = scheme;
  for (let c = 0; c < m.chunkCount; c++) m.chunk.status[c] = "";
  return m;
}

const BLUE = hexToRgb("#2f6fd6")!;
const near = (a: number[], b: number[]) => a.every((x, i) => Math.abs(x - b[i]) < 1e-9);

describe("star colours", () => {
  it("TestChunkRgb_AChunkWithoutStatusTakesItsDocumentsColourLighter", () => {
    const m = law({ "doc.Law": "#2f6fd6" });
    const root = m.doc.root[0];
    expect(near(chunkRgb(m, root), BLUE)).toBe(true);
    const leaf = root === 0 ? 1 : 0;
    const got = chunkRgb(m, leaf);
    // Same hue, lifted toward white: every channel at or above the document's.
    expect(near(got, BLUE)).toBe(false);
    got.forEach((x, i) => expect(x).toBeGreaterThanOrEqual(BLUE[i]));
    expect(near(got, BLUE.map((x) => x + (1 - x) * 0.35))).toBe(true);
  });

  it("TestChunkRgb_AStatusColourStillWins", () => {
    const m = law({ "doc.Law": "#2f6fd6", "chunk.fail": "#d64545" });
    const leaf = m.doc.root[0] === 0 ? 1 : 0;
    m.chunk.status[leaf] = "fail";
    expect(near(chunkRgb(m, leaf), hexToRgb("#d64545")!)).toBe(true);
  });

  it("TestChunkRgb_InheritOffKeepsTheNeutral", () => {
    const off = law({ "doc.Law": "#2f6fd6", "chunk.inherit": "false" });
    const on = law({ "doc.Law": "#2f6fd6" });
    const leaf = off.doc.root[0] === 0 ? 1 : 0;
    expect(near(chunkRgb(off, leaf), chunkRgb(on, leaf))).toBe(false);
    // Neutral is a pale grey-blue: no channel tinted toward the document's blue.
    const [r, g, b] = chunkRgb(off, leaf);
    expect(b - r).toBeLessThan(0.2);
    expect(g).toBeGreaterThan(0.6);
  });
});
