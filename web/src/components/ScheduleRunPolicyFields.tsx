// The two fields that say what a schedule does when its runs overlap or its
// slots are missed: concurrency_policy and catch_up_max. Shared by the create
// and fork forms. Blank = omit from the overlay — the default on create,
// the parent's value on fork — so a form never sends a value the operator
// did not choose.

export const CATCH_UP_MAX_LIMIT = 1000;

interface Props {
  policy: string;
  onPolicy: (v: string) => void;
  catchUpMax: string;
  onCatchUpMax: (v: string) => void;
  // inherit labels the blank choice for a fork ("inherit") vs a create
  // ("default: forbid").
  inherit?: boolean;
}

export default function ScheduleRunPolicyFields({ policy, onPolicy, catchUpMax, onCatchUpMax, inherit }: Props) {
  return (
    <>
      <label className="modal-field">
        <span>concurrency_policy (optional)</span>
        <select value={policy} onChange={(e) => onPolicy(e.target.value)}>
          <option value="">{inherit ? "inherit" : "default: forbid"}</option>
          <option value="forbid">forbid — skip a slot while the last run is still going</option>
          <option value="allow">allow — start another run alongside it</option>
          <option value="replace">replace — cancel the running one, then start</option>
        </select>
      </label>
      <label className="modal-field">
        <span>catch_up_max (optional)</span>
        <input
          type="number"
          min="0"
          max={CATCH_UP_MAX_LIMIT}
          value={catchUpMax}
          onChange={(e) => onCatchUpMax(e.target.value)}
          placeholder={inherit ? "blank = inherit" : "blank or 0 = an outage collapses into one fire; N = run the newest N missed slots"}
        />
      </label>
    </>
  );
}

// applyRunPolicy writes the chosen values into a create/fork overlay. Returns
// an error message for a value the server would refuse, else null.
export function applyRunPolicy(overlay: Record<string, unknown>, policy: string, catchUpMax: string): string | null {
  if (policy) overlay.concurrency_policy = policy;
  if (catchUpMax.trim()) {
    const n = Number(catchUpMax.trim());
    if (!Number.isInteger(n) || n < 0 || n > CATCH_UP_MAX_LIMIT) {
      return `catch_up_max must be an integer from 0 to ${CATCH_UP_MAX_LIMIT}.`;
    }
    overlay.catch_up_max = n;
  }
  return null;
}
