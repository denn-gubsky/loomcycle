import { useCallback, useEffect, useMemo, useState } from "react";
import type { LoomcycleClient } from "@loomcycle/client";
import type { DefRow, LibraryEntry, Principal, ServerCapabilities, SubstrateKind } from "./types";
import { createLoomcycleClient, type Connection } from "./lib/createClient";
import {
  LibraryDataProvider,
  dataLayerFromClient,
  useLibraryData,
  type LibraryDataLayer,
} from "./lib/dataLayer";
import LineagePanel from "./components/LineagePanel";
import LibraryEditModal, {
  type ModalKind,
  type ModalMode,
} from "./components/LibraryEditModal";
import ImportModal, { type LocalAgentSeed } from "./components/ImportModal";

// Library is the embeddable Introspection surface — three sub-tabs over the
// AgentDef / SkillDef / MCPServerDef substrates. Each sub-tab uses the shared
// LineagePanel (list-left + lineage-right) with a substrate-specific definition
// renderer. It surfaces yaml-only entries alongside substrate rows: STATIC /
// DYNAMIC source chips at the name level; static-only entries appear as a
// synthetic v0 row inside the lineage tree.
//
// This is the decoupled port of the loomcycle Web UI's LibraryView: routing is
// replaced by internal tab state (with optional controlled `tab`/`onTabChange`),
// the runtime is reached through an injected data layer (connection → client →
// dataLayer), and the principal / capability gates arrive as props instead of a
// context hook. Styles ship separately: `import "@loomcycle/library/styles.css"`.
//
// Polling cadence: 10 s for the name list. Lineage trees per name refresh on
// selection.

const REFRESH_MS = 10_000;

export type LibraryTab = "agents" | "skills" | "mcp";

const DEFAULT_TABS: LibraryTab[] = ["agents", "skills", "mcp"];

// LibraryActionCapabilities gate the mutating affordances. Each defaults to
// true (omit the prop → current full-power behavior); set one to false to hide
// / disable that action's controls.
export interface LibraryActionCapabilities {
  create?: boolean;
  fork?: boolean;
  clone?: boolean;
  promote?: boolean;
  retire?: boolean;
  import?: boolean;
}

export interface LibraryProps {
  // ---- Data source (provide exactly one; precedence dataLayer > client >
  // connection). The default path is `connection` → an internal LoomcycleClient.
  /** A raw connection (baseUrl + optional token + optional fetch override). */
  connection?: Connection;
  /** A prebuilt @loomcycle/client instance. */
  client?: LoomcycleClient;
  /** A fully custom data layer (e.g. a cookie-authed same-origin fetcher). */
  dataLayer?: LibraryDataLayer;

  // ---- Theming. When set, the root carries data-theme; when omitted the
  // component inherits an ancestor's data-theme (dark is the default palette).
  theme?: "light" | "dark";

  // ---- Tabs. `tab` + `onTabChange` make the active sub-tab controlled (so a
  // host that routes can bridge URL ↔ tab); omit both for internal state.
  // `tabs` selects which sub-tabs to show and their order (default all three).
  tab?: LibraryTab;
  onTabChange?: (tab: LibraryTab) => void;
  tabs?: LibraryTab[];

  // ---- Principal / capabilities.
  /** The authenticated principal. Only `is_admin` is read (to derive tenant vs
   *  admin posture when `tenantScope` isn't given). */
  principal?: Principal;
  /** Host action-gates for the mutating affordances. Default: all enabled. */
  actions?: LibraryActionCapabilities;
  /** Explicit tenant/admin posture. Overrides the principal-derived default. */
  tenantScope?: "tenant" | "admin";
  /** Non-secret runtime posture (stdio import allowed, http allowlist present).
   *  Drives the MCP transport + import-preview gates. */
  serverCapabilities?: ServerCapabilities;

  // ---- Errors. Called on a load/mutation failure (in addition to the inline
  // banners). The component NEVER redirects on 401 — the host owns auth flow.
  onError?: (e: unknown) => void;
}

export default function Library({
  connection,
  client,
  dataLayer,
  theme,
  tab,
  onTabChange,
  tabs,
  principal,
  actions,
  tenantScope,
  serverCapabilities,
  onError,
}: LibraryProps) {
  // Resolve the data layer once per connection identity. Depending on the
  // connection's primitive fields (not the object) keeps an inline
  // `connection={{...}}` from rebuilding the client every render.
  const resolvedDataLayer = useMemo<LibraryDataLayer | null>(() => {
    if (dataLayer) return dataLayer;
    if (client) return dataLayerFromClient(client);
    if (connection) return dataLayerFromClient(createLoomcycleClient(connection));
    return null;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dataLayer, client, connection?.baseUrl, connection?.token, connection?.fetch]);

  if (!resolvedDataLayer) {
    return (
      <div
        className="loomcycle-library"
        {...(theme ? { "data-theme": theme } : {})}
      >
        <div className="error-banner">
          @loomcycle/library: provide a <code>connection</code>,{" "}
          <code>client</code>, or <code>dataLayer</code> prop.
        </div>
      </div>
    );
  }

  return (
    <div className="loomcycle-library" {...(theme ? { "data-theme": theme } : {})}>
      <LibraryDataProvider value={resolvedDataLayer}>
        <LibraryBody
          theme={theme}
          tab={tab}
          onTabChange={onTabChange}
          tabs={tabs}
          principal={principal}
          actions={actions}
          tenantScope={tenantScope}
          serverCapabilities={serverCapabilities}
          onError={onError}
        />
      </LibraryDataProvider>
    </div>
  );
}

// Thin per-tab wrappers for hosts that want to mount a single sub-tab.
export function AgentsLibrary(props: Omit<LibraryProps, "tabs">) {
  return <Library {...props} tabs={["agents"]} />;
}
export function SkillsLibrary(props: Omit<LibraryProps, "tabs">) {
  return <Library {...props} tabs={["skills"]} />;
}
export function McpLibrary(props: Omit<LibraryProps, "tabs">) {
  return <Library {...props} tabs={["mcp"]} />;
}

type LibraryBodyProps = Omit<LibraryProps, "connection" | "client" | "dataLayer">;

function LibraryBody({
  theme: _theme,
  tab,
  onTabChange,
  tabs,
  principal,
  actions,
  tenantScope,
  serverCapabilities,
  onError,
}: LibraryBodyProps) {
  const data = useLibraryData();

  // Three independent unified entry lists. Polled in parallel.
  const [agents, setAgents] = useState<LibraryEntry[]>([]);
  const [skills, setSkills] = useState<LibraryEntry[]>([]);
  const [mcps, setMcps] = useState<LibraryEntry[]>([]);
  const [err, setErr] = useState<string | null>(null);
  // Force-refresh-on-mutation: bump this counter from any mutation success
  // handler to re-run the fetch immediately without waiting for the 10s poll.
  const [refreshKey, setRefreshKey] = useState(0);
  const refreshNow = useCallback(() => setRefreshKey((k) => k + 1), []);

  // Modal state — null = closed; one modal across all tabs. The kind is derived
  // from the active subtab; the mode + forkSource come from which button was
  // pressed.
  const [modal, setModal] = useState<{
    kind: ModalKind;
    mode: ModalMode;
    forkSource?: DefRow;
  } | null>(null);
  // RFC AU — the Claude Code import flow (skills + mcp tabs).
  const [importOpen, setImportOpen] = useState(false);

  // Tab: controlled (tab prop) or internal. `tabs` selects which appear + order.
  const tabsList = tabs && tabs.length > 0 ? tabs : DEFAULT_TABS;
  const [internalTab, setInternalTab] = useState<LibraryTab>(
    () => tab ?? tabsList[0]!,
  );
  const requestedTab = tab ?? internalTab;
  const activeTab: LibraryTab = tabsList.includes(requestedTab)
    ? requestedTab
    : tabsList[0]!;
  const selectTab = (t: LibraryTab) => {
    onTabChange?.(t);
    if (tab === undefined) setInternalTab(t);
  };

  // Non-secret capability gates. stdioAllowed hides the MCP stdio transport;
  // isTenant switches the import preview's static-collision handling (a tenant
  // may create a per-tenant override; admin/open-mode forks over it).
  const stdioAllowed = serverCapabilities?.mcp_allow_dynamic_stdio === true;
  const isTenant =
    tenantScope === "tenant" || (principal ? !principal.is_admin : false);

  // Action gates (default all enabled). Rediscover forks + promotes a new MCP
  // version, so it's gated behind the fork capability.
  const canCreate = actions?.create !== false;
  const canFork = actions?.fork !== false;
  // Clone is a create-from-template (may widen tools); gate it on create, since
  // that's the substrate op it uses.
  const canClone = actions?.clone !== false && canCreate;
  const canPromote = actions?.promote !== false;
  const canRetire = actions?.retire !== false;
  const canImport = actions?.import !== false;

  useEffect(() => {
    let cancelled = false;
    const fetchAll = async () => {
      try {
        const [a, s, m] = await Promise.all([
          data.listAgents(),
          data.listSkills(),
          data.listMcpServers(),
        ]);
        if (cancelled) return;
        setAgents(a.entries ?? []);
        setSkills(s.entries ?? []);
        setMcps(m.entries ?? []);
        setErr(null);
      } catch (e) {
        if (!cancelled) {
          setErr(e instanceof Error ? e.message : String(e));
          onError?.(e);
        }
      }
    };
    fetchAll();
    const t = setInterval(fetchAll, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [data, refreshKey, onError]);

  // Map the active subtab → modal kind + substrate kind so "+ New" / Edit /
  // Retire handlers always target the right substrate.
  const tabKind: ModalKind =
    activeTab === "skills" ? "skill" : activeTab === "mcp" ? "mcp-server" : "agent";
  const tabSubstrate: SubstrateKind =
    activeTab === "skills"
      ? "skilldef"
      : activeTab === "mcp"
        ? "mcpserverdef"
        : "agentdef";

  const handleCreate = () => setModal({ kind: tabKind, mode: "create" });
  const handleEdit = (row: DefRow) =>
    setModal({ kind: tabKind, mode: "fork", forkSource: row });
  // Clone seeds a create modal from the row's full definition (name suggested +
  // editable, tools editable/widenable). Distinct from fork, which can't widen.
  const handleClone = (row: DefRow) =>
    setModal({ kind: tabKind, mode: "clone", forkSource: row });

  // After an import, "Use with a local LLM" opens the agent-create modal
  // prefilled with the imported skills + mcp tools on a local model. A synthetic
  // create-mode forkSource seeds the fields (create ignores forkSource for the
  // write — it only prefills), so no new modal prop is needed.
  const handleWireLocalAgent = (seed: LocalAgentSeed) => {
    const definition: Record<string, unknown> = { model: "local-medium" };
    if (seed.skills.length > 0) definition.skills = seed.skills;
    const allowed = seed.mcpServers.map((s) => `mcp__${s}__*`);
    if (allowed.length > 0) definition.tools = allowed;
    setImportOpen(false);
    setModal({
      kind: "agent",
      mode: "create",
      forkSource: {
        def_id: "prefill:local-agent",
        name: "",
        version: 0,
        created_at: "",
        definition,
      },
    });
  };
  const handlePromote = async (row: DefRow) => {
    try {
      await data.promoteDef(tabSubstrate, row.def_id);
      refreshNow();
    } catch (e) {
      setErr(`Promote failed: ${e instanceof Error ? e.message : String(e)}`);
      onError?.(e);
    }
  };
  const handleRetire = async (row: DefRow) => {
    if (
      !window.confirm(
        `Retire ${row.name} v${row.version}? It stays in lineage but agents stop seeing it as active.`,
      )
    ) {
      return;
    }
    try {
      await data.retireDef(tabSubstrate, row.def_id);
      refreshNow();
    } catch (e) {
      setErr(`Retire failed: ${e instanceof Error ? e.message : String(e)}`);
      onError?.(e);
    }
  };
  const handleRediscover = async (name: string) => {
    if (!window.confirm(`Rediscover tools for ${name}? This forks a new version with the refreshed snapshot.`)) {
      return;
    }
    try {
      await data.rediscoverMcpServerDef(name);
      refreshNow();
    } catch (e) {
      setErr(`Rediscover failed: ${e instanceof Error ? e.message : String(e)}`);
      onError?.(e);
    }
  };

  const tabLabel: Record<LibraryTab, string> = {
    agents: "Agents",
    skills: "Skills",
    mcp: "MCP Servers",
  };
  const tabCount: Record<LibraryTab, number> = {
    agents: agents.length,
    skills: skills.length,
    mcp: mcps.length,
  };

  return (
    <div className="library-view">
      <div className="library-subtabs">
        {tabsList.map((t) => (
          <button
            key={t}
            type="button"
            className={
              activeTab === t
                ? "library-subtab library-subtab-active"
                : "library-subtab"
            }
            onClick={() => selectTab(t)}
          >
            {tabLabel[t]}{" "}
            <span className="library-subtab-count">{tabCount[t]}</span>
          </button>
        ))}
      </div>
      {err && <div className="error-banner">Failed to load library: {err}</div>}
      <div className="library-content">
        {activeTab === "agents" && (
          <LineagePanel
            kind="agentdef"
            kindLabel="agents"
            entries={agents}
            splitterStorageKey="loomcycle.library.agents.split"
            renderDefinition={renderAgentDefinition}
            onCreateNew={canCreate ? handleCreate : undefined}
            onEditRow={canFork ? handleEdit : undefined}
            onCloneRow={canClone ? handleClone : undefined}
            onRetireRow={canRetire ? handleRetire : undefined}
            onPromoteRow={canPromote ? handlePromote : undefined}
            onError={onError}
          />
        )}
        {activeTab === "skills" && (
          <LineagePanel
            kind="skilldef"
            kindLabel="skills"
            entries={skills}
            splitterStorageKey="loomcycle.library.skills.split"
            renderDefinition={renderSkillDefinition}
            onCreateNew={canCreate ? handleCreate : undefined}
            onEditRow={canFork ? handleEdit : undefined}
            onRetireRow={canRetire ? handleRetire : undefined}
            onPromoteRow={canPromote ? handlePromote : undefined}
            onImport={canImport ? () => setImportOpen(true) : undefined}
            onError={onError}
          />
        )}
        {activeTab === "mcp" && (
          <LineagePanel
            kind="mcpserverdef"
            kindLabel="MCP servers"
            entries={mcps}
            splitterStorageKey="loomcycle.library.mcp.split"
            renderDefinition={renderMcpDefinition}
            onCreateNew={canCreate ? handleCreate : undefined}
            onEditRow={canFork ? handleEdit : undefined}
            onRetireRow={canRetire ? handleRetire : undefined}
            onPromoteRow={canPromote ? handlePromote : undefined}
            onRediscover={canFork ? handleRediscover : undefined}
            onImport={canImport ? () => setImportOpen(true) : undefined}
            onError={onError}
          />
        )}
      </div>
      {modal && (
        <LibraryEditModal
          kind={modal.kind}
          mode={modal.mode}
          forkSource={modal.forkSource}
          existingNames={
            modal.kind === "agent" ? agents : modal.kind === "skill" ? skills : mcps
          }
          stdioAllowed={stdioAllowed}
          onClose={() => setModal(null)}
          onSaved={() => {
            setModal(null);
            refreshNow();
          }}
        />
      )}
      {importOpen && (
        <ImportModal
          skills={skills}
          mcp={mcps}
          capabilities={serverCapabilities}
          isTenant={isTenant}
          onClose={() => setImportOpen(false)}
          onImported={refreshNow}
          onWireLocalAgent={handleWireLocalAgent}
        />
      )}
    </div>
  );
}

// ---- Substrate-specific Definition renderers ----

interface AgentDefBody {
  system_prompt?: string;
  code_body?: string;
  tools?: string[];
  description?: string;
  tier?: string;
  provider?: string;
  model?: string;
  effort?: string;
  max_tokens?: number;
  max_iterations?: number;
  skills?: string[];
}

function renderAgentDefinition(row: DefRow) {
  const body = (row.definition as AgentDefBody) ?? {};
  return (
    <div className="def-body">
      {body.description && (
        <div className="def-field">
          <span className="def-field-label">description</span>
          <span>{body.description}</span>
        </div>
      )}
      {body.system_prompt && (
        <div className="def-field def-field-prompt">
          <span className="def-field-label">system_prompt</span>
          <pre className="def-prompt mono">{body.system_prompt}</pre>
        </div>
      )}
      {body.code_body && (
        <div className="def-field def-field-prompt">
          <span className="def-field-label">code_body</span>
          <pre className="def-prompt mono">{body.code_body}</pre>
        </div>
      )}
      {body.tools && body.tools.length > 0 && (
        <div className="def-field">
          <span className="def-field-label">tools</span>
          <div className="def-pill-row">
            {body.tools.map((t) => (
              <span key={t} className="def-pill mono">{t}</span>
            ))}
          </div>
        </div>
      )}
      {body.skills && body.skills.length > 0 && (
        <div className="def-field">
          <span className="def-field-label">skills</span>
          <div className="def-pill-row">
            {body.skills.map((s) => (
              <span key={s} className="def-pill mono">{s}</span>
            ))}
          </div>
        </div>
      )}
      <DefMetaRow
        items={[
          ["tier", body.tier],
          ["provider", body.provider],
          ["model", body.model],
          ["effort", body.effort],
          ["max_tokens", body.max_tokens?.toString()],
          ["max_iterations", body.max_iterations?.toString()],
        ]}
      />
    </div>
  );
}

interface SkillDefBody {
  body?: string;
  description?: string;
  tools?: string[];
}

function renderSkillDefinition(row: DefRow) {
  const body = (row.definition as SkillDefBody) ?? {};
  return (
    <div className="def-body">
      {body.description && (
        <div className="def-field">
          <span className="def-field-label">description</span>
          <span>{body.description}</span>
        </div>
      )}
      {body.body && (
        <div className="def-field def-field-prompt">
          <span className="def-field-label">body</span>
          <pre className="def-prompt mono">{body.body}</pre>
        </div>
      )}
      {body.tools && body.tools.length > 0 && (
        <div className="def-field">
          <span className="def-field-label">tools</span>
          <div className="def-pill-row">
            {body.tools.map((t) => (
              <span key={t} className="def-pill mono">{t}</span>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

interface MCPDiscoveredTool {
  name: string;
  description?: string;
  input_schema?: unknown;
}

interface MCPServerDefBody {
  transport?: string;
  url?: string;
  description?: string;
  headers?: Record<string, string>;
  /**
   * Stdio-only fields (present for static yaml MCP servers; the
   * substrate refuses stdio at create-time, so substrate rows never
   * carry these).
   */
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  pool_size?: number;
  tools?: string[];
  /**
   * Cached tools/list result. Shape mirrors the substrate's
   * mcp_server_defs.definition `discovered_tools` field —
   * `[{name, description, input_schema}]`.
   */
  discovered_tools?: MCPDiscoveredTool[];
}

function renderMcpDefinition(row: DefRow) {
  const body = (row.definition as MCPServerDefBody) ?? {};
  return (
    <div className="def-body">
      {body.description && (
        <div className="def-field">
          <span className="def-field-label">description</span>
          <span>{body.description}</span>
        </div>
      )}
      <DefMetaRow
        items={[
          ["transport", body.transport],
          ["url", body.url],
          ["command", body.command],
          ["pool_size", body.pool_size?.toString()],
        ]}
      />
      {body.args && body.args.length > 0 && (
        <div className="def-field">
          <span className="def-field-label">args</span>
          <div className="def-pill-row">
            {body.args.map((a, i) => (
              <span key={`${i}-${a}`} className="def-pill mono">{a}</span>
            ))}
          </div>
        </div>
      )}
      {body.headers && Object.keys(body.headers).length > 0 && (
        <div className="def-field">
          <span className="def-field-label">headers</span>
          <pre className="def-prompt mono">
            {Object.entries(body.headers)
              .map(([k, v]) => `${k}: ${maskSensitiveValue(k, v)}`)
              .join("\n")}
          </pre>
        </div>
      )}
      {body.env && Object.keys(body.env).length > 0 && (
        <div className="def-field">
          <span className="def-field-label">env</span>
          <pre className="def-prompt mono">
            {Object.entries(body.env)
              .map(([k, v]) => `${k}: ${maskSensitiveValue(k, v)}`)
              .join("\n")}
          </pre>
        </div>
      )}
      {body.tools && body.tools.length > 0 && (
        <div className="def-field">
          <span className="def-field-label">tools</span>
          <div className="def-pill-row">
            {body.tools.map((t) => (
              <span key={t} className="def-pill mono">{t}</span>
            ))}
          </div>
        </div>
      )}
      <div className="def-field">
        <span className="def-field-label">
          discovered_tools
          {body.discovered_tools && body.discovered_tools.length > 0
            ? ` (${body.discovered_tools.length})`
            : ""}
        </span>
        {body.discovered_tools && body.discovered_tools.length > 0 ? (
          <div className="def-tool-list">
            {body.discovered_tools.map((tool, i) => (
              <MCPToolEntry
                key={tool.name || `unnamed-${i}`}
                tool={tool}
                index={i}
              />
            ))}
          </div>
        ) : (
          <div className="def-tool-empty">
            no tools cached yet — this is normal, not an error. Tools are
            discovered on the first agent call that needs this server
            (lazy registration), or eagerly when you run{" "}
            <code>rediscover</code>. An empty list here does{" "}
            <strong>not</strong> mean the server is down: a server that was
            unreachable at boot self-heals on first use. If calls actually
            fail, check the loomcycle log for{" "}
            <code>mcp[{"<name>"}]: handshake failed</code>.
          </div>
        )}
      </div>
    </div>
  );
}

// MCPToolEntry renders one discovered MCP tool as a pill that expands inline to
// show the tool's description + input_schema. The defensive `(unnamed tool #N)`
// fallback covers the case where an upstream MCP server returns a tools/list
// result with empty/missing name fields — a visible signal instead of a blank
// pill that looks like a render bug.
function MCPToolEntry({ tool, index }: { tool: MCPDiscoveredTool; index: number }) {
  const [open, setOpen] = useState(false);
  const label = tool.name && tool.name.length > 0 ? tool.name : `(unnamed tool #${index})`;
  return (
    <div className="def-tool-entry">
      <button
        type="button"
        className="def-pill def-tool-pill mono"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
      >
        <span className="def-tool-caret">{open ? "▾" : "▸"}</span>
        <span className="def-tool-name">{label}</span>
      </button>
      {open && (
        <div className="def-tool-detail">
          {tool.description && (
            <div className="def-tool-desc">{tool.description}</div>
          )}
          {tool.input_schema !== undefined && tool.input_schema !== null && (
            <pre className="def-prompt mono">
              {JSON.stringify(tool.input_schema, null, 2)}
            </pre>
          )}
        </div>
      )}
    </div>
  );
}

function DefMetaRow({ items }: { items: [string, string | undefined][] }) {
  const present = items.filter(([, v]) => v !== undefined && v !== "");
  if (present.length === 0) return null;
  return (
    <div className="def-meta-row">
      {present.map(([k, v]) => (
        <span key={k} className="def-meta-item">
          <span className="def-meta-key mono">{k}</span>
          <span className="def-meta-value mono">{v}</span>
        </span>
      ))}
    </div>
  );
}

// maskSensitiveValue redacts the value of secret-shaped key/value pairs —
// Authorization-style HTTP headers AND stdio env vars whose key matches the
// *_TOKEN / *_KEY / *_SECRET / *_PASSWORD heuristic. Static MCP stdio servers
// routinely carry API keys in their Env map; this filter keeps them from leaking
// into the Library UI's plaintext renderer.
function maskSensitiveValue(key: string, value: string): string {
  const k = key.toLowerCase();
  const sensitive =
    k === "authorization" ||
    k.includes("token") ||
    k.includes("api-key") ||
    k.includes("apikey") ||
    k.endsWith("_key") ||
    k.endsWith("_secret") ||
    k.endsWith("_password") ||
    k.endsWith("_credential") ||
    k.endsWith("_auth");
  if (sensitive) {
    if (value.length <= 12) return "•".repeat(value.length);
    return value.slice(0, 4) + "…" + "•".repeat(8);
  }
  return value;
}
