import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import {
  HealthResponse,
  PresetUnit,
  RuntimeStateResponse,
  getEnvTemplate,
  getHealth,
  getRuntimeState,
  listPresets,
  pauseRuntime,
  resumeRuntime,
  showPreset,
} from "../api";
import { usePrincipal } from "../components/Layout";
import TokenManager from "../components/TokenManager";

// SettingsView is the operator Settings hub (top-bar gear). It web-reaches the
// critical `loomcycle` CLI surfaces so a no-shell deployment (TrueNAS — RFC AR)
// stays operable: tenant/operator tokens (RFC L), the embedded config presets
// (RFC AQ), runtime quiesce, and health. Admin-only — the gear is rendered for
// is_admin in the Layout, and this view re-guards (a tenant that deep-links here
// is told it's operator-only). Surfaces that already have their own pages
// (snapshots, audit) are linked, not duplicated.
type Section = "tokens" | "presets" | "runtime" | "health";

const SECTIONS: { id: Section; label: string }[] = [
  { id: "tokens", label: "Tokens" },
  { id: "presets", label: "Presets" },
  { id: "runtime", label: "Runtime" },
  { id: "health", label: "Health" },
];

export default function SettingsView() {
  const principal = usePrincipal();
  const [section, setSection] = useState<Section>("tokens");

  if (principal && !principal.is_admin) {
    return (
      <div className="settings-view">
        <div className="settings-panel">
          <h2>Settings</h2>
          <p className="settings-muted">
            Settings is operator-admin only. Sign in with an admin (root) token
            to manage tokens, presets, and runtime.
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="settings-view">
      <nav className="settings-tabs">
        {SECTIONS.map((s) => (
          <button
            key={s.id}
            type="button"
            className={"settings-tab" + (section === s.id ? " active" : "")}
            onClick={() => setSection(s.id)}
          >
            {s.label}
          </button>
        ))}
      </nav>
      <div className="settings-body">
        {section === "tokens" && <TokenManager />}
        {section === "presets" && <PresetsSection />}
        {section === "runtime" && <RuntimeSection />}
        {section === "health" && <HealthSection />}
      </div>
    </div>
  );
}

// ─── Presets ─────────────────────────────────────────────────────────────────

function PresetsSection() {
  const [units, setUnits] = useState<PresetUnit[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [yaml, setYaml] = useState<string>("");
  const [yamlBusy, setYamlBusy] = useState(false);

  useEffect(() => {
    listPresets()
      .then((r) => setUnits(r.units ?? []))
      .catch((e) => setErr(e instanceof Error ? e.message : String(e)));
  }, []);

  const view = async (name: string) => {
    setSelected(name);
    setYamlBusy(true);
    try {
      if (name === "__env__") {
        const r = await getEnvTemplate();
        setYaml(r.env);
      } else {
        const r = await showPreset(name);
        setYaml(r.yaml);
      }
    } catch (e) {
      setYaml("# error: " + (e instanceof Error ? e.message : String(e)));
    } finally {
      setYamlBusy(false);
    }
  };

  return (
    <div className="settings-panel">
      <h2>Embedded presets &amp; bundles</h2>
      <p className="settings-help">
        Config layers shipped inside the binary (RFC AQ). Select them with{" "}
        <code>LOOMCYCLE_PRESETS=base,document-agent</code> as the base of the
        config stack. These are read-only here — copy a unit's YAML to fork it.
      </p>
      {err && <div className="settings-error">{err}</div>}
      <div className="presets-layout">
        <div className="presets-list">
          {units.map((u) => (
            <button
              key={u.name}
              type="button"
              className={"presets-item" + (selected === u.name ? " active" : "")}
              onClick={() => view(u.name)}
            >
              <div className="presets-item-head">
                <code>{u.name}</code>
                <span className={"kind-pill kind-" + u.kind}>{u.kind}</span>
              </div>
              <div className="presets-item-desc">{u.description}</div>
            </button>
          ))}
          <button
            type="button"
            className={"presets-item" + (selected === "__env__" ? " active" : "")}
            onClick={() => view("__env__")}
          >
            <div className="presets-item-head">
              <code>.env.insecure.example</code>
              <span className="kind-pill kind-env">env</span>
            </div>
            <div className="presets-item-desc">
              The non-secret env catalogue (the <code>env-template</code> CLI).
            </div>
          </button>
        </div>
        <div className="presets-viewer">
          {selected ? (
            yamlBusy ? (
              <div className="settings-muted">loading…</div>
            ) : (
              <pre className="settings-code">{yaml}</pre>
            )
          ) : (
            <div className="settings-muted">Select a unit to view its YAML.</div>
          )}
        </div>
      </div>
    </div>
  );
}

// ─── Runtime (pause / resume / state) ────────────────────────────────────────

function RuntimeSection() {
  const [state, setState] = useState<RuntimeStateResponse | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [flash, setFlash] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const refresh = async () => {
    try {
      setState(await getRuntimeState());
      setUnavailable(false);
      setErr(null);
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      if (msg.includes("503")) setUnavailable(true);
      else setErr(msg);
    }
  };

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5_000);
    return () => clearInterval(t);
  }, []);

  const doPause = async () => {
    if (busy) return;
    if (!confirm("Pause the runtime? In-flight idempotent tools are cancelled immediately; non-idempotent tools get a 30-second wind-down. New runs return 503 until you resume.")) {
      return;
    }
    setBusy(true);
    try {
      const r = await pauseRuntime();
      setState({ state: r.state as RuntimeStateResponse["state"], paused_runs_count: r.paused_runs_count });
      setFlash(`paused (${r.duration_ms} ms, ${r.force_cancelled_count} force-cancelled, ${r.paused_runs_count} paused runs)`);
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
      const r = await resumeRuntime();
      setState({ state: r.state as RuntimeStateResponse["state"], paused_runs_count: 0 });
      setFlash(`resumed (${r.resumed_runs_count} runs released)`);
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="settings-panel">
      <h2>Runtime</h2>
      <p className="settings-help">
        Quiesce the runtime for a maintenance window or a consistent snapshot
        (the <code>pause</code> / <code>resume</code> CLI). Paused: new runs
        return 503; in-flight runs park at a safe boundary.
      </p>
      {unavailable ? (
        <div className="settings-muted">
          Pause/resume is not wired on this instance.
        </div>
      ) : (
        <>
          <div className="runtime-state">
            state:{" "}
            <span className={"status-pill status-" + (state?.state === "running" ? "ok" : "warn")}>
              {state?.state ?? "…"}
            </span>
            {state && state.paused_runs_count > 0 && (
              <span className="settings-muted"> · {state.paused_runs_count} paused runs</span>
            )}
          </div>
          <div className="settings-row-actions">
            <button type="button" className="ghost-btn danger" disabled={busy || state?.state !== "running"} onClick={doPause}>
              pause
            </button>
            <button type="button" className="primary-btn" disabled={busy || state?.state === "running"} onClick={doResume}>
              resume
            </button>
          </div>
          {flash && <div className="settings-flash">{flash}</div>}
        </>
      )}
      {err && <div className="settings-error">{err}</div>}

      <h3 className="settings-subhead">Snapshots</h3>
      <p className="settings-help">
        Capture / restore runtime state for HA and migration.{" "}
        <Link to="/snapshots">Open the Snapshots page →</Link>
      </p>
      <h3 className="settings-subhead">Audit</h3>
      <p className="settings-help">
        Token mint/rotate/retire and other admin actions are recorded.{" "}
        <Link to="/audit">Open the Audit log →</Link>
      </p>
    </div>
  );
}

// ─── Health ──────────────────────────────────────────────────────────────────

function HealthSection() {
  const [health, setHealth] = useState<HealthResponse | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const refresh = async () => {
    try {
      setHealth(await getHealth());
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  useEffect(() => {
    refresh();
  }, []);

  return (
    <div className="settings-panel">
      <h2>Health</h2>
      <p className="settings-help">
        Liveness + the running binary version (the <code>health</code> /{" "}
        <code>doctor</code> CLIs). For deeper checks (provider keys, storage), run{" "}
        <code>loomcycle doctor</code> where the binary is available.
      </p>
      {err && <div className="settings-error">{err}</div>}
      {health && (
        <table className="settings-table">
          <tbody>
            <tr>
              <td>status</td>
              <td>
                <span className={"status-pill status-" + (health.ok ? "ok" : "warn")}>
                  {health.ok ? "ok" : "degraded"}
                </span>
              </td>
            </tr>
            <tr>
              <td>version</td>
              <td>
                <code>{health.version || "unknown"}</code>
              </td>
            </tr>
          </tbody>
        </table>
      )}
      <button type="button" className="ghost-btn" onClick={refresh}>
        refresh
      </button>
    </div>
  );
}
