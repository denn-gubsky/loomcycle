import { useState } from "react";
import TerminalTranscript from "./TerminalTranscript";
import { type PendingInterrupt, type RunStatus } from "../hooks/useRunStream";
import { type TranscriptEvent } from "../api";

// LiveRunPane renders one run's live transcript + controls. Decoupled
// from useRunStream (takes plain props) so both the single-run tab and
// each ensemble cell can render it. Shows a status pill, the streamed
// transcript via the shared TerminalTranscript, a Cancel button while
// running, an inline Interruption prompt (Claude-Code-style) when the agent
// asks a question, and a Continue input once a session exists (multi-turn).
export default function LiveRunPane({
  events,
  status,
  agentId,
  sessionId,
  error,
  onCancel,
  onContinue,
  pendingInterrupt,
  onAnswerInterrupt,
  compact,
}: {
  events: TranscriptEvent[];
  status: RunStatus;
  agentId: string;
  sessionId: string;
  error: string | null;
  onCancel: () => void;
  onContinue?: (prompt: string) => void;
  pendingInterrupt?: PendingInterrupt | null;
  onAnswerInterrupt?: (answer: string) => void;
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

      {/* Inline interruption prompt — the agent is blocked on a question; the
          same SSE stream resumes once it's answered. Takes precedence over the
          continue box (which only shows between turns). */}
      {pendingInterrupt && onAnswerInterrupt && (
        <InterruptPrompt pending={pendingInterrupt} onAnswer={onAnswerInterrupt} />
      )}

      {onContinue &&
        sessionId &&
        !pendingInterrupt &&
        status !== "running" &&
        status !== "idle" && (
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

// InterruptPrompt renders the agent's pending question inline: a button per
// option when the ask declared a fixed set, else a free-text answer box.
function InterruptPrompt({
  pending,
  onAnswer,
}: {
  pending: PendingInterrupt;
  onAnswer: (answer: string) => void;
}) {
  const [text, setText] = useState("");
  const hasOptions = pending.options && pending.options.length > 0;

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!text.trim()) return;
    onAnswer(text.trim());
    setText("");
  };

  return (
    <div className="live-run-interrupt">
      <div className="live-run-interrupt-q">
        <span className="live-run-interrupt-badge">interruption</span>
        {pending.priority && pending.priority !== "normal" && (
          <span className={`live-run-interrupt-prio prio-${pending.priority}`}>
            {pending.priority}
          </span>
        )}
        <span>{pending.question || "(the agent is waiting for input)"}</span>
      </div>
      {pending.context && (
        <div className="live-run-interrupt-context">{pending.context}</div>
      )}
      {hasOptions ? (
        <div className="live-run-interrupt-options">
          {pending.options!.map((opt) => (
            <button
              key={opt}
              type="button"
              className="live-run-interrupt-option"
              onClick={() => onAnswer(opt)}
            >
              {opt}
            </button>
          ))}
        </div>
      ) : (
        <form className="live-run-interrupt-form" onSubmit={submit}>
          <input
            type="text"
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder="Type your answer…"
            autoFocus
          />
          <button type="submit" disabled={!text.trim()}>
            Answer
          </button>
        </form>
      )}
    </div>
  );
}
