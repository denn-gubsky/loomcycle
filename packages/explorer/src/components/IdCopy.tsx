import { useState, type MouseEvent } from "react";
import { copyToClipboard } from "../lib/clipboard";

export interface IdCopyProps {
  // value is the exact id copied to the clipboard (a document id or a chunk id).
  value: string;
  // label is a short prefix shown before the id in full mode (e.g. "doc", "chunk").
  label?: string;
  // compact renders the copy glyph ONLY (no visible id text) for dense rows like
  // the chunk tree; the id still rides in the button's title/aria-label.
  compact?: boolean;
  // title overrides the button tooltip (default: `Copy <label>: <value>`).
  title?: string;
}

// IdCopy surfaces an id with a click-to-copy button. It exists so an operator
// wiring a document or a specific chunk into an external workflow — n8n over the
// MCP/HTTP API — can grab the EXACT id the API expects, which the viewer
// otherwise only uses internally as React keys. The clipboard always receives
// the full id even when the on-screen value is ellipsized.
export default function IdCopy({ value, label, compact, title }: IdCopyProps) {
  const [state, setState] = useState<"idle" | "copied" | "error">("idle");

  const copy = async (e: MouseEvent) => {
    // Never let the copy click bubble to the row/tile's own handler (which would
    // select the chunk or toggle the tree).
    e.stopPropagation();
    e.preventDefault();
    const ok = await copyToClipboard(value);
    setState(ok ? "copied" : "error");
    window.setTimeout(() => setState("idle"), ok ? 1200 : 1500);
  };

  const glyph = state === "copied" ? "✓" : state === "error" ? "✗" : "⎘";
  const btnTitle = title ?? `Copy ${label ?? "id"}: ${value}`;
  const button = (
    <button
      type="button"
      className={`id-copy-btn id-copy-${state}`}
      onClick={copy}
      title={btnTitle}
      aria-label={btnTitle}
    >
      {glyph}
    </button>
  );

  if (compact) return button;

  return (
    <span className="id-copy">
      {label && <span className="id-copy-label">{label}</span>}
      <code className="id-copy-value" title={value}>
        {value}
      </code>
      {button}
    </span>
  );
}
