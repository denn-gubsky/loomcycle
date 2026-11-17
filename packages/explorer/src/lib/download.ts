// Helpers for handing a file to the browser: the document downloads (RFC BS
// .md / .canvas, RFC GR rendered files) share them.

/** The file formats the runtime renders a document into (RFC GR). */
export type RenderFormat = "pdf" | "docx" | "odt" | "epub" | "html" | "md";

/** Options of a rendered export — `GET /v1/_document/export/{id}`. Unset
 *  options are left to the server's defaults. */
export interface DocumentExportOptions {
  /** Render only the subtree under this chunk. */
  chunk?: string;
  toc?: boolean;
  titlePage?: boolean;
  pageSize?: "a4" | "letter";
  language?: string;
  /** false leaves the provenance note out (the server puts it in by default). */
  provenance?: boolean;
}

/** The file name a Content-Disposition header names: the RFC 8187
 *  `filename*=UTF-8''…` form first (it carries non-ASCII names), then the plain
 *  `filename="…"` / `filename=token` fallback. Undefined when it names none, or
 *  only an undecodable `filename*` with no fallback. Any directory part is
 *  dropped — the name is used as a download name, never a path. */
export function filenameFromContentDisposition(header: string | null | undefined): string | undefined {
  if (!header) return undefined;
  const star = /filename\*\s*=\s*([^']*)'[^']*'([^;]+)/i.exec(header);
  if (star && /^utf-8$/i.test(star[1]!.trim())) {
    try {
      const name = baseName(decodeURIComponent(star[2]!.trim()));
      if (name) return name;
    } catch {
      // malformed percent-encoding: fall back to the plain parameter
    }
  }
  const quoted = /(?:^|;)\s*filename\s*=\s*"((?:[^"\\]|\\.)*)"/i.exec(header);
  if (quoted) return baseName(quoted[1]!.replace(/\\(.)/g, "$1")) || undefined;
  const token = /(?:^|;)\s*filename\s*=\s*([^;\s"]+)/i.exec(header);
  if (token) return baseName(token[1]!) || undefined;
  return undefined;
}

function baseName(name: string): string {
  return name.slice(Math.max(name.lastIndexOf("/"), name.lastIndexOf("\\")) + 1).trim();
}

/** The message of a refused request: the `error` field of the runtime's
 *  `{"code","error"}` body, else the status line (e.g. a proxy's HTML page). */
export function errorMessageFromBody(text: string, status: number, statusText = ""): string {
  try {
    const o = JSON.parse(text) as { error?: unknown };
    if (typeof o.error === "string" && o.error) return o.error;
  } catch {
    // not JSON
  }
  return `${status} ${statusText}`.trim();
}

/** A title made safe as a file-name stem — the rule the .md download has
 *  always used. */
export function fileStem(title: string | undefined): string {
  return (title || "document").replace(/[^\w.-]+/g, "_");
}

/** Hand a Blob to the browser as a download named `filename`. The object URL
 *  is revoked on the next tick, not at once: some browsers start reading the
 *  blob only after click() returns. */
export function saveBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}
