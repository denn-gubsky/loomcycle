import { useEffect, useRef, useState } from "react";
import TerminalTranscript from "./TerminalTranscript";
import {
  type LiveUsage,
  type PendingInterrupt,
  type RunStatus,
} from "../hooks/useRunStream";
import { type TranscriptEvent } from "../api";

// LiveRunPane renders one run's live transcript + controls. Decoupled
// from useRunStream (takes plain props) so both the single-run tab and
// each ensemble cell can render it. Shows a status pill, the streamed
// transcript via the shared TerminalTranscript, a Cancel button while
// running, an inline Interruption prompt (Claude-Code-style) when the agent
// asks a question, and an always-on terminal prompt: while the run is live it
// STEERS the agent mid-flight (or resumes a parked interactive run); between
// turns it CONTINUES the session. (`onContinue` is the legacy between-turns-
// only fallback for ensemble cells that don't pass `onSend`.)
export default function LiveRunPane({
  events,
  status,
  agentId,
  sessionId,
  runId,
  error,
  onCancel,
  onContinue,
  onSend,
  awaitingInput,
  lastUsage,
  pendingInterrupt,
  onAnswerInterrupt,
  compact,
}: {
  events: TranscriptEvent[];
  status: RunStatus;
  agentId: string;
  sessionId: string;
  runId?: string;
  error: string | null;
  onCancel: () => void;
  onContinue?: (prompt: string) => void;
  onSend?: (text: string) => void;
  awaitingInput?: boolean;
  lastUsage?: LiveUsage | null;
  pendingInterrupt?: PendingInterrupt | null;
  onAnswerInterrupt?: (answer: string) => void;
  compact?: boolean;
}) {
  const [followUp, setFollowUp] = useState("");
  const taRef = useRef<HTMLTextAreaElement>(null);
  // Auto-grow the input with its content (up to a cap), and shrink back to one
  // line after a send clears it.
  useEffect(() => {
    const ta = taRef.current;
    if (!ta) return;
    ta.style.height = "auto";
    ta.style.height = `${Math.min(ta.scrollHeight, 160)}px`;
  }, [followUp]);

  const running = status === "running";
  // The terminal prompt is available once a run exists (so steering works as
  // soon as the run_id is known), hidden while an interruption is pending
  // (that prompt takes precedence) and before any run starts.
  const promptHandler = onSend ?? onContinue;
  const showPrompt =
    !!promptHandler && !pendingInterrupt && status !== "idle" && !!(sessionId || runId);
  // The legacy onContinue can only fire between turns; onSend handles both.
  const promptDisabled = !onSend && running;

  const doSend = () => {
    if (!promptHandler || !followUp.trim() || promptDisabled) return;
    promptHandler(followUp.trim());
    setFollowUp("");
  };
  const handleSend = (e: React.FormEvent) => {
    e.preventDefault();
    doSend();
  };
  // Enter sends; Shift+Enter inserts a soft newline (multi-line editing).
  const onKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      doSend();
    }
  };

  return (
    <div className={compact ? "live-run-pane live-run-pane-compact" : "live-run-pane"}>
      <div className="live-run-header">
        <span className={`pill ${status}`}>{status}</span>
        {agentId && <code className="live-run-agentid">{agentId}</code>}
        <ContextGauge usage={lastUsage} />
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

      {awaitingInput && running && (
        <div className="live-run-awaiting">● agent is idle — waiting for your input</div>
      )}

      {showPrompt && (
        <form className="live-run-continue" onSubmit={handleSend}>
          <textarea
            ref={taRef}
            className="live-run-input"
            value={followUp}
            onChange={(e) => setFollowUp(e.target.value)}
            onKeyDown={onKeyDown}
            disabled={promptDisabled}
            rows={1}
            placeholder={
              promptDisabled
                ? "Wait for the turn to finish…"
                : running
                  ? awaitingInput
                    ? "Type to continue (Enter to send, Shift+Enter for newline)…"
                    : "Steer the running agent (Enter to send, Shift+Enter for newline)…"
                  : "Continue the conversation (Enter to send, Shift+Enter for newline)…"
            }
          />
          <button type="submit" disabled={!followUp.trim() || promptDisabled}>
            Send
          </button>
        </form>
      )}
    </div>
  );
}

// ContextGauge renders "ctx used / max (pct%)" from the latest usage event.
// Context used = input + cache_read + cache_creation tokens — the true prompt
// footprint for the turn (input_tokens alone undercounts when prompt caching
// is active, since the cached prefix is reported under cache_read, not input).
// Output tokens are excluded (they're the response, not context the prompt
// consumed). When max is unknown (0 — e.g. Ollama) we show only the absolute
// size with no bar/percent.
function ContextGauge({ usage }: { usage?: LiveUsage | null }) {
  if (!usage) return null;
  const used =
    (usage.input_tokens ?? 0) +
    (usage.cache_read_input_tokens ?? 0) +
    (usage.cache_creation_input_tokens ?? 0);
  if (used <= 0) return null;
  const max = usage.max_context_tokens ?? 0;
  const fmtK = (n: number) =>
    n >= 1000 ? `${(n / 1000).toFixed(1)}k` : String(n);
  if (max <= 0) {
    return (
      <span className="ctx-gauge" title="context used (max window unknown)">
        ctx {fmtK(used)}
      </span>
    );
  }
  const pct = Math.min(100, Math.round((used / max) * 100));
  const level = pct >= 90 ? "ctx-high" : pct >= 70 ? "ctx-mid" : "ctx-low";
  return (
    <span className="ctx-gauge" title={`${used} / ${max} tokens`}>
      <span className="ctx-bar">
        <span className={`ctx-bar-fill ${level}`} style={{ width: `${pct}%` }} />
      </span>
      ctx {fmtK(used)} / {fmtK(max)} ({pct}%)
    </span>
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
