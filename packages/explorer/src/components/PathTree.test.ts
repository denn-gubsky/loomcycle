import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, it, expect } from "vitest";
import type { DirListing } from "../lib/lazyPathTree";
import type { PathEntry } from "../types";
import type { DocSummary } from "../lib/colorScheme";
import PathTree, {
  buildLazyTree,
  countLabel,
  folderDocuments,
  formatCount,
  nodeBrief,
  nodeCard,
  type PathTreeProps,
} from "./PathTree";

const dir = (full_path: string): PathEntry => ({
  name: full_path.slice(full_path.lastIndexOf("/") + 1),
  kind: "directory",
  full_path,
});
const doc = (full_path: string): PathEntry => ({ ...dir(full_path), kind: "document" });
const listing = (entries: PathEntry[], extra: Partial<DirListing> = {}): DirListing => ({
  entries,
  loaded: true,
  loading: false,
  ...extra,
});

function render(
  dirs: Map<string, DirListing>,
  expanded: string[],
  summaries?: Map<string, DocSummary>,
  filtered?: boolean,
): string {
  const noop = () => {};
  const props: PathTreeProps = {
    tree: buildLazyTree(dirs),
    root: dirs.get(""),
    expanded: new Set(expanded),
    onToggle: noop,
    onLoadMore: noop,
    onRetry: noop,
    onSelect: noop,
    summaries,
    filtered,
  };
  return renderToStaticMarkup(createElement(PathTree, props));
}

describe("PathTree", () => {
  it("ends a clipped directory with a load-more row instead of dropping its tail", () => {
    const dirs = new Map([
      ["", listing([dir("/facts"), dir("/memory")])],
      ["/facts", listing([doc("/facts/f0"), doc("/facts/f1")], { nextCursor: "c" })],
    ]);
    const html = render(dirs, ["/facts"]);
    expect(html).toContain("Load more (2 shown)");
    expect(html).toContain(">memory<");
  });

  it("draws an unopened directory collapsed, with a live caret and no children", () => {
    const dirs = new Map([
      ["", listing([dir("/facts")])],
      ["/facts", listing([doc("/facts/f0")])],
    ]);
    const html = render(dirs, []);
    expect(html).toContain('aria-expanded="false"');
    expect(html).not.toContain(">f0<");
  });

  it("shows an opened directory's loading, empty and failed states", () => {
    const dirs = new Map<string, DirListing>([
      ["", listing([dir("/a"), dir("/b"), dir("/c")])],
      ["/a", listing([], { loaded: false, loading: true })],
      ["/b", listing([])],
      ["/c", listing([], { loaded: false, error: "boom" })],
    ]);
    const html = render(dirs, ["/a", "/b", "/c"]);
    expect(html).toContain("loading…");
    expect(html).toContain("empty");
    expect(html).toContain("failed to load: boom");
    expect(html).toContain(">retry<");
  });

  // RFC GC: a name that is a document AND has names beneath it opens like a
  // directory, and still draws as the document.
  it("gives a document with children a live caret, and keeps a leaf document's caret inert", () => {
    const parent: PathEntry = { ...doc("/a/b"), has_children: true };
    const dirs = new Map([
      ["", listing([dir("/a")])],
      ["/a", listing([parent, doc("/a/leaf")])],
    ]);
    const html = render(dirs, ["/a"]);
    const rowOf = (name: string) => {
      const i = html.indexOf(`>${name}<`);
      return html.slice(html.lastIndexOf('<li class="etree-node path-node', i), i);
    };
    expect(rowOf("b")).toContain("kind-document");
    expect(rowOf("b")).toContain("etree-svg-document");
    expect(rowOf("b")).toContain('aria-expanded="false"');
    expect(rowOf("b")).not.toContain("disabled");
    expect(rowOf("leaf")).toContain("disabled");
    expect(rowOf("leaf")).not.toContain("aria-expanded");
  });

  it("lists an opened document's children, with paging, under the document row", () => {
    const parent: PathEntry = { ...doc("/a/b"), has_children: true };
    const dirs = new Map([
      ["", listing([dir("/a")])],
      ["/a", listing([parent])],
      ["/a/b", listing([doc("/a/b/c")], { nextCursor: "next" })],
    ]);
    const tree = buildLazyTree(dirs);
    const b = tree[0].children[0];
    expect(b.kind).toBe("document");
    expect(b.hasChildren).toBe(true);
    expect(b.children.map((n) => n.fullPath)).toEqual(["/a/b/c"]);

    const html = render(dirs, ["/a", "/a/b"]);
    expect(html).toContain('aria-expanded="true"');
    expect(html).toContain(">c<");
    expect(html).toContain("Load more (1 shown)");
    // Collapsed, the children are not drawn.
    expect(render(dirs, ["/a"])).not.toContain(">c<");
  });

  it("never opens a document the runtime did not mark, even if a listing exists at its path", () => {
    const dirs = new Map([
      ["", listing([doc("/b")])],
      ["/b", listing([doc("/b/c")])],
    ]);
    const tree = buildLazyTree(dirs);
    expect(tree[0].hasChildren).toBeUndefined();
    expect(tree[0].children).toEqual([]);
    expect(render(dirs, ["/b"])).not.toContain(">c<");
  });

  it("shows a document's brief from documents_summary, and a folder's from its _annotation", () => {
    const docRef = (full_path: string, id: string): PathEntry => ({
      ...doc(full_path),
      resource_ref: { document_id: id },
    });
    const dirs = new Map([
      ["", listing([dir("/rfcs"), dir("/notes")])],
      ["/rfcs", listing([docRef("/rfcs/_annotation", "ann"), docRef("/rfcs/ge", "ge"), docRef("/rfcs/gf", "gf")])],
    ]);
    const sum = (document_id: string, brief?: string): DocSummary => ({
      document_id,
      title: document_id,
      color_enabled: false,
      ...(brief ? { brief } : {}),
      ...(document_id === "ann" ? { type: "Annotation" } : {}),
    });
    const summaries = new Map([
      ["ann", sum("ann", "Design records for the runtime.")],
      ["ge", sum("ge", "Annotations at every level.")],
      ["gf", sum("gf")],
    ]);
    const html = render(dirs, ["/rfcs"], summaries);
    // The folder's card carries its _annotation's brief, a document's its own;
    // neither is a second line, nor a native tooltip beside the card.
    const tree = buildLazyTree(dirs);
    const rfcs = tree.find((n) => n.name === "rfcs")!;
    expect(nodeCard(rfcs, summaries)).toMatchObject({
      title: "rfcs",
      type: "folder",
      brief: "Design records for the runtime.",
    });
    const ge = rfcs.children.find((n) => n.name === "ge")!;
    expect(nodeCard(ge, summaries)).toMatchObject({ title: "ge", path: "/rfcs/ge", brief: "Annotations at every level." });
    const gf = rfcs.children.find((n) => n.name === "gf")!;
    expect(nodeCard(gf, summaries).brief).toBeUndefined();
    expect(html).not.toContain("title=");
    expect(html).not.toContain("path-brief");
  });

  it("leaves a folder's _annotation entry out of its rows while the folder keeps its brief", () => {
    const dirs = new Map([
      ["", listing([dir("/rfcs")])],
      ["/rfcs", listing([{ ...doc("/rfcs/_annotation"), resource_ref: { document_id: "ann" } }, doc("/rfcs/ge")])],
    ]);
    const summaries = new Map([
      ["ann", { document_id: "ann", title: "ann", color_enabled: false, brief: "Design records." } as DocSummary],
    ]);
    const tree = buildLazyTree(dirs);
    // The entry is still in the built tree, which is where the brief comes from.
    expect(tree[0].children.map((n) => n.name)).toContain("_annotation");
    expect(nodeBrief(tree[0], summaries)).toBe("Design records.");
    const html = render(dirs, ["/rfcs"], summaries);
    expect(html).not.toContain(">_annotation<");
    expect(html).not.toContain("etree-svg-annotation");
    expect(html).toContain(">ge<");
    expect(nodeCard(tree[0], summaries).brief).toBe("Design records.");
  });

  it("says a folder holding only its _annotation is empty", () => {
    const dirs = new Map([
      ["", listing([dir("/rfcs")])],
      ["/rfcs", listing([doc("/rfcs/_annotation")])],
    ]);
    expect(render(dirs, ["/rfcs"])).toContain(">empty<");
  });

  it("draws type as a named tag and status as a dot, with the full labels in the row's card", () => {
    const dirs = new Map([["", listing([{ ...doc("/plan"), resource_ref: { document_id: "p" } }])]]);
    const summaries = new Map([
      ["p", { document_id: "p", title: "p", color_enabled: false, type: "Research", status: "draft" } as DocSummary],
    ]);
    const html = render(dirs, [], summaries);
    expect(html).toContain(">Research</span>");
    expect(html).toContain('aria-label="status draft"');
    // No native tooltip on the marks: the card names them in full.
    expect(html).not.toContain('title="type: Research"');
    expect(nodeCard(buildLazyTree(dirs)[0], summaries)).toMatchObject({ type: "Research", status: "draft" });
    expect(html).not.toContain("chunk-badge");
  });

  it("TestPathTree_FolderCountSlotIsEmptyWithoutServerData", () => {
    const dirs = new Map([["", listing([dir("/rfcs"), doc("/readme")])]]);
    const html = render(dirs, []);
    // One slot, on the folder row only, and nothing in it: a count worked out
    // from the opened folders would be wrong, so none is shown.
    expect(html.match(/class="etree-count"/g)?.length).toBe(1);
    expect(html).toContain('<span class="etree-count"></span>');
  });

  it("TestPathTree_FolderCountSlotShowsTheRuntimesCount", () => {
    // RFC HA: a listing entry that carries `documents` fills the slot.
    const counted = { ...dir("/rfcs"), documents: 12 } as PathEntry;
    expect(folderDocuments(counted)).toBe(12);
    expect(folderDocuments({ ...doc("/x"), documents: 3 } as PathEntry)).toBeUndefined();
    const html = render(new Map([["", listing([counted])]]), []);
    expect(html).toContain('aria-label="12 documents">12</span>');
  });

  it("TestPathTree_FolderCountShowsCappedAsAFloor", () => {
    // RFC HA counts exactly to 10,000; past that the count is a floor.
    const capped = { ...dir("/facts"), documents: 10000, documents_capped: true } as PathEntry;
    const big = { ...dir("/big"), documents: 2345 } as PathEntry;
    const html = render(new Map([["", listing([capped, big])]]), []);
    expect(html).toContain('<span class="etree-count capped" aria-label="10,000 or more documents">10k+</span>');
    expect(html).toContain('aria-label="2,345 documents">2,345</span>');
    expect(formatCount(1, false)).toBe("1");
    expect(formatCount(10000, false)).toBe("10k");
    expect(formatCount(500, true)).toBe("500+");
    expect(countLabel(1, false, true)).toBe("1 matching document");
  });

  it("TestPathTree_AbsentCountLeavesTheSlotEmptyNeverZero", () => {
    // A runtime that cannot count sends no `documents`: nothing is drawn,
    // and a real zero is drawn as 0.
    const absent = { ...dir("/a"), documents_capped: true } as PathEntry;
    const zero = { ...dir("/b"), documents: 0 } as PathEntry;
    const bogus = { ...dir("/c"), documents: "7" } as unknown as PathEntry;
    expect(folderDocuments(absent)).toBeUndefined();
    expect(folderDocuments(bogus)).toBeUndefined();
    const html = render(new Map([["", listing([absent, zero, bogus])]]), []);
    expect(html.match(/<span class="etree-count"><\/span>/g)?.length).toBe(2);
    expect(html).toContain('aria-label="0 documents">0</span>');
  });

  it("TestPathTree_DocumentWithNamesBelowShowsTheirCount", () => {
    const docFolder = { ...doc("/spec"), has_children: true, documents: 4 } as PathEntry;
    expect(folderDocuments(docFolder)).toBe(4);
    const html = render(new Map([["", listing([docFolder])]]), []);
    expect(html).toContain('aria-label="4 documents">4</span>');
  });

  it("TestPathTree_FilteredTreeMarksMatchesAndQuietsKeptDocuments", () => {
    // Under the runtime's filter: a document that matched itself is marked; a
    // document kept for the names below it is drawn quieter; folders carry no
    // mark, and their counts are of matches.
    const dirs = new Map([
      [
        "",
        listing(
          [
            { ...dir("/kb"), documents: 2 } as PathEntry,
            { ...doc("/plan"), matched: true } as PathEntry,
            { ...doc("/spec"), has_children: true, documents: 1 } as PathEntry,
          ],
          { appliedFilters: { status: "draft" }, nextCursor: "c" },
        ),
      ],
    ]);
    const html = render(dirs, [], undefined, true);
    expect(html).toMatch(/class="etree-row filter-match"[^>]*><button[^]*?>plan</);
    expect(html).toMatch(/class="etree-row filter-kept"[^>]*><button[^]*?>spec</);
    expect(html).not.toMatch(/class="etree-row filter-(match|kept)"[^>]*><button[^]*?>kb</);
    expect(html).toContain('aria-label="2 matching documents">2</span>');
    // The page may stop early: more may match.
    expect(html).toContain("Look for more matches");
    expect(html).not.toContain("Load more");
    // Unfiltered, the same rows carry no marks.
    expect(render(dirs, [])).not.toContain("filter-");
  });

  it("TestPathTree_FilteredTreeWithNothingSaysSo", () => {
    const dirs = new Map([["", listing([], { appliedFilters: { name_contains: "zz" } })]]);
    expect(render(dirs, [], undefined, true)).toContain("Nothing in this tree matches the filter.");
  });
});
