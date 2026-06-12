import { useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { listLibraryAgents, type StartRunRequest } from "../api";
import { useUserId } from "../components/Layout";
import { useRunStream } from "../hooks/useRunStream";
import RunForm from "../components/RunForm";
import LiveRunPane from "../components/LiveRunPane";
import FanOutForm from "../components/FanOutForm";
import EnsembleDashboard from "../components/EnsembleDashboard";
import OrchestratorView from "../components/OrchestratorView";

// RunView is the manual run launcher with three modes:
//   single       — one agent, one prompt, live transcript + multi-turn
//   fanout       — N independent top-level runs in a live grid
//   orchestrator — one parent agent that parallel_spawns a live child tree
type Tab = "single" | "fanout" | "orchestrator";

export default function RunView() {
  const defaultUserId = useUserId();
  const [agents, setAgents] = useState<string[]>([]);
  const [agentsErr, setAgentsErr] = useState<string | null>(null);
  const [tab, setTab] = useState<Tab>("single");

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
      <div className="run-view-head">
        <h2>Run an agent</h2>
        <div className="run-tabs">
          <TabButton tab="single" current={tab} onClick={setTab}>
            Single
          </TabButton>
          <TabButton tab="fanout" current={tab} onClick={setTab}>
            Fan-out
          </TabButton>
          <TabButton tab="orchestrator" current={tab} onClick={setTab}>
            Orchestrator
          </TabButton>
        </div>
      </div>
      {agentsErr && (
        <div className="error-banner">Failed to load agents: {agentsErr}</div>
      )}

      {tab === "single" && (
        <SingleRunTab agents={agents} defaultUserId={defaultUserId} />
      )}
      {tab === "fanout" && (
        <FanOutTab agents={agents} defaultUserId={defaultUserId} />
      )}
      {tab === "orchestrator" && (
        <OrchestratorView agents={agents} defaultUserId={defaultUserId} />
      )}
    </div>
  );
}

function TabButton({
  tab,
  current,
  onClick,
  children,
}: {
  tab: Tab;
  current: Tab;
  onClick: (t: Tab) => void;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      className={`run-tab${tab === current ? " run-tab-active" : ""}`}
      onClick={() => onClick(tab)}
    >
      {children}
    </button>
  );
}

function SingleRunTab({
  agents,
  defaultUserId,
}: {
  agents: string[];
  defaultUserId: string;
}) {
  const run = useRunStream();
  const [searchParams, setSearchParams] = useSearchParams();
  // `?attach=<run_id>` re-attaches to a detached interactive run (the operator
  // returned from the runs list). Attach once on mount, then drop the param so
  // a manual reset/new-run isn't re-hijacked by a stale URL.
  const attachRunId = searchParams.get("attach");
  const attachedRef = useRef<string | null>(null);
  useEffect(() => {
    if (attachRunId && attachedRef.current !== attachRunId) {
      attachedRef.current = attachRunId;
      run.attach(attachRunId);
      const next = new URLSearchParams(searchParams);
      next.delete("attach");
      setSearchParams(next, { replace: true });
    }
  }, [attachRunId, run, searchParams, setSearchParams]);

  return (
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
          runId={run.runId}
          error={run.error}
          onCancel={run.cancel}
          onSend={run.send}
          onCompact={run.compact}
          awaitingInput={run.awaitingInput}
          lastUsage={run.lastUsage}
          pendingInterrupt={run.pendingInterrupt}
          onAnswerInterrupt={run.answerInterrupt}
        />
      </div>
    </div>
  );
}

function FanOutTab({
  agents,
  defaultUserId,
}: {
  agents: string[];
  defaultUserId: string;
}) {
  const [batch, setBatch] = useState<StartRunRequest[] | null>(null);
  const [batchKey, setBatchKey] = useState(0);

  return (
    <div className="fanout-tab">
      {batch === null ? (
        <FanOutForm
          agents={agents}
          defaultUserId={defaultUserId}
          disabled={false}
          onLaunch={(reqs) => {
            setBatch(reqs);
            setBatchKey((k) => k + 1);
          }}
        />
      ) : (
        <>
          <div className="fanout-running-bar">
            <span>{batch.length} runs launched</span>
            <button type="button" onClick={() => setBatch(null)}>
              New batch
            </button>
          </div>
          <EnsembleDashboard key={batchKey} specs={batch} />
        </>
      )}
    </div>
  );
}
