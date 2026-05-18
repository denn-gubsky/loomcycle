import { useEffect, useState } from "react";
import { getRuntimeState, pauseRuntime, resumeRuntime, RuntimeStateResponse } from "../api";

// PauseControls is the v0.8.17 runtime-quiesce surface in the topbar.
// Polls /v1/_state every 5 s; renders a pill showing the current state
// + paused-runs count and a Pause/Resume button. Operator interaction
// hits POST /v1/_pause or /v1/_resume; errors surface inline with a
// 4-second auto-dismiss.
//
// Design intent: this is an OPERATOR control, not an agent control.
// The Layout component renders it only when the page is loaded by
// someone with a valid bearer-cookie session — which is the same
// posture all other admin pages (Memory, Interrupts) assume.
const POLL_MS = 5_000;

export default function PauseControls() {
  const [state, setState] = useState<RuntimeStateResponse | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [flash, setFlash] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const poll = async () => {
      try {
        const s = await getRuntimeState();
        if (!cancelled) {
          setState(s);
          setErr(null);
        }
      } catch (e) {
        // 503 happens when SetPauseManager wasn't called on the server.
        // Treat as a soft "feature not wired" rather than a noisy error
        // — the operator sees the pill greyed out and no button.
        const msg = e instanceof Error ? e.message : String(e);
        if (msg.includes("503")) {
          if (!cancelled) setState(null);
        } else if (!cancelled) {
          setErr(msg);
        }
      }
    };
    poll();
    const t = setInterval(poll, POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, []);

  // Auto-dismiss flash messages so the topbar doesn't accumulate
  // stale feedback after a successful action.
  useEffect(() => {
    if (!flash) return;
    const t = setTimeout(() => setFlash(null), 4_000);
    return () => clearTimeout(t);
  }, [flash]);

  const doPause = async () => {
    if (busy) return;
    if (!confirm("Pause the runtime? In-flight idempotent tools are cancelled immediately; non-idempotent tools get a 30-second wind-down. New runs return 503 until you resume.")) {
      return;
    }
    setBusy(true);
    try {
      const res = await pauseRuntime();
      setState({ state: res.state as RuntimeStateResponse["state"], paused_runs_count: res.paused_runs_count });
      setFlash(
        `paused (${res.duration_ms} ms, ${res.force_cancelled_count} force-cancelled, ${res.paused_runs_count} paused runs)`,
      );
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const doResume = async () => {
    if (busy) return;
    setBusy(true);
    try {
      const res = await resumeRuntime();
      setState({ state: res.state as RuntimeStateResponse["state"], paused_runs_count: 0 });
      setFlash(`resumed (${res.resumed_runs_count} runs)`);
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  // No pause manager wired on this server — render nothing rather
  // than a confusing greyed-out pill. The Snapshots page will surface
  // the same gap with a clearer message when the user navigates to it.
  if (state === null && !err) {
    return null;
  }

  return (
    <div className="pause-controls">
      {state && (
        <span className={`runtime-state runtime-state-${state.state}`} title={`paused_runs_count=${state.paused_runs_count}`}>
          {state.state}
          {state.paused_runs_count > 0 && ` · ${state.paused_runs_count}`}
        </span>
      )}
      {state?.state === "running" && (
        <button type="button" disabled={busy} onClick={doPause} title="Quiesce the runtime: cancel idempotent tools, drain non-idempotent ones, reject new runs.">
          {busy ? "pausing…" : "pause"}
        </button>
      )}
      {state?.state === "paused" && (
        <button type="button" disabled={busy} onClick={doResume} title="Release the pause: resume paused runs, accept new runs again.">
          {busy ? "resuming…" : "resume"}
        </button>
      )}
      {state?.state === "pausing" && <span className="hint">draining…</span>}
      {flash && <span className="flash">{flash}</span>}
      {err && (
        <span className="err" title={err}>
          {err.split(":")[0]}
        </span>
      )}
    </div>
  );
}
