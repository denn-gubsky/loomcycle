// A bulk ruling on proposed statements (POST /v1/_statements/rule), run the one
// way every surface runs it: dry run first, then the real call carrying the dry
// run's count, in chunks of at most RULING_CHUNK. The document viewer's
// Connections panel and the Web UI's Review page both go through runRuling, so
// the order of calls a reviewer's click makes is decided here alone.

// The most statements one call rules (the runtime's cap).
export const RULING_CHUNK = 1000;
// A queue that keeps changing under a ruling stops being retried after this
// many fresh dry runs, rather than looping on a busy scope.
export const QUEUE_CHANGED_RETRIES = 3;
// A hard ceiling on calls for one ruling: a million statements.
const MAX_CHUNKS = 1000;

export type RulingAction = "accept" | "reject";
export type RulingScope = "tenant" | "user";

/** One statement's outcome in a ruling or a revert: the status it was ruled to,
 *  or the refusal's code and message. */
export interface RulingItem {
  id: string;
  status?: string;
  error?: string;
  message?: string;
}

/** A rule call's body. `F` is the queue filter's shape. */
export interface RulingRequest<F> {
  action: RulingAction;
  ids?: string[];
  filter?: F;
  reason?: string;
  dry_run: boolean;
  expect_count?: number;
  max?: number;
  scope?: RulingScope;
}

/** The counts of a rule call's answer that the sequence reads. */
export interface RulingCounts {
  dry_run: boolean;
  ruling_id?: string;
  /** What the selection matches that the caller may read; expect_count repeats it. */
  matched: number;
  /** The part this call considers: at most RULING_CHUNK, first in queue order. */
  selected: number;
  /** Of `selected`, the ones the caller may not rule on. */
  skipped: number;
  to_rule: number;
  ruled: number;
  failed: number;
  items?: RulingItem[] | null;
}

export interface RulingApi<F, R extends RulingCounts> {
  rule(req: RulingRequest<F>): Promise<R>;
}

export interface RulingSelection<F> {
  action: RulingAction;
  ids?: string[];
  filter?: F;
  reason?: string;
  scope?: RulingScope;
}

/** Why a reviewer is asked to confirm: the first dry run, a dry run after the
 *  queue changed under the ruling (`queue_changed`), or one that found more
 *  than was confirmed while ruling in chunks. */
export type ConfirmReason = "first" | "queue_changed" | "grew";

export interface RulingProgress {
  ruled: number;
  failed: number;
  /** What the reviewer confirmed: the dry run's match count. */
  total: number;
  chunks: number;
}

export interface RulingOutcome {
  /** Each call that ruled made one ruling; undoing the whole ruling reverts each. */
  rulingIds: string[];
  ruled: number;
  failed: number;
  /** The last dry run's count of statements the caller may not rule on. */
  skipped: number;
  /** What the reviewer confirmed (0 when nothing matched). */
  total: number;
  /** The reviewer declined a confirm. */
  cancelled: boolean;
  /** The statements that did not take the ruling, with the runtime's reason. */
  failures: RulingItem[];
  /** A call failed part way; whatever was ruled before it is in rulingIds. */
  error?: unknown;
}

export interface RulingHooks<R> {
  /** Asked with each dry run the reviewer has not seen yet; false stops. Omitted
   *  (a single row, a hand-picked selection), nothing is asked. */
  confirm?: (dry: R, why: ConfirmReason) => Promise<boolean>;
  onProgress?: (p: RulingProgress) => void;
}

/** isQueueChanged recognizes the refusal of a real call whose expect_count no
 *  longer matches. Read off the error's `code`, which both @loomcycle/client's
 *  errors and a host's own carry. */
export function isQueueChanged(e: unknown): boolean {
  return !!e && typeof e === "object" && (e as { code?: unknown }).code === "queue_changed";
}

/** runRuling rules a selection: ALWAYS a dry run first (the confirm sees its
 *  count and sample), then each real call carries that dry run's count as
 *  expect_count. Over RULING_CHUNK matches it rules in chunks — dry run, rule,
 *  dry run — until the selection is ruled, a chunk rules nothing (what is left
 *  cannot be ruled by this caller), or the reviewer stops.
 *
 *  A queue that changed between a dry run and its ruling (queue_changed) is
 *  dry-run again and confirmed again. So is one that grew past what was
 *  confirmed while chunking: "all matching" means what the reviewer saw. */
export async function runRuling<F, R extends RulingCounts>(
  api: RulingApi<F, R>,
  sel: RulingSelection<F>,
  hooks: RulingHooks<R> = {},
): Promise<RulingOutcome> {
  const base: Omit<RulingRequest<F>, "dry_run"> = {
    action: sel.action,
    ...(sel.ids ? { ids: sel.ids } : { filter: sel.filter ?? ({} as F) }),
    ...(sel.reason ? { reason: sel.reason } : {}),
    ...(sel.scope && sel.scope !== "tenant" ? { scope: sel.scope } : {}),
  };
  const out: RulingOutcome = { rulingIds: [], ruled: 0, failed: 0, skipped: 0, total: 0, cancelled: false, failures: [] };
  const dryRun = async () => {
    const d = await api.rule({ ...base, dry_run: true });
    out.skipped = d.skipped;
    return d;
  };
  const confirm = async (d: R, why: ConfirmReason) => {
    if (!hooks.confirm || (await hooks.confirm(d, why))) {
      out.total = out.ruled + d.matched;
      return true;
    }
    out.cancelled = true;
    return false;
  };

  let dry: R;
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
    let res: R;
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

export interface UndoOutcome {
  reverted: number;
  changed: number;
  /** Rulings the runtime could not undo (not found, refused), with why. */
  errors: string[];
}

/** undoRulings reverts each ruling of a bulk ruling, newest first, and adds up
 *  what came back. One ruling failing does not stop the others. */
export async function undoRulings(
  revert: (rulingId: string) => Promise<{ reverted: number; changed: number }>,
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

/** chunkCount is how many calls a confirmed ruling of `matched` takes. */
export function chunkCount(matched: number): number {
  return Math.max(1, Math.ceil(matched / RULING_CHUNK));
}
