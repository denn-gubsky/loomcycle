import type { TranscriptionEventInfo } from "../api";

// transcriptionLine is the one line a `transcription` event renders as, in the
// run page's cards and the terminal alike: which file was read, how much of it,
// by what, for how much, and how it ended. A dry run is an estimate and says
// so; the run, the output and the tokens stay in the expanded view.
export function transcriptionLine(t: TranscriptionEventInfo | undefined): string {
  if (!t) return "transcription";
  const what = t.purpose === "stt" ? "speech-to-text" : t.purpose === "ocr" ? "OCR" : t.purpose || "transcription";
  const parts = [`${what} ${baseName(t.ref)}`];
  const units = unitsOf(t);
  if (units) parts.push(units);
  const by = t.model || t.engine;
  if (by) parts.push(by);
  if (t.cost !== undefined && t.cost !== null) parts.push(`${t.dry_run ? "~" : ""}${money(t.cost, t.currency)}`);
  parts.push(outcome(t));
  return `⟲ ${parts.join(" · ")}`;
}

// transcriptionDetail is the expanded record: every field, an unpriced pass
// named as such rather than left blank.
export function transcriptionDetail(t: TranscriptionEventInfo): string {
  const lines = [`ref: ${t.ref}`];
  if (t.out) lines.push(`out: ${t.out}`);
  lines.push(`purpose: ${t.purpose}`, `engine: ${t.engine}`);
  if (t.provider || t.model) lines.push(`model: ${[t.provider, t.model].filter(Boolean).join("/")}`);
  const units = unitsOf(t);
  if (units) lines.push(`units: ${units}`);
  if (t.input_tokens || t.output_tokens) lines.push(`tokens: ${t.input_tokens ?? 0} in / ${t.output_tokens ?? 0} out`);
  lines.push(`cost: ${t.cost !== undefined && t.cost !== null ? money(t.cost, t.currency) : "unpriced"}`);
  lines.push(`status: ${t.status}${t.dry_run ? " (dry run — an estimate, nothing was spent)" : ""}`);
  if (t.run_id) lines.push(`run_id: ${t.run_id}`);
  return lines.join("\n");
}

function outcome(t: TranscriptionEventInfo): string {
  const status = t.status || "?";
  if (!t.dry_run) return status;
  return status === "ok" ? "estimate" : `estimate · ${status}`;
}

function unitsOf(t: TranscriptionEventInfo): string {
  if (t.pages) return `${t.pages} page${t.pages === 1 ? "" : "s"}`;
  if (t.seconds) return t.seconds >= 60 ? `${(t.seconds / 60).toFixed(1)} min` : `${t.seconds.toFixed(1)} s`;
  return "";
}

// money keeps a sub-cent cost readable: two significant digits below a cent,
// two decimals above.
function money(cost: number, currency?: string): string {
  const n = cost === 0 ? "0" : cost < 0.01 ? cost.toPrecision(2) : cost.toFixed(2);
  return !currency || currency === "USD" ? `$${n}` : `${n} ${currency}`;
}

function baseName(ref: string): string {
  if (!ref) return "?";
  const i = Math.max(ref.lastIndexOf("/"), ref.lastIndexOf(":"));
  return ref.slice(i + 1) || ref;
}
