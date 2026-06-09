import { useEffect, useState } from "react";
import { listLibraryAgents } from "../api";
import { useUserId } from "../components/Layout";
import { useRunStream } from "../hooks/useRunStream";
import RunForm from "../components/RunForm";
import LiveRunPane from "../components/LiveRunPane";

// RunView is the manual run launcher. v0.24.0 ships the single-run
// experience: pick a defined agent, give it a prompt, fire it, and watch
// the live SSE transcript with cancel + multi-turn continue. The fan-out
// and orchestrator ensemble modes layer on as tabs in a follow-up commit.
export default function RunView() {
  const defaultUserId = useUserId();
  const [agents, setAgents] = useState<string[]>([]);
  const [agentsErr, setAgentsErr] = useState<string | null>(null);
  const run = useRunStream();

  useEffect(() => {
    let cancelled = false;
    listLibraryAgents()
      .then((r) => {
        if (cancelled) return;
        setAgents((r.entries ?? []).map((e) => e.name).sort());
      })
      .catch((e) => {
        if (!cancelled) setAgentsErr(e instanceof Error ? e.message : String(e));
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <div className="run-view">
      <h2>Run an agent</h2>
      {agentsErr && (
        <div className="error-banner">Failed to load agents: {agentsErr}</div>
      )}
      <div className="run-view-body">
        <div className="run-view-form-col">
          <RunForm
            agents={agents}
            defaultUserId={defaultUserId}
            submitting={run.status === "running"}
            onSubmit={(req) => run.start(req)}
          />
        </div>
        <div className="run-view-pane-col">
          <LiveRunPane
            events={run.events}
            status={run.status}
            agentId={run.agentId}
            sessionId={run.sessionId}
            error={run.error}
            onCancel={run.cancel}
            onContinue={run.sendMessage}
          />
        </div>
      </div>
    </div>
  );
}
