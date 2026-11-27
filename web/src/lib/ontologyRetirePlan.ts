// Retiring an ontology entry from the Ontology page (RFC GQ P2.4): plan the retire,
// show the operator what rests on the entry, then send the retire with the plan's
// fingerprint. The runtime refuses a retire of an entry in use without one
// (plan_required) or with one from before another change (plan_stale); a stale answer
// re-plans once, so the operator confirms against the counts that hold now.

import { ApiError } from "./apiError";

export interface OntologyPlanCount {
  n: number;
  effect: string;
  tenant_scope: number;
  other_scopes?: number;
}

export interface OntologyPlannedChange {
  op: string;
  entry: { chunk_id: string; name: string; section: string; kind: string };
  risk: "destroys" | "stales" | "none";
  counts: Record<string, OntologyPlanCount>;
  subtree?: { chunk_id: string; name: string; kind: string }[];
  notes?: string[];
  /** The release a rollback plans toward (op release); absent otherwise. */
  release?: number;
}

export interface OntologyPlanResult {
  fingerprint: string;
  changes: OntologyPlannedChange[];
  complete: boolean;
  scopes_skipped?: number;
  /** Advisories of the ontology after every change that it does not report now. */
  new_issues?: string[];
}

// retireConfirmText is the confirmation the operator reads before the retire: the
// counts of what rests on the entry (kept, readable, no longer offered for new writes)
// and the entries of its subtree that are retired with it.
export function retireConfirmText(name: string, plan: OntologyPlanResult): string {
  const ch = plan.changes[0];
  const lines = [`Retire "${name}"? Everything that uses it is kept and readable; nothing new may use it.`];
  const counts = ch
    ? Object.entries(ch.counts)
        .filter(([, c]) => c.n > 0)
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([k, c]) => `${k.replace(/_/g, " ")}: ${c.n}`)
    : [];
  lines.push(counts.length > 0 ? `What rests on it: ${counts.join(", ")}.` : "Nothing rests on it.");
  if (ch?.subtree && ch.subtree.length > 0) {
    lines.push(`Retired with it: ${ch.subtree.map((e) => `${e.kind} "${e.name}"`).join(", ")}.`);
  }
  if (!plan.complete) {
    lines.push(`Not every scope could be counted (${plan.scopes_skipped ?? 0} skipped); the counts may be low.`);
  }
  return lines.join("\n");
}

export interface RetireSteps<T> {
  plan: () => Promise<OntologyPlanResult>;
  retire: (fingerprint: string) => Promise<T>;
  confirm: (text: string) => boolean;
}

// retireWithPlan plans, asks, and retires with the plan's fingerprint. It returns null
// when the operator declines. A plan_stale answer re-plans and asks again, once; any
// other refusal, or a second stale one, is thrown with the server's message.
export function retireWithPlan<T>(name: string, steps: RetireSteps<T>): Promise<T | null> {
  return applyWithPlan((plan) => retireConfirmText(name, plan), {
    plan: steps.plan,
    apply: steps.retire,
    confirm: steps.confirm,
  });
}

export interface PlanSteps<T> {
  plan: () => Promise<OntologyPlanResult>;
  apply: (fingerprint: string) => Promise<T>;
  confirm: (text: string) => boolean;
}

// plannedRefusal is the plan's note of a change no fingerprint lets through
// (rename_job_pending), or null.
export function plannedRefusal(plan: OntologyPlanResult): string | null {
  for (const ch of plan.changes) {
    const note = (ch.notes ?? []).find((n) => n.startsWith("rename_job_pending:"));
    if (note) return note;
  }
  return null;
}

// applyWithPlan is retireWithPlan for any planned change (RFC GQ P4.2 accepts a change
// proposal this way): plan, show confirmText of the plan, apply with its fingerprint,
// and re-plan once on plan_stale.
export async function applyWithPlan<T>(
  confirmText: (plan: OntologyPlanResult) => string,
  steps: PlanSteps<T>,
): Promise<T | null> {
  for (let attempt = 0; ; attempt++) {
    const plan = await steps.plan();
    // A change the runtime refuses whatever the fingerprint (a class something rests on
    // renamed before RFC GQ P5's rename job exists) is shown as the refusal it is, not
    // offered for confirmation.
    const refused = plannedRefusal(plan);
    if (refused) throw new Error(refused);
    if (!steps.confirm(confirmText(plan))) return null;
    try {
      return await steps.apply(plan.fingerprint);
    } catch (e) {
      if (attempt === 0 && e instanceof ApiError && e.code === "plan_stale") continue;
      throw e;
    }
  }
}
