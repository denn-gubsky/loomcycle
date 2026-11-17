import { useCallback, useEffect, useId, useRef, useState, type KeyboardEvent } from "react";
import type { BrowseScope, DocScope } from "../types";
import { useExplorerData, type ExplorerDataLayer } from "../lib/dataLayer";
import { fileStem, saveBlob, type RenderFormat } from "../lib/download";

/** One entry of the document's download menu. */
export type DownloadKind = "pdf" | "docx" | "odt" | "epub" | "html" | "md" | "canvas";

interface DownloadEntry {
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

/** The entries the menu offers for a data layer: the rendered formats only when
 *  it can fetch a rendered file. */
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

export interface DownloadMenuProps {
  documentId: string;
  scope: DocScope;
  browse?: BrowseScope;
  /** The document's title, for a file name the server does not supply. */
  title?: string;
  onError: (message: string) => void;
}

// DownloadMenu is the document toolbar's one "download" control: a menu button
// listing every file the document can be downloaded as.
//
// Follows the ARIA menu-button pattern: Enter / Space / ArrowDown open onto the
// first item (ArrowUp onto the last), the arrows wrap, Home / End jump, Escape
// closes and returns focus to the button, Tab and an outside click close it.
// While a download runs (a PDF render can take seconds) the button is disabled
// and names what it is fetching.
export default function DownloadMenu({ documentId, scope, browse, title, onError }: DownloadMenuProps) {
  const data = useExplorerData();
  const entries = downloadEntries(data);
  // null = closed; otherwise the index to focus on opening.
  const [openAt, setOpenAt] = useState<number | null>(null);
  const open = openAt !== null;
  const [busy, setBusy] = useState<DownloadKind | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const itemRefs = useRef<(HTMLButtonElement | null)[]>([]);
  const menuId = useId();

  useEffect(() => {
    if (openAt !== null) itemRefs.current[openAt]?.focus();
  }, [openAt]);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpenAt(null);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  const choose = useCallback(
    async (kind: DownloadKind) => {
      setOpenAt(null);
      buttonRef.current?.focus();
      setBusy(kind);
      try {
        await runDownload(data, kind, documentId, scope, browse, title);
      } catch (e) {
        onError(e instanceof Error ? e.message : String(e));
      } finally {
        setBusy(null);
      }
    },
    [data, documentId, scope, browse, title, onError],
  );

  const focusItem = (i: number) => itemRefs.current[(i + entries.length) % entries.length]?.focus();

  const onItemKeyDown = (e: KeyboardEvent<HTMLButtonElement>, i: number) => {
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        focusItem(i + 1);
        break;
      case "ArrowUp":
        e.preventDefault();
        focusItem(i - 1);
        break;
      case "Home":
        e.preventDefault();
        focusItem(0);
        break;
      case "End":
        e.preventDefault();
        focusItem(entries.length - 1);
        break;
      case "Escape":
        e.preventDefault();
        setOpenAt(null);
        buttonRef.current?.focus();
        break;
      case "Tab":
        setOpenAt(null);
        break;
    }
  };

  const busyLabel = busy ? ENTRIES.find((e) => e.kind === busy)?.label : undefined;

  return (
    <div className="doc-download-menu" ref={rootRef}>
      <button
        ref={buttonRef}
        type="button"
        className={open ? "active" : ""}
        title={busyLabel ? `Preparing ${busyLabel}…` : "Download the whole document as a file"}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        aria-busy={busy !== null}
        disabled={busy !== null}
        onClick={() => setOpenAt(open ? null : 0)}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown") {
            e.preventDefault();
            setOpenAt(0);
          } else if (e.key === "ArrowUp") {
            e.preventDefault();
            setOpenAt(entries.length - 1);
          } else if (e.key === "Escape" && open) {
            e.preventDefault();
            setOpenAt(null);
          }
        }}
      >
        {busyLabel ? `↓ ${busyLabel}…` : "↓ download ▾"}
      </button>
      {open && (
        <div className="doc-download-list" role="menu" id={menuId} aria-label="Download as">
          {entries.map((entry, i) => (
            <button
              key={entry.kind}
              ref={(el) => {
                itemRefs.current[i] = el;
              }}
              type="button"
              role="menuitem"
              tabIndex={-1}
              title={entry.title}
              data-kind={entry.kind}
              onClick={() => void choose(entry.kind)}
              onKeyDown={(e) => onItemKeyDown(e, i)}
            >
              {entry.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
