import { useEffect, useState } from "react";
import { type Agent, getRun } from "../api";
import AgentDetailPane from "./AgentDetailPane";
import TeamWalkPane from "./TeamWalkPane";
import type { BreadcrumbAncestor } from "./Breadcrumbs";
import { runPaneFor } from "../lib/walkView";
import { isTeamWalkAgentId, type RunSelection } from "../lib/runLineage";

// RunDetail picks the runs page's right-hand pane: TeamWalkPane for a team
// walk (agent_id "team:<name>"), AgentDetailPane for every other run.
//
// The listed row decides when there is one. A ?run= the listing does not hold
// — a finished walk under the default "running" filter, a link from elsewhere
// — is read once to learn its agent id before either pane mounts, so a walk
// never flashes through the agent pane.
export interface RunDetailProps {
  agents: readonly Agent[];
  selectedKey?: string;
  // The run the pane reads (paneRunId): ?run=, or for a legacy ?agent=team:…
  // link the newest listed walk's run id.
  runId?: string;
  agentId?: string;
  ancestors?: BreadcrumbAncestor[];
  onSelect?: (sel: RunSelection) => void;
}

export default function RunDetail({ agents, selectedKey, runId, agentId, ancestors, onSelect }: RunDetailProps) {
  const choice = runPaneFor(agents, selectedKey, runId);
  const unknownRunId = choice.kind === "unknown" ? choice.runId : undefined;
  // What the one read said: the run id it was for, and whether it is a walk.
  // undefined isWalk = the read failed; the agent pane then reports the error.
  const [resolved, setResolved] = useState<{ runId: string; isWalk?: boolean } | null>(null);

  useEffect(() => {
    if (!unknownRunId || resolved?.runId === unknownRunId) return;
    let cancelled = false;
    getRun(unknownRunId)
      .then((a) => {
        if (!cancelled) setResolved({ runId: unknownRunId, isWalk: isTeamWalkAgentId(a.agent_id) });
      })
      .catch(() => {
        if (!cancelled) setResolved({ runId: unknownRunId });
      });
    return () => {
      cancelled = true;
    };
  }, [unknownRunId, resolved?.runId]);

  if (choice.kind === "walk") return <TeamWalkPane runId={choice.runId} />;
  if (choice.kind === "unknown") {
    if (resolved?.runId !== choice.runId) {
      return (
        <div className="agent-detail">
          <div className="empty">loading…</div>
        </div>
      );
    }
    if (resolved.isWalk) return <TeamWalkPane runId={choice.runId} />;
  }
  return <AgentDetailPane runId={runId} agentId={agentId} ancestors={ancestors} onSelect={onSelect} />;
}
