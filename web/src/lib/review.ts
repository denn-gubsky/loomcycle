// The Review page's logic, kept out of the component so each rule is testable
// on its own: the filters a URL carries, the groups a summary makes, the keys
// a reviewer presses, and the dry run → confirm → rule sequence a bulk ruling
// goes through.

import type {
  ReviewFilter,
  ReviewGroupCount,
  ReviewScope,
  ReviewStatement,
  RevertRulingResponse,
  RuleStatementsRequest,
  RuleStatementsResponse,
  RulingItem,
} from "../api";
import { ApiError } from "./apiError";

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

// ---- a ruling --------------------------------------------------------------

// The most statements one call rules (the runtime's cap, RFC FS D6).
export const RULING_CHUNK = 1000;
// A queue that keeps changing under a ruling stops being retried after this
// many fresh dry runs, rather than looping on a busy scope.
export const QUEUE_CHANGED_RETRIES = 3;
// A hard ceiling on calls for one ruling: a million statements.
const MAX_CHUNKS = 1000;

export interface RulingApi {
  rule(req: RuleStatementsRequest): Promise<RuleStatementsResponse>;
}

export interface RulingSelection {
  action: "accept" | "reject";
  ids?: string[];
  filter?: ReviewFilter;
  reason?: string;
  scope?: ReviewScope;
}

// Why the page is asking the reviewer to confirm: the first dry run, a dry run
// after the queue changed under the ruling (queue_changed), or one that found
// more than was confirmed while ruling in chunks.
export type ConfirmReason = "first" | "queue_changed" | "grew";

export interface RulingProgress {
  ruled: number;
  failed: number;
  // What the reviewer confirmed: the dry run's match count.
  total: number;
  chunks: number;
}

export interface RulingOutcome {
  // Each call that ruled made one ruling; undoing the whole ruling reverts each.
  rulingIds: string[];
  ruled: number;
  failed: number;
  // The last dry run's count of statements the caller may not rule on.
  skipped: number;
  // What the reviewer confirmed (0 when nothing matched).
  total: number;
  // The reviewer declined a confirm.
  cancelled: boolean;
  // The statements that did not take the ruling, with the runtime's reason.
  failures: RulingItem[];
  // A call failed part way; whatever was ruled before it is in rulingIds.
  error?: unknown;
}

export interface RulingHooks {
  // Asked with each dry run the reviewer has not seen yet; false stops.
  // Omitted (a single row, a hand-picked selection), nothing is asked.
  confirm?: (dry: RuleStatementsResponse, why: ConfirmReason) => Promise<boolean>;
  onProgress?: (p: RulingProgress) => void;
}

export function isQueueChanged(e: unknown): boolean {
  return e instanceof ApiError && e.code === "queue_changed";
}

// runRuling rules a selection: ALWAYS a dry run first (the confirm sees its
// count and sample), then each real call carries that dry run's count as
// expect_count. Over RULING_CHUNK matches it rules in chunks — dry run, rule,
// dry run — until the selection is ruled, a chunk rules nothing (what is left
// cannot be ruled by this caller), or the reviewer stops.
//
// A queue that changed between a dry run and its ruling (queue_changed) is
// dry-run again and confirmed again. So is one that grew past what was
// confirmed while chunking: "all matching" means what the reviewer saw.
export async function runRuling(api: RulingApi, sel: RulingSelection, hooks: RulingHooks = {}): Promise<RulingOutcome> {
  const base: Omit<RuleStatementsRequest, "dry_run"> = {
    action: sel.action,
    ...(sel.ids ? { ids: sel.ids } : { filter: sel.filter ?? {} }),
    ...(sel.reason ? { reason: sel.reason } : {}),
    ...(sel.scope && sel.scope !== "tenant" ? { scope: sel.scope } : {}),
  };
  const out: RulingOutcome = { rulingIds: [], ruled: 0, failed: 0, skipped: 0, total: 0, cancelled: false, failures: [] };
  const dryRun = async () => {
    const d = await api.rule({ ...base, dry_run: true });
    out.skipped = d.skipped;
    return d;
  };
  const confirm = async (d: RuleStatementsResponse, why: ConfirmReason) => {
    if (!hooks.confirm || (await hooks.confirm(d, why))) {
      out.total = out.ruled + d.matched;
      return true;
    }
    out.cancelled = true;
    return false;
  };

  let dry: RuleStatementsResponse;
  try {
    dry = await dryRun();
  } catch (e) {
    out.error = e;
    return out;
  }
  if (dry.matched === 0 || !(await confirm(dry, "first"))) return out;

  let retries = 0;
  for (let chunk = 0; chunk < MAX_CHUNKS; chunk++) {
    if (dry.to_rule === 0) break;
    let res: RuleStatementsResponse;
    try {
      res = await api.rule({ ...base, dry_run: false, expect_count: dry.matched });
    } catch (e) {
      if (!isQueueChanged(e) || retries >= QUEUE_CHANGED_RETRIES) {
        out.error = e;
        return out;
      }
      retries++;
      try {
        dry = await dryRun();
      } catch (e2) {
        out.error = e2;
        return out;
      }
      if (dry.matched === 0 || !(await confirm(dry, "queue_changed"))) return out;
      continue;
    }
    if (res.ruling_id) out.rulingIds.push(res.ruling_id);
    out.ruled += res.ruled;
    out.failed += res.failed;
    for (const it of res.items ?? []) if (it.error) out.failures.push(it);
    hooks.onProgress?.({ ruled: out.ruled, failed: out.failed, total: out.total, chunks: out.rulingIds.length });
    // Everything the selection matched was in this call's window, or the call
    // ruled nothing: what is left is not this caller's to rule.
    if (res.ruled === 0 || res.matched <= res.selected) break;
    try {
      dry = await dryRun();
    } catch (e) {
      out.error = e;
      return out;
    }
    if (dry.matched === 0) break;
    if (dry.matched > out.total - out.ruled && !(await confirm(dry, "grew"))) return out;
  }
  return out;
}

// ---- undo ------------------------------------------------------------------

export interface UndoOutcome {
  reverted: number;
  changed: number;
  // Rulings the runtime could not undo (not found, refused), with why.
  errors: string[];
}

// undoRulings reverts each ruling of a bulk ruling, newest first, and adds up
// what came back. One ruling failing does not stop the others.
export async function undoRulings(
  revert: (rulingId: string) => Promise<RevertRulingResponse>,
  rulingIds: string[],
): Promise<UndoOutcome> {
  const out: UndoOutcome = { reverted: 0, changed: 0, errors: [] };
  for (const id of [...rulingIds].reverse()) {
    try {
      const r = await revert(id);
      out.reverted += r.reverted;
      out.changed += r.changed;
    } catch (e) {
      out.errors.push(e instanceof Error ? e.message : String(e));
    }
  }
  return out;
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

// chunkCount is how many calls a confirmed ruling of `matched` takes.
export function chunkCount(matched: number): number {
  return Math.max(1, Math.ceil(matched / RULING_CHUNK));
}
