import { useEffect } from "react";
import { type StartRunRequest } from "../api";
import { useRunStream } from "../hooks/useRunStream";
import LiveRunPane from "./LiveRunPane";

// EnsembleDashboard renders one live cell per launched run spec. Each cell
// owns its own useRunStream (so the N runs stream independently) and
// auto-starts on mount. A cell is a compact LiveRunPane with per-cell
// cancel. This is the fan-out ensemble view — N independent top-level runs
// watched side by side.
export default function EnsembleDashboard({ specs }: { specs: StartRunRequest[] }) {
  return (
    <div className="ensemble-grid">
      {specs.map((spec, i) => (
        <EnsembleCell key={i} spec={spec} index={i} />
      ))}
    </div>
  );
}

function EnsembleCell({ spec, index }: { spec: StartRunRequest; index: number }) {
  const run = useRunStream();

  // Start once on mount. The specs array is fixed for the lifetime of a
  // batch (the parent remounts the dashboard with a fresh key on relaunch),
  // so an empty-dep effect is the correct "fire once" trigger.
  useEffect(() => {
    run.start(spec);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div className="ensemble-cell">
      <div className="ensemble-cell-title">
        <span className="ensemble-cell-agent">{spec.agent}</span>
        <span className="ensemble-cell-index">#{index + 1}</span>
      </div>
      <LiveRunPane
        events={run.events}
        status={run.status}
        agentId={run.agentId}
        sessionId={run.sessionId}
        error={run.error}
        onCancel={run.cancel}
        compact
      />
    </div>
  );
}
