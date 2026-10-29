import type { EdgeEvidence, StatementFields } from "../types";

// Display helpers for the RFC FN statement envelope an edge carries. Pure, so the
// rules for what a reference row shows are testable without rendering it.

// Methods that are the ordinary way a link comes to exist in a document — typed
// by hand or written as a [[wikilink]]. Only the others are worth a label.
const ORDINARY_METHODS = new Set(["manual", "wikilink"]);

// EVIDENCE_PREVIEW is how many quotes a reference row shows; a listing carries at
// most three anyway.
export const EVIDENCE_PREVIEW = 3;
const QUOTE_MAX = 280;

// statusLabel is the badge for a statement that does not simply hold: proposed or
// rejected. An asserted statement — or an older server's edge with no status —
// gets none.
export function statusLabel(s: StatementFields): string | undefined {
  return s.status && s.status !== "asserted" ? s.status : undefined;
}

// methodLabel names how the statement was made, when that is not the ordinary
// manual / wikilink way (an import, a sync, a canvas, an agent at runtime…).
export function methodLabel(s: StatementFields): string | undefined {
  return s.method && !ORDINARY_METHODS.has(s.method) ? s.method : undefined;
}

// isProposed: the statement waits for an operator's ruling.
export function isProposed(s: StatementFields): boolean {
  return s.status === "proposed" && !!s.id;
}

// formatNsDate renders a Unix-nanosecond timestamp as a UTC calendar date.
export function formatNsDate(ns: number): string {
  return new Date(Math.floor(ns / 1e6)).toISOString().slice(0, 10);
}

// validityLabel describes when the related thing holds, from valid_at / invalid_at,
// or undefined when the statement carries neither.
export function validityLabel(s: StatementFields): string | undefined {
  const from = s.valid_at != null ? formatNsDate(s.valid_at) : undefined;
  const until = s.invalid_at != null ? formatNsDate(s.invalid_at) : undefined;
  if (from && until) return `valid ${from} → ${until}`;
  if (from) return `valid from ${from}`;
  if (until) return `valid until ${until}`;
  return undefined;
}

// verdictHint summarizes a ruling — an operator's or a judge model's — for a
// tooltip: who judged, when, and why. undefined when nobody has judged it.
export function verdictHint(s: StatementFields): string | undefined {
  if (s.judged_at == null) return undefined;
  const head = `judged by ${s.judged_by || "a judge"} on ${formatNsDate(s.judged_at)}`;
  return s.judge_reason ? `${head}: ${s.judge_reason}` : head;
}

// evidencePreview picks the quotes a row shows and how many it leaves out.
// `more` counts against evidence_count (the total), not the rows the listing
// happened to carry.
export function evidencePreview(
  s: StatementFields,
  max = EVIDENCE_PREVIEW,
): { rows: EdgeEvidence[]; more: number } {
  const rows = (s.evidence ?? []).slice(0, max);
  const total = Math.max(s.evidence_count ?? 0, s.evidence?.length ?? 0);
  return { rows, more: Math.max(0, total - rows.length) };
}

// clipQuote shortens a long quote for the row; the full text stays the element's
// tooltip.
export function clipQuote(quote: string, max = QUOTE_MAX): string {
  const q = quote.replace(/\s+/g, " ").trim();
  return q.length > max ? q.slice(0, max - 1) + "…" : q;
}
