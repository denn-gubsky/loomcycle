import { describe, expect, it } from "vitest";
import type { TeamDraftIssue, TeamDraftVerification } from "../api";
import { checkVerdict, forkOverlayFor } from "./teamCheck";

const result = (over: Partial<TeamDraftVerification>, issues: TeamDraftIssue[] = []): TeamDraftVerification => ({
  name: "sdlc",
  valid: true,
  runnable: true,
  checked_as: "fork",
  matches: false,
  deployed: true,
  issues,
  ...over,
});
const issue = (severity: TeamDraftIssue["severity"]): TeamDraftIssue => ({ kind: "k", severity, detail: "d" });

describe("checkVerdict", () => {
  it("leads with a refusal, counting only the refused issues", () => {
    const v = checkVerdict(result({ valid: false, runnable: false }, [issue("refused"), issue("refused"), issue("unrunnable")]));
    expect(v).toEqual({ tone: "error", text: "A save would be refused: 2 problems to fix." });
  });

  it("says a valid team that could not run would still save", () => {
    const v = checkVerdict(result({ runnable: false }, [issue("unrunnable")]));
    expect(v).toEqual({ tone: "warn", text: "It would save, but a walk could not run it: 1 problem." });
  });

  it("says when the draft is what is already deployed, and counts notes", () => {
    expect(checkVerdict(result({ matches: true })).text).toBe("No changes from the deployed version — nothing to save.");
    expect(checkVerdict(result({}, [issue("advisory")]))).toEqual({ tone: "ok", text: "Ready to save (1 note)." });
  });
});

describe("forkOverlayFor", () => {
  // Check must judge the save the button makes, so a removal the editor turns
  // into a clear on save is a clear in what Check sends too.
  it("sends a removed variable list and cap as clears, like Save", () => {
    const loaded = { entry: "a", vars: { tone: "formal" }, max_iterations: 12 };
    const edited = { entry: "a" };
    expect(forkOverlayFor(loaded, edited)).toEqual({ entry: "a", vars: {}, max_iterations: 0 });
  });
});
