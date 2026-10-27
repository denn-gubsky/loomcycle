import type { FileCapturedEventInfo } from "../api";

// fileCapturedLine is the one line a `file_captured` event renders as, in the
// run page's cards and the terminal alike: what the runtime kept in place of
// the bytes a tool returned — the file's reference, type, size and where it
// came from. The sha256 stays in the expanded view.
export function fileCapturedLine(f: FileCapturedEventInfo | undefined): string {
  if (!f) return "file kept";
  const parts = [f.ref || "?", f.media_type || "?", humanBytes(f.size ?? 0)];
  if (f.origin) parts.push(`from ${f.origin}`);
  return `⇩ ${parts.join(" · ")}`;
}

// humanBytes mirrors the runtime's stub (inbox.humanBytes): binary units, one
// decimal past a KiB.
export function humanBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}

// inboxDownloadPath is the route that serves one inbox file's bytes:
// GET /v1/runs/{run_id}/inbox/{path}. ref is the event's
// `volume:inbox:<path>`; each segment is encoded, the slashes kept. Any run of
// the tree reaches the tree's inbox, so the run that returned the file serves.
export function inboxDownloadPath(runId: string, ref: string): string {
  const rel = ref.replace(/^volume:inbox:/, "");
  return `/v1/runs/${encodeURIComponent(runId)}/inbox/${rel.split("/").map(encodeURIComponent).join("/")}`;
}
