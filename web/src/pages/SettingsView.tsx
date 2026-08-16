import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import {
  canSee,
  hasTenantScope as principalHasTenantScope,
  type Visibility,
} from "../lib/visibility";
import { ontologistRunHref, ontologyEditHref } from "../lib/ontologyEditHref";
import {
  CredentialMeta,
  CredentialScope,
  HealthResponse,
  MemoryOrphanReport,
  OntologyProposal,
  OntologyResponse,
  PresetUnit,
  RuntimeStateResponse,
  adoptOntologyType,
  createCredential,
  deleteCredential,
  getEnvTemplate,
  getHealth,
  getOntology,
  getRuntimeState,
  listCredentials,
  listPresets,
  pauseRuntime,
  repairTenantMemory,
  resumeRuntime,
  resolveOntologyProposal,
  setOntologyStatus,
  showPreset,
  OntologyTerm,
} from "../api";
import { usePrincipal, useUserId } from "../components/Layout";
import LimitsView from "./LimitsView";
import RoutingView from "./RoutingView";
import TokenManager from "../components/TokenManager";

// SettingsView is the Settings hub (top-bar gear). It web-reaches the critical
// `loomcycle` CLI + tenant surfaces so a no-shell deployment (TrueNAS — RFC AR)
// stays operable. Visible to admins AND substrate:tenant operators (the gear is
// rendered for both in Layout); the tabs are filtered by scope:
//   - tenant-visible (admin + substrate:tenant): credentials (enter your own
//     provider API keys — RFC AR), limits (per-scope token budgets, RFC AW),
//     routing (the resolved model cascade), ontology (the tenant entity types +
//     the draft→confirmed gate). Their data is tenant-scoped server-side — a
//     tenant operator sees only its own tenant.
//   - admin-only: tokens (minting, RFC L), presets (RFC AQ), runtime
//     (pause/resume), health.
// The backend gates every surface too (defence in depth). Surfaces with their
// own pages (snapshots, audit) are linked, not duplicated.
type Section =
  | "credentials"
  | "limits"
  | "routing"
  | "ontology"
  | "tokens"
  | "presets"
  | "runtime"
  | "maintenance"
  | "health";

interface SectionDef {
  id: Section;
  label: string;
  // Which roles may reach this tab, using the same three-tier class the left nav
  // uses. It replaces a binary `admin` boolean, which could not express the middle
  // tier: a substrate:tenant operator is not an admin, but is not a delegated user
  // either, and every tab below is at least tenant-gated on the server.
  //
  // ⚠️ ASSIGNED FROM THE ROUTE GATE, never from taste. A tab is "tenant" only where
  // requiredScopeFor on its backing endpoint returns ScopeTenant. Mislabelling one
  // grants nothing — the server still refuses — but it produces a control that
  // 403s, which is exactly the defect this replaces.
  vis: Visibility;
}

const SECTIONS: SectionDef[] = [
  // Every tab here is at least tenant-gated: there is no "all" settings surface,
  // because a delegated user administers nothing. The nav's gear is already gated on
  // (admin || tenant), so this list is what protects the direct-URL path.
  //
  // ScopeTenant on the server: /v1/_credentialdef (isTenantConfinedDefPath),
  // /v1/_limits, /v1/_routing and /v1/_ontology. (Memory has its own console at
  // /memory — the shared @loomcycle/memory-view package — rather than a tab here.)
  { id: "credentials", label: "Credentials", vis: "tenant" },
  { id: "limits", label: "Limits", vis: "tenant" },
  { id: "routing", label: "Routing", vis: "tenant" },
  { id: "ontology", label: "Ontology", vis: "tenant" },
  // ScopeAdmin: token minting has no tenant axis and is deliberately excluded from
  // the tenant-confined def set; presets/runtime/health fall through to the /v1/_*
  // catch-all; and repair-tenant is explicitly admin because it rewrites rows across
  // every scope in one statement.
  { id: "tokens", label: "Tokens", vis: "admin" },
  { id: "presets", label: "Presets", vis: "admin" },
  { id: "runtime", label: "Runtime", vis: "admin" },
  { id: "maintenance", label: "Maintenance", vis: "admin" },
  { id: "health", label: "Health", vis: "admin" },
];

export default function SettingsView() {
  const principal = usePrincipal();
  // A null principal = open mode / pre-resolution → admin-equivalent (matches
  // handleWhoami's open-mode synthetic admin). Layout only renders this view
  // once the principal has resolved, so this reflects the real role.
  const isAdmin = !principal || principal.is_admin;
  const hasTenantScope = principalHasTenantScope(principal?.scopes);
  const visible = SECTIONS.filter((s) => canSee(s.vis, isAdmin, hasTenantScope));
  // Default to the first tab the principal can actually see, rather than to a
  // hardcoded one. The old default was "credentials" for anyone non-admin, which a
  // delegated user could reach by typing /settings — rendering a panel whose every
  // call 403s.
  const [section, setSection] = useState<Section>(visible[0]?.id ?? "credentials");
  // And re-derive on every render rather than trusting the stored value: the role
  // resolves asynchronously, and a section selected before it lands (or left over
  // from a previous principal) must not render just because state remembers it.
  // Selection is a preference; visibility is a rule.
  const active = visible.some((s) => s.id === section) ? section : visible[0]?.id;

  if (visible.length === 0) {
    // Reachable by direct URL only — the nav's gear is gated on (admin || tenant) —
    // but "no tabs at all" renders as a blank page, which reads as broken rather
    // than as forbidden. Say which it is.
    return (
      <div className="settings-view">
        <div className="settings-panel">
          <h2>Settings</h2>
          <p className="settings-help">
            Nothing here is available to your access level. Settings administers a
            tenant or the operator plane; your token holds neither.
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="settings-view">
      <nav className="settings-tabs">
        {visible.map((s) => (
          <button
            key={s.id}
            type="button"
            className={"settings-tab" + (active === s.id ? " active" : "")}
            onClick={() => setSection(s.id)}
          >
            {s.label}
          </button>
        ))}
      </nav>
      <div className="settings-body">
        {active === "credentials" && <CredentialsSection />}
        {active === "limits" && <LimitsView />}
        {active === "routing" && <RoutingView />}
        {active === "ontology" && <OntologySection />}
        {active === "tokens" && <TokenManager />}
        {active === "presets" && <PresetsSection />}
        {active === "runtime" && <RuntimeSection />}
        {active === "maintenance" && <MaintenanceSection />}
        {active === "health" && <HealthSection />}
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
// ─── Ontology ────────────────────────────────────────────────────────────────

// ─── Ontology tree rendering (RFC BZ P4) ─────────────────────────────────────

// OntologyNode is a term with its subclasses attached.
type OntologyNode = OntologyTerm & { children: OntologyNode[] };

// buildOntologyTree assembles the parent/child tree the panel renders.
//
// An ORPHAN — a term whose named parent is absent — is rendered at the root rather
// than dropped, matching what the prompt renderer does with the same input. A type
// missing from this panel is a type the operator cannot see is in force, which is the
// silent-loss failure the whole feature exists to end.
function buildOntologyTree(terms: OntologyTerm[]): OntologyNode[] {
  const byName = new Map<string, OntologyNode>();
  terms.forEach((t) => byName.set(t.name, { ...t, children: [] }));
  const roots: OntologyNode[] = [];
  byName.forEach((n) => {
    const parent = n.parent ? byName.get(n.parent) : undefined;
    if (parent && parent !== n) parent.children.push(n);
    else roots.push(n);
  });
  return roots;
}

// ontologyFieldSummary describes a type's fields the way an operator needs to judge it:
// how many there are in total, which the type declared, and which it inherited.
function OntologyFieldSummary({ term, isLeaf }: { term: OntologyNode; isLeaf: boolean }) {
  const declared = term.fields ?? [];
  const inherited = term.inherited ?? [];
  const total = declared.length + inherited.length;
  // THE PHANTOM TEST IS ON DECLARED FIELDS, NOT ON THE TOTAL, and getting this wrong
  // would have missed the exact case worth warning about. The scenario is an operator
  // adding a section chunk for readability — "Notes on naming" under `project` — which
  // silently becomes a subclass. It INHERITS its parent's fields, so a total-based test
  // sees a well-formed type and says nothing. A leaf that declares nothing of its own
  // is a folder, not a class.
  //
  // A node with CHILDREN that declares nothing is different and fine: that is a
  // deliberate abstract grouping type (`work item` over `project` and `task`).
  const phantom = declared.length === 0 && isLeaf;
  return (
    <span className="settings-muted">
      {" "}
      {total === 0 ? "no fields" : `${total} field${total === 1 ? "" : "s"}`}
      {declared.length > 0 && <> — {declared.join(", ")}</>}
      {inherited.length > 0 && (
        <span className="ontology-inherited">
          {" "}
          · inherited: {inherited.join(", ")}
        </span>
      )}
      {phantom && (
        <span className="ontology-warn">
          {" "}
          · declares no fields of its own — a section heading rather than a type?
        </span>
      )}
      {term.name_issue && (
        <span className="ontology-warn">
          {" "}
          · name {term.name_issue}
        </span>
      )}
    </span>
  );
}

// OntologyTermTree renders the nodes, recursing into subclasses.
//
// Depth-bounded even though the chunk tree cannot cycle: a malformed term list must not
// be able to hang the operator's browser on the one page they would use to fix it.
function OntologyTermTree({
  nodes,
  depth = 0,
  markSource = false,
}: {
  nodes: OntologyNode[];
  depth?: number;
  markSource?: boolean;
}) {
  if (nodes.length === 0 || depth > 8) return null;
  return (
    <ul className="ontology-tree">
      {nodes.map((n) => (
        <li key={n.name}>
          <code>{n.name}</code>
          {markSource && n.source === "tenant" && (
            <span className="settings-flash"> yours</span>
          )}
          <OntologyFieldSummary term={n} isLeaf={n.children.length === 0} />
          <OntologyTermTree nodes={n.children} depth={depth + 1} markSource={markSource} />
        </li>
      ))}
    </ul>
  );
}

// OntologySection is the operator control for the tenant's entity types.
//
// The failure this exists to prevent is not an error message: it is an operator
// who edits the ontology document, never learns that editing alone changes
// nothing, and concludes the feature is broken. So the panel leads with the state
// of the gate, and shows the tenant's own types NEXT TO the types actually in
// force — the gap between the two columns is the only visible evidence that a
// draft is inert.
//
// Authoring stays in the Path browser (a Markdown document deserves the document
// editor); the two things that cannot live there are the gate and the layered
// result, so those are what this panel is.
function OntologySection() {
  // The topbar user picker IS the survey scope: facts live per user, so which user the
  // curation run belongs to decides what it can see.
  const pickedUser = useUserId();
  const [state, setState] = useState<OntologyResponse | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      setState(await getOntology());
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // RFC CA: accept puts the entity in force where it already sits; reject keeps it as a
  // tombstone so a curator stops re-proposing it. Both re-read the whole state from the
  // response, so the panel shows the effective result rather than an optimistic guess.
  const resolveProposal = async (chunkId: string, action: "accept" | "reject") => {
    if (busy) return;
    setBusy(true);
    try {
      setState(await resolveOntologyProposal(chunkId, action));
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const adopt = async (name: string) => {
    if (busy) return;
    setBusy(true);
    try {
      setState(await adoptOntologyType(name));
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const flip = async (status: "confirmed" | "draft") => {
    if (busy) return;
    setBusy(true);
    try {
      setState(await setOntologyStatus(status));
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const tenantTerms = state?.terms ?? [];
  // Split by status rather than filtering twice at the render site: pending needs a
  // decision, rejected is a record. Conflating them would put "reject" buttons next to
  // things already rejected.
  const allProposals: OntologyProposal[] = state?.proposals ?? [];
  const pendingProposals = allProposals.filter((p) => p.status === "proposed");
  const rejectedProposals = allProposals.filter((p) => p.status === "rejected");
  // A status that is neither of the two words the gate accepts — reachable by
  // editing the document's status field by hand. Called out explicitly, because
  // this is precisely the state that looks confirmed and behaves as draft.
  const oddStatus =
    !!state?.status && state.status !== "confirmed" && state.status !== "draft";

  return (
    <div className="settings-panel">
      <h2>Ontology</h2>
      <p className="settings-help">
        The entity types agents extract against. Every tenant starts from a
        standard set; the types you define are layered on top — but only once
        you confirm them below. Until then your edits are stored and inert, so
        you can draft an ontology without changing what any running agent is
        told.
      </p>

      {/* THE EDIT AFFORDANCE, and it is deliberately a button rather than the
          path in prose. An operator reported being unable to find any way to
          change the ontology: this panel renders types and offers "Revert to
          draft", so it reads as the place you edit them, while the only route in
          was a sentence linking to the Paths browser — a file tree, from which
          you still had to know which document to open. A labelled action that
          lands ON the document removes the guess.

          Links by document_id when the API returned one (it is provisioned and
          the caller may read it); falls back to the Paths browser only when
          there is no id to address, which is the un-provisioned case the panel
          already explains below.

          ?scope=tenant IS LOAD-BEARING. The ontology lives at tenant scope, and
          the viewer folds an absent scope to `user` — so without it this link
          opened the right document id in the wrong store, the read 422'd, and the
          operator got "No chunks" and no create buttons. The affordance existed
          and led nowhere, which is most of why the ontology UI was reported
          unusable. */}
      <p className="settings-help">
        {state?.document_id ? (
          <Link className="settings-action-link" to={ontologyEditHref(state.document_id)}>
            Edit ontology →
          </Link>
        ) : (
          <Link to="/paths">{state?.path ?? "/memory/ontology"}</Link>
        )}
        {" "}
        <span className="settings-muted-inline">
          {state?.path ?? "/memory/ontology"} (tenant scope)
        </span>
      </p>

      {/* The format, stated here rather than left to be inferred from the
          template. Guessing it was the second half of the same report. */}
      <details className="settings-help">
        <summary>How the document is written</summary>
        <p>
          Each section is one entity type: the <code>## heading</code> names it,
          and each <code>- `field`</code> bullet declares a field. Prose around
          them is documentation and is ignored. Reuse a standard type's name to
          override its fields.
        </p>
        <p>
          <strong>Nest a heading to make a subclass.</strong> A{" "}
          <code>### incident</code> under <code>## event</code> is a kind of event:
          it inherits every field above it and adds its own, and a search for the
          general type also finds the specific ones. Nest up to four levels. To
          subclass a <em>standard</em> type, give it a <code>##</code> heading of
          its own first — that overrides the standard one — then nest beneath your
          copy. <code>preference</code> and <code>fact</code> are the memory tier's
          own types and always stay top-level: you may nest types <em>under</em> them,
          but not them under something else.
        </p>
        <p>
          <strong>In the editor:</strong> select the type you want to extend, then
          use <strong>+ child</strong> to nest a subtype under it (<strong>+ text</strong>{" "}
          adds a sibling at the same level instead). Rename it in the dialog that
          opens — a type's name is its heading — and list its own fields as
          backticked bullets in the body. Changes take effect on the next run once
          the document is confirmed.
        </p>
      </details>

      {err && <div className="settings-error">{err}</div>}

      {state && !state.provisioned && (
        <div className="settings-muted">
          No ontology document — this deployment has no SQL Memory configured, so
          agents run on the standard types alone.
        </div>
      )}

      {state?.provisioned && (
        <>
          <div className="settings-row">
            <span className={state.confirmed ? "settings-flash" : "settings-muted"}>
              {state.confirmed
                ? `Confirmed — your ${tenantTerms.length} type(s) are in force.`
                : `Draft — your ${tenantTerms.length} type(s) are NOT in force.`}
            </span>
            {state.confirmed ? (
              <button type="button" onClick={() => flip("draft")} disabled={busy}>
                Revert to draft
              </button>
            ) : (
              <button
                type="button"
                onClick={() => flip("confirmed")}
                disabled={busy}
              >
                Confirm
              </button>
            )}
          </div>

          {(state.notes ?? []).map((n) => (
            <p className="settings-help" key={n}>
              {n}
            </p>
          ))}

          {oddStatus && (
            <p className="settings-help">
              The document's status reads <code>{state.status}</code>, which is
              neither <code>draft</code> nor <code>confirmed</code> — the tenant
              layer is treated as draft. Use the button above rather than editing
              the status by hand; only the exact word activates it.
            </p>
          )}

          {state.confirmed && tenantTerms.length === 0 && (
            <p className="settings-help">
              Confirmed, but no types were found in the document. Each type needs
              its own <code>## name</code> heading, with field names in backticks
              on the bullets beneath it — anything else is skipped, so a
              differently-formatted document confirms to nothing. Nest a heading
              (<code>### name</code>) to make that type a <em>subclass</em> of the
              one above it.
            </p>
          )}

          {/* PROPOSALS — entities in the document that are not in force. Above the
              table on purpose: they are the only thing here that needs a decision,
              and a pending decision buried under a type listing is a decision nobody
              makes. Rejected ones are kept as tombstones (so a curator stops
              re-proposing them) but folded away, since they compete with live types
              for attention and never need action. */}
          {pendingProposals.length > 0 && (
            <div className="settings-row ontology-proposals">
              <div>
                <strong>
                  {pendingProposals.length} suggested type
                  {pendingProposals.length === 1 ? "" : "s"}
                </strong>{" "}
                <span className="settings-muted">
                  not in force until you accept — accepting keeps each one exactly where
                  it sits in the document
                </span>
                {pendingProposals.map((p) => (
                  <div className="ontology-proposal" key={p.chunk_id}>
                    <div>
                      <code>{p.name}</code>
                      {p.parent && (
                        <span className="settings-muted">
                          {" "}
                          under <code>{p.parent}</code>
                        </span>
                      )}
                      {p.fields && p.fields.length > 0 && (
                        <span className="settings-muted"> — {p.fields.join(", ")}</span>
                      )}
                    </div>
                    {/* The evidence, so the operator judges a case and not a name. */}
                    {p.body && <pre className="ontology-evidence">{p.body.trim()}</pre>}
                    <div className="settings-row-actions">
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => void resolveProposal(p.chunk_id, "accept")}
                      >
                        accept
                      </button>
                      <button
                        type="button"
                        className="ghost-btn"
                        disabled={busy}
                        onClick={() => void resolveProposal(p.chunk_id, "reject")}
                      >
                        reject
                      </button>
                    </div>
                  </div>
                ))}
              </div>
            </div>
          )}

          {/* Where suggestions COME FROM. Placed with them rather than at the top of the
              panel: an operator reading a list of suggestions is the one who wants
              another pass, and an operator who has never seen one needs to know a pass
              is a thing they can ask for. A link, not a button — it stages the run in
              the terminal so the agent and prompt are visible before tokens are spent. */}
          <p className="settings-help">
            <Link className="settings-action-link" to={ontologistRunHref(pickedUser)}>
              Review suggestions →
            </Link>{" "}
            <span className="settings-muted">
              stages a pass that reads {pickedUser ? <code>{pickedUser}</code> : "a user"}
              &rsquo;s stored facts and files suggestions here. It cannot change anything
              on its own — everything it files waits for you.
            </span>
          </p>

          {rejectedProposals.length > 0 && (
            <details className="settings-help">
              <summary>
                {rejectedProposals.length} rejected type
                {rejectedProposals.length === 1 ? "" : "s"}
              </summary>
              <p>
                Kept on purpose: a rejection is a record, so an automated curator can
                see what you already turned down instead of proposing it again. They are
                not in force, and they never will be unless you clear the status in the
                document editor.
              </p>
              {rejectedProposals.map((p) => (
                <div key={p.chunk_id}>
                  <code>{p.name}</code>
                  {p.parent && (
                    <span className="settings-muted">
                      {" "}
                      under <code>{p.parent}</code>
                    </span>
                  )}
                </div>
              ))}
            </details>
          )}

          {/* ADOPT — the standard types this document does not declare. Offered here
              because the documented way to subclass one is to declare it yourself
              first, which otherwise means retyping its field names off the column on
              the right, by hand, correctly. */}
          {(state.adoptable ?? []).length > 0 && (
            <p className="settings-help">
              <strong>Extend a standard type:</strong> adopting one copies it into your
              document with its fields, so you can add to it or nest subtypes under it.
              Your copy then overrides the standard one, wholesale — a field a later
              loomcycle release adds to it will not reach your copy.
              <span className="ontology-adopt-row">
                {(state.adoptable ?? []).map((name) => (
                  <button
                    key={name}
                    type="button"
                    className="ghost-btn"
                    disabled={busy}
                    onClick={() => void adopt(name)}
                    title={`Copy the standard "${name}" into your document so you can extend or subclass it`}
                  >
                    adopt <code>{name}</code>
                  </button>
                ))}
              </span>
            </p>
          )}

          <div className="settings-row">
            <table className="settings-table ontology-compare">
              <thead>
                <tr>
                  <th>this deployment defines</th>
                  <th>in force now</th>
                </tr>
              </thead>
              <tbody>
                <tr>
                  <td>
                    {tenantTerms.length === 0 ? (
                      <span className="settings-muted">none yet</span>
                    ) : (
                      <OntologyTermTree nodes={buildOntologyTree(tenantTerms)} />
                    )}
                  </td>
                  <td>
                    <OntologyTermTree
                      nodes={buildOntologyTree(state.effective ?? [])}
                      markSource
                    />
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </>
      )}
    </div>
  );
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
