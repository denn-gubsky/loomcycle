import { useEffect, useState } from "react";
import { getRun, type Agent } from "../api";
import { contextMethodFromSpec, type ContextMethod } from "../lib/compactionMethod";

// ContextMethodBadge shows what a run does with its context as the window
// fills — "append · auto-compact at 80%", "recap · auto at 85%", "stateful" —
// with the details (what a compaction keeps, the summary model, …) as its
// tooltip. See lib/compactionMethod for how the label is derived.
export default function ContextMethodBadge({ method }: { method: ContextMethod | null }) {
  if (!method) return null;
  return (
    <span className="ctx-method" title={method.details.join("\n")}>
      {method.label}
    </span>
  );
}

// RunContextMethod labels a run from its own recorded configuration (the
// single-run read's `spec`: per-run overrides merged over the definition when
// it started). Nothing renders when the run carries no spec.
export function RunContextMethod({ run }: { run: Pick<Agent, "spec" | "interactive"> }) {
  return <ContextMethodBadge method={contextMethodFromSpec(run.spec, { interactive: run.interactive })} />;
}

// useRunContextMethod reads a live run's recorded configuration once its id is
// known, for the live pane — which streams events and holds no run row. A new
// run id (a continuation is a new run) reads again; a failed read shows nothing.
export function useRunContextMethod(runId: string | undefined): ContextMethod | null {
  const [method, setMethod] = useState<ContextMethod | null>(null);
  useEffect(() => {
    setMethod(null);
    if (!runId) return;
    let cancelled = false;
    getRun(runId)
      .then((a) => {
        if (!cancelled) setMethod(contextMethodFromSpec(a.spec, { interactive: a.interactive }));
      })
      .catch(() => {
        // Best-effort: the label is informational, and the pane works without it.
      });
    return () => {
      cancelled = true;
    };
  }, [runId]);
  return method;
}
