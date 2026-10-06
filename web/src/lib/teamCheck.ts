import type { TeamDraftIssue, TeamDraftVerification } from "../api";
import { keepOwnDefinitionRemovals } from "./teamLocal";
import { keepWalkHookRemoval } from "./teamHooks";

/** forkOverlayFor is what "Save new version" sends for an edited team: the
 *  edit, with every walk hook, variable list, local kind or cap the operator
 *  removed sent as a clear. Check sends the same, so it judges the save the
 *  button would make — not the text as typed. */
export function forkOverlayFor(loadedDef: unknown, edited: unknown): unknown {
  return keepOwnDefinitionRemovals(loadedDef, keepWalkHookRemoval(loadedDef, edited));
}

export type CheckTone = "ok" | "warn" | "error";

/** checkVerdict is the one-line answer a check leads with. */
export function checkVerdict(r: TeamDraftVerification): { tone: CheckTone; text: string } {
  const count = (sev: TeamDraftIssue["severity"]) => r.issues.filter((i) => i.severity === sev).length;
  const advisories = count("advisory");
  const notes = advisories > 0 ? ` (${advisories} note${advisories === 1 ? "" : "s"})` : "";
  if (!r.valid) {
    const n = count("refused");
    return { tone: "error", text: `A save would be refused: ${n} problem${n === 1 ? "" : "s"} to fix.` };
  }
  if (!r.runnable) {
    const n = count("unrunnable");
    return {
      tone: "warn",
      text: `It would save, but a walk could not run it: ${n} problem${n === 1 ? "" : "s"}.`,
    };
  }
  if (r.matches) return { tone: "ok", text: `No changes from the deployed version — nothing to save${notes}.` };
  return { tone: "ok", text: `Ready to save${notes}.` };
}
