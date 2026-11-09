import type { AnnotationTarget } from "@loomcycle/explorer";
import type { Principal, RunStreamHandlers, SSEFrame, StartRunRequest } from "../api";

// The knowledge-base bundle's annotating agent. Regenerate rewrites the
// document's annotations (its branches, then the document) from the current
// text; `force` asks it to rewrite records whose text has not changed too.
export const ANNOTATOR_AGENT = "kb/annotator";

// annotatorInput is the annotator's run input for a target. The agent works a
// whole document at a time, so a branch target regenerates its document; a
// folder's record (on its `_annotation` document) regenerates that folder.
export function annotatorInput(target: AnnotationTarget): Record<string, unknown> {
  if (target.level === "path" && target.folder) {
    return { mode: "path", path: target.folder, write: "annotate", force: true };
  }
  return { mode: "document", document_id: target.documentId, write: "annotate", force: true };
}

// canRegenerateAnnotations: starting the annotator is an operator's action.
export function canRegenerateAnnotations(p: Principal | null | undefined): boolean {
  return !!p && (p.is_admin || p.scopes.includes("substrate:tenant"));
}

// runFailure reads a run stream frame and returns the run's error, if the
// frame reports one.
export function runFailure(f: SSEFrame): string | undefined {
  let ev: { type?: unknown; error?: unknown };
  try {
    ev = JSON.parse(f.data);
  } catch {
    return undefined;
  }
  if (ev?.type !== "error") return undefined;
  return typeof ev.error === "string" && ev.error ? ev.error : "The annotator run failed.";
}

type StartRun = (req: StartRunRequest, h: RunStreamHandlers) => Promise<void>;

// regenerateAnnotation starts the annotator and resolves when its run ends, so
// the viewer reloads the annotation it wrote; a failed run rejects with the
// run's error, which the viewer shows in the annotation panel.
export async function regenerateAnnotation(target: AnnotationTarget, start: StartRun): Promise<void> {
  let failure: string | undefined;
  await start(
    { agent: ANNOTATOR_AGENT, prompt: JSON.stringify(annotatorInput(target)) },
    { onFrame: (f) => (failure = runFailure(f) ?? failure) },
  );
  if (failure) throw new Error(failure);
}
