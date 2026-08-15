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
  withheld?: boolean;
  confidence?: number;
  origin?: string;
  natural_key?: string;
}

// OPERATOR_REASON_PREFIX marks a verdict a human recorded.
//
// A prefix rather than a column: nothing BRANCHES on who judged, it only changes how the
// reason renders, and a display distinction is not worth a migration. If anything ever
// needs to act on it — a judge that must not overwrite a human's decision, say — this
// becomes a real field and this constant is the thing to grep for.
export const OPERATOR_REASON_PREFIX = "operator: ";

export function operatorReason(text: string): string {
  return OPERATOR_REASON_PREFIX + text.trim();
}

export interface FactVerdict {
  state: FactState;
  /** True when a person recorded this verdict rather than the judge. */
  byOperator: boolean;
  /** The reason with any operator marker removed — the marker is rendered separately. */
  reason: string;
}

export function factVerdict(entity: FactEntity | undefined): FactVerdict {
  const reasonRaw = entity?.judge_reason ?? "";
  const byOperator = reasonRaw.startsWith(OPERATOR_REASON_PREFIX);
  const reason = byOperator ? reasonRaw.slice(OPERATOR_REASON_PREFIX.length) : reasonRaw;
  // `judged_at` is the discriminator, NOT the confidence. A fact can legitimately carry
  // a confidence with no verdict (an extractor could supply one), and reading that as
  // "checked" would report a machine's guess as a decision.
  if (!entity?.judged_at) {
    return { state: "unverified", byOperator: false, reason: "" };
  }
  return { state: entity.withheld ? "withheld" : "checked", byOperator, reason };
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
