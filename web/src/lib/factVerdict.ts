// How a fact's verification state is read and written by the console.
//
// THE UI DOES NOT RE-DERIVE THE SERVER'S VERDICT SCALE. The runtime maps a verdict word
// to a confidence number (supported / unclear / mistyped / unsupported) and owns the
// floor below which a fact is withheld. Reverse-mapping that number back to a word here
// would put a second copy of the scale in the browser, and the two would drift the first
// time a threshold moved — silently, since both would keep rendering something plausible.
//
// So the console reads only what the server states outright: whether a verdict exists
// (`judged_at`), whether it withheld the fact (`withheld`), and what reason was given.
// The nuance between "supported" and "unclear" lives in the reason text, which is where
// the judge put it.
export type FactState = "unverified" | "checked" | "withheld";

export interface FactEntity {
  source_quote?: string;
  subject?: string;
  judged_at?: number;
  judge_reason?: string;
  /** Who reached the verdict: "operator", or the agent's name. Server-stamped — a
   *  writer does not get to label its own provenance. Absent on a verdict recorded
   *  before the column existed, which reads as unknown rather than as either party. */
  judged_by?: string;
  withheld?: boolean;
  confidence?: number;
  origin?: string;
  natural_key?: string;
}

// OPERATOR marks a verdict a human recorded. It is the value the SERVER stamps for an
// off-run call, not something this client sends — the column is provenance, and a writer
// that could set it could launder a machine's verdict into a human's.
export const OPERATOR = "operator";

export interface FactVerdict {
  state: FactState;
  /** True when a person recorded this verdict rather than an agent. */
  byOperator: boolean;
  /** Who judged, for display: "an operator", the agent's name, or "" when the store
   *  does not know (a verdict predating the column). */
  judgedBy: string;
  reason: string;
}

export function factVerdict(entity: FactEntity | undefined): FactVerdict {
  const reason = entity?.judge_reason ?? "";
  const judgedBy = entity?.judged_by ?? "";
  const byOperator = judgedBy === OPERATOR;
  // `judged_at` is the discriminator, NOT the confidence. A fact can legitimately carry
  // a confidence with no verdict (an extractor could supply one), and reading that as
  // "checked" would report a machine's guess as a decision.
  if (!entity?.judged_at) {
    return { state: "unverified", byOperator: false, judgedBy: "", reason: "" };
  }
  return { state: entity.withheld ? "withheld" : "checked", byOperator, judgedBy, reason };
}

// factActions decides which controls a row offers.
//
// Both directions are always available on a judged fact, deliberately: the safety valve
// this panel exists for is re-judging a fact the judge got WRONG, and a UI that only
// offered "mark wrong" would let an operator withhold but never restore.
export function factActions(v: FactVerdict): { canMarkWrong: boolean; canMarkGood: boolean } {
  return {
    canMarkWrong: v.state !== "withheld",
    canMarkGood: v.state !== "checked",
  };
}
