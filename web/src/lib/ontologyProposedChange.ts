import type { ProposedChange } from "../api";
import type { OntologyPlanResult } from "./ontologyRetirePlan";

/** One change the plan route takes (POST /v1/_ontology/plan): a direct change of an
 *  entry, or an open proposal by its chunk id (RFC GQ P4.3), which the runtime plans as
 *  the direct changes accepting it makes. */
export type OntologyPlanChange =
  | { op: "retire" | "rename" | "move" | "edit"; entry: string; name?: string; parent?: string; body?: string }
  | { op: "proposal"; id: string };

// changeConfirmText is what the operator reads before accepting a change proposal: the
// change, and what the plan found resting on it.
export function changeConfirmText(c: ProposedChange, plan: OntologyPlanResult | null): string {
  const lines = [`Accept this change to the ${c.kind} "${c.target_name}"?`, proposedChangeText(c)];
  const counts = new Map<string, number>();
  for (const ch of plan?.changes ?? []) {
    for (const [k, v] of Object.entries(ch.counts)) {
      if (v.n > 0 && v.effect !== "kept") counts.set(k, (counts.get(k) ?? 0) + v.n);
    }
  }
  const named = [...counts.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([k, n]) => `${k.replace(/_/g, " ")}: ${n}`);
  if (plan) lines.push(named.length > 0 ? `What rests on it: ${named.join(", ")}.` : "Nothing rests on it.");
  if (plan && !plan.complete) {
    lines.push(`Not every scope could be counted (${plan.scopes_skipped ?? 0} skipped); the counts may be low.`);
  }
  return lines.join("\n");
}

// proposedChangeText is what a change or retire proposal (RFC GQ P4.1) would do to the
// entry it names, one line per change, as plain text (rendered in a <pre>, so markup in
// a name or value stays text).
export function proposedChangeText(c: ProposedChange): string {
  if (c.op === "retire") {
    return `retire the ${c.kind} ${c.target_name}: nothing new may use it; what uses it stays`;
  }
  const lines: string[] = [];
  if (c.name) lines.push(`rename ${c.target_name} to ${c.name}`);
  if (c.parent) lines.push(`move under ${c.parent_name || "the section"}`);
  if (c.meaning) lines.push(`meaning: ${c.meaning}`);
  if (c.sets_directives) {
    const ds = c.directives ?? [];
    lines.push(
      ds.length === 0 ? "directives: none (clears them)" : "directives:",
    );
    for (const d of ds)
      lines.push(`    @${d.name}${d.value ? " " + d.value : ""}`);
  }
  return lines.join("\n");
}
