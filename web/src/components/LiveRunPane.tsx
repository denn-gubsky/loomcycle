import { useState } from "react";
import TerminalTranscript from "./TerminalTranscript";
import { type RunStatus } from "../hooks/useRunStream";
import { type TranscriptEvent } from "../api";

// LiveRunPane renders one run's live transcript + controls. Decoupled
// from useRunStream (takes plain props) so both the single-run tab and
// each ensemble cell can render it. Shows a status pill, the streamed
// transcript via the shared TerminalTranscript, a Cancel button while
// running, and a Continue input once a session exists (multi-turn).
export default function LiveRunPane({
  events,
  status,
  agentId,
  sessionId,
  error,
  onCancel,
  onContinue,
  compact,
}: {
  events: TranscriptEvent[];
  status: RunStatus;
  agentId: string;
  sessionId: string;
  error: string | null;
  onCancel: () => void;
  onContinue?: (prompt: string) => void;
  compact?: boolean;
}) {
  const [followUp, setFollowUp] = useState("");

  const handleContinue = (e: React.FormEvent) => {
    e.preventDefault();
    if (!onContinue || !followUp.trim()) return;
    onContinue(followUp.trim());
    setFollowUp("");
  };

  return (
    <div className={compact ? "live-run-pane live-run-pane-compact" : "live-run-pane"}>
      <div className="live-run-header">
        <span className={`pill ${status}`}>{status}</span>
        {agentId && <code className="live-run-agentid">{agentId}</code>}
        {status === "running" && (
          <button type="button" className="live-run-cancel" onClick={onCancel}>
            Cancel
          </button>
        )}
      </div>

      {error && <div className="error-banner">{error}</div>}

      {events.length === 0 && status === "idle" ? (
        <div className="empty-state">No run yet.</div>
      ) : (
        <TerminalTranscript events={events} />
      )}

      {onContinue && sessionId && status !== "running" && status !== "idle" && (
        <form className="live-run-continue" onSubmit={handleContinue}>
          <input
            type="text"
            value={followUp}
            onChange={(e) => setFollowUp(e.target.value)}
            placeholder="Continue the conversation…"
          />
          <button type="submit" disabled={!followUp.trim()}>
            Send
          </button>
        </form>
      )}
    </div>
  );
}
