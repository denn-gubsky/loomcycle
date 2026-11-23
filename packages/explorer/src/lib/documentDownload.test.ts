import { describe, expect, it, vi } from "vitest";
import type { ExplorerDataLayer } from "./dataLayer";
import { downloadEntries, runDownload } from "./documentDownload";

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

describe("downloadEntries", () => {
  it("lists the seven formats when the data layer can fetch rendered files", () => {
    expect(downloadEntries(fakeData(true).data).map((e) => e.label)).toEqual([
      "PDF",
      "Word (.docx)",
      "OpenDocument (.odt)",
      "E-book (.epub)",
      "HTML",
      "Markdown (.md)",
      "Canvas (.canvas)",
    ]);
  });

  it("offers only .md and .canvas without rendered files", () => {
    expect(downloadEntries(fakeData(false).data).map((e) => e.kind)).toEqual(["md", "canvas"]);
  });
});

describe("runDownload", () => {
  it("renders PDF, Word, ODT, EPUB and HTML through documentExportFile, saved under the server's name", async () => {
    const f = fakeData(true);
    const saved: string[] = [];
    for (const kind of ["pdf", "docx", "odt", "epub", "html"] as const) {
      await runDownload(f.data, kind, "d1", "tenant", BROWSE, "My doc", (_b, n) => saved.push(n));
      expect(f.documentExportFile).toHaveBeenLastCalledWith("d1", "tenant", kind, BROWSE);
    }
    expect(saved).toEqual(Array(5).fill("Server name.pdf"));
    expect(f.documentExportMd).not.toHaveBeenCalled();
  });

  it("names a rendered file by the title when the server names none", async () => {
    const f = fakeData(true);
    f.documentExportFile.mockResolvedValueOnce({ blob: new Blob(["x"]), filename: "" });
    const saved: string[] = [];
    await runDownload(f.data, "docx", "d1", "tenant", BROWSE, "My doc", (_b, n) => saved.push(n));
    expect(saved).toEqual(["My_doc.docx"]);
  });

  it("keeps .md as the round-trippable export with metadata, and .canvas as the canvas graph", async () => {
    const f = fakeData(true);
    const saved: string[] = [];
    await runDownload(f.data, "md", "d1", "tenant", BROWSE, "My doc", (_b, n) => saved.push(n));
    await runDownload(f.data, "canvas", "d1", "tenant", BROWSE, "My doc", (_b, n) => saved.push(n));
    expect(f.documentExportMd).toHaveBeenCalledWith("d1", "tenant", true, BROWSE);
    expect(f.documentExportCanvas).toHaveBeenCalledWith("d1", "tenant", BROWSE);
    expect(f.documentExportFile).not.toHaveBeenCalled();
    expect(saved).toEqual(["My_doc.md", "My_doc.canvas"]);
  });

  it("refuses a rendered format when the data layer cannot fetch one", async () => {
    await expect(runDownload(fakeData(false).data, "pdf", "d1", "tenant", BROWSE, "x")).rejects.toThrow(
      /cannot download rendered files/,
    );
  });
});
