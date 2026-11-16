// @vitest-environment happy-dom
import { act, createElement } from "react";
import { type Root, createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ExplorerDataProvider, type ExplorerDataLayer } from "../lib/dataLayer";
import DownloadMenu from "./DownloadMenu";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const BROWSE = { scopeId: "u1", tenant: "t1" };

function fakeData(withRender: boolean) {
  const documentExportMd = vi.fn().mockResolvedValue({ markdown: "# T", title: "My doc", document_id: "d1" });
  const documentExportCanvas = vi.fn().mockResolvedValue({ canvas: { nodes: [], edges: [] }, document_id: "d1" });
  const documentExportFile = vi
    .fn()
    .mockResolvedValue({ blob: new Blob(["x"], { type: "application/pdf" }), filename: "Server name.pdf" });
  const data = {
    documentExportMd,
    documentExportCanvas,
    ...(withRender ? { documentExportFile } : {}),
  } as unknown as ExplorerDataLayer;
  return { data, documentExportMd, documentExportCanvas, documentExportFile };
}

let mounted: { root: Root; el: HTMLElement } | null = null;
let saved: string[] = [];

beforeEach(() => {
  saved = [];
  // saveBlob: record the download name instead of navigating.
  vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:x");
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
    saved.push(this.download);
  });
});

afterEach(() => {
  if (mounted) {
    act(() => mounted!.root.unmount());
    mounted.el.remove();
    mounted = null;
  }
  vi.restoreAllMocks();
});

function mount(data: ExplorerDataLayer, onError: (m: string) => void = () => {}) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  act(() =>
    root.render(
      createElement(ExplorerDataProvider, {
        value: data,
        children: createElement(DownloadMenu, {
          documentId: "d1",
          scope: "tenant",
          browse: BROWSE,
          title: "My doc",
          onError,
        }),
      }),
    ),
  );
  mounted = { root, el };
  return el;
}

const menuButton = (el: HTMLElement) => el.querySelector<HTMLButtonElement>('button[aria-haspopup="menu"]')!;
const items = (el: HTMLElement) => Array.from(el.querySelectorAll<HTMLButtonElement>('[role="menuitem"]'));
const openMenu = (el: HTMLElement) => act(() => menuButton(el).click());
async function pick(el: HTMLElement, kind: string) {
  await act(async () => {
    el.querySelector<HTMLButtonElement>(`[role="menuitem"][data-kind="${kind}"]`)!.click();
  });
}

describe("DownloadMenu", () => {
  it("opens a menu listing the five download formats", () => {
    const el = mount(fakeData(true).data);
    expect(el.querySelector('[role="menu"]')).toBeNull();
    expect(menuButton(el).getAttribute("aria-expanded")).toBe("false");
    openMenu(el);
    expect(menuButton(el).getAttribute("aria-expanded")).toBe("true");
    expect(items(el).map((b) => b.textContent)).toEqual([
      "PDF",
      "Word (.docx)",
      "HTML",
      "Markdown (.md)",
      "Canvas (.canvas)",
    ]);
    expect(document.activeElement).toBe(items(el)[0]);
  });

  it("offers only .md and .canvas when the data layer cannot fetch rendered files", () => {
    const el = mount(fakeData(false).data);
    openMenu(el);
    expect(items(el).map((b) => b.dataset.kind)).toEqual(["md", "canvas"]);
  });

  it("renders PDF, Word and HTML through documentExportFile, saved under the server's name", async () => {
    const f = fakeData(true);
    const el = mount(f.data);
    for (const kind of ["pdf", "docx", "html"]) {
      openMenu(el);
      await pick(el, kind);
      expect(f.documentExportFile).toHaveBeenLastCalledWith("d1", "tenant", kind, BROWSE);
    }
    expect(saved).toEqual(["Server name.pdf", "Server name.pdf", "Server name.pdf"]);
    expect(f.documentExportMd).not.toHaveBeenCalled();
    expect(f.documentExportCanvas).not.toHaveBeenCalled();
  });

  it("names a rendered file by the title when the server names none", async () => {
    const f = fakeData(true);
    f.documentExportFile.mockResolvedValueOnce({ blob: new Blob(["x"]), filename: "" });
    const el = mount(f.data);
    openMenu(el);
    await pick(el, "docx");
    expect(saved).toEqual(["My_doc.docx"]);
  });

  it("keeps .md as the round-trippable export with metadata, and .canvas as the canvas graph", async () => {
    const f = fakeData(true);
    const el = mount(f.data);
    openMenu(el);
    await pick(el, "md");
    expect(f.documentExportMd).toHaveBeenCalledWith("d1", "tenant", true, BROWSE);
    openMenu(el);
    await pick(el, "canvas");
    expect(f.documentExportCanvas).toHaveBeenCalledWith("d1", "tenant", BROWSE);
    expect(f.documentExportFile).not.toHaveBeenCalled();
    expect(saved).toEqual(["My_doc.md", "My_doc.canvas"]);
  });

  it("closes after a selection, on Escape and on an outside click", async () => {
    const el = mount(fakeData(true).data);
    openMenu(el);
    await pick(el, "md");
    expect(el.querySelector('[role="menu"]')).toBeNull();

    openMenu(el);
    act(() => {
      items(el)[0]!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    });
    expect(el.querySelector('[role="menu"]')).toBeNull();
    expect(document.activeElement).toBe(menuButton(el));

    openMenu(el);
    act(() => {
      document.body.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
    });
    expect(el.querySelector('[role="menu"]')).toBeNull();
  });

  it("moves through the items with the arrow keys, wrapping", () => {
    const el = mount(fakeData(true).data);
    openMenu(el);
    const key = (k: string) =>
      act(() => {
        (document.activeElement as HTMLElement).dispatchEvent(new KeyboardEvent("keydown", { key: k, bubbles: true }));
      });
    key("ArrowUp");
    expect(document.activeElement).toBe(items(el)[4]);
    key("ArrowDown");
    expect(document.activeElement).toBe(items(el)[0]);
    key("End");
    expect(document.activeElement).toBe(items(el)[4]);
  });

  it("shows the download in progress on the button and reports a refusal through onError", async () => {
    const f = fakeData(true);
    let fail!: (e: Error) => void;
    f.documentExportFile.mockReturnValueOnce(new Promise((_, reject) => (fail = reject)));
    const onError = vi.fn();
    const el = mount(f.data, onError);
    openMenu(el);
    await pick(el, "pdf");
    expect(menuButton(el).disabled).toBe(true);
    expect(menuButton(el).getAttribute("aria-busy")).toBe("true");
    expect(menuButton(el).textContent).toBe("↓ PDF…");
    await act(async () => fail(new Error("this server cannot write PDF: the typesetter (Typst) is not installed")));
    expect(onError).toHaveBeenCalledWith("this server cannot write PDF: the typesetter (Typst) is not installed");
    expect(menuButton(el).disabled).toBe(false);
    expect(menuButton(el).textContent).toBe("↓ download ▾");
    expect(saved).toEqual([]);
  });
});
