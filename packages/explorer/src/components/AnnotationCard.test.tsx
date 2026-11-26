// @vitest-environment happy-dom
import { act, createElement, type ReactElement } from "react";
import { type Root, createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ExplorerDataLayer } from "../lib/dataLayer";
import ChunkTarget, { type ChunkTargetProps } from "./ChunkTarget";
import { CARD_DELAY_MS, CardCacheProvider, CardScopeContext, rowSummary, chunkCard } from "./AnnotationCard";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let mounted: { root: Root; el: HTMLElement } | null = null;

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  if (mounted) {
    act(() => mounted!.root.unmount());
    mounted.el.remove();
    mounted = null;
  }
  vi.useRealTimers();
  vi.restoreAllMocks();
});

function fakeData(brief?: string) {
  const documentsSummary = vi.fn().mockResolvedValue({
    documents: brief
      ? [{ document_id: "doc-b", title: "RFC CV", type: "rfc", status: "done", color_enabled: false, brief }]
      : [],
  });
  const documentGetAnnotation = vi.fn().mockResolvedValue({ annotations: [] });
  return { data: { documentsSummary, documentGetAnnotation } as unknown as ExplorerDataLayer, documentsSummary };
}

function mount(children: ReactElement, data: ExplorerDataLayer): HTMLElement {
  const el = document.createElement("div");
  el.className = "loomcycle-explorer";
  document.body.appendChild(el);
  const root = createRoot(el);
  act(() =>
    root.render(
      createElement(
        CardCacheProvider,
        null,
        createElement(CardScopeContext.Provider, { value: { data, scope: "tenant" } }, children),
      ),
    ),
  );
  mounted = { root, el };
  return el;
}

function target(over: Partial<ChunkTargetProps> = {}): ReactElement {
  return createElement(ChunkTarget, {
    id: "chunk-9",
    title: "One chunk store",
    targetDocumentId: "doc-b",
    documentId: "doc-a",
    onSelectChunk: () => {},
    onOpenDocument: () => {},
    ...over,
  });
}

const card = (el: HTMLElement) => el.querySelector<HTMLElement>(".ex-card");
async function settle() {
  await act(async () => {
    await Promise.resolve();
  });
}

describe("AnnotationCard", () => {
  it("TestAnnotationCard_OpensAfterTheHoverDelay", async () => {
    const { data } = fakeData("Chunks live in one store.");
    const el = mount(target(), data);
    const link = el.querySelector<HTMLButtonElement>(".doc-ref-target")!;
    act(() => {
      link.dispatchEvent(new MouseEvent("mouseover", { bubbles: true, relatedTarget: document.body }));
    });
    act(() => vi.advanceTimersByTime(CARD_DELAY_MS - 50));
    expect(card(el)).toBeNull();
    act(() => vi.advanceTimersByTime(60));
    await settle();
    expect(card(el)).not.toBeNull();
    expect(card(el)!.getAttribute("role")).toBe("tooltip");
    expect(card(el)!.textContent).toContain("RFC CV");
    expect(card(el)!.textContent).toContain("rfc · done");
    expect(card(el)!.textContent).toContain("Chunks live in one store.");
    expect(link.getAttribute("aria-describedby")).toBe(card(el)!.id);
    // Leaving closes it.
    act(() => {
      link.dispatchEvent(new MouseEvent("mouseout", { bubbles: true, relatedTarget: document.body }));
    });
    expect(card(el)).toBeNull();
  });

  it("TestAnnotationCard_OpensOnKeyboardFocusAndClosesOnEscape", async () => {
    const { data } = fakeData("Chunks live in one store.");
    const el = mount(target(), data);
    const link = el.querySelector<HTMLButtonElement>(".doc-ref-target")!;
    act(() => link.focus());
    await settle();
    expect(card(el)!.textContent).toContain("Chunks live in one store.");
    act(() => {
      link.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    });
    expect(card(el)).toBeNull();
  });

  it("TestAnnotationCard_NeverOpensOnAPlainClick", async () => {
    const onOpenDocument = vi.fn();
    const { data, documentsSummary } = fakeData("brief");
    const el = mount(target({ onOpenDocument }), data);
    const link = el.querySelector<HTMLButtonElement>(".doc-ref-target")!;
    act(() => {
      link.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
      link.focus();
      link.click();
    });
    act(() => vi.advanceTimersByTime(CARD_DELAY_MS * 2));
    await settle();
    expect(card(el)).toBeNull();
    expect(documentsSummary).not.toHaveBeenCalled();
    // The click still follows the link.
    expect(onOpenDocument).toHaveBeenCalledWith("doc-b", "chunk-9", "One chunk store");
  });

  it("TestAnnotationCard_FallsBackToTheTitleWithNoAnnotation", async () => {
    const { data } = fakeData(undefined);
    const el = mount(target(), data);
    act(() => el.querySelector<HTMLButtonElement>(".doc-ref-target")!.focus());
    await settle();
    expect(card(el)!.querySelector(".ex-card-title")!.textContent).toBe("One chunk store");
    expect(card(el)!.querySelector(".ex-card-brief")).toBeNull();
    expect(card(el)!.querySelector(".ex-card-loading")).toBeNull();
  });

  it("TestAnnotationCard_CachesTheBriefForTheSession", async () => {
    const { data, documentsSummary } = fakeData("Chunks live in one store.");
    // Two links to the same document: one read between them.
    const el = mount(createElement("div", null, target(), target({ id: "chunk-10" })), data);
    const [a, b] = Array.from(el.querySelectorAll<HTMLButtonElement>(".doc-ref-target"));
    act(() => a.focus());
    await settle();
    act(() => a.blur());
    act(() => b.focus());
    await settle();
    act(() => b.blur());
    act(() => a.focus());
    await settle();
    expect(card(el)!.textContent).toContain("Chunks live in one store.");
    expect(documentsSummary).toHaveBeenCalledTimes(1);
  });

  it("TestAnnotationCard_UsesTheBriefOnAChunkRowWhenTheListCarriesIt", () => {
    // RFC HA: once the chunk list carries `summary`, the card reads it and
    // makes no call.
    const row = { id: "c", document_id: "d", position: 0, revision: 1, title: "Intro" };
    expect(rowSummary(row)).toBeUndefined();
    const withSummary = { ...row, summary: { brief: "Why it exists.", abstract: "More." } };
    const spec = chunkCard(withSummary, { data: {} as ExplorerDataLayer, scope: "tenant" });
    expect(spec).toMatchObject({ title: "Intro", brief: "Why it exists.", abstract: "More." });
    expect(spec.load).toBeUndefined();
    expect(chunkCard(row, { data: {} as ExplorerDataLayer, scope: "tenant" }).load).toBeTypeOf("function");
    // Once the list carries briefs, a row without one has none of its own:
    // nothing is read for it.
    expect(chunkCard(row, { data: {} as ExplorerDataLayer, scope: "tenant" }, true).load).toBeUndefined();
    // A stale brief says so.
    expect(rowSummary({ ...row, summary: { brief: "Old.", stale: true } })).toMatchObject({ brief: "Old.", stale: true });
  });
});

describe("ChunkTarget", () => {
  it("a cross-document target is a button that opens that document at the chunk", () => {
    const onOpenDocument = vi.fn();
    const onSelectChunk = vi.fn();
    const el = mount(target({ onOpenDocument, onSelectChunk }), fakeData().data);
    const b = el.querySelector<HTMLButtonElement>("button.doc-ref-target.external")!;
    expect(b.textContent).toBe("One chunk store ↗");
    act(() => b.click());
    expect(onOpenDocument).toHaveBeenCalledWith("doc-b", "chunk-9", "One chunk store");
    expect(onSelectChunk).not.toHaveBeenCalled();
    // The card replaces the one-size-fits-all tooltip.
    expect(b.getAttribute("title")).toBeNull();
  });

  it("a same-document target selects the chunk in place", () => {
    const onOpenDocument = vi.fn();
    const onSelectChunk = vi.fn();
    const el = mount(target({ targetDocumentId: "doc-a", onOpenDocument, onSelectChunk }), fakeData().data);
    const b = el.querySelector<HTMLButtonElement>("button.doc-ref-target")!;
    expect(b.textContent).not.toContain("↗");
    act(() => b.click());
    expect(onSelectChunk).toHaveBeenCalledWith("chunk-9");
    expect(onOpenDocument).not.toHaveBeenCalled();
  });

  it("a target with no document id of its own is treated as this document's", () => {
    const onSelectChunk = vi.fn();
    const el = mount(target({ targetDocumentId: undefined, onSelectChunk }), fakeData().data);
    act(() => el.querySelector<HTMLButtonElement>("button.doc-ref-target")!.click());
    expect(onSelectChunk).toHaveBeenCalledWith("chunk-9");
  });

  it("stays a label — not a dead button — for a host that cannot open documents", () => {
    const el = mount(target({ onOpenDocument: undefined }), fakeData().data);
    expect(el.querySelector("button")).toBeNull();
    expect(el.querySelector("span.doc-ref-target.external")!.textContent).toBe("One chunk store ↗");
  });
});
