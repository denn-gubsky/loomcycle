import { useEffect, useState } from "react";
import { type Agent, type StartRunRequest, listAgents } from "../api";
import { useRunStream } from "../hooks/useRunStream";
import { buildTree } from "./AgentsTree";
import AgentsTree from "./AgentsTree";
import AgentDetailPane from "./AgentDetailPane";
import RunForm from "./RunForm";
import LiveRunPane from "./LiveRunPane";

// OrchestratorView launches ONE run of an orchestrator agent (one whose
// definition drives Agent.parallel_spawn) and shows the parent's live SSE
// transcript alongside the live parent→child tree. The tree reuses the
// existing AgentsTree + AgentDetailPane, fed by polling listAgents(userId)
// (real Agent rows, full fidelity) rather than a lossy run-state adapter —
// the children share the parent's user_id, so they appear in that list as
// the orchestrator spawns them. This is the "team coordinated by an agent"
// ensemble mode.
const POLL_MS = 3_000;

export default function OrchestratorView({
  agents,
  defaultUserId,
}: {
  agents: string[];
  defaultUserId: string;
}) {
  const run = useRunStream();
  const [treeUserId, setTreeUserId] = useState("");
  const [children, setChildren] = useState<Agent[]>([]);
  const [selected, setSelected] = useState<string | undefined>(undefined);

  const launched = run.status !== "idle";

  // Poll the run's user_id scope for the spawned tree once launched.
  useEffect(() => {
    if (!launched || !treeUserId) return;
    let cancelled = false;
    const fetchOnce = async () => {
      try {
        const r = await listAgents(treeUserId);
        if (!cancelled) setChildren(r.agents ?? []);
      } catch {
        // best-effort; the parent transcript is the primary signal
      }
    };
    fetchOnce();
    const t = setInterval(fetchOnce, POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [launched, treeUserId]);

  const tree = buildTree(children);

  const handleSubmit = (req: StartRunRequest) => {
    setTreeUserId(req.user_id ?? "");
    setChildren([]);
    setSelected(undefined);
    run.start(req);
  };

  return (
    <div className="orchestrator-view">
      <RunForm
        agents={agents}
        defaultUserId={defaultUserId}
        submitting={run.status === "running"}
        onSubmit={handleSubmit}
      />

      {launched && (
        <div className="orchestrator-body">
          <div className="orchestrator-parent">
            <h4>Orchestrator (parent)</h4>
            <LiveRunPane
              events={run.events}
              status={run.status}
              agentId={run.agentId}
              sessionId={run.sessionId}
              error={run.error}
              onCancel={run.cancel}
            />
          </div>
          <div className="orchestrator-children">
            <h4>Spawned tree {children.length > 0 ? `(${children.length})` : ""}</h4>
            {!treeUserId ? (
              <div className="empty-state">
                Set a <code>user_id</code> in the form's advanced section to
                track the spawned children.
              </div>
            ) : tree.length === 0 ? (
              <div className="empty-state">
                No spawned agents yet — they appear here as the orchestrator
                calls Agent.parallel_spawn.
              </div>
            ) : (
              <AgentsTree tree={tree} selectedId={selected} onSelect={setSelected} />
            )}
            {selected && (
              <AgentDetailPane agentId={selected} onSelect={setSelected} />
            )}
          </div>
        </div>
      )}
    </div>
  );
}
