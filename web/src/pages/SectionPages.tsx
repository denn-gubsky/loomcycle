import { useCallback, useEffect, useState } from "react";
import { Link, Navigate, useParams } from "react-router-dom";
import { canSee, grantOf, hasTenantScope as principalHasTenantScope, surfacesOf } from "../lib/visibility";
import { NAV_ITEMS } from "../components/navItems";
import { accessSummary, erasureGate, residueMeaning } from "../lib/erasure";
import { countMeaning, exportMisconfigured, familyEffect } from "../lib/retention";
import {
  HealthResponse,
  MemoryOrphanReport,
  PresetUnit,
  RuntimeStateResponse,
  getEnvTemplate,
  getHealth,
  executeErasure,
  getErasureReport,
  getRetentionReport,
  getRuntimeState,
  listPresets,
  pauseRuntime,
  repairTenantMemory,
  resumeRuntime,
  showPreset,
  ErasureReportResponse,
  ErasureResult,
  RetentionReport,
} from "../api";
import { usePrincipal } from "../components/Layout";
import { CredentialsPanel } from "../components/CredentialsPanel";
import LanesView from "./LanesView";
import OntologySection from "./OntologyPage";
import LimitsView from "./LimitsView";
import RoutingView from "./RoutingView";

// The tenant's and the runtime's configuration pages — what the Settings hub
// (the top bar's gear) used to hold as tabs. Each is now a page of its own in the
// left nav's groups (credentials in connections, ontology in knowledge, limits
// and routing in cost & models, retention and erasure in compliance, the
// operator plane in system), at /<section>; /settings/<section> redirects there.
// They web-reach the critical `loomcycle` CLI + tenant surfaces so a no-shell
// deployment (TrueNAS — RFC AR) stays operable.
//
// Data is tenant-scoped server-side (a tenant operator sees only its own
// tenant), and the backend gates every surface too (defence in depth).
export const SECTION_IDS = [
  "credentials",
  "limits",
  "routing",
  "ontology",
  "retention",
  "erasure",
  "presets",
  "runtime",
  "lanes",
  "maintenance",
  "health",
] as const;

export type SectionId = (typeof SECTION_IDS)[number];

// SettingsRedirect keeps the hub's old addresses working: /settings/<section> is
// now /<section>. A bare /settings, or a section that no longer exists there,
// goes to the start page.
export function SettingsRedirect() {
  const { section } = useParams<{ section?: string }>();
  return <Navigate to={settingsRedirectTarget(section)} replace />;
}

export function settingsRedirectTarget(section: string | undefined): string {
  return section && (SECTION_IDS as readonly string[]).includes(section) ? "/" + section : "/";
}

// SectionPage renders one section, for a viewer the nav would show it to. The nav
// hides it from anyone else, so this is reached only by a pasted link — and a
// blank page reads as broken rather than as forbidden, so it says which it is.
export default function SectionPage({ id }: { id: SectionId }) {
  const principal = usePrincipal();
  // A null principal = open mode / pre-resolution → admin-equivalent (matches
  // handleWhoami's open-mode synthetic admin). Layout only renders this view
  // once the principal has resolved, so this reflects the real role.
  const isAdmin = !principal || principal.is_admin;
  const item = NAV_ITEMS.find((it) => it.id === id);
  const allowed =
    !!item &&
    canSee(item.vis, isAdmin, principalHasTenantScope(principal?.scopes), grantOf(surfacesOf(principal), id));

  if (!allowed) {
    return (
      <div className="settings-view">
        <div className="settings-panel">
          <h2>{item?.label ?? id}</h2>
          <p className="settings-help">
            This page is not available to your access level: it administers a tenant or the operator
            plane, and your token holds neither.
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="settings-view">
      <div className="settings-body">
        {id === "credentials" && <CredentialsPanel />}
        {id === "limits" && <LimitsView />}
        {id === "routing" && <RoutingView />}
        {id === "ontology" && <OntologySection />}
        {id === "retention" && <RetentionSection />}
        {id === "erasure" && <ErasureSection />}
        {id === "presets" && <PresetsSection />}
        {id === "runtime" && <RuntimeSection />}
        {id === "lanes" && <LanesView />}
        {id === "maintenance" && <MaintenanceSection />}
        {id === "health" && <HealthSection />}
      </div>
    </div>
  );
}

// ─── Credentials (RFC AR) — the panel lives in ../components/CredentialsPanel
// (shared with the standalone My Credentials page, RFC CN). ──────────────────


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

// ─── Maintenance: legacy-tenant memory repair ────────────────────────────────

// MaintenanceSection repairs memory rows stranded at the legacy "" tenant.
//
// RFC BL added tenant_id to the memory table without backfilling pre-existing
// rows, so anything written before that upgrade is unreadable from a
// tenant-scoped session. It presents as corruption rather than as an access
// problem: Document keeps chunk BODIES there but chunk STRUCTURE in SQL Memory,
// which is not partitioned the same way, so an affected document still lists,
// still opens, and still exports as a full heading tree with empty sections.
//
// This lives in the UI because the deployments most likely to be affected are
// appliance-style ones with no shell — the alternative is hand-writing a
// collision-aware UPDATE against a live database.
// ─── Retention ───────────────────────────────────────────────────────────────
//
// What the sweeper is configured to delete, and — for an admin — how much the current
// settings would match right now.
//
// READ-ONLY BY DESIGN. Retention is config: changing it here would mean an operator
// deleting data through a form whose effect they cannot preview, and the preview IS this
// page. The report is the instrument; the yaml is the control.
function RetentionSection() {
  const [rep, setRep] = useState<RetentionReport | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    getRetentionReport()
      .then((r) => {
        setRep(r);
        setErr(null);
      })
      .catch((e) => setErr(e instanceof Error ? e.message : String(e)));
  }, []);

  const days = (ms: number) => (ms > 0 ? `${Math.round(ms / 86_400_000)}d` : "—");

  const families = rep
    ? [
        { key: "defs", label: "definition versions", mode: rep.defs_mode, age: rep.defs_max_age_ms },
        { key: "chats", label: "chats", mode: rep.chats_mode, age: rep.chats_max_age_ms },
        {
          key: "chats_internal",
          label: "chats (internal agents)",
          mode: rep.chats_mode,
          age: rep.chats_internal_max_age_ms,
        },
        { key: "mem", label: "retired agents' memory", mode: rep.mem_mode, age: rep.mem_max_age_ms },
        {
          key: "mem_content",
          label: "superseded memory content",
          mode: rep.mem_content_mode,
          age: rep.mem_content_max_age_ms,
        },
      ]
    : [];

  return (
    <div className="settings-panel">
      <h2>Retention</h2>
      <p className="settings-help">
        What the background sweeper is configured to remove, and when. This page reports;
        the settings themselves live in the operator&rsquo;s yaml — a form that deleted
        data without a preview would be the wrong control, and this is the preview.
      </p>
      {err && <div className="settings-error">{err}</div>}
      {rep && (
        <>
          <div className="settings-muted">
            {rep.enabled
              ? `sweeper on, every ${Math.round(rep.interval_ms / 60_000)} min`
              : "sweeper OFF — nothing is removed on any schedule, whatever the families below say"}
          </div>
          <dl className="settings-stats">
            {families.map((f) => {
              const effect = familyEffect(f.mode);
              const count = rep.purgeable?.[f.key];
              return (
                <div key={f.key}>
                  <dt>{f.label}</dt>
                  <dd>
                    <code>{f.mode}</code>
                    {effect !== "inert" && <> · older than {days(f.age)}</>}
                  </dd>
                  {/* The count NEVER stands alone — see lib/retention for why a bare
                      number beside an `off` family reads as a countdown. */}
                  {count !== undefined && (
                    <dd className="settings-muted">{countMeaning(f.mode, count)}</dd>
                  )}
                  {exportMisconfigured(f.mode, rep.export_dir) && (
                    <dd className="settings-error">
                      exports nowhere — no export directory is configured, so the sweeper
                      refuses this family and nothing is removed
                    </dd>
                  )}
                </div>
              );
            })}
          </dl>
          {!rep.admin && (
            <div className="settings-muted">
              Counts are admin-only, so this shows the configuration without them — an
              absent number here means &ldquo;not shown to you&rdquo;, never zero.
            </div>
          )}
          {rep.export_dir && (
            <div className="settings-muted">
              exports to <code>{rep.export_dir}</code>
            </div>
          )}
        </>
      )}
    </div>
  );
}

// ─── Erasure ─────────────────────────────────────────────────────────────────
//
// The most consequential control in this console, and the only one nothing can undo.
//
// REPORT BEFORE EXECUTE, ALWAYS. The execute path does not exist until a report has been
// shown for the SAME subject: changing the field invalidates it, because otherwise an
// operator could read what would go for one person and confirm the deletion of another.
//
// TYPED CONFIRMATION, NOT A DIALOG. A modal with a red button is dismissed by reflex;
// typing the identifier is the smallest gesture that cannot be made absent-mindedly —
// and the identifier is what matters, since the failure mode is erasing the WRONG
// subject rather than erasing accidentally. The server enforces the same rule.
function ErasureSection() {
  const [subject, setSubject] = useState("");
  const [tenant, setTenant] = useState("");
  const [report, setReport] = useState<ErasureReportResponse | null>(null);
  const [reportedSubject, setReportedSubject] = useState("");
  const [confirm, setConfirm] = useState("");
  const [result, setResult] = useState<ErasureResult | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const gate = erasureGate({ subject, reportedSubject, confirm });

  const runReport = async () => {
    setBusy(true);
    setResult(null);
    try {
      const rep = await getErasureReport(subject.trim(), tenant);
      setReport(rep);
      setReportedSubject(subject.trim());
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setReport(null);
      setReportedSubject("");
    } finally {
      setBusy(false);
    }
  };

  const runErase = async () => {
    if (!gate.canExecute) return;
    setBusy(true);
    try {
      const res = await executeErasure(subject.trim(), {
        dryRun: false,
        confirm,
        tenant,
      });
      setResult(res);
      // The report described a state that no longer exists. Clearing it forces a fresh
      // one before anything else can be erased.
      setReport(null);
      setReportedSubject("");
      setConfirm("");
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="settings-panel">
      <h2>Erasure</h2>
      <p className="settings-help">
        Report what this deployment holds about one subject, and — after reading it —
        delete what can be deleted. <strong>An erasure cannot be undone.</strong> Nothing
        else in this console is irreversible; this is.
      </p>

      <div className="settings-row">
        <label>
          Subject
          <input
            value={subject}
            placeholder="user id"
            onChange={(e) => setSubject(e.target.value)}
          />
        </label>
        <label>
          Tenant
          <input value={tenant} placeholder="(admin: required)" onChange={(e) => setTenant(e.target.value)} />
        </label>
        <button type="button" onClick={() => void runReport()} disabled={busy || !subject.trim()}>
          {busy ? "reading…" : "Report"}
        </button>
      </div>
      <p className="settings-help settings-muted">
        A subject id is only meaningful inside a tenant — the same string in two tenants
        is two different people — so an admin token must name one. A tenant operator&rsquo;s
        own tenant is used when this is blank.
      </p>

      {err && <div className="settings-error">{err}</div>}

      {report && (
        <>
          <h3 className="settings-subhead">
            What is held about <code>{report.subject}</code>
          </h3>
          {/* The three tiers are degrees of REACH, not of confidence, and they are never
              summed: a total would announce a deletion far larger than the one that
              happens. */}
          <dl className="settings-stats">
            <div>
              <dt>will be deleted</dt>
              <dd>{report.tier1_covered.total}</dd>
              <dd className="settings-muted">
                {Object.entries(report.tier1_covered.counts)
                  .filter(([, n]) => n > 0)
                  .map(([k, n]) => `${k} ${n}`)
                  .join(" · ") || "nothing"}
              </dd>
            </div>
            <div>
              <dt>held, but NOT deleted</dt>
              <dd>{report.tier2_uncovered.total}</dd>
              <dd className="settings-muted">
                subject-keyed data no primitive removes yet — it stays after an erasure
              </dd>
            </div>
            <div>
              <dt>cannot be reached</dt>
              <dd>{report.tier3_residue.rows}</dd>
              <dd className="settings-muted">{residueMeaning(report.tier3_residue)}</dd>
            </div>
          </dl>
          {accessSummary(report.access) && (
            <div className="settings-muted">
              access removed with it: {accessSummary(report.access)}
            </div>
          )}
          {report.errors && report.errors.length > 0 && (
            <div className="settings-error">
              some planes could not be read, so this report is incomplete:{" "}
              {report.errors.join("; ")}
            </div>
          )}

          <div className="settings-row">
            <label>
              Type <code>{report.subject}</code> to confirm
              <input value={confirm} onChange={(e) => setConfirm(e.target.value)} />
            </label>
            <button
              type="button"
              className="danger-btn"
              disabled={busy || !gate.canExecute}
              title={gate.canExecute ? undefined : gateHint(gate.reason)}
              onClick={() => void runErase()}
            >
              Erase permanently
            </button>
          </div>
        </>
      )}

      {/* A report for a DIFFERENT subject than the one in the field is stale, and saying
          so is more useful than silently disabling the button. */}
      {reportedSubject && reportedSubject !== subject.trim() && (
        <div className="settings-muted">
          the report above is for <code>{reportedSubject}</code>; report again for{" "}
          <code>{subject.trim() || "…"}</code> before erasing
        </div>
      )}

      {result && (
        <>
          <h3 className="settings-subhead">Erased</h3>
          <div className="settings-flash">
            {Object.entries(result.deleted)
              .map(([k, n]) => `${k} ${n}`)
              .join(" · ")}
          </div>
          {accessSummary(result.access) && (
            <div className="settings-muted">
              {result.dry_run ? "would remove" : "removed"} access: {accessSummary(result.access)}
            </div>
          )}
          {Object.keys(result.retained).length > 0 && (
            <div className="settings-muted">
              kept: {Object.entries(result.retained).map(([k, why]) => `${k} (${why})`).join("; ")}
            </div>
          )}
          {result.errors && result.errors.length > 0 && (
            <div className="settings-error">{result.errors.join("; ")}</div>
          )}
        </>
      )}
    </div>
  );
}

// gateHint explains a disabled erase button. Silence would read as a broken control.
function gateHint(reason: string): string {
  switch (reason) {
    case "no-subject":
      return "name a subject first";
    case "no-report":
      return "run the report first — nothing is erased from this console unsighted";
    case "report-is-for-another-subject":
      return "the report shown is for a different subject; report again";
    default:
      return "type the subject id exactly to confirm";
  }
}

function MaintenanceSection() {
  // Deliberately NOT prefilled from the principal. A legacy/open-mode token
  // reports tenant "default", which holds none of the stranded rows — prefilling
  // it would put a plausible-but-wrong destination one click from Apply. The
  // tenantless discovery call below reports the real candidates instead.
  const [tenant, setTenant] = useState("");
  const [candidates, setCandidates] = useState<string[]>([]);
  const [report, setReport] = useState<MemoryOrphanReport | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [flash, setFlash] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // Discovery: what is stranded, and which tenants could own it. Cannot write.
  const discover = useCallback(async () => {
    try {
      const r = await repairTenantMemory("", true);
      setCandidates(r.candidate_tenants ?? []);
      setReport(r);
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    discover();
  }, [discover]);

  const run = async (dryRun: boolean) => {
    if (busy) return;
    if (!dryRun) {
      const n = report ? report.orphaned - report.collisions : 0;
      if (!confirm(`Move ${n} memory row(s) onto tenant "${tenant}"? Collisions and global-scope rows are left untouched. This rewrites rows in place.`)) {
        return;
      }
    }
    setBusy(true);
    try {
      const r = await repairTenantMemory(tenant, dryRun);
      setReport(r);
      setErr(null);
      setFlash(
        r.applied
          ? `repaired: ${r.moved} row(s) moved onto ${r.tenant}`
          : `previewed ${r.tenant}: nothing written`,
      );
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  // Nothing movable = nothing to apply: all orphans collide, or there are none.
  const movable = report ? report.orphaned - report.collisions : 0;

  return (
    <div className="settings-panel">
      <h2>Maintenance</h2>
      <p className="settings-help">
        Re-stamp memory rows stranded at the legacy tenant. Upgrading to v1.33.0
        partitioned memory by tenant without moving the rows already in it, so
        anything written earlier is invisible to a tenant-scoped session —
        documents open with empty sections, and older agent memory reads as
        absent. Nothing was deleted.
      </p>
      <div className="settings-row">
        <label>
          Target tenant
          {candidates.length > 0 ? (
            <select value={tenant} onChange={(e) => setTenant(e.target.value)}>
              <option value="">select…</option>
              {candidates.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </select>
          ) : (
            <input
              value={tenant}
              onChange={(e) => setTenant(e.target.value)}
              placeholder="tenant id"
            />
          )}
        </label>
        <button type="button" onClick={() => run(true)} disabled={busy || !tenant}>
          Preview
        </button>
        <button
          type="button"
          onClick={() => run(false)}
          disabled={busy || !tenant || movable <= 0}
          title={movable <= 0 ? "Preview first — nothing movable found" : undefined}
        >
          Apply
        </button>
      </div>
      {err && <div className="settings-error">{err}</div>}
      {flash && <div className="settings-flash">{flash}</div>}
      {report && (
        <>
          <div className="settings-muted">
            {report.orphaned === 0 ? (
              <>Nothing stranded — this deployment is not affected.</>
            ) : (
              <>
                {report.orphaned} stranded · {report.collisions} collision(s)
                skipped · {report.skipped_global} global row(s) left in place
                {report.applied && <> · {report.moved} moved</>}
              </>
            )}
          </div>
          {report.collisions > 0 && (
            <p className="settings-help">
              A collision means the target tenant already holds a row for the
              same key. Those are never merged — which side should win depends on
              content the server cannot judge, and overwriting would destroy live
              data. Nothing is lost; inspect them directly.
            </p>
          )}
          {report.groups && report.groups.length > 0 && (
            <table className="settings-table">
              <thead>
                <tr>
                  <th>scope</th>
                  <th>scope_id</th>
                  <th>rows</th>
                  <th>collisions</th>
                </tr>
              </thead>
              <tbody>
                {report.groups.map((g) => (
                  <tr key={g.scope + "/" + g.scope_id}>
                    <td>{g.scope}</td>
                    <td>{g.scope_id}</td>
                    <td>{g.rows}</td>
                    <td>{g.collisions}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {report.groups && report.groups.length > 1 && (
            <p className="settings-help">
              More than one scope_id is stranded. If they belong to different
              tenants, applying here would move them all to{" "}
              <code>{tenant}</code> — the legacy partition records no owner, so
              check before applying.
            </p>
          )}
        </>
      )}
    </div>
  );
}

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
    if (!confirm("Pause the runtime? Each in-flight run finishes the call it is in and parks at its next step; nothing is cancelled. New runs return 503 until you resume.")) {
      return;
    }
    setBusy(true);
    try {
      const r = await pauseRuntime();
      setState({ state: r.state as RuntimeStateResponse["state"], paused_runs_count: r.paused_runs_count });
      setFlash(`paused (${r.duration_ms} ms, ${r.paused_runs_count} paused runs)`);
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
