import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import {
  CredentialMeta,
  CredentialScope,
  HealthResponse,
  PresetUnit,
  RuntimeStateResponse,
  createCredential,
  deleteCredential,
  getEnvTemplate,
  getHealth,
  getRuntimeState,
  listCredentials,
  listPresets,
  pauseRuntime,
  resumeRuntime,
  showPreset,
} from "../api";
import { usePrincipal } from "../components/Layout";
import LimitsView from "./LimitsView";
import RoutingView from "./RoutingView";
import TokenManager from "../components/TokenManager";

// SettingsView is the Settings hub (top-bar gear). It web-reaches the critical
// `loomcycle` CLI + tenant surfaces so a no-shell deployment (TrueNAS — RFC AR)
// stays operable. Visible to admins AND substrate:tenant operators (the gear is
// rendered for both in Layout); the tabs are filtered by scope:
//   - tenant-visible (admin + substrate:tenant): credentials (enter your own
//     provider API keys — RFC AR), limits (per-scope token budgets, RFC AW),
//     routing (the resolved model cascade). Their data is tenant-scoped
//     server-side — a tenant operator sees only its own tenant.
//   - admin-only: tokens (minting, RFC L), presets (RFC AQ), runtime
//     (pause/resume), health.
// The backend gates every surface too (defence in depth). Surfaces with their
// own pages (snapshots, audit) are linked, not duplicated.
type Section =
  | "credentials"
  | "limits"
  | "routing"
  | "tokens"
  | "presets"
  | "runtime"
  | "health";

interface SectionDef {
  id: Section;
  label: string;
  admin: boolean; // true = super-admin only
}

const SECTIONS: SectionDef[] = [
  { id: "credentials", label: "Credentials", admin: false },
  { id: "limits", label: "Limits", admin: false },
  { id: "routing", label: "Routing", admin: false },
  { id: "tokens", label: "Tokens", admin: true },
  { id: "presets", label: "Presets", admin: true },
  { id: "runtime", label: "Runtime", admin: true },
  { id: "health", label: "Health", admin: true },
];

export default function SettingsView() {
  const principal = usePrincipal();
  // A null principal = open mode / pre-resolution → admin-equivalent (matches
  // handleWhoami's open-mode synthetic admin). Layout only renders this view
  // once the principal has resolved, so this reflects the real role.
  const isAdmin = !principal || principal.is_admin;
  const visible = SECTIONS.filter((s) => isAdmin || !s.admin);
  // Default to the first tab the principal can see: tokens for an admin,
  // credentials for a tenant operator.
  const [section, setSection] = useState<Section>(
    isAdmin ? "tokens" : "credentials",
  );

  return (
    <div className="settings-view">
      <nav className="settings-tabs">
        {visible.map((s) => (
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
        {section === "credentials" && <CredentialsSection />}
        {section === "limits" && <LimitsView />}
        {section === "routing" && <RoutingView />}
        {section === "tokens" && <TokenManager />}
        {section === "presets" && <PresetsSection />}
        {section === "runtime" && <RuntimeSection />}
        {section === "health" && <HealthSection />}
      </div>
    </div>
  );
}

// ─── Credentials (RFC AR) ────────────────────────────────────────────────────

// isCredStoreDisabled detects the fail-closed error surfaced when the operator
// hasn't set LOOMCYCLE_SECRET_KEY (the inline backend is off). The server
// returns the tool's error text verbatim inside the 422 envelope, so we match on
// its stable markers.
function isCredStoreDisabled(msg: string): boolean {
  return (
    msg.includes("LOOMCYCLE_SECRET_KEY") ||
    msg.includes("no credential engine") ||
    msg.includes("disabled")
  );
}

// CredentialRow is a metadata row tagged with the scope it was listed under (the
// API groups by scope; we merge tenant + user into one table).
type CredentialRow = CredentialMeta & { _scope: CredentialScope };

// well-known provider/tool key env-var names (mirror docs/CREDENTIALS.md);
// free-form custom names (e.g. $cred: labels) are still allowed.
const KNOWN_KEY_NAMES = [
  "ANTHROPIC_API_KEY",
  "OPENAI_API_KEY",
  "DEEPSEEK_API_KEY",
  "GEMINI_API_KEY",
  "OLLAMA_API_KEY",
  // Web-search providers (RFC BB).
  "BRAVE_API_KEY",
  "SERPER_API_KEY",
  "EXA_API_KEY",
  "TAVILY_API_KEY",
];

function CredentialsSection() {
  const [rows, setRows] = useState<CredentialRow[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [disabled, setDisabled] = useState(false);
  const [flash, setFlash] = useState<string | null>(null);
  // Create form. `value` is write-only — cleared on submit (success OR failure)
  // and never re-displayed.
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const [scope, setScope] = useState<CredentialScope>("tenant");
  const [busy, setBusy] = useState(false);

  const refresh = useCallback(async () => {
    setErr(null);
    try {
      // List BOTH scopes so the table shows the tenant-shared + own-user
      // credentials together. list returns metadata only, never a value.
      const [t, u] = await Promise.all([
        listCredentials("tenant"),
        listCredentials("user"),
      ]);
      setRows([
        ...(t.credentials ?? []).map((c) => ({ ...c, _scope: "tenant" as const })),
        ...(u.credentials ?? []).map((c) => ({ ...c, _scope: "user" as const })),
      ]);
      setDisabled(false);
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      if (isCredStoreDisabled(msg)) setDisabled(true);
      else setErr(msg);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const onCreate = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || !name.trim() || !value) return;
    setBusy(true);
    setErr(null);
    setFlash(null);
    const created = name.trim();
    try {
      await createCredential({ scope, name: created, value });
      setValue(""); // clear the secret immediately
      setName("");
      setFlash(`stored ${scope} credential "${created}"`);
      setDisabled(false);
      await refresh();
    } catch (e2) {
      setValue(""); // clear the secret even on failure — never retained
      const msg = e2 instanceof Error ? e2.message : String(e2);
      if (isCredStoreDisabled(msg)) setDisabled(true);
      else setErr(msg);
    } finally {
      setBusy(false);
    }
  };

  const onDelete = async (r: CredentialRow) => {
    if (
      !confirm(
        `Delete the ${r._scope} credential "${r.name}"? Anything referencing $cred:${r.name} will stop resolving.`,
      )
    ) {
      return;
    }
    try {
      await deleteCredential({ scope: r._scope, name: r.name });
      await refresh();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <div className="settings-panel">
      <h2>Credentials</h2>
      <p className="settings-help">
        Encrypted API credentials for this tenant (RFC AR). A stored secret is
        referenced elsewhere as <code>$cred:&lt;name&gt;</code> in an MCP
        server&apos;s env or headers, and the runtime binds it server-side — the
        value is never shown again after you save it. Naming one after a provider
        key env-var (e.g. <code>ANTHROPIC_API_KEY</code>,{" "}
        <code>BRAVE_API_KEY</code>) overrides the operator&apos;s key for this
        tenant&apos;s runs (RFC AR / AX). <strong>tenant</strong> scope is shared
        across the tenant; <strong>user</strong> scope is private to your own
        subject (per-user tokens, e.g. a personal Telegram bot token).
      </p>

      {disabled && (
        <div className="settings-error">
          The credential store is disabled. The operator must set{" "}
          <code>LOOMCYCLE_SECRET_KEY</code> to enable encrypted credential
          storage.
        </div>
      )}
      {err && <div className="settings-error">{err}</div>}
      {flash && <div className="settings-flash">{flash}</div>}

      <form className="cred-create" onSubmit={onCreate}>
        <input
          type="text"
          list="cred-key-names"
          placeholder="name (e.g. ANTHROPIC_API_KEY)"
          value={name}
          onChange={(e) => setName(e.target.value)}
          autoComplete="off"
          spellCheck={false}
        />
        <datalist id="cred-key-names">
          {KNOWN_KEY_NAMES.map((n) => (
            <option key={n} value={n} />
          ))}
        </datalist>
        <input
          type="password"
          placeholder="value (secret — write-only)"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          autoComplete="new-password"
        />
        <select
          value={scope}
          onChange={(e) => setScope(e.target.value as CredentialScope)}
          title="tenant = shared; user = your own subject"
        >
          <option value="tenant">tenant</option>
          <option value="user">user</option>
        </select>
        <button
          type="submit"
          className="primary-btn"
          disabled={busy || !name.trim() || !value}
        >
          {busy ? "storing…" : "store"}
        </button>
      </form>

      <table className="settings-table cred-table">
        <thead>
          <tr>
            <th>name</th>
            <th>scope</th>
            <th>updated</th>
            <th aria-label="actions" />
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 ? (
            <tr>
              <td colSpan={4} className="settings-muted">
                no credentials stored.
              </td>
            </tr>
          ) : (
            rows.map((r) => (
              <tr key={`${r._scope}/${r.name}`}>
                <td>
                  <code>{r.name}</code>
                </td>
                <td>{r._scope}</td>
                <td>
                  {r.updated_at
                    ? new Date(r.updated_at).toLocaleString()
                    : "—"}
                </td>
                <td>
                  <button
                    type="button"
                    className="ghost-btn danger"
                    onClick={() => void onDelete(r)}
                  >
                    delete
                  </button>
                </td>
              </tr>
            ))
          )}
        </tbody>
      </table>
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
