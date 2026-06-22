import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { type DocScope, type LibraryEntry, listLibraryAgents } from "../api";
import { usePrincipal } from "./Layout";
import { useRunStream } from "../hooks/useRunStream";
import LiveRunPane from "./LiveRunPane";

// DocumentAssistantPanel is the RFC AM Phase 3 Document Assistant: instead of a
// manual structural-editing UI, the operator types instructions and a
// `doc-manager` agent performs the Document ops (semantic import, move, link,
// delete). It reuses useRunStream + LiveRunPane wholesale.
//
// Lifecycle: lazy — the interactive run is spawned on the FIRST instruction
// (token-responsible: opening a document doesn't start a run). Each instruction
// is prefixed with a machine line "[ctx] selected_chunk_id=<id>" so the agent
// always knows the live selection; the first one starts the run (metadata
// {document_id, scope}), the rest steer it. The run's user_id is the principal's
// subject, so the agent's Document tool (scope=user) operates on the same
// documents the viewer shows. onChanged fires at each turn boundary to refresh.

const ASSISTANT_AGENT = "doc-manager";

export interface DocumentAssistantPanelProps {
  documentId: string;
  scope: DocScope;
  selectedChunkId?: string;
  onChanged: () => void;
}

export default function DocumentAssistantPanel({
  documentId,
  scope,
  selectedChunkId,
  onChanged,
}: DocumentAssistantPanelProps) {
  const principal = usePrincipal();
  const run = useRunStream();
  const [available, setAvailable] = useState<boolean | null>(null); // null = checking
  const [started, setStarted] = useState(false);
  const [draft, setDraft] = useState("");

  // Read the live selection through a ref so the send closures aren't stale.
  const chunkRef = useRef<string | undefined>(selectedChunkId);
  useEffect(() => {
    chunkRef.current = selectedChunkId;
  }, [selectedChunkId]);

  // Graceful degrade: confirm the assistant agent is registered.
  useEffect(() => {
    let cancelled = false;
    listLibraryAgents()
      .then((r) => {
        if (cancelled) return;
        const ok = (r.entries ?? []).some(
          (e: LibraryEntry) => e.name === ASSISTANT_AGENT && (e.in_static || e.in_substrate),
        );
        setAvailable(ok);
      })
      .catch(() => !cancelled && setAvailable(false));
    return () => {
      cancelled = true;
    };
  }, []);

  const withCtx = useCallback((text: string) => {
    const id = chunkRef.current;
    return id ? `[ctx] selected_chunk_id=${id}\n\n${text}` : text;
  }, []);

  const send = useCallback(
    (text: string) => {
      const t = text.trim();
      if (!t) return;
      if (!started) {
        run.start({
          agent: ASSISTANT_AGENT,
          prompt: withCtx(t),
          user_id: principal?.subject || undefined,
          interactive: true,
          metadata: { document_id: documentId, scope },
        });
        setStarted(true);
      } else {
        run.send(withCtx(t));
      }
    },
    [started, run, withCtx, principal, documentId, scope],
  );

  // Refresh the viewer at each turn boundary: when the agent parks awaiting
  // input, or when the run completes. Edge-triggered so it fires once per turn.
  const prevAwait = useRef(false);
  useEffect(() => {
    if (run.awaitingInput && !prevAwait.current) onChanged();
    prevAwait.current = run.awaitingInput;
  }, [run.awaitingInput, onChanged]);
  const prevDone = useRef(false);
  useEffect(() => {
    const done = run.status === "completed";
    if (done && !prevDone.current) onChanged();
    prevDone.current = done;
  }, [run.status, onChanged]);

  if (available === false) {
    return (
      <div className="doc-assistant doc-assistant-hint">
        <p>
          The Document Assistant needs the <code>{ASSISTANT_AGENT}</code> agent. Enable the{" "}
          <code>bundles/document-agent</code> bundle (see its README), then reload.
        </p>
      </div>
    );
  }

  if (started) {
    return (
      <div className="doc-assistant">
        <LiveRunPane
          events={run.events}
          status={run.status}
          agentId={run.agentId}
          sessionId={run.sessionId}
          runId={run.runId}
          error={run.error}
          onCancel={run.cancel}
          onSend={send}
          awaitingInput={run.awaitingInput}
          lastUsage={run.lastUsage}
          pendingInterrupt={run.pendingInterrupt}
          onAnswerInterrupt={run.answerInterrupt}
          compact
        />
      </div>
    );
  }

  const submit = (e: FormEvent) => {
    e.preventDefault();
    send(draft);
    setDraft("");
  };
  return (
    <div className="doc-assistant">
      <form className="doc-assistant-start" onSubmit={submit}>
        <textarea
          className="doc-assistant-input"
          rows={2}
          value={draft}
          placeholder={
            available === null
              ? "Checking assistant…"
              : "Ask the assistant to restructure, import, link, or clean up this document…"
          }
          disabled={available !== true}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              send(draft);
              setDraft("");
            }
          }}
        />
        <button type="submit" className="primary" disabled={available !== true || !draft.trim()}>
          send
        </button>
      </form>
    </div>
  );
}
