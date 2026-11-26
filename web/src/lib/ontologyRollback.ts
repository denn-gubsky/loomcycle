import type { OntologyRollbackFailure, OntologyRollbackReport } from "../api";
import type { OntologyPlannedChange, OntologyPlanResult } from "./ontologyRetirePlan";

// Rolling the tenant ontology back to a release from the Releases tab (RFC GQ D8,
// P5.7): the page plans {op: release, seq} first and shows the plan, and only the
// operator's confirm applies it, with the plan's fingerprint. Pure, so the wording an
// operator decides from is tested without mounting the page.

/** One planned change as the plan table shows it. */
export interface RollbackPlanRow {
  op: string;
  name: string;
  kind: string;
  risk: string;
  /** What the change does to what rests on the entry, e.g. "stale: typed documents 3". */
  effects: string[];
  notes: string[];
  /** A change the plan lists without counting what rests on it (a grammar chunk, a
   *  move under an entry the rollback adds back): planned as stales, nothing counted. */
  uncounted: boolean;
}

// The effects worth reading before a rollback: "kept" is shown for reference by the
// plan and changes nothing, so it is left out.
const EFFECT_ORDER = ["destroyed", "orphaned", "stale"];

function effectLines(ch: OntologyPlannedChange): string[] {
  const by = new Map<string, string[]>();
  for (const [k, c] of Object.entries(ch.counts ?? {}).sort(([a], [b]) => a.localeCompare(b))) {
    if (c.n <= 0 || c.effect === "kept") continue;
    const list = by.get(c.effect) ?? [];
    list.push(`${k.replace(/_/g, " ")} ${c.n}`);
    by.set(c.effect, list);
  }
  const effects = [...by.keys()].sort((a, b) => {
    const ia = EFFECT_ORDER.indexOf(a);
    const ib = EFFECT_ORDER.indexOf(b);
    return (ia < 0 ? EFFECT_ORDER.length : ia) - (ib < 0 ? EFFECT_ORDER.length : ib) || a.localeCompare(b);
  });
  return effects.map((e) => `${e}: ${by.get(e)!.join(", ")}`);
}

/** The plan's changes, one row each, in the order the server planned them (the
 *  counted changes first, the uncounted ones after). */
export function rollbackPlanRows(plan: OntologyPlanResult): RollbackPlanRow[] {
  return (plan.changes ?? []).map((ch) => {
    const effects = effectLines(ch);
    return {
      op: ch.op,
      name: ch.entry.name,
      kind: ch.entry.kind,
      risk: ch.risk,
      effects,
      notes: ch.notes ?? [],
      uncounted: ch.risk !== "none" && Object.values(ch.counts ?? {}).every((c) => c.n <= 0),
    };
  });
}

/** The plan's worst risk: destroys over stales over none. */
export function rollbackPlanRisk(plan: OntologyPlanResult): "destroys" | "stales" | "none" {
  const risks = (plan.changes ?? []).map((c) => c.risk);
  if (risks.includes("destroys")) return "destroys";
  if (risks.includes("stales")) return "stales";
  return "none";
}

/** The warning shown above the confirm when the plan's risk is not none; null when it is. */
export function rollbackRiskWarning(plan: OntologyPlanResult): string | null {
  switch (rollbackPlanRisk(plan)) {
    case "destroys":
      return "This rollback destroys stored rows that rest on the entries it changes. Read each change before applying it.";
    case "stales":
      return "This rollback leaves stored rows that rest on the entries it changes in need of rework (stale). They are kept and readable.";
    default:
      return null;
  }
}

/** Why the ontology after the rollback does not declare exactly what the release
 *  did, or null when it does. */
export function rollbackMatchesText(rep: OntologyRollbackReport): string | null {
  if (rep.matches) return null;
  return (
    "The ontology now declares what release " +
    rep.release +
    " declared, but its content version differs from the release's: an entry removed since was added back under a new id, or a class was renamed through the rename job (which keeps @renamed_from)."
  );
}

/** A refusal's code and the server's sentence, read from a 409 or 422 body. */
export function rollbackRefusal(f: OntologyRollbackFailure): { code: string; message: string } {
  const b = f.body ?? {};
  return {
    code: typeof b.code === "string" && b.code ? b.code : `http_${f.status}`,
    message: typeof b.error === "string" ? b.error : "",
  };
}
