// @vitest-environment happy-dom
import { act, createElement, type ReactElement } from "react";
import { type Root, createRoot } from "react-dom/client";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { DirListing } from "../lib/lazyPathTree";
import type { DocSummary } from "../lib/colorScheme";
import type { ChunkRow, PathEntry } from "../types";
import PathTree, { buildLazyTree, type PathTreeProps } from "./PathTree";
import DocumentChunkTree, { buildChunkTree } from "./DocumentChunkTree";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let mounted: { root: Root; el: HTMLElement } | null = null;

afterEach(() => {
  if (mounted) {
    act(() => mounted!.root.unmount());
    mounted.el.remove();
    mounted = null;
  }
});

function mount(element: ReactElement): HTMLElement {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  act(() => root.render(element));
  mounted = { root, el };
  return el;
}

const entry = (full_path: string, kind: string, document_id?: string): PathEntry => ({
  name: full_path.slice(full_path.lastIndexOf("/") + 1),
  kind,
  full_path,
  ...(document_id ? { resource_ref: { document_id } } : {}),
});
const listing = (entries: PathEntry[]): DirListing => ({ entries, loaded: true, loading: false });

function pathTree(over: Partial<PathTreeProps> & { dirs: Map<string, DirListing> }) {
  const { dirs, ...rest } = over;
  const props: PathTreeProps = {
    tree: buildLazyTree(dirs),
    root: dirs.get(""),
    expanded: new Set(),
    onToggle: () => {},
    onLoadMore: () => {},
    onRetry: () => {},
    onSelect: () => {},
    ...rest,
  };
  return createElement(PathTree, props);
}

// itemOf finds the treeitem whose own row shows `name`.
function itemOf(el: HTMLElement, name: string): HTMLElement {
  const item = Array.from(el.querySelectorAll<HTMLElement>('[role="treeitem"]')).find(
    (li) => li.querySelector(":scope > .etree-row > .etree-name")?.textContent === name,
  );
  if (!item) throw new Error(`no row named ${name}`);
  return item;
}
const rowOf = (el: HTMLElement, name: string) => itemOf(el, name).querySelector<HTMLElement>(":scope > .etree-row")!;
const caretOf = (el: HTMLElement, name: string) => rowOf(el, name).querySelector<HTMLButtonElement>(".etree-caret")!;

describe("explorer tree rows", () => {
  const dirs = new Map([
    ["", listing([entry("/rfcs", "directory"), entry("/plan", "document", "p")])],
    ["/rfcs", listing([entry("/rfcs/ge", "document", "ge")])],
  ]);

  it("selects a Path entry when any part of its row is clicked, not just its name", () => {
    const onSelect = vi.fn();
    const el = mount(pathTree({ dirs, onSelect }));
    // The icon cell and the marks sit outside the name; a click there selects.
    act(() => rowOf(el, "plan").querySelector<HTMLElement>(".etree-icon")!.click());
    act(() => rowOf(el, "rfcs").click());
    expect(onSelect.mock.calls.map(([n]) => n.fullPath)).toEqual(["/plan", "/rfcs"]);
  });

  it("folds a folder from its caret without selecting it", () => {
    const onSelect = vi.fn();
    const onToggle = vi.fn();
    const el = mount(pathTree({ dirs, onSelect, onToggle }));
    act(() => caretOf(el, "rfcs").click());
    expect(onToggle).toHaveBeenCalledWith("/rfcs");
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("marks the selected row aria-selected even when the document is coloured", () => {
    const summaries = new Map<string, DocSummary>([
      ["p", { document_id: "p", title: "plan", color_enabled: true, type: "rfc", status: "draft" }],
    ]);
    const el = mount(pathTree({ dirs, summaries, selectedPath: "/plan" }));
    const item = itemOf(el, "plan");
    expect(item.getAttribute("aria-selected")).toBe("true");
    expect(item.tabIndex).toBe(0);
    // The colour reaches the row only as a custom property, never a background
    // that could cover the selected layer.
    const style = rowOf(el, "plan").getAttribute("style") ?? "";
    expect(style).toContain("--etree-tint");
    expect(style).not.toMatch(/background|border/);
    expect(itemOf(el, "rfcs").getAttribute("aria-selected")).toBe("false");
  });

  it("keeps one tab stop: the selected row while drawn, else the first row", () => {
    const stops = (el: HTMLElement) =>
      Array.from(el.querySelectorAll<HTMLElement>('[role="treeitem"]'))
        .filter((li) => li.tabIndex === 0)
        .map((li) => li.querySelector(":scope > .etree-row > .etree-name")?.textContent);
    // Nothing selected: the first row.
    expect(stops(mount(pathTree({ dirs })))).toEqual(["rfcs"]);
    // The selected row is drawn: only it, also after arrow keys moved the focus.
    const el = mount(pathTree({ dirs, selectedPath: "/plan" }));
    expect(stops(el)).toEqual(["plan"]);
    const rfcs = itemOf(el, "rfcs");
    act(() => {
      rfcs.focus();
      rfcs.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true }));
    });
    expect(stops(el)).toEqual(["plan"]);
    // The selected row sits in a closed folder: back to the first row.
    expect(stops(mount(pathTree({ dirs, selectedPath: "/rfcs/ge" })))).toEqual(["rfcs"]);
  });

  it("indents each level by its depth and exposes it as aria-level", () => {
    const el = mount(pathTree({ dirs, expanded: new Set(["/rfcs"]) }));
    expect(el.querySelector('[role="tree"]')).not.toBeNull();
    expect(itemOf(el, "rfcs").getAttribute("aria-level")).toBe("1");
    expect(itemOf(el, "rfcs").getAttribute("aria-expanded")).toBe("true");
    expect(itemOf(el, "ge").getAttribute("aria-level")).toBe("2");
    expect(rowOf(el, "ge").getAttribute("style")).toContain("--etree-depth: 1");
  });

  it("moves, opens and selects rows from the keyboard", () => {
    const onSelect = vi.fn();
    const onToggle = vi.fn();
    const el = mount(pathTree({ dirs, onSelect, onToggle }));
    const key = (target: HTMLElement, k: string) =>
      act(() => {
        target.dispatchEvent(new KeyboardEvent("keydown", { key: k, bubbles: true }));
      });
    const rfcs = itemOf(el, "rfcs");
    rfcs.focus();
    key(rfcs, "ArrowRight");
    expect(onToggle).toHaveBeenCalledWith("/rfcs");
    key(rfcs, "ArrowDown");
    expect(document.activeElement).toBe(itemOf(el, "plan"));
    key(itemOf(el, "plan"), "Enter");
    expect(onSelect.mock.calls.map(([n]) => n.fullPath)).toEqual(["/plan"]);
  });

  const chunk = (id: string, title: string, parent_id?: string, position = 0): ChunkRow =>
    ({ id, title, parent_id, position }) as ChunkRow;
  const chunks = [chunk("root", "Doc"), chunk("a", "Intro", "root", 0), chunk("b", "Body", "root", 1)];

  it("draws chunk rows without a copy-id button, and selects one from anywhere on its row", () => {
    const onSelect = vi.fn();
    const el = mount(
      createElement(DocumentChunkTree, { tree: buildChunkTree(chunks), selectedId: "a", onSelect }),
    );
    expect(el.querySelector(".id-copy, .id-copy-btn")).toBeNull();
    // A row's only button is its caret.
    expect(rowOf(el, "Body").querySelectorAll("button").length).toBe(1);
    expect(itemOf(el, "Intro").getAttribute("aria-selected")).toBe("true");
    act(() => rowOf(el, "Body").click());
    expect(onSelect.mock.calls.map(([n]) => n.row.id)).toEqual(["b"]);
  });

  it("marks a chunk's type and status only where they differ from the document's", () => {
    const typed = [
      { ...chunk("root", "Doc"), type: "spec", status: "draft" },
      { ...chunk("a", "Same", "root", 0), type: "spec", status: "draft" },
      { ...chunk("b", "Done", "root", 1), type: "spec", status: "done" },
      { ...chunk("c", "Note", "root", 2), type: "note", status: "draft" },
    ];
    const el = mount(
      createElement(DocumentChunkTree, {
        tree: buildChunkTree(typed),
        onSelect: () => {},
        documentType: "spec",
        documentStatus: "draft",
      }),
    );
    const marks = (name: string) => ({
      type: rowOf(el, name).querySelector(".etree-mark-type")?.textContent ?? null,
      status: rowOf(el, name).querySelector(".etree-mark-status")?.getAttribute("aria-label") ?? null,
    });
    expect(marks("Doc")).toEqual({ type: null, status: null });
    expect(marks("Same")).toEqual({ type: null, status: null });
    expect(marks("Done")).toEqual({ type: null, status: "status done" });
    expect(marks("Note")).toEqual({ type: "note", status: null });
  });

  it("folds a chunk from its caret without selecting it", () => {
    const onSelect = vi.fn();
    const el = mount(createElement(DocumentChunkTree, { tree: buildChunkTree(chunks), onSelect }));
    expect(el.textContent).toContain("Intro");
    act(() => caretOf(el, "Doc").click());
    expect(onSelect).not.toHaveBeenCalled();
    expect(el.textContent).not.toContain("Intro");
    expect(itemOf(el, "Doc").getAttribute("aria-expanded")).toBe("false");
  });
});
