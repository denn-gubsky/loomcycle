import { describe, expect, it } from "vitest";
import { erasureGate, residueMeaning } from "./erasure";

describe("erasureGate", () => {
  const ready = { subject: "alice", reportedSubject: "alice", confirm: "alice" };

  it("allows an erasure only when the report, the subject and the confirmation agree", () => {
    expect(erasureGate(ready)).toEqual({ canExecute: true, reason: "ready" });
  });

  it("refuses when the report is for a DIFFERENT subject", () => {
    // The failure this exists for: report on alice, read what would go, edit the field
    // to alicia, click erase — confirming against a preview of someone else's data.
    const g = erasureGate({ subject: "alicia", reportedSubject: "alice", confirm: "alicia" });
    expect(g.canExecute).toBe(false);
    expect(g.reason).toBe("report-is-for-another-subject");
  });

  it("refuses before any report has been run", () => {
    expect(erasureGate({ ...ready, reportedSubject: "" }).reason).toBe("no-report");
  });

  it("requires the confirmation to match the subject EXACTLY", () => {
    expect(erasureGate({ ...ready, confirm: "" }).reason).toBe("confirm-does-not-match");
    expect(erasureGate({ ...ready, confirm: "Alice" }).reason).toBe("confirm-does-not-match");
    // Not trimmed: the server compares exactly, so trimming here would enable a button
    // the server then refuses.
    expect(erasureGate({ ...ready, confirm: "alice " }).reason).toBe("confirm-does-not-match");
  });

  it("refuses an empty subject before anything else", () => {
    expect(erasureGate({ subject: "  ", reportedSubject: "", confirm: "" }).reason).toBe("no-subject");
  });
});

describe("residueMeaning", () => {
  it("says UNKNOWN when no sessions were examined", () => {
    // The most reassuring possible lie on a screen whose whole job is to say what
    // erasure cannot reach: reporting "0 rows" as "nothing left behind" when in fact
    // there was nothing to trace from.
    const m = residueMeaning({ rows: 0, sessions_examined: 0 });
    expect(m).toContain("unknown");
    expect(m).not.toContain("none found");
  });

  it("says none only when sessions WERE examined", () => {
    expect(residueMeaning({ rows: 0, sessions_examined: 4 })).toContain("none found");
  });

  it("reports a truncated scan as a floor, not a total", () => {
    const m = residueMeaning({ rows: 12, sessions_examined: 4, truncated: true });
    expect(m).toContain("at least 12");
  });

  it("reports an exact count when the scan was complete", () => {
    const m = residueMeaning({ rows: 12, sessions_examined: 4 });
    expect(m).toContain("12 row(s)");
    expect(m).not.toContain("at least");
  });
});
