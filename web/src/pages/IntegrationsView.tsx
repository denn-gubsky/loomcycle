import { useCallback, useEffect, useState } from "react";
import { NavLink, Outlet, useLocation } from "react-router-dom";
import {
  type DefNameSummary,
  type DefRow,
  type LibraryEntry,
  type SubstrateKind,
  listA2AAgentDefNames,
  listA2AServerCardDefNames,
  listMemoryBackendDefNames,
  listWebhookDefNames,
  retireDef,
} from "../api";
import LineagePanel from "../components/LineagePanel";
import IntegrationEditModal, {
  type IntegrationKind,
  type ModalMode,
} from "../components/IntegrationEditModal";

// IntegrationsView — v0.24.0 manual-management surface for the four
// substrate families that connect loomcycle to the outside world:
// inbound Webhooks, A2A Server Cards (what we expose), A2A Agents (peers
// we call), and pluggable Memory Backends. Sibling of LibraryView, same
// list-left / lineage-right shape via the shared LineagePanel.
//
// Unlike Library (agents/skills/mcp), these families have no unified
// /v1/_library/* endpoint — only /names. So the list is driven from
// /names and adapted to the LibraryEntry shape LineagePanel consumes
// (always in_substrate:true, in_static:false; per-name lineage fetched
// on selection via listDefVersionsByName). There is no static/dynamic
// "source" chip because there's no static-yaml merge in the names list.
//
// No promote button: these families have no standalone promote op (their
// create/fork auto-promote by default). To roll back to an older version,
// Edit (fork) from it with "Promote immediately" checked.

const REFRESH_MS = 10_000;

type SubKey = "webhooks" | "a2a-server-cards" | "a2a-agents" | "memory-backends";

export default function IntegrationsView() {
  const [webhooks, setWebhooks] = useState<LibraryEntry[]>([]);
  const [serverCards, setServerCards] = useState<LibraryEntry[]>([]);
  const [a2aAgents, setA2AAgents] = useState<LibraryEntry[]>([]);
  const [memBackends, setMemBackends] = useState<LibraryEntry[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [refreshKey, setRefreshKey] = useState(0);
  const refreshNow = useCallback(() => setRefreshKey((k) => k + 1), []);

  const [modal, setModal] = useState<{
    kind: IntegrationKind;
    mode: ModalMode;
    forkSource?: DefRow;
  } | null>(null);

  useEffect(() => {
    let cancelled = false;
    const fetchAll = async () => {
      try {
        const [wh, sc, ag, mb] = await Promise.all([
          listWebhookDefNames(),
          listA2AServerCardDefNames(),
          listA2AAgentDefNames(),
          listMemoryBackendDefNames(),
        ]);
        if (cancelled) return;
        setWebhooks(toEntries(wh.names));
        setServerCards(toEntries(sc.names));
        setA2AAgents(toEntries(ag.names));
        setMemBackends(toEntries(mb.names));
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
  const sub: SubKey = path.startsWith("/integrations/a2a-server-cards")
    ? "a2a-server-cards"
    : path.startsWith("/integrations/a2a-agents")
      ? "a2a-agents"
      : path.startsWith("/integrations/memory-backends")
        ? "memory-backends"
        : "webhooks";

  const cfg = TAB_CONFIG[sub];
  const tabEntries =
    sub === "a2a-server-cards"
      ? serverCards
      : sub === "a2a-agents"
        ? a2aAgents
        : sub === "memory-backends"
          ? memBackends
          : webhooks;

  const handleCreate = () => setModal({ kind: cfg.kind, mode: "create" });
  const handleEdit = (row: DefRow) =>
    setModal({ kind: cfg.kind, mode: "fork", forkSource: row });
  const handleRetire = async (row: DefRow) => {
    if (
      !window.confirm(
        `Retire ${row.name} v${row.version}? It stays in lineage but stops being active.`,
      )
    ) {
      return;
    }
    try {
      await retireDef(cfg.substrate, row.def_id);
      refreshNow();
    } catch (e) {
      setErr(`Retire failed: ${e instanceof Error ? e.message : String(e)}`);
    }
  };

  return (
    <div className="library-view">
      <div className="library-subtabs">
        <NavLink to="/integrations/webhooks" end className={subtabClass}>
          Webhooks{" "}
          <span className="library-subtab-count">{webhooks.length}</span>
        </NavLink>
        <NavLink to="/integrations/a2a-server-cards" end className={subtabClass}>
          A2A Server Cards{" "}
          <span className="library-subtab-count">{serverCards.length}</span>
        </NavLink>
        <NavLink to="/integrations/a2a-agents" end className={subtabClass}>
          A2A Agents{" "}
          <span className="library-subtab-count">{a2aAgents.length}</span>
        </NavLink>
        <NavLink to="/integrations/memory-backends" end className={subtabClass}>
          Memory Backends{" "}
          <span className="library-subtab-count">{memBackends.length}</span>
        </NavLink>
      </div>
      {err && (
        <div className="error-banner">Failed to load integrations: {err}</div>
      )}
      <div className="library-content">
        <LineagePanel
          key={sub}
          kind={cfg.substrate}
          kindLabel={cfg.label}
          entries={tabEntries}
          splitterStorageKey={`loomcycle.integrations.${sub}.split`}
          renderDefinition={cfg.render}
          onCreateNew={handleCreate}
          onEditRow={handleEdit}
          onRetireRow={handleRetire}
        />
      </div>
      {modal && (
        <IntegrationEditModal
          kind={modal.kind}
          mode={modal.mode}
          forkSource={modal.forkSource}
          existingNames={tabEntries.map((e) => e.name)}
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

// toEntries adapts the /names summaries to the LibraryEntry shape
// LineagePanel consumes. These families are always substrate-backed.
function toEntries(names: DefNameSummary[]): LibraryEntry[] {
  return (names ?? []).map((n) => ({
    name: n.name,
    source: "dynamic-only",
    in_static: false,
    in_substrate: true,
    version_count: n.version_count,
    active_def_id: n.active_def_id,
    latest_version: n.latest_version,
    last_updated: n.last_updated,
  }));
}

interface TabCfg {
  kind: IntegrationKind;
  substrate: SubstrateKind;
  label: string;
  render: (row: DefRow) => React.ReactNode;
}

const TAB_CONFIG: Record<SubKey, TabCfg> = {
  webhooks: {
    kind: "webhook",
    substrate: "webhookdef",
    label: "webhooks",
    render: renderWebhook,
  },
  "a2a-server-cards": {
    kind: "a2a-server-card",
    substrate: "a2aservercarddef",
    label: "A2A server cards",
    render: renderServerCard,
  },
  "a2a-agents": {
    kind: "a2a-agent",
    substrate: "a2aagentdef",
    label: "A2A agents",
    render: renderA2AAgent,
  },
  "memory-backends": {
    kind: "memory-backend",
    substrate: "memorybackenddef",
    label: "memory backends",
    render: renderMemoryBackend,
  },
};

// ---- per-family Definition renderers ----

function renderWebhook(row: DefRow) {
  const d = (row.definition as Record<string, unknown>) ?? {};
  const auth = obj(d.auth);
  const rl = obj(d.rate_limit);
  const creds = strMap(d.user_credentials_from_env);
  const mapping = strMap(d.payload_mapping);
  const onComplete = Array.isArray(d.on_complete) ? d.on_complete.length : 0;
  return (
    <div className="def-body">
      {str(d.description) && <Field label="description" value={str(d.description)} />}
      <MetaRow
        items={[
          ["enabled", String(d.enabled === true)],
          ["delivery", str(d.delivery) || "spawn"],
          ["agent", str(d.agent)],
          ["channel", str(d.channel)],
          ["auth.kind", str(auth.kind) || "hmac"],
          ["auth.signing_secret_env", str(auth.signing_secret_env)],
          ["auth.bearer_token_env", str(auth.bearer_token_env)],
          ["rate_limit.rpm", numStr(rl.requests_per_minute)],
          ["rate_limit.burst", numStr(rl.burst)],
          ["body_size_limit_bytes", numStr(d.body_size_limit_bytes)],
          ["on_complete hooks", onComplete > 0 ? String(onComplete) : ""],
        ]}
      />
      <KVField label="user_credentials_from_env" map={creds} />
      <KVField label="payload_mapping" map={mapping} />
    </div>
  );
}

function renderServerCard(row: DefRow) {
  const d = (row.definition as Record<string, unknown>) ?? {};
  const provider = obj(d.provider);
  const caps = obj(d.capabilities);
  const exposed = Array.isArray(d.exposed_agents) ? d.exposed_agents : [];
  const schemes = Array.isArray(d.security_schemes) ? d.security_schemes : [];
  const capList = [
    caps.streaming === true ? "streaming" : "",
    caps.push_notifications === true ? "push_notifications" : "",
    caps.extended_agent_card === true ? "extended_agent_card" : "",
  ].filter(Boolean);
  return (
    <div className="def-body">
      {str(d.description) && <Field label="description" value={str(d.description)} />}
      <MetaRow
        items={[
          ["provider.organization", str(provider.organization)],
          ["provider.url", str(provider.url)],
          ["sign_with_key_env", str(d.sign_with_key_env)],
        ]}
      />
      {capList.length > 0 && <Pills label="capabilities" items={capList} />}
      {exposed.length > 0 && (
        <Pills
          label="exposed_agents"
          items={exposed.map((e) => str(obj(e).agent_name) || "(unnamed)")}
        />
      )}
      {schemes.length > 0 && (
        <Pills
          label="security_schemes"
          items={schemes.map((s) => {
            const o = obj(s);
            return `${str(o.kind)}${o.scheme ? `:${str(o.scheme)}` : ""}`;
          })}
        />
      )}
    </div>
  );
}

function renderA2AAgent(row: DefRow) {
  const d = (row.definition as Record<string, unknown>) ?? {};
  const auth = obj(d.auth);
  const skills = Array.isArray(d.expected_skills) ? d.expected_skills : [];
  return (
    <div className="def-body">
      {str(d.description) && <Field label="description" value={str(d.description)} />}
      <MetaRow
        items={[
          ["agent_card_url", str(d.agent_card_url)],
          ["endpoint", str(d.endpoint)],
          ["binding", str(d.binding)],
          ["auth.scheme", str(auth.scheme)],
          ["auth.bearer_credential_ref", str(auth.bearer_credential_ref)],
          ["verify_signed_card", String(d.verify_signed_card === true)],
        ]}
      />
      {skills.length > 0 && (
        <Pills
          label="expected_skills"
          items={skills.map((s) => {
            const o = obj(s);
            return `${str(o.id)}${o.required === true ? " (required)" : ""}`;
          })}
        />
      )}
    </div>
  );
}

function renderMemoryBackend(row: DefRow) {
  const d = (row.definition as Record<string, unknown>) ?? {};
  const config = obj(d.config);
  const tenancy = obj(d.tenancy_strategy);
  return (
    <div className="def-body">
      {str(d.description) && <Field label="description" value={str(d.description)} />}
      <MetaRow
        items={[
          ["kind", str(d.kind) || "inprocess"],
          ["config.base_url", str(config.base_url)],
          ["config.api_version", str(config.api_version)],
          ["config.api_key_env", str(config.api_key_env)],
          ["tenancy_strategy.kind", str(tenancy.kind)],
          ["tenancy_strategy.env_pattern", str(tenancy.env_pattern)],
          ["tenancy_strategy.prefix_pattern", str(tenancy.prefix_pattern)],
          ["fallback_on_error", str(d.fallback_on_error)],
          ["health_check_interval_seconds", numStr(d.health_check_interval_seconds)],
        ]}
      />
    </div>
  );
}

// ---- small render helpers (local; mirror LibraryView's DefMetaRow) ----

function Field({ label, value }: { label: string; value: string }) {
  return (
    <div className="def-field">
      <span className="def-field-label">{label}</span>
      <span>{value}</span>
    </div>
  );
}

function Pills({ label, items }: { label: string; items: string[] }) {
  return (
    <div className="def-field">
      <span className="def-field-label">{label}</span>
      <div className="def-pill-row">
        {items.map((t, i) => (
          <span key={`${i}-${t}`} className="def-pill mono">
            {t}
          </span>
        ))}
      </div>
    </div>
  );
}

function KVField({ label, map }: { label: string; map: Record<string, string> }) {
  const entries = Object.entries(map);
  if (entries.length === 0) return null;
  return (
    <div className="def-field">
      <span className="def-field-label">{label}</span>
      <pre className="def-prompt mono">
        {entries.map(([k, v]) => `${k}: ${v}`).join("\n")}
      </pre>
    </div>
  );
}

function MetaRow({ items }: { items: [string, string | undefined][] }) {
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

function obj(v: unknown): Record<string, unknown> {
  return v && typeof v === "object" && !Array.isArray(v)
    ? (v as Record<string, unknown>)
    : {};
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

function numStr(v: unknown): string {
  return typeof v === "number" && Number.isFinite(v) && v !== 0 ? String(v) : "";
}

function strMap(v: unknown): Record<string, string> {
  const o = obj(v);
  const out: Record<string, string> = {};
  for (const [k, val] of Object.entries(o)) {
    if (typeof val === "string") out[k] = val;
  }
  return out;
}
