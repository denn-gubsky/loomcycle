import { useEffect, useState } from "react";
import { NavLink, Outlet, useLocation } from "react-router-dom";
import {
  DefNameSummary,
  DefRow,
  listAgentDefNames,
  listMcpServerDefNames,
  listSkillDefNames,
} from "../api";
import LineagePanel from "../components/LineagePanel";

// LibraryView is the v0.9.x Introspection surface — three sub-tabs
// over the AgentDef / SkillDef / MCPServerDef substrates. Each
// sub-tab uses the shared LineagePanel (list-left + lineage-right
// Splitter) with a substrate-specific definition renderer.
//
// Polling cadence: 10 s for the name list. Lineage trees per name
// refresh on selection.

const REFRESH_MS = 10_000;

export default function LibraryView() {
  // Three independent name lists. Polled in parallel from one effect.
  const [agents, setAgents] = useState<DefNameSummary[]>([]);
  const [skills, setSkills] = useState<DefNameSummary[]>([]);
  const [mcps, setMcps] = useState<DefNameSummary[]>([]);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const fetchAll = async () => {
      try {
        const [a, s, m] = await Promise.all([
          listAgentDefNames(),
          listSkillDefNames(),
          listMcpServerDefNames(),
        ]);
        if (cancelled) return;
        setAgents(a.names ?? []);
        setSkills(s.names ?? []);
        setMcps(m.names ?? []);
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
  }, []);

  const loc = useLocation();
  const path = loc.pathname;
  const sub = (() => {
    if (path.startsWith("/library/skills")) return "skills";
    if (path.startsWith("/library/mcp-servers")) return "mcp-servers";
    return "agents";
  })();

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
            names={agents}
            splitterStorageKey="loomcycle.library.agents.split"
            renderDefinition={renderAgentDefinition}
          />
        )}
        {sub === "skills" && (
          <LineagePanel
            kind="skilldef"
            kindLabel="skills"
            names={skills}
            splitterStorageKey="loomcycle.library.skills.split"
            renderDefinition={renderSkillDefinition}
          />
        )}
        {sub === "mcp-servers" && (
          <LineagePanel
            kind="mcpserverdef"
            kindLabel="MCP servers"
            names={mcps}
            splitterStorageKey="loomcycle.library.mcp.split"
            renderDefinition={renderMcpDefinition}
          />
        )}
      </div>
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

interface MCPServerDefBody {
  transport?: string;
  url?: string;
  description?: string;
  headers?: Record<string, string>;
  discovered_tools?: string[];
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
        ]}
      />
      {body.headers && Object.keys(body.headers).length > 0 && (
        <div className="def-field">
          <span className="def-field-label">headers</span>
          <pre className="def-prompt mono">
            {Object.entries(body.headers)
              .map(([k, v]) => `${k}: ${maskHeaderValue(k, v)}`)
              .join("\n")}
          </pre>
        </div>
      )}
      {body.discovered_tools && body.discovered_tools.length > 0 && (
        <div className="def-field">
          <span className="def-field-label">
            discovered_tools ({body.discovered_tools.length})
          </span>
          <div className="def-pill-row">
            {body.discovered_tools.map((t) => (
              <span key={t} className="def-pill mono">{t}</span>
            ))}
          </div>
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

// maskHeaderValue redacts the value of Authorization-shaped headers
// so a bearer token or API key in an MCP server registration's
// headers map doesn't get displayed in plaintext.
function maskHeaderValue(key: string, value: string): string {
  const k = key.toLowerCase();
  if (k === "authorization" || k.includes("token") || k.includes("api-key") || k.includes("apikey")) {
    if (value.length <= 12) return "•".repeat(value.length);
    return value.slice(0, 4) + "…" + "•".repeat(8);
  }
  return value;
}
