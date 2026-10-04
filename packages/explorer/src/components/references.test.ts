import { createElement, type ReactElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, it, expect, vi } from "vitest";
import type { DocEdge } from "../types";
import ChunkTarget, { type ChunkTargetProps } from "./ChunkTarget";
import { RefRow, graphEndpoint, nodeId } from "./CrossReferences";
import type { Trail } from "../lib/docTrail";
import DocTrail from "./DocTrail";
import DocUnavailable from "./DocUnavailable";
import { flowchartNode } from "./Mermaid";

// The package tests without a DOM. ChunkTarget and RefRow are hook-free, so
// calling them returns the element they render — its type and onClick can be
// asserted (and invoked) directly.
type El = ReactElement<{
  onClick?: () => void;
  className?: string;
  title?: string;
  children?: unknown;
}>;

function target(over: Partial<ChunkTargetProps> = {}) {
  const onSelectChunk = vi.fn();
  const onOpenDocument = vi.fn();
  const el = ChunkTarget({
    id: "chunk-9",
    title: "RFC CV — One chunk store",
    targetDocumentId: "doc-b",
    documentId: "doc-a",
    onSelectChunk,
    onOpenDocument,
    ...over,
  }) as El;
  return { el, onSelectChunk, onOpenDocument };
}

describe("ChunkTarget", () => {
  it("a cross-document target is a button that opens that document at the chunk", () => {
    const { el, onSelectChunk, onOpenDocument } = target();
    expect(el.type).toBe("button");
    el.props.onClick?.();
    expect(onOpenDocument).toHaveBeenCalledWith("doc-b", "chunk-9", "RFC CV — One chunk store");
    expect(onSelectChunk).not.toHaveBeenCalled();
  });

  it("keeps the ↗ mark and the another-document hint on a cross-document target", () => {
    const { el } = target();
    expect(renderToStaticMarkup(el)).toContain("RFC CV — One chunk store ↗");
    expect(el.props.title).toMatch(/another document/i);
    expect(el.props.className).toBe("doc-ref-target external");
  });

  it("a same-document target selects the chunk in place", () => {
    const { el, onSelectChunk, onOpenDocument } = target({ targetDocumentId: "doc-a" });
    expect(el.type).toBe("button");
    el.props.onClick?.();
    expect(onSelectChunk).toHaveBeenCalledWith("chunk-9");
    expect(onOpenDocument).not.toHaveBeenCalled();
    expect(renderToStaticMarkup(el)).not.toContain("↗");
  });

  it("a target with no document id of its own is treated as this document's", () => {
    const { el, onSelectChunk } = target({ targetDocumentId: undefined });
    el.props.onClick?.();
    expect(onSelectChunk).toHaveBeenCalledWith("chunk-9");
  });

  it("stays a label — not a dead button — for a host that cannot open documents", () => {
    const { el } = target({ onOpenDocument: undefined });
    expect(el.type).toBe("span");
    expect(el.props.onClick).toBeUndefined();
  });
});

describe("RefRow", () => {
  const edge: DocEdge = {
    from_id: "here",
    to_id: "there",
    kind: "related",
    from_title: "Findings",
    from_document_id: "doc-a",
    to_title: "RFC CW",
    to_document_id: "doc-b",
  };
  const row = (selectedId: string) =>
    renderToStaticMarkup(
      createElement(RefRow, {
        edge,
        selectedId,
        documentId: selectedId === "here" ? "doc-a" : "doc-b",
        onSelectChunk: () => {},
        onOpenDocument: () => {},
      }),
    );

  it("an outgoing cross-document edge renders its far end as a button", () => {
    const html = row("here");
    expect(html).toContain("<button");
    expect(html).toContain("RFC CW ↗");
    expect(html).not.toContain("<span class=\"doc-ref-target");
  });

  it("an incoming edge points at the SOURCE chunk's document", () => {
    const html = row("there");
    expect(html).toContain("Findings ↗");
    expect(html).toContain("←");
  });
});

describe("relationship graph nodes", () => {
  const edges: DocEdge[] = [
    { from_id: "c-1", to_id: "c-2", kind: "related", from_document_id: "doc-a", to_document_id: "doc-b", to_title: "RFC CV" },
  ];

  it("maps a drawn node back to its chunk and that chunk's document", () => {
    expect(graphEndpoint(edges, nodeId("c-2"))).toEqual({ id: "c-2", documentId: "doc-b", title: "RFC CV" });
    expect(graphEndpoint(edges, nodeId("c-1"))).toMatchObject({ id: "c-1", documentId: "doc-a" });
    expect(graphEndpoint(edges, "nunknown")).toBeUndefined();
  });

  it("reads the declared node id out of the id mermaid gives the drawn node", () => {
    const inNode = (id: string) => ({ closest: () => ({ id }) }) as unknown as Element;
    expect(flowchartNode(inNode("flowchart-nc2-7"))).toBe("nc2");
    expect(flowchartNode(inNode("lc-mmd-3-flowchart-nc2-12"))).toBe("nc2");
    expect(flowchartNode({ closest: () => null } as unknown as Element)).toBeUndefined();
    expect(flowchartNode(null)).toBeUndefined();
  });
});

describe("DocTrail", () => {
  const A = { documentId: "doc-a", chunkId: "a-3", title: "Memory accuracy" };
  const B = { documentId: "doc-b", chunkId: "b-1", title: "RFC CV — One chunk store" };
  const render = (trail: Trail) =>
    renderToStaticMarkup(createElement(DocTrail, { trail, onJump: () => {} }));

  it("renders nothing until a reference has been followed", () => {
    expect(render([])).toBe("");
    expect(render([A])).toBe("");
  });

  it("shows every document on the way, the earlier ones as buttons, and a Back button", () => {
    const html = render([A, B]);
    expect(html).toContain('title="Back to Memory accuracy"');
    expect(html).toMatch(/<button[^>]*class="doc-trail-crumb"[^>]*>Memory accuracy<\/button>/);
    expect(html).toMatch(/<span[^>]*aria-current="page"[^>]*>RFC CV — One chunk store<\/span>/);
  });

  it("folds the middle of a long trail into one expandable gap", () => {
    const trail = Array.from({ length: 7 }, (_, i) => ({ documentId: `d${i}`, title: `Doc ${i}` }));
    const html = render(trail);
    expect(html).toContain("doc-trail-gap");
    expect(html).toContain("Show 3 hidden documents");
    expect(html).not.toContain(">Doc 2<");
    expect(html).toContain(">Doc 0<");
    expect(html).toContain(">Doc 6<");
  });
});

describe("DocUnavailable", () => {
  it("says the document cannot be opened, names it, and carries the runtime's reason", () => {
    const html = renderToStaticMarkup(
      createElement(DocUnavailable, { documentId: "doc-gone", scope: "user", detail: "no such document" }),
    );
    expect(html).toContain('role="alert"');
    expect(html).toContain("can’t be opened");
    expect(html).toContain("doc-gone");
    expect(html).toContain("no such document");
  });
});
