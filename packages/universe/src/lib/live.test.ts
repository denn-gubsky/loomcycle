import { describe, expect, it } from "vitest";
import { addPage, emptyModel, modelToPage, spliceDocuments, type UniverseModel } from "./model";
import { syntheticSnapshot } from "./synthetic";
import { parseSSE, toFeedFrame } from "./feed";
import type { GraphSnapshot } from "../types";

function load(pages: GraphSnapshot[]): UniverseModel {
  const m = emptyModel();
  for (const p of pages) addPage(m, p);
  return m;
}

// The parts of a model a reader sees, in a comparable shape.
function shape(m: UniverseModel) {
  return {
    docCount: m.docCount,
    chunkCount: m.chunkCount,
    doc: m.doc,
    chunk: { ...m.chunk, proj: m.chunk.proj.map((x) => (Number.isNaN(x) ? "NaN" : x)) },
    edges: [...m.edges].sort((a, b) => a.id.localeCompare(b.id)),
    projection: m.projection,
    dirs: [...m.dirs].sort(),
  };
}

const sky = () =>
  load([
    syntheticSnapshot({ docs: 6, chunksPerDoc: 7, edgesPerDoc: 3, idPrefix: "a", projection: { missingEvery: 5 } }),
    syntheticSnapshot({ docs: 4, chunksPerDoc: 5, edgesPerDoc: 2, idPrefix: "b", seed: 9, projection: { missingEvery: 5 } }),
  ]);

describe("live splice", () => {
  it("TestModelToPage_RoundTripsTheWholeSky", () => {
    const m = sky();
    const again = load([modelToPage(m, () => true)]);
    expect(shape(again)).toEqual(shape(m));
  });

  it("TestSpliceDocuments_ReplacesInPlaceAndKeepsTheRest", () => {
    const m = sky();
    const before = shape(m);
    const v = m.version;
    const target = m.doc.id[2];
    // A fresh page of that document: retitled, one chunk fewer.
    const fresh = modelToPage(m, (d) => d === 2);
    fresh.documents!.titles[0] = "Retitled";
    fresh.chunks!.ids.pop();
    fresh.chunks!.document_idx.pop();
    fresh.chunks!.parent_idx.pop();
    fresh.chunks!.type_code.pop();
    fresh.chunks!.status_code.pop();
    fresh.chunks!.titles.pop();
    fresh.projection!.xyz.splice(-3, 3);
    fresh.projection!.missing = fresh.projection!.missing.filter((i) => i < fresh.chunks!.ids.length);

    const same = spliceDocuments(m, fresh, new Set([target]));
    expect(same).toBe(m);
    expect(m.version).toBeGreaterThan(v);
    expect(m.docCount).toBe(before.docCount);
    expect(m.chunkCount).toBe(before.chunkCount - 1);
    const di = m.docIndex.get(target)!;
    expect(m.doc.title[di]).toBe("Retitled");
    // Every other document is untouched.
    for (let d = 0; d < before.docCount; d++) {
      const id = before.doc.id[d];
      if (id === target) continue;
      expect(m.doc.title[m.docIndex.get(id)!]).toBe(before.doc.title[d]);
    }
    // Edges between two untouched documents survive as they were.
    const untouched = (e: { fromDocId: string; toDocId: string }) => e.fromDocId !== target && e.toDocId !== target;
    expect(m.edges.filter(untouched).map((e) => e.id).sort()).toEqual(before.edges.filter(untouched).map((e) => e.id).sort());
  });

  it("TestSpliceDocuments_ADeletedDocumentGoesAndANewOneArrives", () => {
    const m = sky();
    const gone = m.doc.id[0];
    const newcomer = syntheticSnapshot({ docs: 1, chunksPerDoc: 3, idPrefix: "n" });
    spliceDocuments(m, newcomer, new Set([gone, newcomer.documents!.ids[0]]));
    expect(m.docIndex.has(gone)).toBe(false);
    expect(m.docIndex.has(newcomer.documents!.ids[0])).toBe(true);
    expect(m.edges.some((e) => e.fromDocId === gone || e.toDocId === gone)).toBe(false);
    // Contiguity still holds after the splice.
    for (let d = 0; d < m.docCount; d++)
      for (let c = m.doc.chunkStart[d]; c < m.doc.chunkStart[d] + m.doc.chunkCount[d]; c++) expect(m.chunk.doc[c]).toBe(d);
  });
});

describe("reader feed", () => {
  function stream(text: string): ReadableStream<Uint8Array> {
    const bytes = new TextEncoder().encode(text);
    // Split mid-line to prove the parser carries partial lines across reads.
    return new ReadableStream({
      start(c) {
        c.enqueue(bytes.slice(0, 17));
        c.enqueue(bytes.slice(17));
        c.close();
      },
    });
  }

  it("TestFeed_ParsesFramesAcrossChunkBoundaries", async () => {
    const text =
      'event: feed\ndata: {"enabled":true}\n\n' +
      ": keepalive\n\n" +
      'event: change\ndata: {"type":"document.chunk.updated","chunk_id":"c1","document_id":"d1","at":"x"}\n\n' +
      'event: error\ndata: {"code":"credential_revoked","error":"no"}\n\n';
    const frames = [];
    for await (const ev of parseSSE(stream(text))) frames.push(toFeedFrame(ev));
    expect(frames).toEqual([
      { kind: "feed", enabled: true },
      { kind: "change", type: "document.chunk.updated", documentId: "d1", chunkId: "c1" },
      { kind: "error", code: "credential_revoked", message: "no" },
    ]);
  });
});
