import { useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  ChannelCursorEntry,
  InterruptRow,
  MemoryEntry,
  listAgentChannels,
  listMemoryEntries,
  listRunInterrupts,
} from "../api";

// AgentDetailTabs is the per-agent sub-tab strip + the three new
// scoped-view components (Memory / Interrupts / Channels) that
// hang off the existing AgentDetailPane. The default tab is
// "transcript" — when active, AgentDetailPane renders its existing
// event list as before. The other three tabs are read-only views
// over data scoped to the current agent.

export type AgentTab = "transcript" | "memory" | "interrupts" | "channels";

// useAgentTab persists the active sub-tab in the URL search param
// `?tab=` so deep-links + refreshes preserve the operator's choice.
// Default = "transcript" (param absent or any unrecognised value).
export function useAgentTab(): [AgentTab, (t: AgentTab) => void] {
  const [searchParams, setSearchParams] = useSearchParams();
  const raw = searchParams.get("tab");
  const tab: AgentTab =
    raw === "memory" || raw === "interrupts" || raw === "channels" ? raw : "transcript";
  const set = (t: AgentTab) => {
    const next = new URLSearchParams(searchParams);
    if (t === "transcript") {
      next.delete("tab");
    } else {
      next.set("tab", t);
    }
    setSearchParams(next, { replace: true });
  };
  return [tab, set];
}

export interface AgentTabStripProps {
  tab: AgentTab;
  onChange: (t: AgentTab) => void;
}

export function AgentTabStrip({ tab, onChange }: AgentTabStripProps) {
  return (
    <div className="agent-tabs" role="tablist" aria-label="agent detail tabs">
      <TabBtn label="transcript" tab="transcript" current={tab} onChange={onChange} />
      <TabBtn label="memory" tab="memory" current={tab} onChange={onChange} />
      <TabBtn label="interrupts" tab="interrupts" current={tab} onChange={onChange} />
      <TabBtn label="channels" tab="channels" current={tab} onChange={onChange} />
    </div>
  );
}

function TabBtn({
  label,
  tab,
  current,
  onChange,
}: {
  label: string;
  tab: AgentTab;
  current: AgentTab;
  onChange: (t: AgentTab) => void;
}) {
  return (
    <button
      type="button"
      role="tab"
      aria-selected={tab === current}
      className={tab === current ? "agent-tab agent-tab-active" : "agent-tab"}
      onClick={() => onChange(tab)}
    >
      {label}
    </button>
  );
}

// ---- Memory tab: scope=agent, scope_id=agent.agent (the agent name) ----

const MEMORY_LIMIT = 100;

export function MemoryTab({ agentName }: { agentName: string }) {
  const [entries, setEntries] = useState<MemoryEntry[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!agentName) return;
    let cancelled = false;
    setLoading(true);
    setErr(null);
    listMemoryEntries("agent", agentName, undefined, MEMORY_LIMIT)
      .then((r) => {
        if (cancelled) return;
        setEntries(r.entries ?? []);
        setLoading(false);
      })
      .catch((e: Error) => {
        if (cancelled) return;
        setErr(e.message);
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [agentName]);

  if (!agentName) {
    return <div className="empty-state">Agent name unknown — cannot scope memory query.</div>;
  }
  if (err) {
    return <div className="error-banner">Failed to load memory: {err}</div>;
  }
  if (loading) {
    return <div className="loading-indicator">loading memory entries…</div>;
  }
  if (entries.length === 0) {
    return (
      <div className="empty-state">
        No memory entries in <code className="mono">scope=agent / scope_id={agentName}</code>.
      </div>
    );
  }
  return (
    <div className="agent-tab-body">
      <div className="agent-tab-meta">
        <span>scope=agent</span>
        <span>scope_id={agentName}</span>
        <span>{entries.length} entries</span>
      </div>
      <ul className="memory-entry-list">
        {entries.map((e) => (
          <li key={e.key} className="memory-entry">
            <div className="memory-entry-key mono">{e.key}</div>
            <pre className="memory-entry-value mono">
              {tryPretty(e.value)}
            </pre>
            <div className="memory-entry-meta">
              updated {new Date(e.updated_at).toLocaleString()}
            </div>
          </li>
        ))}
      </ul>
      {entries.length >= MEMORY_LIMIT && (
        <div className="agent-tab-note">
          Showing first {MEMORY_LIMIT} entries. For the full picker (search +
          edit), open <a href="/ui/memory">memory</a>.
        </div>
      )}
    </div>
  );
}

// ---- Interrupts tab: scoped to the run_id of the agent ----

export function InterruptsTab({ runID }: { runID: string }) {
  const [interrupts, setInterrupts] = useState<InterruptRow[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!runID) return;
    let cancelled = false;
    setLoading(true);
    setErr(null);
    // Status=all returns pending + resolved + cancelled so the
    // operator sees the whole interrupt history for this run.
    listRunInterrupts(runID, "all")
      .then((r) => {
        if (cancelled) return;
        setInterrupts(r.interrupts ?? []);
        setLoading(false);
      })
      .catch((e: Error) => {
        if (cancelled) return;
        setErr(e.message);
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [runID]);

  if (!runID) {
    return <div className="empty-state">Run id unknown — cannot scope interrupt query.</div>;
  }
  if (err) {
    return <div className="error-banner">Failed to load interrupts: {err}</div>;
  }
  if (loading) {
    return <div className="loading-indicator">loading interrupts…</div>;
  }
  if (interrupts.length === 0) {
    return (
      <div className="empty-state">
        No interrupts on run <code className="mono">{runID}</code>.
      </div>
    );
  }
  return (
    <div className="agent-tab-body">
      <div className="agent-tab-meta">
        <span>run_id={runID}</span>
        <span>{interrupts.length} interrupts</span>
      </div>
      <ul className="interrupt-list">
        {interrupts.map((it) => (
          <li key={it.interrupt_id} className={`interrupt-row interrupt-${it.status}`}>
            <div className="interrupt-header">
              <span className={`pill ${it.status}`}>{it.status}</span>
              <span className="interrupt-kind mono">{it.kind}</span>
              <span className="interrupt-id mono">{it.interrupt_id}</span>
            </div>
            {it.question && <div className="interrupt-question">{it.question}</div>}
            {it.answer && (
              <div className="interrupt-answer">
                <span className="agent-tab-meta-key">answer:</span> {it.answer}
              </div>
            )}
            <div className="agent-tab-meta">
              <span>created {new Date(it.created_at).toLocaleString()}</span>
              {it.resolved_at && (
                <span>resolved {new Date(it.resolved_at).toLocaleString()}</span>
              )}
            </div>
          </li>
        ))}
      </ul>
      <div className="agent-tab-note">
        To resolve a pending interrupt, open <a href="/ui/interrupts">interrupts</a>.
      </div>
    </div>
  );
}

// ---- Channels tab: scope=agent cursors for this agent name ----

export function ChannelsTab({ agentName }: { agentName: string }) {
  const [channels, setChannels] = useState<ChannelCursorEntry[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!agentName) return;
    let cancelled = false;
    setLoading(true);
    setErr(null);
    listAgentChannels(agentName)
      .then((r) => {
        if (cancelled) return;
        setChannels(r.channels ?? []);
        setLoading(false);
      })
      .catch((e: Error) => {
        if (cancelled) return;
        setErr(e.message);
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [agentName]);

  if (!agentName) {
    return <div className="empty-state">Agent name unknown — cannot scope channel query.</div>;
  }
  if (err) {
    return <div className="error-banner">Failed to load channels: {err}</div>;
  }
  if (loading) {
    return <div className="loading-indicator">loading channels…</div>;
  }
  if (channels.length === 0) {
    return (
      <div className="empty-state">
        Agent <code className="mono">{agentName}</code> has no channel cursors.
      </div>
    );
  }
  return (
    <div className="agent-tab-body">
      <div className="agent-tab-meta">
        <span>scope=agent</span>
        <span>scope_id={agentName}</span>
        <span>{channels.length} channels</span>
      </div>
      <ul className="channel-cursor-list">
        {channels.map((c) => (
          <li key={c.channel} className="channel-cursor-row">
            <div className="channel-cursor-name mono">{c.channel}</div>
            <div className="channel-cursor-meta">
              <span className="mono" title={c.cursor}>
                cursor: {shortCursor(c.cursor)}
              </span>
              <span>updated {new Date(c.updated_at).toLocaleString()}</span>
            </div>
          </li>
        ))}
      </ul>
      <div className="agent-tab-note">
        To inspect channel content + recent messages, open{" "}
        <a href="/ui/channels">channels</a>.
      </div>
    </div>
  );
}

function shortCursor(cur: string): string {
  // cur_<16hex>_<msg_id>. Show the first 18 chars (cur_ + visible_at
  // hex) for a compact display; full cursor available via tooltip.
  if (cur.length <= 18) return cur;
  return cur.slice(0, 18) + "…";
}

function tryPretty(value: unknown): string {
  if (typeof value === "string") {
    // Many memory values are JSON-encoded strings; try to parse so
    // the operator sees structure rather than a quoted blob.
    try {
      return JSON.stringify(JSON.parse(value), null, 2);
    } catch {
      return value;
    }
  }
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}
