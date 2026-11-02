// The Review page's logic, kept out of the component so each rule is testable
// on its own: the filters a URL carries, the groups a summary makes and the keys
// a reviewer presses. The dry run → confirm → rule sequence a bulk ruling goes
// through is @loomcycle/explorer's runRuling, shared with the document viewer.

import type { RulingOutcome, UndoOutcome } from "@loomcycle/explorer";
import type { ReviewFilter, ReviewGroupCount, ReviewScope, ReviewStatement, RulingRecord } from "../api";

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
  "judge_audit",
  "method_exact",
  "kind_exact",
  "from_path_prefix",
] as const;

// Every query parameter the filter owns. searchFromFilters rewrites exactly
// these and leaves the rest of the URL (a tenant focus, the scope) alone.
export const FILTER_PARAMS: readonly string[] = [
  ...TEXT_KEYS,
  "kinds",
  "min_confidence",
  "has_judge_verdict",
  "from_unfiled",
];

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
  if (p.get("from_unfiled") === "true") f.from_unfiled = true;
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
  if (f.from_unfiled) p.set("from_unfiled", "true");
  return p;
}

// reviewScopeFromSearch is the document scope a review link names: the tenant
// (default) or a user scope (whose: reviewScopeId).
export function reviewScopeFromSearch(p: URLSearchParams): ReviewScope {
  return p.get("scope") === "user" ? "user" : "tenant";
}

// reviewScopeId is the subject whose user documents a user-scope review acts
// on: the subject picker's choice when it names someone other than the viewer,
// else none (the viewer's own). The tenant scope names nobody; the runtime
// refuses scope_id there, and refuses a member naming another subject.
export function reviewScopeId(scope: ReviewScope, picked: string | undefined, self: string | undefined): string | undefined {
  if (scope !== "user" || !picked || picked === self) return undefined;
  return picked;
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
// server's order). Each group's filter is the one the server sends with it,
// which selects exactly the group's count (its exact kind, method, path prefix
// and verdict, narrowed from the summary's own filter). A group without one —
// an older server — gets no group action: nothing on this side can rebuild it
// without risking a wider selection.
//
// The server's filter carries the time bounds as nanosecond numbers, which a
// JS number cannot hold exactly; the page's own bounds (text, as the summary
// was asked with them) are put back so a ruling's filter is the summary's.
export function groupsFromSummary(summary: ReviewGroupCount[] | null | undefined, base: ReviewFilter): ReviewGroup[] {
  return (summary ?? []).map((g) => ({
    key: groupKey(g),
    kind: g.kind,
    method: g.method,
    fromPathPrefix: g.from_path_prefix,
    judgeVerdict: g.judge_verdict,
    count: g.count,
    filter: g.filter ? withTimeBounds(g.filter, base) : undefined,
  }));
}

function withTimeBounds(f: ReviewFilter, base: ReviewFilter): ReviewFilter {
  const out: ReviewFilter = { ...f };
  delete out.created_before;
  delete out.created_after;
  if (base.created_before) out.created_before = base.created_before;
  if (base.created_after) out.created_after = base.created_after;
  return out;
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

// auditOf reads a judge's acceptance's audit state (attributes.judge.audited):
// pending while it waits for a person, confirmed or overturned once settled.
export function auditOf(s: ReviewStatement): "pending" | "confirmed" | "overturned" | undefined {
  const j = s.attributes?.judge;
  if (!j || typeof j !== "object") return undefined;
  const a = (j as Record<string, unknown>).audited;
  return a === "pending" || a === "confirmed" || a === "overturned" ? a : undefined;
}

// auditFilter is the queue filter for the judge's acceptances in one audit
// state. The queue lists proposals by default, and an acceptance is asserted
// (a confirmed one too, now a person's) or, overturned, rejected.
export function auditFilter(state: "pending" | "confirmed" | "overturned"): ReviewFilter {
  return { status: state === "overturned" ? "rejected" : "asserted", judge_audit: state };
}

// percent renders a 0..1 rate, or a dash when there is none yet.
export function percent(rate: number | undefined): string {
  return typeof rate === "number" ? `${Math.round(rate * 100)}%` : "—";
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

// ---- earlier rulings --------------------------------------------------------

// canUndo says whether a listed ruling can be undone from the list: an accept
// or a reject that ruled something and has not been undone. A revert is not
// itself undone (the runtime refuses it), and a reverted ruling has nothing
// left to revert.
export function canUndo(r: Pick<RulingRecord, "action" | "reverted" | "ruled">): boolean {
  return (r.action === "accept" || r.action === "reject") && !r.reverted && r.ruled > 0;
}

// rulingSelection says what a listed ruling selected, in a few words.
export function rulingSelection(r: Pick<RulingRecord, "action" | "selector">): string {
  const sel = r.selector ?? {};
  if (r.action === "revert") return sel.reverts ? `undid ${sel.reverts.slice(0, 8)}` : "undid a ruling";
  if (sel.ids_count) return `${n(sel.ids_count)} picked by hand`;
  const f = sel.filter ?? {};
  const parts = Object.entries(f)
    .filter(([, v]) => v !== undefined && v !== "" && !(Array.isArray(v) && v.length === 0))
    .map(([k, v]) => `${k}=${Array.isArray(v) ? v.join(",") : String(v)}`);
  return parts.length > 0 ? parts.join(" ") : "all proposals";
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
