import type { FactEntity } from "../types";

// How a fact's verification state is read and written (RFC CC).
//
// THIS DOES NOT RE-DERIVE THE SERVER'S VERDICT SCALE. The runtime maps a verdict word to
// a confidence and owns the floor below which a fact is withheld. Reverse-mapping that
// number back into words here would put a second copy of the scale in every consumer of
// this package, and they would drift the first time a threshold moved — silently, since
// each would keep rendering something plausible.
//
// So it reads only what the server states outright: whether a verdict exists
// (`judged_at`), whether it withheld the fact (`withheld`), who reached it, and why. The
// nuance between "supported" and "unclear" lives in the reason text, where the judge put
// it.
export type FactState = "unverified" | "checked" | "withheld";

/** The value the server stamps for a verdict a person recorded off-run. */
export const OPERATOR = "operator";

export interface FactVerdict {
  state: FactState;
  /** True when a person recorded this verdict rather than an agent. */
  byOperator: boolean;
  /** Who judged, for display: "operator", an agent's name, or "" when the store does
   *  not know (a verdict predating the column). */
  judgedBy: string;
  reason: string;
}

export function factVerdict(entity: FactEntity | undefined): FactVerdict {
  const reason = entity?.judge_reason ?? "";
  const judgedBy = entity?.judged_by ?? "";
  // `judged_at` is the discriminator, NOT the confidence. A fact can legitimately carry
  // a confidence with no verdict, and reading that as "checked" would report a machine's
  // guess as a decision.
  if (!entity?.judged_at) {
    return { state: "unverified", byOperator: false, judgedBy: "", reason: "" };
  }
  return {
    state: entity.withheld ? "withheld" : "checked",
    byOperator: judgedBy === OPERATOR,
    judgedBy,
    reason,
  };
}

// factActions decides which controls a fact offers.
//
// Both directions are available on a judged fact, deliberately: the reason this surface
// exists is to correct a verdict the judge got WRONG, and a panel that only offered
// "mark wrong" would let an operator withhold but never restore.
export function factActions(v: FactVerdict): { canMarkWrong: boolean; canMarkGood: boolean } {
  return { canMarkWrong: v.state !== "withheld", canMarkGood: v.state !== "checked" };
}
