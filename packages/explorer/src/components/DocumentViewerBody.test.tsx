// @vitest-environment happy-dom
import { act, createElement, type ReactElement } from "react";
import { type Root, createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ExplorerDataProvider, type ExplorerDataLayer } from "../lib/dataLayer";
import type { ChunkDetail, ChunkRow } from "../types";
import DocumentViewerBody, { docLink, type DocumentViewerBodyProps } from "./DocumentViewerBody";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

// Ids as the runtime writes them: 32 hex characters. None may reach the screen.
const DOC = "0123456789abcdef0123456789abcdef";
const id = (c: string) => c.repeat(32);
const ROOT = id("a");
const INTRO = id("b");
const DETAIL = id("c");
const OUTRO = id("d");

const rows: ChunkRow[] = [
  { id: ROOT, document_id: DOC, position: 0, title: "Architecture", type: "spec", status: "draft", revision: 3 },
  { id: OUTRO, document_id: DOC, parent_id: ROOT, position: 1, title: "Outro", type: "spec", status: "draft", revision: 1 },
  { id: INTRO, document_id: DOC, parent_id: ROOT, position: 0, title: "Intro", type: "spec", status: "draft", revision: 1 },
  { id: DETAIL, document_id: DOC, parent_id: INTRO, position: 0, title: "Detail", type: "spec", status: "done", revision: 2 },
];
const detail = (r: ChunkRow): ChunkDetail => ({ ...r, body: `Text of ${r.title}.` });

function fakeData() {
  const documentCreateChunk = vi.fn().mockResolvedValue(detail({ ...rows[1], id: id("e"), title: "Text" }));
  const documentExportFile = vi.fn().mockResolvedValue({ blob: new Blob(["x"]), filename: "Architecture.pdf" });
  const documentReorderChunk = vi.fn().mockResolvedValue({});
  const data = {
    documentQueryChunks: vi.fn().mockResolvedValue({ chunks: rows }),
    documentGet: vi.fn().mockResolvedValue({ document_id: DOC, root_chunk_id: ROOT, type: "spec", status: "draft" }),
    documentGetChunk: vi.fn((cid: string) => Promise.resolve(detail(rows.find((r) => r.id === cid)!))),
    documentGetEdges: vi.fn().mockResolvedValue({ edges: [] }),
    documentCreateChunk,
    documentReorderChunk,
    documentDeleteChunk: vi.fn().mockResolvedValue({}),
    documentSetAsset: vi.fn(),
    documentExportMd: vi.fn().mockResolvedValue({ markdown: "# A", title: "Architecture", document_id: DOC }),
    documentExportCanvas: vi.fn().mockResolvedValue({ canvas: { nodes: [], edges: [] }, document_id: DOC }),
    documentExportFile,
    documentClassify: vi.fn().mockResolvedValue({}),
  } as unknown as ExplorerDataLayer;
  return { data, documentCreateChunk, documentExportFile, documentReorderChunk };
}

let mounted: { root: Root; el: HTMLElement } | null = null;
let writeText: ReturnType<typeof vi.fn>;

beforeEach(() => {
  writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:x");
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
});

afterEach(() => {
  if (mounted) {
    act(() => mounted!.root.unmount());
    mounted.el.remove();
    mounted = null;
  }
  vi.restoreAllMocks();
});

async function mount(data: ExplorerDataLayer, props: Partial<DocumentViewerBodyProps> = {}) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  const body: ReactElement = createElement(DocumentViewerBody, { documentId: DOC, scope: "tenant", ...props });
  await act(async () => root.render(createElement(ExplorerDataProvider, { value: data, children: body })));
  mounted = { root, el };
  // Let the list, the meta, the edges and the bodies arrive.
  await vi.waitFor(() => expect(el.querySelectorAll(".doc-chunk .md").length).toBe(rows.length));
  return el;
}

const sections = (el: HTMLElement) => Array.from(el.querySelectorAll<HTMLElement>(".doc-chunk"));
const menuButton = (el: HTMLElement) => el.querySelector<HTMLButtonElement>(".doc-more-button")!;
const menuItems = (el: HTMLElement) => Array.from(el.querySelectorAll<HTMLButtonElement>('[role="menuitem"]'));
async function pick(el: HTMLElement, label: string) {
  act(() => menuButton(el).click());
  const item = menuItems(el).find((b) => b.textContent === label);
  if (!item) throw new Error(`no menu item ${label}: ${menuItems(el).map((b) => b.textContent).join(", ")}`);
  await act(async () => item.click());
}

describe("DocumentViewerBody", () => {
  it("renders every chunk on one page, in document order, headed by its depth", async () => {
    const el = await mount(fakeData().data);
    expect(sections(el).map((s) => s.dataset.chunkId)).toEqual([ROOT, INTRO, DETAIL, OUTRO]);
    // The root's title is the document's, shown once in the header.
    expect(el.querySelectorAll("h1").length).toBe(1);
    expect(el.querySelector("h1")!.textContent).toBe("Architecture");
    expect(sections(el)[0].querySelector(".doc-chunk-title")).toBeNull();
    expect(sections(el).slice(1).map((s) => s.querySelector(".doc-chunk-title")!.tagName)).toEqual(["H2", "H3", "H2"]);
    expect(sections(el).map((s) => s.querySelector(".md")!.textContent)).toEqual([
      "Text of Architecture.",
      "Text of Intro.",
      "Text of Detail.",
      "Text of Outro.",
    ]);
    // One page, not a per-chunk mode switch.
    expect(el.textContent).not.toMatch(/\bmarkdown\b/);
  });

  it("prints no id on screen", async () => {
    const el = await mount(fakeData().data, { pathLabel: "/kb/architecture" });
    for (const v of [DOC, ROOT, INTRO, DETAIL, OUTRO]) expect(el.textContent).not.toContain(v);
  });

  it("shows the header on one line: the path, the title once, the marks once, edit and more", async () => {
    const el = await mount(fakeData().data, { pathLabel: "/kb/architecture" });
    const header = el.querySelector<HTMLElement>(".doc-header")!;
    expect(header.querySelector(".doc-header-path")!.textContent).toBe("/kb/architecture");
    expect(header.querySelectorAll(".etree-mark-type").length).toBe(1);
    expect(header.querySelector(".doc-edit")!.textContent).toBe("edit");
    expect(menuButton(el).getAttribute("aria-haspopup")).toBe("menu");
    // The title is not repeated on the page (the outline's top row names the
    // document as a row).
    const page = el.querySelector<HTMLElement>(".doc-page")!;
    expect(page.textContent!.replace("Text of Architecture.", "")).not.toContain("Architecture");
  });

  it("lists every document action in the more menu and runs the one picked", async () => {
    const f = fakeData();
    const rename = vi.fn();
    const transfer = vi.fn();
    const del = vi.fn();
    const el = await mount(f.data, {
      documentActions: { rename, transfer, delete: del },
      renderAssistant: () => createElement("div", { className: "assistant-probe" }, "assistant here"),
    });
    act(() => menuButton(el).click());
    expect(menuItems(el).map((b) => b.textContent)).toEqual([
      "add text",
      "add child",
      "add image",
      "add diagram",
      "colours",
      "classify",
      "assistant",
      "download PDF",
      "download Word (.docx)",
      "download OpenDocument (.odt)",
      "download E-book (.epub)",
      "download HTML",
      "download Markdown (.md)",
      "download Canvas (.canvas)",
      "rename / move",
      "copy / move to…",
      "copy document id",
      "copy link",
      "reload",
      "delete document…",
    ]);
    expect(menuItems(el).find((b) => b.textContent === "delete document…")!.className).toBe("danger");
    expect(document.activeElement).toBe(menuItems(el)[0]);
    act(() => {
      menuItems(el)[0].dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    });
    expect(el.querySelector('[role="menu"]')).toBeNull();
    expect(document.activeElement).toBe(menuButton(el));

    await pick(el, "add text");
    expect(f.documentCreateChunk).toHaveBeenCalledWith(DOC, ROOT, { title: "Text", body: "" }, "tenant", undefined, undefined);
    act(() => el.querySelector<HTMLButtonElement>(".modal-buttons-top button")!.click()); // cancel the editor
    await pick(el, "download PDF");
    expect(f.documentExportFile).toHaveBeenCalledWith(DOC, "tenant", "pdf", undefined);
    await pick(el, "copy document id");
    expect(writeText).toHaveBeenLastCalledWith(DOC);
    await pick(el, "copy link");
    expect(writeText).toHaveBeenLastCalledWith(`loom:doc/${DOC}?scope=tenant`);
    await pick(el, "assistant");
    expect(el.querySelector(".assistant-probe")).not.toBeNull();
    await pick(el, "rename / move");
    await pick(el, "copy / move to…");
    await pick(el, "delete document…");
    expect([rename, transfer, del].map((fn) => fn.mock.calls.length)).toEqual([1, 1, 1]);
  });

  it("acts on the chunk the reader is on: picked in the outline, it is the one moved", async () => {
    const f = fakeData();
    const el = await mount(f.data);
    const outlineRow = Array.from(el.querySelectorAll<HTMLElement>(".chunk-tree .etree-row")).find(
      (r) => r.querySelector(".etree-name")!.textContent === "Outro",
    )!;
    act(() => outlineRow.click());
    expect(el.querySelector(".doc-chunk.current")!.getAttribute("data-chunk-id")).toBe(OUTRO);
    await pick(el, "move chunk up");
    expect(f.documentReorderChunk).toHaveBeenCalledWith(OUTRO, "up", "tenant", undefined);
  });

  it("scrolls the page to a chunk picked in the outline", async () => {
    const el = await mount(fakeData().data);
    const page = el.querySelector<HTMLElement>(".doc-page")!;
    const target = el.querySelector<HTMLElement>(`[data-chunk-id="${DETAIL}"]`)!;
    vi.spyOn(page, "getBoundingClientRect").mockReturnValue(new DOMRect(0, 100, 600, 400));
    vi.spyOn(target, "getBoundingClientRect").mockReturnValue(new DOMRect(0, 700, 600, 80));
    const outlineRow = Array.from(el.querySelectorAll<HTMLElement>(".chunk-tree .etree-row")).find(
      (r) => r.querySelector(".etree-name")!.textContent === "Detail",
    )!;
    act(() => outlineRow.click());
    expect(page.scrollTop).toBe(592);
    expect(target.classList.contains("current")).toBe(true);
    const item = outlineRow.closest('[role="treeitem"]')!;
    expect(item.getAttribute("aria-selected")).toBe("true");
  });

  it("opens on the chunk a link names", async () => {
    const el = await mount(fakeData().data, { initialChunkId: OUTRO });
    expect(el.querySelector(".doc-chunk.current")!.getAttribute("data-chunk-id")).toBe(OUTRO);
  });

  it("gives each chunk an action bar: marks, revision, view source, history, copy id and edit", async () => {
    const el = await mount(fakeData().data);
    const section = el.querySelector<HTMLElement>(`[data-chunk-id="${DETAIL}"]`)!;
    const bar = section.querySelector<HTMLElement>(".doc-chunk-bar")!;
    expect(bar.getAttribute("role")).toBe("toolbar");
    expect(bar.querySelector(".doc-chunk-rev")!.textContent).toBe("rev 2");
    expect(Array.from(bar.querySelectorAll("button")).map((b) => b.getAttribute("aria-label") ?? b.textContent)).toEqual([
      "view source",
      "history",
      "copy id",
      "edit",
    ]);
    // view source shows the chunk's own Markdown in place of the rendered text.
    act(() => bar.querySelector<HTMLButtonElement>('[aria-label="view source"]')!.click());
    expect(section.querySelector(".doc-chunk-source")!.textContent).toBe("Text of Detail.");
    expect(section.querySelector(".md")).toBeNull();
    await act(async () => bar.querySelector<HTMLButtonElement>('[aria-label="copy id"]')!.click());
    expect(writeText).toHaveBeenLastCalledWith(DETAIL);
  });

  it("keeps annotation, references and classification closed until asked for", async () => {
    const el = await mount(fakeData().data);
    expect(el.querySelector(".doc-panel")).toBeNull();
    const toggle = (label: string) =>
      Array.from(el.querySelectorAll<HTMLButtonElement>(".doc-panel-toggles button")).find((b) =>
        b.textContent!.startsWith(label),
      )!;
    act(() => toggle("references").click());
    expect(el.querySelector(".doc-panel .doc-links-pane")).not.toBeNull();
    act(() => toggle("references").click());
    expect(el.querySelector(".doc-panel")).toBeNull();
    act(() => toggle("classification").click());
    expect(el.querySelector(".doc-panel")!.textContent).toContain("classify this document");
  });
});

describe("docLink", () => {
  it("names the scope unless it is the user's own", () => {
    expect(docLink("x", "user")).toBe("loom:doc/x");
    expect(docLink("x", "agent")).toBe("loom:doc/x?scope=agent");
  });
});
