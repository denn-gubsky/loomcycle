// The rules the erasure console enforces before it will offer to delete anything.
//
// These live here rather than inline because each one is a guard against a specific way
// an operator could destroy the wrong data, and a guard nobody can test is a guard
// nobody should trust.

export interface ErasureResidue {
  rows: number;
  scopes?: string[];
  sessions_examined: number;
  truncated?: boolean;
}

export interface ErasureGateState {
  /** The subject typed into the field right now. */
  subject: string;
  /** The subject the CURRENTLY DISPLAYED report was run for, or "" if none. */
  reportedSubject: string;
  /** What the operator typed into the confirmation box. */
  confirm: string;
}

export type GateReason =
  | "ready"
  | "no-subject"
  | "no-report"
  | "report-is-for-another-subject"
  | "confirm-does-not-match";

// erasureGate decides whether the execute control may fire, and why not when it may not.
//
// THE STALE-REPORT RULE IS THE IMPORTANT ONE. An operator reports on `alice`, reads what
// would go, then edits the field to `alicia` and clicks erase — without this, they would
// be confirming against a preview of somebody else's data. So changing the subject
// invalidates the report, and the report must be re-run for the subject actually being
// erased.
//
// The typed confirmation is an EXACT match on the subject id, not a yes/no. A modal with
// a red button is dismissed by reflex; typing the identifier is the smallest gesture
// that cannot be made absent-mindedly — and the identifier is what matters, because the
// failure mode is erasing the WRONG subject rather than erasing accidentally.
export function erasureGate(s: ErasureGateState): { canExecute: boolean; reason: GateReason } {
  const subject = s.subject.trim();
  if (!subject) return { canExecute: false, reason: "no-subject" };
  if (!s.reportedSubject) return { canExecute: false, reason: "no-report" };
  if (s.reportedSubject !== subject) {
    return { canExecute: false, reason: "report-is-for-another-subject" };
  }
  // Untrimmed on purpose: the server compares `confirm` to the subject exactly, and a UI
  // that trimmed here would enable a button the server then refuses.
  if (s.confirm !== subject) return { canExecute: false, reason: "confirm-does-not-match" };
  return { canExecute: true, reason: "ready" };
}

// residueMeaning renders what the residue count actually tells you.
//
// A ZERO ACROSS ZERO SESSIONS IS UNKNOWN, NOT NONE. The residue is found by tracing what
// derives from the subject's sessions; with no sessions examined there was nothing to
// trace FROM, so "0 rows" means the question could not be asked — and reporting that as
// "nothing left behind" would be the most reassuring possible lie on a screen whose
// whole job is to say what erasure cannot reach.
export function residueMeaning(r: ErasureResidue | undefined): string {
  if (!r) return "";
  if (r.sessions_examined === 0) {
    return "unknown — no sessions were examined, so there was nothing to trace derived data from";
  }
  const scope = r.truncated ? "at least " : "";
  if (r.rows === 0) {
    return `none found across ${r.sessions_examined} session(s)`;
  }
  return `${scope}${r.rows} row(s) derived from this subject's sessions, which a subject-keyed delete does not reach`;
}
