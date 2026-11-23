import type { BrowseScope, DocScope } from "../types";
import type { ExplorerDataLayer } from "./dataLayer";
import { fileStem, saveBlob, type RenderFormat } from "./download";

// The files a whole document downloads as (RFC BS .md / .canvas, RFC GR
// rendered files). The document header's "more" menu lists them.

/** One download of the document. */
export type DownloadKind = "pdf" | "docx" | "odt" | "epub" | "html" | "md" | "canvas";

export interface DownloadEntry {
  kind: DownloadKind;
  label: string;
  title: string;
  // rendered: the runtime renders the file (RFC GR) — needs documentExportFile.
  rendered: boolean;
}

const ENTRIES: DownloadEntry[] = [
  { kind: "pdf", label: "PDF", title: "Render the whole document as a PDF file", rendered: true },
  { kind: "docx", label: "Word (.docx)", title: "Render the whole document as a Word document", rendered: true },
  { kind: "odt", label: "OpenDocument (.odt)", title: "Render the whole document as an OpenDocument text file", rendered: true },
  { kind: "epub", label: "E-book (.epub)", title: "Render the whole document as an EPUB e-book", rendered: true },
  { kind: "html", label: "HTML", title: "Render the whole document as a web page", rendered: true },
  {
    kind: "md",
    label: "Markdown (.md)",
    title: "Download the whole document as Markdown, with the metadata comments that let it be imported back",
    rendered: false,
  },
  {
    kind: "canvas",
    label: "Canvas (.canvas)",
    title: "Download the whole document as a node/edge canvas graph (.canvas)",
    rendered: false,
  },
];

/** The downloads offered for a data layer: the rendered formats only when it
 *  can fetch a rendered file. */
export function downloadEntries(data: Pick<ExplorerDataLayer, "documentExportFile">): DownloadEntry[] {
  return ENTRIES.filter((e) => !e.rendered || !!data.documentExportFile);
}

/** Fetch one download through the data layer and hand it to the browser.
 *  .md and .canvas keep their own exports (the round-trippable Markdown with
 *  metadata comments, the canvas graph); the rest are rendered by the runtime. */
export async function runDownload(
  data: ExplorerDataLayer,
  kind: DownloadKind,
  documentId: string,
  scope: DocScope,
  browse: BrowseScope | undefined,
  title: string | undefined,
  save: (blob: Blob, filename: string) => void = saveBlob,
): Promise<void> {
  if (kind === "md") {
    const r = await data.documentExportMd(documentId, scope, true, browse);
    save(new Blob([r.markdown], { type: "text/markdown" }), fileStem(r.title) + ".md");
    return;
  }
  if (kind === "canvas") {
    const r = await data.documentExportCanvas(documentId, scope, browse);
    save(new Blob([JSON.stringify(r.canvas, null, 2)], { type: "application/json" }), fileStem(title) + ".canvas");
    return;
  }
  if (!data.documentExportFile) throw new Error("this connection cannot download rendered files");
  const format: RenderFormat = kind;
  const r = await data.documentExportFile(documentId, scope, format, browse);
  // The server names the file; without a name, the title and the format.
  save(r.blob, r.filename || fileStem(title) + "." + format);
}
