// @vitest-environment happy-dom
import { act, createElement } from "react";
import { type Root, createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ExplorerDataLayer, PathLsPage, PathLsResult } from "./lib/dataLayer";
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

// serverData answers like an RFC HA runtime: a one-level ls asked with_counts
// counts the readable documents under each folder (a folder's _annotation is
// not one), and a filtered one keeps only what leads to a match, marks a
// document that matched itself and echoes the filters. `pageOf` clips the
// root's filtered answer to pages of that size, to page through.
function serverData(opts: { pageOf?: number } = {}) {
  const data = fakeData();
  const byId = new Map(summaries.map((s) => [s.document_id, s]));
  const matches = (e: PathEntry, f: NonNullable<PathLsPage["filters"]>) => {
    const s = byId.get((e.resource_ref as { document_id?: string } | undefined)?.document_id ?? "");
    if (!s || e.name === "_annotation") return false;
    if (f.name_contains && !`${e.name} ${s.title}`.toLowerCase().includes(f.name_contains.toLowerCase())) return false;
    if (f.type && s.type !== f.type) return false;
    if (f.status && s.status !== f.status) return false;
    return true;
  };
  const countUnder = (path: string, f?: PathLsPage["filters"]): number =>
    (listings[path] ?? []).reduce(
      (n, e) =>
        n +
        (e.kind === "directory"
          ? countUnder(e.full_path, f)
          : e.name !== "_annotation" && (!f || matches(e, f))
            ? 1
            : 0),
      0,
    );
  const pathLs = vi.fn(
    (path: string, _scope: unknown, _rec: boolean, _browse: unknown, page?: PathLsPage): Promise<PathLsResult> => {
      const f = page?.filters;
      let entries = (listings[path] ?? []).flatMap((e): PathEntry[] => {
        const counted = e.kind === "directory" && page?.withCounts ? { documents: countUnder(e.full_path, f) } : {};
        if (!f) return [{ ...e, ...counted }];
        if (e.kind === "directory") return countUnder(e.full_path, f) > 0 ? [{ ...e, ...counted }] : [];
        return matches(e, f) ? [{ ...e, matched: true }] : [];
      });
      const out: PathLsResult = { path, entries };
      if (f) out.filters = f;
      if (f && path === "/" && opts.pageOf) {
        const from = page?.cursor ? Number(page.cursor) : 0;
        entries = entries.slice(from, from + opts.pageOf);
        out.entries = entries;
        if (from + opts.pageOf < (listings[path] ?? []).length) {
          out.truncated = true;
          out.next_cursor = String(from + opts.pageOf);
        }
      }
      return Promise.resolve(out);
    },
  );
  (data as { pathLs: unknown }).pathLs = pathLs;
  return { data, pathLs };
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

async function mount(props: { path?: string } = {}, dataLayer: ExplorerDataLayer = fakeData()) {
  unmount();
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () => root.render(createElement(PathExplorer, { dataLayer, defaultScope: "tenant", ...props })));
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
const countOf = (el: HTMLElement, name: string) => rowOf(el, name)?.querySelector(".etree-count")?.textContent;
const note = (el: HTMLElement) => el.querySelector(".paths-find-note")!.textContent;
async function openFilter(el: HTMLElement) {
  act(() => el.querySelector<HTMLButtonElement>(".paths-find-toggle")!.click());
  act(() =>
    Array.from(el.querySelectorAll<HTMLButtonElement>(".paths-find-mode button"))
      .find((b) => b.textContent === "filter")!
      .click(),
  );
}
async function typeFilterName(el: HTMLElement, v: string) {
  const input = el.querySelector<HTMLInputElement>(".paths-find-input")!;
  await act(async () => {
    const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
    set.call(input, v);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

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

  it("TestPathExplorer_FolderRowsShowTheRuntimesCounts", async () => {
    const { data, pathLs } = serverData();
    const el = await mount({}, data);
    // Every listing asks for counts; a folder's _annotation is not counted.
    expect(pathLs.mock.calls[0][4]).toMatchObject({ withCounts: true });
    await vi.waitFor(() => expect(countOf(el, "kb")).toBe("2"));
    expect(countOf(el, "notes")).toBe("1");
    // The old runtime's tree (no counts sent) keeps the slots empty.
    const old = await mount();
    expect(countOf(old, "kb")).toBe("");
  });

  it("TestPathExplorer_ServerFilterCoversFoldersNobodyOpenedAndMarksMatches", async () => {
    const { data, pathLs } = serverData();
    const el = await mount({}, data);
    // Nothing is opened: the local filter would find nothing below the root.
    await openFilter(el);
    await typeFilterName(el, "plan");
    // The runtime is asked (once typing pauses) with the filter and counts.
    await vi.waitFor(() =>
      expect(pathLs).toHaveBeenCalledWith("/", "tenant", false, undefined, expect.objectContaining({
        withCounts: true,
        filters: { name_contains: "plan" },
      })),
    );
    // Its kept folders open by themselves, down to the match.
    await vi.waitFor(() => expect(rowNames(el)).toEqual(["kb", "docs", "plan"]));
    expect(note(el)).toBe("filtering the whole tree");
    expect(countOf(el, "kb")).toBe("1");
    expect(rowOf(el, "kb").querySelector(".etree-count")!.getAttribute("aria-label")).toBe("1 matching document");
    expect(rowOf(el, "plan").classList.contains("filter-match")).toBe(true);
    expect(rowOf(el, "kb").classList.contains("filter-match")).toBe(false);
    // Every filtered listing carries the filter.
    for (const call of pathLs.mock.calls.filter((c) => c[4]?.filters)) {
      expect(call[4]!.filters).toEqual({ name_contains: "plan" });
    }
    // A folder the reader closes stays closed.
    await expand(el, "kb");
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });
    expect(rowNames(el)).toEqual(["kb"]);
    // Closing the filter gives back the tree as it was: nothing opened.
    act(() => el.querySelector<HTMLButtonElement>(".paths-find-close")!.click());
    expect(rowNames(el)).toEqual(["kb", "notes"]);
  });

  it("TestPathExplorer_ServerFilterPagesWithLookForMoreMatches", async () => {
    // A filtered page can stop short: the tree ends in a row that asks for
    // the next one, with the same filter.
    const { data, pathLs } = serverData({ pageOf: 1 });
    const el = await mount({}, data);
    await openFilter(el);
    const status = el.querySelector<HTMLSelectElement>('select[aria-label="Filter by status"]')!;
    // The picker offers what the loaded documents carry; "done" is set directly.
    await act(async () => {
      const opt = document.createElement("option");
      opt.value = "done";
      status.appendChild(opt);
      status.value = "done";
      status.dispatchEvent(new Event("change", { bubbles: true }));
    });
    await vi.waitFor(() => expect(note(el)).toBe("filtering the whole tree"));
    await vi.waitFor(() => expect(rowNames(el)).toEqual(["kb", "docs", "architecture"]));
    const more = () =>
      Array.from(el.querySelectorAll<HTMLButtonElement>(".path-more")).find((b) => b.textContent === "Look for more matches");
    expect(more()).toBeDefined();
    await act(async () => more()!.click());
    expect(pathLs).toHaveBeenCalledWith("/", "tenant", false, undefined, expect.objectContaining({
      cursor: "1",
      filters: { status: "done" },
    }));
    await vi.waitFor(() => expect(rowNames(el)).toContain("archive"));
    expect(more()).toBeUndefined();
  });

  it("TestPathExplorer_FilterFallsBackToOpenedFoldersOnAnOlderRuntime", async () => {
    // fakeData answers like a runtime before RFC HA: the filters are ignored
    // and not echoed, so the filter runs over the listed folders here.
    const data = fakeData();
    const el = await mount({}, data);
    await expand(el, "kb");
    await expand(el, "docs");
    await openFilter(el);
    await typeFilterName(el, "plan");
    await vi.waitFor(() =>
      expect(data.pathLs).toHaveBeenCalledWith("/", "tenant", false, undefined, expect.objectContaining({
        filters: { name_contains: "plan" },
      })),
    );
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });
    expect(note(el)).toBe("filtering opened folders");
    expect(rowNames(el)).toEqual(["kb", "docs", "plan"]);
    expect(rowOf(el, "plan").classList.contains("filter-match")).toBe(false);
    // Once known, the runtime is not asked again for the next filter.
    const asked = (data.pathLs as ReturnType<typeof vi.fn>).mock.calls.length;
    await typeFilterName(el, "pla");
    await act(async () => {
      await new Promise((r) => setTimeout(r, 300));
    });
    expect((data.pathLs as ReturnType<typeof vi.fn>).mock.calls.length).toBe(asked);
    expect(rowNames(el)).toEqual(["kb", "docs", "plan"]);
  });

  it("TestPathExplorer_FilterFallsBackWhenTheRuntimeRefusesIt", async () => {
    // A scope whose documents the runtime cannot read refuses a filtered ls.
    const data = fakeData();
    const plain = data.pathLs;
    (data as { pathLs: unknown }).pathLs = vi.fn((...args: Parameters<ExplorerDataLayer["pathLs"]>) =>
      args[4]?.filters ? Promise.reject(new Error("ls: name_contains, type and status read ...")) : plain(...args),
    );
    const el = await mount({}, data);
    await expand(el, "notes");
    await openFilter(el);
    await typeFilterName(el, "arch");
    await vi.waitFor(() => expect(note(el)).toBe("filtering opened folders"));
    await act(async () => {
      await new Promise((r) => setTimeout(r, 300));
    });
    expect(note(el)).toBe("filtering opened folders");
    expect(rowNames(el)).toEqual(["notes", "archive"]);
    expect(el.querySelector(".paths-err")).toBeNull();
  });
});
