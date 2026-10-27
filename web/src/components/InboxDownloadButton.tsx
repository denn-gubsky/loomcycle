import { useState } from "react";
import { downloadInboxFile } from "../api";
import { inboxDownloadPath } from "../lib/fileCaptured";

// InboxDownloadButton saves a file a run's tool returned — one a
// `file_captured` event names — from the run tree's inbox. The inbox is
// deleted when the run ends, so the refusal is shown in place.
export function InboxDownloadButton({ runId, fileRef }: { runId: string; fileRef: string }) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const name = fileRef.split("/").pop() || "file";
  return (
    <span className="inbox-download" onClick={(e) => e.stopPropagation()}>
      <button
        type="button"
        className="ghost-btn"
        disabled={busy || !runId}
        title="Download this file from the run's inbox"
        onClick={async () => {
          setBusy(true);
          setErr("");
          try {
            await downloadInboxFile(inboxDownloadPath(runId, fileRef), name);
          } catch (e) {
            setErr(e instanceof Error ? e.message : String(e));
          } finally {
            setBusy(false);
          }
        }}
      >
        {busy ? "downloading…" : "download"}
      </button>
      {err && <span className="err"> {err}</span>}
    </span>
  );
}
