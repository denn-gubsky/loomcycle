import { useCallback, useEffect, useRef, useState } from "react";
import {
  cancelAgent,
  continueSession,
  resolveInterrupt,
  startRun,
  sseEventToTranscript,
  type EventPayload,
  type StartRunRequest,
  type TranscriptEvent,
} from "../api";

// PendingInterrupt is the inline-prompt shape the terminal renders when the
// running agent raises an Interruption question (the `interruption_pending`
// SSE event). Answering it resolves the interrupt and the SAME stream resumes.
export interface PendingInterrupt {
  interruptId: string;
  question: string;
  options?: string[];
  context?: string;
  priority?: string;
  expiresAt?: string;
}

// useRunStream owns one live run's stream lifecycle: it POSTs /v1/runs (or
// a continuation), reduces the SSE frames into the TranscriptEvent[] shape
// TerminalTranscript renders, captures the run's agent_id/session_id/run_id
// from the side-channel frames, and tracks status. Shared by the single
// run pane and each ensemble cell so the reducer logic lives in one place.
//
// The `session`/`agent` frames are handled out-of-band (id capture); every
// other frame is a providers.Event pushed to the transcript. Stream EOF
// (the startRun promise resolving) finalizes a still-"running" status to
// "completed"; a `done`/`error` frame sets it first.

export type RunStatus =
  | "idle"
  | "running"
  | "completed"
  | "failed"
  | "cancelled";

export interface UseRunStream {
  events: TranscriptEvent[];
  status: RunStatus;
  agentId: string;
  sessionId: string;
  runId: string;
  error: string | null;
  // pendingInterrupt is set while the running agent is blocked on an
  // Interruption question; answerInterrupt resolves it inline and the stream
  // resumes. null when nothing is pending.
  pendingInterrupt: PendingInterrupt | null;
  answerInterrupt: (answer: string) => void;
  start: (req: StartRunRequest) => void;
  sendMessage: (prompt: string) => void;
  cancel: () => void;
  reset: () => void;
}

export function useRunStream(): UseRunStream {
  const [events, setEvents] = useState<TranscriptEvent[]>([]);
  const [status, setStatus] = useState<RunStatus>("idle");
  const [agentId, setAgentId] = useState("");
  const [sessionId, setSessionId] = useState("");
  const [runId, setRunId] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [pendingInterrupt, setPendingInterrupt] = useState<PendingInterrupt | null>(null);

  const seqRef = useRef(0);
  const ctrlRef = useRef<AbortController | null>(null);
  // runId mirror so answerInterrupt resolves against the live run_id without a
  // stale-closure race (the click happens after the `agent` frame set it).
  const runIdRef = useRef("");

  // Abort the in-flight stream on unmount so a navigated-away run doesn't
  // keep a dangling reader. (Does not cancel the run server-side — that's
  // an explicit cancel() via the button.)
  useEffect(() => () => ctrlRef.current?.abort(), []);

  const onFrame = useCallback((f: { event: string; data: string }) => {
    let parsed: Record<string, unknown>;
    try {
      parsed = JSON.parse(f.data);
    } catch {
      return; // skip a malformed frame rather than break the stream
    }
    if (f.event === "session") {
      const sid = typeof parsed.text === "string" ? parsed.text : "";
      if (sid) setSessionId(sid);
      return;
    }
    if (f.event === "agent") {
      if (typeof parsed.agent_id === "string") setAgentId(parsed.agent_id);
      if (typeof parsed.run_id === "string") {
        setRunId(parsed.run_id);
        runIdRef.current = parsed.run_id;
      }
      if (typeof parsed.session_id === "string" && parsed.session_id)
        setSessionId(parsed.session_id);
      return;
    }
    const ev = parsed as unknown as EventPayload;
    if (ev.type === "done") {
      setStatus("completed");
      setPendingInterrupt(null);
    }
    if (ev.type === "error") {
      setStatus("failed");
      if (typeof ev.error === "string") setError(ev.error);
      setPendingInterrupt(null);
    }
    // The agent raised an Interruption question and is now blocked; surface it
    // as an inline prompt. The event is also pushed to the transcript below so
    // it stays in the scrollback.
    if (ev.type === "interruption_pending" && ev.interruption?.interrupt_id) {
      const i = ev.interruption;
      setPendingInterrupt({
        interruptId: i.interrupt_id,
        question: i.question ?? "",
        options: i.options,
        context: i.context,
        priority: i.priority,
        expiresAt: i.expires_at,
      });
    }
    setEvents((prev) => [...prev, sseEventToTranscript(seqRef.current++, ev)]);
  }, []);

  const answerInterrupt = useCallback(
    (answer: string) => {
      const rid = runIdRef.current;
      const cur = pendingInterrupt;
      if (!rid || !cur) return;
      // Optimistically clear the prompt; the agent wakes and the same stream
      // resumes. Surface a failure (e.g. already resolved / expired) in the
      // error banner.
      setPendingInterrupt(null);
      void resolveInterrupt(rid, cur.interruptId, answer).catch((e) => {
        setError(e instanceof Error ? e.message : String(e));
      });
    },
    [pendingInterrupt],
  );

  const runStream = useCallback(
    (promise: Promise<void>) => {
      promise
        .then(() => {
          // EOF — if no done/error frame moved us off "running", treat the
          // stream close as completion.
          setStatus((s) => (s === "running" ? "completed" : s));
        })
        .catch((e) => {
          // AbortError = we tore down the reader (cancel/unmount); not a
          // failure to surface.
          if (e instanceof DOMException && e.name === "AbortError") return;
          setStatus("failed");
          setError(e instanceof Error ? e.message : String(e));
        });
    },
    [],
  );

  const start = useCallback(
    (req: StartRunRequest) => {
      ctrlRef.current?.abort();
      const ctrl = new AbortController();
      ctrlRef.current = ctrl;
      seqRef.current = 0;
      setEvents([]);
      setAgentId(req.agent_id ?? "");
      setSessionId("");
      setRunId("");
      runIdRef.current = "";
      setError(null);
      setPendingInterrupt(null);
      setStatus("running");
      runStream(startRun(req, { onFrame, signal: ctrl.signal }));
    },
    [onFrame, runStream],
  );

  const sendMessage = useCallback(
    (prompt: string) => {
      if (!sessionId) return;
      ctrlRef.current?.abort(); // tear down any prior reader before the new turn
      const ctrl = new AbortController();
      ctrlRef.current = ctrl;
      setError(null);
      setPendingInterrupt(null);
      setStatus("running");
      runStream(
        continueSession(sessionId, prompt, { onFrame, signal: ctrl.signal }),
      );
    },
    [sessionId, onFrame, runStream],
  );

  const cancel = useCallback(() => {
    if (agentId) {
      void cancelAgent(agentId, "cancelled from UI").catch(() => {
        // best-effort; the abort below tears down the local reader anyway
      });
    }
    ctrlRef.current?.abort();
    setStatus((s) => (s === "running" ? "cancelled" : s));
  }, [agentId]);

  const reset = useCallback(() => {
    ctrlRef.current?.abort();
    seqRef.current = 0;
    setEvents([]);
    setAgentId("");
    setSessionId("");
    setRunId("");
    runIdRef.current = "";
    setError(null);
    setPendingInterrupt(null);
    setStatus("idle");
  }, []);

  return {
    events,
    status,
    agentId,
    sessionId,
    runId,
    error,
    pendingInterrupt,
    answerInterrupt,
    start,
    sendMessage,
    cancel,
    reset,
  };
}
