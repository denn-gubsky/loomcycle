// @vitest-environment happy-dom
import { act, createElement } from "react";
import { type Root, createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ExplorerDataLayer } from "./lib/dataLayer";
import type { ChunkRow, PathEntry } from "./types";
import PathExplorer from "./PathExplorer";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

// A small tenant tree:
//   /kb                    folder (annotated: its _annotation's brief)
//   /kb/_annotation        the folder's annotation document
//   /kb/docs               folder
//   /kb/docs/architecture  document "Architecture" (DesignNote, done)
//   /kb/docs/plan          document "Plan" (Plan, draft)
//   /notes                 folder
//   /notes/archive         document "Archive"
const dir = (full_path: string): PathEntry => ({
  name: full_path.slice(full_path.lastIndexOf("/") + 1),
  kind: "directory",
  full_path,
});
const doc = (full_path: string, id: string): PathEntry => ({
  ...dir(full_path),
  kind: "document",
  resource_ref: { document_id: id },
});
const listings: Record<string, PathEntry[]> = {
  "/": [dir("/kb"), dir("/notes")],
  "/kb": [doc("/kb/_annotation", "ann"), dir("/kb/docs")],
  "/kb/docs": [doc("/kb/docs/architecture", "arch"), doc("/kb/docs/plan", "plan")],
  "/notes": [doc("/notes/archive", "archive")],
};
const summaries = [
  { document_id: "ann", title: "kb", color_enabled: false, brief: "The knowledge base." },
  { document_id: "arch", title: "Architecture", type: "DesignNote", status: "done", color_enabled: false },
  { document_id: "plan", title: "Plan", type: "Plan", status: "draft", color_enabled: false },
  { document_id: "archive", title: "Archive", type: "Plan", status: "done", color_enabled: false },
];

function fakeData(): ExplorerDataLayer {
  const chunks: ChunkRow[] = [
    { id: "r".repeat(32), document_id: "arch", position: 0, title: "Architecture", revision: 1 },
  ];
  return {
    pathLs: vi.fn((path: string) => Promise.resolve({ path, entries: listings[path] ?? [] })),
    documentsSummary: vi.fn((opts: { documentIds?: string[] }) =>
      Promise.resolve({ documents: summaries.filter((s) => opts.documentIds?.includes(s.document_id)) }),
    ),
    documentQueryChunks: vi.fn(() => Promise.resolve({ chunks: [...chunks] })),
    documentGet: vi.fn().mockResolvedValue({ document_id: "arch", title: "Architecture", color_enabled: false }),
    documentGetChunk: vi.fn((id: string) => Promise.resolve({ ...chunks.find((c) => c.id === id)!, body: "Text." })),
    documentGetEdges: vi.fn().mockResolvedValue({ edges: [] }),
  } as unknown as ExplorerDataLayer;
}

let mounted: { root: Root; el: HTMLElement } | null = null;

beforeEach(() => localStorage.clear());

afterEach(() => {
  unmount();
  vi.restoreAllMocks();
});

function unmount() {
  if (!mounted) return;
  act(() => mounted!.root.unmount());
  mounted.el.remove();
  mounted = null;
}

async function mount(props: { path?: string } = {}) {
  unmount();
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(createElement(PathExplorer, { dataLayer: fakeData(), defaultScope: "tenant", ...props })),
  );
  mounted = { root, el };
  await vi.waitFor(() => expect(rowNames(el)).toContain("kb"));
  return el;
}

const rows = (el: HTMLElement) => Array.from(el.querySelectorAll<HTMLElement>(".path-tree .etree-row"));
const rowNames = (el: HTMLElement) => rows(el).map((r) => r.querySelector(".etree-name")!.textContent);
const rowOf = (el: HTMLElement, name: string) => rows(el).find((r) => r.querySelector(".etree-name")!.textContent === name)!;
async function expand(el: HTMLElement, name: string) {
  await act(async () => rowOf(el, name).querySelector<HTMLButtonElement>(".etree-caret")!.click());
}
const treePane = (el: HTMLElement) => el.querySelector<HTMLElement>(".paths-view > .splitter-left")!;

describe("PathExplorer", () => {
  it("TestPathExplorer_TreeCollapsesToARailAndTheStateIsRemembered", async () => {
    let el = await mount();
    expect(treePane(el).hidden).toBe(false);
    act(() => el.querySelector<HTMLButtonElement>(".paths-toolbar .pane-collapse")!.click());
    expect(treePane(el).hidden).toBe(true);
    const rail = el.querySelector<HTMLButtonElement>(".paths-view > .pane-rail .pane-rail-button")!;
    expect(rail.getAttribute("aria-label")).toBe("Show the Path tree");
    // Document counts are RFC HA's: the tree's rail shows its icon only.
    expect(rail.querySelector(".pane-rail-count")).toBeNull();
    expect(localStorage.getItem("loomcycle.explorer.collapsed.paths")).toBe("1");
    el = await mount();
    expect(treePane(el).hidden).toBe(true);
    act(() => el.querySelector<HTMLButtonElement>(".paths-view > .pane-rail .pane-rail-button")!.click());
    expect(treePane(el).hidden).toBe(false);
    // The divider's double-click and "[" fold it as well.
    act(() => {
      el.querySelector(".paths-view > .splitter-handle")!.dispatchEvent(new MouseEvent("dblclick", { bubbles: true }));
    });
    expect(treePane(el).hidden).toBe(true);
    act(() => {
      document.body.dispatchEvent(new KeyboardEvent("keydown", { key: "[", bubbles: true }));
    });
    expect(treePane(el).hidden).toBe(false);
  });

  it("TestPathExplorer_SearchStepsThroughMatchesAndOpensTheirFolders", async () => {
    const el = await mount();
    await expand(el, "kb");
    await expand(el, "docs");
    await expand(el, "notes");
    // Fold kb again: its listing stays, so the search still reaches it.
    await expand(el, "kb");
    expect(rowNames(el)).not.toContain("plan");
    act(() => {
      document.body.dispatchEvent(new KeyboardEvent("keydown", { key: "/", bubbles: true }));
    });
    const input = el.querySelector<HTMLInputElement>(".paths-find-input")!;
    expect(input).not.toBeNull();
    expect(el.querySelector(".paths-find-note")!.textContent).toContain("searching opened folders");
    await act(async () => {
      const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
      set.call(input, "ar");
      input.dispatchEvent(new Event("input", { bubbles: true }));
    });
    // "architecture" and "archive", in tree order; the first is opened to.
    expect(el.querySelector(".paths-find-note")!.textContent).toContain("1 of 2");
    expect(rowNames(el)).toContain("architecture");
    expect(el.querySelector(".etree-row.search-current .etree-name")!.textContent).toBe("architecture");
    act(() => {
      input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    });
    expect(el.querySelector(".etree-row.search-current .etree-name")!.textContent).toBe("archive");
    expect(el.querySelector(".paths-find-note")!.textContent).toContain("2 of 2");
    act(() => {
      input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", shiftKey: true, bubbles: true }));
    });
    expect(el.querySelector(".etree-row.search-current .etree-name")!.textContent).toBe("architecture");
    act(() => {
      input.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    });
    expect(el.querySelector(".paths-find")).toBeNull();
    expect(el.querySelector(".search-hit")).toBeNull();
  });

  it("TestPathExplorer_FilterHidesFoldersWithNoMatchAndSaysItCoversOpenedFolders", async () => {
    const el = await mount();
    await expand(el, "kb");
    await expand(el, "docs");
    await expand(el, "notes");
    await vi.waitFor(() => expect(rowOf(el, "plan").querySelector(".etree-mark-type")).not.toBeNull());
    act(() => el.querySelector<HTMLButtonElement>(".paths-find-toggle")!.click());
    act(() =>
      Array.from(el.querySelectorAll<HTMLButtonElement>(".paths-find-mode button"))
        .find((b) => b.textContent === "filter")!
        .click(),
    );
    expect(el.querySelector(".paths-find-note")!.textContent).toBe("filtering opened folders");
    const status = el.querySelector<HTMLSelectElement>('select[aria-label="Filter by status"]')!;
    expect(Array.from(status.options).map((o) => o.value)).toEqual(["", "done", "draft"]);
    await act(async () => {
      status.value = "draft";
      status.dispatchEvent(new Event("change", { bubbles: true }));
    });
    // Only "plan" is a draft: /notes has nothing and is gone; the folders
    // above "plan" stay, open.
    expect(rowNames(el)).toEqual(["kb", "docs", "plan"]);
  });

  it("TestPathExplorer_BreadcrumbSegmentSelectsAndRevealsItsFolder", async () => {
    const el = await mount();
    await expand(el, "kb");
    await expand(el, "docs");
    await act(async () => rowOf(el, "architecture").click());
    // Fold kb: the breadcrumb is what brings it back.
    await expand(el, "kb");
    await vi.waitFor(() => expect(el.querySelector(".doc-crumbs")).not.toBeNull());
    const crumbs = el.querySelector<HTMLElement>(".doc-crumbs")!;
    expect(Array.from(crumbs.querySelectorAll("li")).map((li) => li.textContent)).toEqual([
      "kb",
      "docs",
      "architecture",
    ]);
    // The last segment is the document, not a link.
    expect(crumbs.querySelector('[aria-current="page"]')!.textContent).toBe("architecture");
    expect(crumbs.querySelectorAll("button").length).toBe(2);
    // Fold the tree away, then follow "kb": it is selected, and the tree comes
    // back with it shown and open.
    act(() => el.querySelector<HTMLButtonElement>(".paths-toolbar .pane-collapse")!.click());
    expect(treePane(el).hidden).toBe(true);
    await act(async () =>
      Array.from(crumbs.querySelectorAll<HTMLButtonElement>("button"))
        .find((b) => b.textContent === "kb")!
        .click(),
    );
    expect(treePane(el).hidden).toBe(false);
    const kb = rowOf(el, "kb").closest('[role="treeitem"]')!;
    expect(kb.getAttribute("aria-selected")).toBe("true");
    expect(kb.getAttribute("aria-expanded")).toBe("true");
    // The folder's detail is its card, with no row or stopgap for _annotation.
    const detail = el.querySelector<HTMLElement>(".paths-detail")!;
    await vi.waitFor(() => expect(detail.querySelector(".ex-card-brief")?.textContent).toBe("The knowledge base."));
  });

  it("TestPathExplorer_FolderAnnotationIsEditedFromTheFolderActionsNotAStopgap", async () => {
    const el = await mount();
    await expand(el, "kb");
    await act(async () => rowOf(el, "kb").click());
    const detail = el.querySelector<HTMLElement>(".paths-detail")!;
    const labels = Array.from(detail.querySelectorAll("button")).map((b) => b.textContent);
    expect(labels).not.toContain("open the folder's annotation");
    expect(labels).toContain("edit folder annotation");
    expect(rowNames(el)).not.toContain("_annotation");
    await act(async () =>
      Array.from(detail.querySelectorAll<HTMLButtonElement>("button"))
        .find((b) => b.textContent === "edit folder annotation")!
        .click(),
    );
    // It opens the annotation document.
    await vi.waitFor(() => expect(el.querySelector(".doc-crumbs [aria-current]")?.textContent).toBe("_annotation"));
  });
});
