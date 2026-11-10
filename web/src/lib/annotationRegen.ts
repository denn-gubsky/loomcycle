import type { AnnotationTarget } from "@loomcycle/explorer";
import type { Principal, RunStreamHandlers, SSEFrame, StartRunRequest } from "../api";
import { canDecideOntology } from "./ontologyAuthority";

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

// canRegenerateAnnotations: writing and regenerating annotations is the
// operator plane's, the same principals that decide the tenant ontology.
export function canRegenerateAnnotations(p: Principal | null | undefined): boolean {
  return canDecideOntology(p);
}

// reportFailure reads the annotator's report (its final text, a JSON object)
// and returns why the pass did not complete, if it did not. The agent reports a
// refused input, a held lease or a budget stop in a normal run, not as an
// error frame.
export function reportFailure(finalText: string): string | undefined {
  const start = finalText.lastIndexOf("{\"status\"");
  if (start < 0) return undefined;
  let rep: { status?: unknown; reason?: unknown; failure?: unknown };
  try {
    rep = JSON.parse(finalText.slice(start));
  } catch {
    return undefined;
  }
  if (typeof rep.status !== "string" || rep.status === "complete") return undefined;
  const why = typeof rep.reason === "string" ? rep.reason : typeof rep.failure === "string" ? rep.failure : "";
  switch (rep.status) {
    case "busy":
      return "Another annotation pass is running on this knowledge base; try again when it ends.";
    case "budget_stopped":
      return "The annotator stopped at its call budget before finishing.";
    default:
      return `The annotator did not finish (${rep.status}${why ? ": " + why : ""}).`;
  }
}

// textOf is a text frame's text, if the frame is one.
function textOf(f: SSEFrame): string {
  try {
    const ev = JSON.parse(f.data) as { type?: unknown; text?: unknown };
    return ev?.type === "text" && typeof ev.text === "string" ? ev.text : "";
  } catch {
    return "";
  }
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
  let text = "";
  await start(
    { agent: ANNOTATOR_AGENT, prompt: JSON.stringify(annotatorInput(target)) },
    {
      onFrame: (f) => {
        failure = runFailure(f) ?? failure;
        text += textOf(f);
      },
    },
  );
  failure = failure ?? reportFailure(text);
  if (failure) throw new Error(failure);
}
