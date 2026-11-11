import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, it, expect } from "vitest";
import type { DirListing } from "../lib/lazyPathTree";
import type { PathEntry } from "../types";
import type { DocSummary } from "../lib/colorScheme";
import PathTree, { buildLazyTree, type PathTreeProps } from "./PathTree";

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
      return html.slice(html.lastIndexOf('<li class="node path-node', i), i);
    };
    expect(rowOf("b")).toContain("kind-document");
    expect(rowOf("b")).toContain("📄");
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
    // The folder row carries its _annotation's brief; the _annotation row does
    // not repeat it.
    expect(html.match(/Design records for the runtime\./g)?.length).toBe(2); // the line + the tooltip
    expect(html).toContain('<span class="path-brief">Design records for the runtime.</span>');
    expect(html).toContain('<span class="path-brief">Annotations at every level.</span>');
    // A document without a record gets no line, and an unlisted folder no brief.
    expect(html.match(/class="path-brief"/g)?.length).toBe(2);
    expect(html).toContain("📝");
  });
});
