import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, it, expect } from "vitest";
import type { DirListing } from "../lib/lazyPathTree";
import type { PathEntry } from "../types";
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

function render(dirs: Map<string, DirListing>, expanded: string[]): string {
  const noop = () => {};
  const props: PathTreeProps = {
    tree: buildLazyTree(dirs),
    root: dirs.get(""),
    expanded: new Set(expanded),
    onToggle: noop,
    onLoadMore: noop,
    onRetry: noop,
    onSelect: noop,
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
});
