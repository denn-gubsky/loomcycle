// The Review page's logic, kept out of the component so each rule is testable
// on its own: the filters a URL carries, the groups a summary makes and the keys
// a reviewer presses. The dry run → confirm → rule sequence a bulk ruling goes
// through is @loomcycle/explorer's runRuling, shared with the document viewer.

import type { RulingOutcome, UndoOutcome } from "@loomcycle/explorer";
import type { ReviewFilter, ReviewGroupCount, ReviewScope, ReviewStatement } from "../api";

// ---- filters ↔ URL ---------------------------------------------------------

// The filter fields a URL carries as plain text, in the order they are written.
const TEXT_KEYS = [
  "status",
  "kind",
  "method",
  "actor",
  "document_id",
  "under_path",
  "from_document_id",
  "to_document_id",
  "from_under_path",
  "to_under_path",
  "created_after",
  "created_before",
  "judge_verdict",
] as const;

// Every query parameter the filter owns. searchFromFilters rewrites exactly
// these and leaves the rest of the URL (a tenant focus, the scope) alone.
export const FILTER_PARAMS: readonly string[] = [...TEXT_KEYS, "kinds", "min_confidence", "has_judge_verdict"];

// filtersFromSearch reads the queue filter off a URL. A malformed number or
// boolean is dropped rather than guessed at: a review link that lost a bound
// shows more rows, and the dry run still shows what a ruling would take.
export function filtersFromSearch(p: URLSearchParams): ReviewFilter {
  const f: ReviewFilter = {};
  for (const k of TEXT_KEYS) {
    const v = p.get(k)?.trim();
    if (v) f[k] = v;
  }
  const kinds = (p.get("kinds") ?? "")
    .split(",")
    .map((k) => k.trim())
    .filter(Boolean);
  if (kinds.length > 0) f.kinds = kinds;
  const conf = p.get("min_confidence");
  if (conf !== null && conf.trim() !== "") {
    const n = Number(conf);
    if (Number.isFinite(n) && n >= 0 && n <= 1) f.min_confidence = n;
  }
  const judged = p.get("has_judge_verdict");
  if (judged === "true" || judged === "false") f.has_judge_verdict = judged === "true";
  return f;
}

// searchFromFilters writes the filter into a URL's query, replacing whatever
// filter it carried and keeping every other parameter.
export function searchFromFilters(f: ReviewFilter, base?: URLSearchParams): URLSearchParams {
  const p = new URLSearchParams(base);
  for (const k of FILTER_PARAMS) p.delete(k);
  for (const k of TEXT_KEYS) {
    const v = f[k];
    if (v) p.set(k, v);
  }
  if (f.kinds && f.kinds.length > 0) p.set("kinds", f.kinds.join(","));
  if (f.min_confidence !== undefined) p.set("min_confidence", String(f.min_confidence));
  if (f.has_judge_verdict !== undefined) p.set("has_judge_verdict", String(f.has_judge_verdict));
  return p;
}

// reviewScopeFromSearch is the document scope a review link names: the tenant
// (default) or the viewer's own user scope.
export function reviewScopeFromSearch(p: URLSearchParams): ReviewScope {
  return p.get("scope") === "user" ? "user" : "tenant";
}

// ---- groups ----------------------------------------------------------------

export interface ReviewGroup {
  key: string;
  kind: string;
  method: string;
  fromPathPrefix: string;
  judgeVerdict: string;
  count: number;
  // The filter that selects this group, or undefined when the queue's filters
  // cannot name it without also taking statements outside it (see groupFilter).
  filter?: ReviewFilter;
}

export function groupKey(g: ReviewGroupCount): string {
  return [g.kind, g.method, g.from_path_prefix, g.judge_verdict].join("\u0000");
}

// groupsFromSummary turns the queue's summary into groups, largest first (the
// server's order), each with the filter that selects it under `base`.
export function groupsFromSummary(summary: ReviewGroupCount[] | null | undefined, base: ReviewFilter): ReviewGroup[] {
  return (summary ?? []).map((g) => ({
    key: groupKey(g),
    kind: g.kind,
    method: g.method,
    fromPathPrefix: g.from_path_prefix,
    judgeVerdict: g.judge_verdict,
    count: g.count,
    filter: groupFilter(g, base),
  }));
}

// narrower is the more specific of two prefixes, when one contains the other;
// undefined when they are unrelated and no single prefix selects both.
function narrower(a: string | undefined, b: string, sep: string): string | undefined {
  if (!a) return b;
  if (b === a || b.startsWith(a.endsWith(sep) ? a : a + sep)) return b;
  if (a.startsWith(b.endsWith(sep) ? b : b + sep)) return a;
  return undefined;
}

// groupFilter is the queue filter for one summary group, narrowed from the
// page's own filter. It never widens what the page shows: where the group and
// the page disagree it is undefined, and the group is ruled row by row.
//
// The filters can only approximate a group, and the dry run is what shows the
// difference before anything is ruled:
//   - method is a prefix, so `rule:cite` also takes `rule:citation`;
//   - a group's path prefix is its from-document's first Path name, and a
//     document filed under two names matches from_under_path for either.
// A group the filters cannot name at all — no method, no kind, or a
// from-document filed nowhere — has no filter: an empty value is "any", which
// would rule the whole queue.
export function groupFilter(g: ReviewGroupCount, base: ReviewFilter): ReviewFilter | undefined {
  if (!g.kind || !g.method || !g.from_path_prefix) return undefined;
  if (base.kind && base.kind !== g.kind) return undefined;
  if (base.judge_verdict && base.judge_verdict !== g.judge_verdict) return undefined;
  const method = base.method ? (g.method.startsWith(base.method) ? g.method : undefined) : g.method;
  const path = narrower(base.from_under_path, g.from_path_prefix, "/");
  if (method === undefined || path === undefined) return undefined;
  const f: ReviewFilter = { ...base, kind: g.kind, method, from_under_path: path };
  if (g.judge_verdict) {
    f.judge_verdict = g.judge_verdict;
    delete f.has_judge_verdict;
  } else {
    if (base.has_judge_verdict === true) return undefined;
    f.has_judge_verdict = false;
  }
  return f;
}

export function groupLabel(g: Pick<ReviewGroup, "kind" | "method" | "fromPathPrefix" | "judgeVerdict">): string {
  const parts = [g.kind || "(no kind)", g.method || "(no method)", g.fromPathPrefix || "(not filed)"];
  if (g.judgeVerdict) parts.push(`judge: ${g.judgeVerdict}`);
  return parts.join(" · ");
}

// judgeOf is the judge's verdict recorded on a statement (attributes.judge),
// or undefined when none is.
export function judgeOf(s: ReviewStatement): { verdict: string; confidence?: number; rationale?: string } | undefined {
  const j = s.attributes?.judge;
  if (!j || typeof j !== "object") return undefined;
  const o = j as Record<string, unknown>;
  if (typeof o.verdict !== "string" || o.verdict === "") return undefined;
  return {
    verdict: o.verdict,
    ...(typeof o.confidence === "number" ? { confidence: o.confidence } : {}),
    ...(typeof o.rationale === "string" && o.rationale ? { rationale: o.rationale } : {}),
  };
}

// ---- keyboard --------------------------------------------------------------

export type ReviewKeyAction =
  | { type: "move"; index: number }
  | { type: "accept"; index: number }
  | { type: "reject"; index: number }
  | { type: "expand"; index: number };

export interface ReviewKeyEvent {
  key: string;
  ctrlKey?: boolean;
  metaKey?: boolean;
  altKey?: boolean;
  // The key went to a text field (a filter, a reason): it is typing, not review.
  editing?: boolean;
}

// reviewKey maps one key press to what it does to the focused row: j/k move,
// a accepts, r rejects (the page asks for the reason), e expands the evidence.
// focus -1 is "no row yet": j/k land on the first row, a/r/e do nothing.
export function reviewKey(e: ReviewKeyEvent, focus: number, rows: number): ReviewKeyAction | null {
  if (e.editing || e.ctrlKey || e.metaKey || e.altKey || rows === 0) return null;
  switch (e.key) {
    case "j":
      return { type: "move", index: focus < 0 ? 0 : Math.min(focus + 1, rows - 1) };
    case "k":
      return { type: "move", index: focus < 0 ? 0 : Math.max(focus - 1, 0) };
    case "a":
    case "r":
    case "e":
      if (focus < 0 || focus >= rows) return null;
      return { type: e.key === "a" ? "accept" : e.key === "r" ? "reject" : "expand", index: focus };
  }
  return null;
}

// ---- words -----------------------------------------------------------------

const n = (x: number) => x.toLocaleString("en-US");
const statements = (x: number) => `${n(x)} statement${x === 1 ? "" : "s"}`;

export function rulingSummary(action: "accept" | "reject", o: RulingOutcome): string {
  const parts = [`${action === "accept" ? "Accepted" : "Rejected"} ${statements(o.ruled)}`];
  if (o.failed > 0) parts.push(`${n(o.failed)} did not take the ruling`);
  if (o.skipped > 0) parts.push(`${n(o.skipped)} skipped: not yours to rule`);
  if (o.cancelled) parts.push("stopped before the rest");
  return parts.join("; ") + ".";
}

export function undoSummary(u: UndoOutcome): string {
  const parts = [`${statements(u.reverted)} came back to review`];
  if (u.changed > 0) parts.push(`${n(u.changed)} changed since and ${u.changed === 1 ? "was" : "were"} left as ruled`);
  if (u.errors.length > 0) parts.push(`${n(u.errors.length)} ruling${u.errors.length === 1 ? "" : "s"} could not be undone`);
  return parts.join("; ") + ".";
}
