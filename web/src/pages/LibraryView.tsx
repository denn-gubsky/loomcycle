import { useCallback, useEffect, useState } from "react";
import { NavLink, Outlet, useLocation } from "react-router-dom";
import {
  DefRow,
  LibraryEntry,
  listLibraryAgents,
  listLibraryMcpServers,
  listLibrarySkills,
  promoteDef,
  rediscoverMcpServerDef,
  retireDef,
  type SubstrateKind,
} from "../api";
import LineagePanel from "../components/LineagePanel";
import LibraryEditModal, {
  type ModalKind,
  type ModalMode,
} from "../components/LibraryEditModal";

// LibraryView is the v0.9.x Introspection surface — three sub-tabs
// over the AgentDef / SkillDef / MCPServerDef substrates. Each
// sub-tab uses the shared LineagePanel (list-left + lineage-right
// Splitter) with a substrate-specific definition renderer.
//
// v0.9.x Library v2: surfaces yaml-only entries alongside substrate
// rows. STATIC / DYNAMIC source chips at the name level; static-only
// entries appear as a synthetic v0 row inside the lineage tree.
//
// Polling cadence: 10 s for the name list. Lineage trees per name
// refresh on selection.

const REFRESH_MS = 10_000;

export default function LibraryView() {
  // Three independent unified entry lists. Polled in parallel.
  const [agents, setAgents] = useState<LibraryEntry[]>([]);
  const [skills, setSkills] = useState<LibraryEntry[]>([]);
  const [mcps, setMcps] = useState<LibraryEntry[]>([]);
  const [err, setErr] = useState<string | null>(null);
  // Force-refresh-on-mutation: bump this counter from any mutation
  // success handler to re-run the fetch immediately without waiting
  // for the 10s poll.
  const [refreshKey, setRefreshKey] = useState(0);
  const refreshNow = useCallback(() => setRefreshKey((k) => k + 1), []);

  // Modal state — null = closed; one modal across all tabs (only one
  // can be open at a time anyway). The kind is derived from the
  // active subtab; the mode + forkSource come from which button was
  // pressed.
  const [modal, setModal] = useState<{
    kind: ModalKind;
    mode: ModalMode;
    forkSource?: DefRow;
  } | null>(null);

  useEffect(() => {
    let cancelled = false;
    const fetchAll = async () => {
      try {
        const [a, s, m] = await Promise.all([
          listLibraryAgents(),
          listLibrarySkills(),
          listLibraryMcpServers(),
        ]);
        if (cancelled) return;
        setAgents(a.entries ?? []);
        setSkills(s.entries ?? []);
        setMcps(m.entries ?? []);
        setErr(null);
      } catch (e) {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      }
    };
    fetchAll();
    const t = setInterval(fetchAll, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [refreshKey]);

  const loc = useLocation();
  const path = loc.pathname;
  const sub = (() => {
    if (path.startsWith("/library/skills")) return "skills";
    if (path.startsWith("/library/mcp-servers")) return "mcp-servers";
    return "agents";
  })();

  // Map the active subtab → modal kind so "+ New" / Edit / Retire
  // handlers always target the right substrate.
  const tabKind: ModalKind =
    sub === "skills" ? "skill" : sub === "mcp-servers" ? "mcp-server" : "agent";
  const tabSubstrate: SubstrateKind =
    sub === "skills" ? "skilldef" : sub === "mcp-servers" ? "mcpserverdef" : "agentdef";
  const tabEntries: LibraryEntry[] =
    sub === "skills" ? skills : sub === "mcp-servers" ? mcps : agents;

  const handleCreate = () => setModal({ kind: tabKind, mode: "create" });
  const handleEdit = (row: DefRow) =>
    setModal({ kind: tabKind, mode: "fork", forkSource: row });
  const handlePromote = async (row: DefRow) => {
    try {
      await promoteDef(tabSubstrate, row.def_id);
      refreshNow();
    } catch (e) {
      setErr(`Promote failed: ${e instanceof Error ? e.message : String(e)}`);
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
      await retireDef(tabSubstrate, row.def_id);
      refreshNow();
    } catch (e) {
      setErr(`Retire failed: ${e instanceof Error ? e.message : String(e)}`);
    }
  };
  const handleRediscover = async (name: string) => {
    if (!window.confirm(`Rediscover tools for ${name}? This forks a new version with the refreshed snapshot.`)) {
      return;
    }
    try {
      await rediscoverMcpServerDef(name);
      refreshNow();
    } catch (e) {
      setErr(`Rediscover failed: ${e instanceof Error ? e.message : String(e)}`);
    }
  };

  return (
    <div className="library-view">
      <div className="library-subtabs">
        <NavLink to="/library/agents" end className={subtabClass}>
          Agents <span className="library-subtab-count">{agents.length}</span>
        </NavLink>
        <NavLink to="/library/skills" end className={subtabClass}>
          Skills <span className="library-subtab-count">{skills.length}</span>
        </NavLink>
        <NavLink to="/library/mcp-servers" end className={subtabClass}>
          MCP Servers <span className="library-subtab-count">{mcps.length}</span>
        </NavLink>
      </div>
      {err && <div className="error-banner">Failed to load library: {err}</div>}
      <div className="library-content">
        {sub === "agents" && (
          <LineagePanel
            kind="agentdef"
            kindLabel="agents"
            entries={agents}
            splitterStorageKey="loomcycle.library.agents.split"
            renderDefinition={renderAgentDefinition}
            onCreateNew={handleCreate}
            onEditRow={handleEdit}
            onRetireRow={handleRetire}
            onPromoteRow={handlePromote}
          />
        )}
        {sub === "skills" && (
          <LineagePanel
            kind="skilldef"
            kindLabel="skills"
            entries={skills}
            splitterStorageKey="loomcycle.library.skills.split"
            renderDefinition={renderSkillDefinition}
            onCreateNew={handleCreate}
            onEditRow={handleEdit}
            onRetireRow={handleRetire}
            onPromoteRow={handlePromote}
          />
        )}
        {sub === "mcp-servers" && (
          <LineagePanel
            kind="mcpserverdef"
            kindLabel="MCP servers"
            entries={mcps}
            splitterStorageKey="loomcycle.library.mcp.split"
            renderDefinition={renderMcpDefinition}
            onCreateNew={handleCreate}
            onEditRow={handleEdit}
            onRetireRow={handleRetire}
            onPromoteRow={handlePromote}
            onRediscover={handleRediscover}
          />
        )}
      </div>
      {modal && (
        <LibraryEditModal
          kind={modal.kind}
          mode={modal.mode}
          forkSource={modal.forkSource}
          existingNames={tabEntries}
          onClose={() => setModal(null)}
          onSaved={() => {
            setModal(null);
            refreshNow();
          }}
        />
      )}
      <Outlet />
    </div>
  );
}

const subtabClass = ({ isActive }: { isActive: boolean }) =>
  isActive ? "library-subtab library-subtab-active" : "library-subtab";

// ---- Substrate-specific Definition renderers ----

interface AgentDefBody {
  system_prompt?: string;
  allowed_tools?: string[];
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
      {body.allowed_tools && body.allowed_tools.length > 0 && (
        <div className="def-field">
          <span className="def-field-label">allowed_tools</span>
          <div className="def-pill-row">
            {body.allowed_tools.map((t) => (
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
  allowed_tools?: string[];
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
      {body.allowed_tools && body.allowed_tools.length > 0 && (
        <div className="def-field">
          <span className="def-field-label">allowed_tools</span>
          <div className="def-pill-row">
            {body.allowed_tools.map((t) => (
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
  allowed_tools?: string[];
  /**
   * Cached tools/list result. Shape mirrors the substrate's
   * mcp_server_defs.definition `discovered_tools` field —
   * `[{name, description, input_schema}]`. Older revisions of this
   * type declared `string[]`; the persisted JSON has always been the
   * object shape — the wire type was just wrong.
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
      {body.allowed_tools && body.allowed_tools.length > 0 && (
        <div className="def-field">
          <span className="def-field-label">allowed_tools</span>
          <div className="def-pill-row">
            {body.allowed_tools.map((t) => (
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
            no tools cached — MCP handshake pending, the server is
            unreachable, or <code>rediscover</code> hasn't been called
            for a substrate entry. Check the loomcycle log for{" "}
            <code>mcp[{"<name>"}]: handshake failed</code> lines.
          </div>
        )}
      </div>
    </div>
  );
}

// MCPToolEntry renders one discovered MCP tool as a pill that
// expands inline to show the tool's description + input_schema.
// The defensive `(unnamed tool #N)` fallback covers the case where
// an upstream MCP server returns a tools/list result with empty/
// missing name fields — surfaces a visible signal instead of a blank
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

// maskSensitiveValue redacts the value of secret-shaped key/value
// pairs — Authorization-style HTTP headers AND stdio env vars whose
// key matches the *_TOKEN / *_KEY / *_SECRET / *_PASSWORD heuristic.
// Static MCP stdio servers (cfg.MCPServers entries) routinely carry
// API keys in their Env map; this filter keeps them from leaking
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
