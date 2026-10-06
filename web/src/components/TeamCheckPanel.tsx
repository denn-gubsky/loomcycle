import type { TeamDraftVerification } from "../api";
import { checkVerdict } from "../lib/teamCheck";

// Themed tokens (tokens.css), so the panel follows light and dark mode.
const TONE_COLOR = {
  ok: "var(--lc-completed)",
  warn: "var(--lc-running)",
  error: "var(--lc-danger)",
} as const;

const SEVERITY_LABEL = { refused: "refused", unrunnable: "can't run", advisory: "note" } as const;
const SEVERITY_TONE = { refused: "error", unrunnable: "warn", advisory: "ok" } as const;

/** TeamCheckPanel shows what Check found: a verdict line, then every issue
 *  with where it is (its JSON path, or its state) and the server's own words. */
export function TeamCheckPanel({ result, onClose }: { result: TeamDraftVerification; onClose: () => void }) {
  const verdict = checkVerdict(result);
  return (
    <div
      role="status"
      style={{
        flex: "0 1 auto",
        maxHeight: "35%",
        overflow: "auto",
        border: `1px solid ${TONE_COLOR[verdict.tone]}`,
        borderRadius: 6,
        padding: "0.5rem 0.6rem",
        fontSize: "0.85em",
      }}
    >
      <div style={{ display: "flex", justifyContent: "space-between", gap: "0.5rem", alignItems: "baseline" }}>
        <strong style={{ color: TONE_COLOR[verdict.tone] }}>{verdict.text}</strong>
        <button onClick={onClose} aria-label="Close check results" style={{ fontSize: "0.8em", padding: "0 0.4rem" }}>
          ×
        </button>
      </div>
      <div style={{ opacity: 0.7, marginTop: "0.15rem" }}>Checked as a {result.checked_as}. Nothing was saved.</div>
      {result.issues.length > 0 && (
        <ol style={{ margin: "0.4rem 0 0", paddingLeft: "1.2rem" }}>
          {result.issues.map((issue, i) => (
            <li key={i} style={{ marginBottom: "0.35rem" }}>
              <span style={{ color: TONE_COLOR[SEVERITY_TONE[issue.severity]], fontWeight: 600 }}>
                {SEVERITY_LABEL[issue.severity]}
              </span>{" "}
              {issue.path ? <code>{issue.path}</code> : issue.state ? <code>{issue.state}</code> : null}
              <div style={{ whiteSpace: "pre-wrap" }}>{issue.detail}</div>
            </li>
          ))}
        </ol>
      )}
    </div>
  );
}
