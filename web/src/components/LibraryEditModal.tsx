import { useEffect, useMemo, useState } from "react";
import {
  createDef,
  forkDef,
  type DefRow,
  type LibraryEntry,
  type SubstrateKind,
} from "../api";

// LibraryEditModal — v0.10.4 Library admin UI.
//
// Hybrid form for create / fork on the three substrate flavors. Common
// required fields render as structured inputs; the rest goes into a JSON
// textarea (agents) / markdown body textarea (skills) / structured fields
// only (mcp-servers — substrate body is exhaustively covered by them).
//
// On submit, calls createDef() or forkDef() depending on `mode`. On
// refusal (substrate returns 422), the thrown jsonFetch error contains
// the `{code:"tool_refused", error:"<human text>", tool:"..."}` envelope
// — explainRefusal() pattern-matches the text and renders a friendlier
// message above the action buttons.
//
// Visual structure mirrors AnswerModal in InterruptInbox.tsx — same
// .modal-overlay + .modal + .modal-buttons class anchors, plus
// .library-modal-* extensions for the wider layout.

export type ModalKind = "agent" | "skill" | "mcp-server";
export type ModalMode = "create" | "fork";

export interface LibraryEditModalProps {
  kind: ModalKind;
  mode: ModalMode;
  // For mode="fork": the active row whose definition we pre-fill the
  // form with. The fork goes to the substrate; if `forkSource` is a
  // static-yaml synthetic row (def_id starts with "static:"), the
  // substrate auto-bootstraps a v1 lineage root from cfg before
  // attaching this fork as v2.
  forkSource?: DefRow;
  // For mode="create": the existing entry list so we can fail-fast on
  // name collisions before round-tripping the server. Optional —
  // server-side check is authoritative.
  existingNames?: LibraryEntry[];
  onClose: () => void;
  onSaved: (row: DefRow) => void;
}

export default function LibraryEditModal({
  kind,
  mode,
  forkSource,
  existingNames,
  onClose,
  onSaved,
}: LibraryEditModalProps) {
  const [submitting, setSubmitting] = useState(false);
  const [submitErr, setSubmitErr] = useState<string | null>(null);

  // --- Common fields across all flavors
  const [name, setName] = useState(forkSource?.name ?? "");
  const [description, setDescription] = useState<string>(
    pickString(forkSource?.definition, "description"),
  );
  // Promote default: ON for create (operator created it, they want it
  // active), OFF for fork (review the new version before activating).
  // Matches substrate tool defaults.
  const [promote, setPromote] = useState(mode === "create");

  // --- Agent-flavor specific
  const [provider, setProvider] = useState<string>(
    pickString(forkSource?.definition, "provider"),
  );
  const [model, setModel] = useState<string>(
    pickString(forkSource?.definition, "model"),
  );
  const [tier, setTier] = useState<string>(
    pickString(forkSource?.definition, "tier"),
  );
  const [effort, setEffort] = useState<string>(
    pickString(forkSource?.definition, "effort"),
  );
  // Pre-fill the JSON-extras textarea with everything NOT in the
  // structured fields above. On submit we deep-merge structured +
  // parsed JSON.
  const initialExtrasJSON = useMemo(() => {
    if (!forkSource?.definition) return "{}";
    const def = forkSource.definition as Record<string, unknown>;
    const extras = { ...def };
    delete extras.description;
    delete extras.provider;
    delete extras.model;
    delete extras.tier;
    delete extras.effort;
    // Server-set fields — never echo in the overlay.
    delete extras.def_id;
    delete extras.name;
    delete extras.version;
    delete extras.parent_def_id;
    delete extras.created_at;
    delete extras.created_by_agent_id;
    delete extras.created_by_run_id;
    delete extras.content_sha256;
    delete extras.retired;
    delete extras.bootstrapped_from_static;
    return Object.keys(extras).length === 0
      ? "{}"
      : JSON.stringify(extras, null, 2);
  }, [forkSource]);
  const [agentExtrasJSON, setAgentExtrasJSON] = useState(initialExtrasJSON);
  const [showAgentSchemaHint, setShowAgentSchemaHint] = useState(false);

  // --- Skill-flavor specific
  const [skillBody, setSkillBody] = useState<string>(
    pickString(forkSource?.definition, "body"),
  );
  const [skillAllowedTools, setSkillAllowedTools] = useState<string>(
    pickStringArray(forkSource?.definition, "allowed_tools").join(", "),
  );

  // --- MCP-flavor specific
  type Transport = "http" | "streamable-http";
  const [mcpTransport, setMcpTransport] = useState<Transport>(
    (pickString(forkSource?.definition, "transport") as Transport) ||
      "streamable-http",
  );
  const [mcpUrl, setMcpUrl] = useState<string>(
    pickString(forkSource?.definition, "url"),
  );
  const [mcpHeaders, setMcpHeaders] = useState<{ key: string; value: string }[]>(
    () => {
      const h = pickStringMap(forkSource?.definition, "headers");
      const entries = Object.entries(h);
      return entries.length === 0
        ? [{ key: "", value: "" }]
        : entries.map(([key, value]) => ({ key, value }));
    },
  );

  const titlePrefix = mode === "create" ? "Create" : "Edit (fork)";
  const titleLabel =
    kind === "agent"
      ? "agent"
      : kind === "skill"
        ? "skill"
        : "MCP server";

  // ESC closes the modal — small UX nicety matching standard dialog
  // behaviour. Submitting blocks the close path so a mid-flight save
  // doesn't get cancelled mid-request.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !submitting) onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [submitting, onClose]);

  const validateLocal = (): string | null => {
    if (!name.trim()) return "Name is required.";
    if (mode === "create" && existingNames) {
      const hit = existingNames.find((e) => e.name === name.trim());
      if (hit) {
        return `An entry named "${name.trim()}" already exists. Use Edit (fork) on its row instead.`;
      }
    }
    if (kind === "agent") {
      // Only validate JSON syntax — the substrate owns the schema
      // refusal. Empty body → {} → no extras, that's valid.
      try {
        const parsed = JSON.parse(agentExtrasJSON || "{}");
        if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
          return "Advanced fields JSON must be an object (key/value).";
        }
      } catch (e) {
        return `Advanced fields JSON parse error: ${(e as Error).message}`;
      }
    }
    if (kind === "skill") {
      if (!skillBody.trim()) return "Skill body is required (substrate refuses empty bodies).";
    }
    if (kind === "mcp-server") {
      if (!mcpUrl.trim()) return "URL is required.";
      try {
        const u = new URL(mcpUrl.trim());
        if (u.protocol !== "http:" && u.protocol !== "https:") {
          return "URL protocol must be http or https.";
        }
      } catch {
        return "URL is not a valid http(s) URI.";
      }
    }
    return null;
  };

  const buildOverlay = (): Record<string, unknown> => {
    if (kind === "agent") {
      const extras = JSON.parse(agentExtrasJSON || "{}") as Record<string, unknown>;
      const ov: Record<string, unknown> = { ...extras };
      if (description.trim()) ov.description = description.trim();
      if (provider.trim()) ov.provider = provider.trim();
      if (model.trim()) ov.model = model.trim();
      if (tier.trim()) ov.tier = tier.trim();
      if (effort.trim()) ov.effort = effort.trim();
      return ov;
    }
    if (kind === "skill") {
      const ov: Record<string, unknown> = { body: skillBody };
      if (description.trim()) ov.description = description.trim();
      const tools = skillAllowedTools
        .split(",")
        .map((t) => t.trim())
        .filter((t) => t.length > 0);
      if (tools.length > 0) ov.allowed_tools = tools;
      return ov;
    }
    // mcp-server
    const headers: Record<string, string> = {};
    mcpHeaders.forEach(({ key, value }) => {
      const k = key.trim();
      if (k) headers[k] = value;
    });
    const ov: Record<string, unknown> = {
      transport: mcpTransport,
      url: mcpUrl.trim(),
    };
    if (description.trim()) ov.description = description.trim();
    if (Object.keys(headers).length > 0) ov.headers = headers;
    return ov;
  };

  const handleSubmit = async () => {
    const localErr = validateLocal();
    if (localErr) {
      setSubmitErr(localErr);
      return;
    }
    setSubmitErr(null);
    setSubmitting(true);
    try {
      const substrateKind = kindToSubstrate(kind);
      const overlay = buildOverlay();
      let row: DefRow;
      if (mode === "create") {
        row = await createDef(substrateKind, name.trim(), overlay, promote);
      } else {
        // Pass the active def_id as parent_def_id so the fork hangs
        // off the right ancestor even when the substrate state has
        // raced ahead since the modal opened.
        row = await forkDef(
          substrateKind,
          name.trim(),
          overlay,
          promote,
          forkSource?.def_id?.startsWith("static:")
            ? undefined
            : forkSource?.def_id,
        );
      }
      onSaved(row);
    } catch (e) {
      setSubmitErr(explainServerError(e));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={submitting ? undefined : onClose}>
      <div
        className="modal library-modal"
        onClick={(e) => e.stopPropagation()}
      >
        <h3>
          {titlePrefix} {titleLabel}
          {mode === "fork" && forkSource?.def_id?.startsWith("static:") && (
            <span
              className="library-modal-bootstrap-hint"
              title="The first fork of a yaml-static entry auto-bootstraps a v1 lineage root from cfg before attaching this fork as v2."
            >
              {" "}— forks from yaml
            </span>
          )}
        </h3>

        <div className="library-form-row">
          <label htmlFor="lib-name">name</label>
          <input
            id="lib-name"
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={mode === "fork" || submitting}
            placeholder="my-agent"
            autoFocus={mode === "create"}
          />
        </div>

        <div className="library-form-row">
          <label htmlFor="lib-desc">description</label>
          <input
            id="lib-desc"
            type="text"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            disabled={submitting}
            placeholder="short summary"
          />
        </div>

        {kind === "agent" && (
          <AgentFields
            provider={provider}
            setProvider={setProvider}
            model={model}
            setModel={setModel}
            tier={tier}
            setTier={setTier}
            effort={effort}
            setEffort={setEffort}
            extrasJSON={agentExtrasJSON}
            setExtrasJSON={setAgentExtrasJSON}
            showSchema={showAgentSchemaHint}
            setShowSchema={setShowAgentSchemaHint}
            submitting={submitting}
          />
        )}

        {kind === "skill" && (
          <SkillFields
            allowedTools={skillAllowedTools}
            setAllowedTools={setSkillAllowedTools}
            body={skillBody}
            setBody={setSkillBody}
            submitting={submitting}
          />
        )}

        {kind === "mcp-server" && (
          <McpFields
            transport={mcpTransport}
            setTransport={setMcpTransport}
            url={mcpUrl}
            setUrl={setMcpUrl}
            headers={mcpHeaders}
            setHeaders={setMcpHeaders}
            submitting={submitting}
          />
        )}

        <div className="library-form-row library-form-row-checkbox">
          <label>
            <input
              type="checkbox"
              checked={promote}
              onChange={(e) => setPromote(e.target.checked)}
              disabled={submitting}
            />{" "}
            Promote immediately (set as active version)
          </label>
        </div>

        {submitErr && <div className="modal-err">{submitErr}</div>}

        <div className="modal-buttons">
          <button type="button" onClick={onClose} disabled={submitting}>
            Cancel
          </button>
          <button
            type="button"
            className="primary"
            onClick={handleSubmit}
            disabled={submitting}
          >
            {submitting
              ? "Saving…"
              : mode === "create"
                ? "Create"
                : "Save fork"}
          </button>
        </div>
      </div>
    </div>
  );
}

// --- Per-flavor field clusters

function AgentFields(props: {
  provider: string;
  setProvider: (v: string) => void;
  model: string;
  setModel: (v: string) => void;
  tier: string;
  setTier: (v: string) => void;
  effort: string;
  setEffort: (v: string) => void;
  extrasJSON: string;
  setExtrasJSON: (v: string) => void;
  showSchema: boolean;
  setShowSchema: (v: boolean) => void;
  submitting: boolean;
}) {
  return (
    <>
      <div className="library-form-row library-form-row-quad">
        <label htmlFor="lib-provider">provider</label>
        <input
          id="lib-provider"
          type="text"
          value={props.provider}
          onChange={(e) => props.setProvider(e.target.value)}
          disabled={props.submitting}
          placeholder="anthropic"
        />
        <label htmlFor="lib-model">model</label>
        <input
          id="lib-model"
          type="text"
          value={props.model}
          onChange={(e) => props.setModel(e.target.value)}
          disabled={props.submitting}
          placeholder="claude-opus-4-7"
        />
      </div>
      <div className="library-form-row library-form-row-quad">
        <label htmlFor="lib-tier">tier</label>
        <input
          id="lib-tier"
          type="text"
          value={props.tier}
          onChange={(e) => props.setTier(e.target.value)}
          disabled={props.submitting}
          placeholder="standard"
        />
        <label htmlFor="lib-effort">effort</label>
        <input
          id="lib-effort"
          type="text"
          value={props.effort}
          onChange={(e) => props.setEffort(e.target.value)}
          disabled={props.submitting}
          placeholder="medium"
        />
      </div>
      <div className="library-form-row">
        <label htmlFor="lib-extras">
          advanced (JSON)
          <button
            type="button"
            className="library-schema-hint-toggle"
            onClick={() => props.setShowSchema(!props.showSchema)}
          >
            {props.showSchema ? "hide" : "show"} schema
          </button>
        </label>
        <textarea
          id="lib-extras"
          className="library-json-textarea mono"
          value={props.extrasJSON}
          onChange={(e) => props.setExtrasJSON(e.target.value)}
          disabled={props.submitting}
          rows={10}
          spellCheck={false}
        />
      </div>
      {props.showSchema && (
        <pre className="library-schema-hint mono">{AGENT_SCHEMA_HINT}</pre>
      )}
    </>
  );
}

const AGENT_SCHEMA_HINT = `// AgentDef overlay fields (all optional; structured ones above):
{
  "system_prompt": "string — main system prompt",
  "system_prompt_base": "string — base system prompt, normalised from system_prompt when empty",
  "allowed_tools": ["Read","Write","WebSearch","..."],
  "skills": ["literature-review","..."],
  "providers": ["anthropic","openai"],
  "models": {"tier-name": [{"provider":"x","model":"y"}]},
  "memory_scopes": ["agent","user"],
  "memory_quota_bytes": 1048576,
  "max_tokens": 4096,
  "max_iterations": 20
}`;

function SkillFields(props: {
  allowedTools: string;
  setAllowedTools: (v: string) => void;
  body: string;
  setBody: (v: string) => void;
  submitting: boolean;
}) {
  return (
    <>
      <div className="library-form-row">
        <label htmlFor="lib-skill-tools">
          allowed_tools (comma-separated, optional)
        </label>
        <input
          id="lib-skill-tools"
          type="text"
          value={props.allowedTools}
          onChange={(e) => props.setAllowedTools(e.target.value)}
          disabled={props.submitting}
          placeholder="WebFetch, Read"
        />
      </div>
      <div className="library-form-row">
        <label htmlFor="lib-skill-body">body (markdown — required)</label>
        <textarea
          id="lib-skill-body"
          className="library-json-textarea mono"
          value={props.body}
          onChange={(e) => props.setBody(e.target.value)}
          disabled={props.submitting}
          rows={12}
          spellCheck={false}
          placeholder="# Skill instructions in markdown..."
        />
      </div>
    </>
  );
}

function McpFields(props: {
  transport: "http" | "streamable-http";
  setTransport: (v: "http" | "streamable-http") => void;
  url: string;
  setUrl: (v: string) => void;
  headers: { key: string; value: string }[];
  setHeaders: (v: { key: string; value: string }[]) => void;
  submitting: boolean;
}) {
  const updateHeader = (i: number, patch: Partial<{ key: string; value: string }>) => {
    const next = [...props.headers];
    next[i] = { ...next[i]!, ...patch };
    props.setHeaders(next);
  };
  const addHeader = () =>
    props.setHeaders([...props.headers, { key: "", value: "" }]);
  const removeHeader = (i: number) => {
    if (props.headers.length === 1) {
      props.setHeaders([{ key: "", value: "" }]);
      return;
    }
    props.setHeaders(props.headers.filter((_, idx) => idx !== i));
  };

  return (
    <>
      <div className="library-form-row">
        <label>transport</label>
        <div className="library-radio-group">
          <label>
            <input
              type="radio"
              name="lib-mcp-transport"
              value="streamable-http"
              checked={props.transport === "streamable-http"}
              onChange={() => props.setTransport("streamable-http")}
              disabled={props.submitting}
            />{" "}
            streamable-http
          </label>
          <label>
            <input
              type="radio"
              name="lib-mcp-transport"
              value="http"
              checked={props.transport === "http"}
              onChange={() => props.setTransport("http")}
              disabled={props.submitting}
            />{" "}
            http
          </label>
          <span className="library-radio-note">
            stdio servers stay yaml-only
          </span>
        </div>
      </div>
      <div className="library-form-row">
        <label htmlFor="lib-mcp-url">url</label>
        <input
          id="lib-mcp-url"
          type="text"
          value={props.url}
          onChange={(e) => props.setUrl(e.target.value)}
          disabled={props.submitting}
          placeholder="https://n8n.example.com/mcp"
        />
      </div>
      <div className="library-form-row">
        <label>
          headers
          <button
            type="button"
            className="library-schema-hint-toggle"
            onClick={addHeader}
            disabled={props.submitting}
          >
            + add row
          </button>
        </label>
        <div className="library-headers-grid">
          {props.headers.map((h, i) => (
            <div key={i} className="library-header-row">
              <input
                type="text"
                placeholder="header-name"
                value={h.key}
                onChange={(e) => updateHeader(i, { key: e.target.value })}
                disabled={props.submitting}
              />
              <input
                type="text"
                placeholder="value (e.g. Bearer ${LOOMCYCLE_X_TOKEN})"
                value={h.value}
                onChange={(e) => updateHeader(i, { value: e.target.value })}
                disabled={props.submitting}
              />
              <button
                type="button"
                onClick={() => removeHeader(i)}
                disabled={props.submitting}
                title="remove"
              >
                ×
              </button>
            </div>
          ))}
        </div>
      </div>
    </>
  );
}

// --- Helpers

function kindToSubstrate(k: ModalKind): SubstrateKind {
  switch (k) {
    case "agent":
      return "agentdef";
    case "skill":
      return "skilldef";
    case "mcp-server":
      return "mcpserverdef";
  }
}

function pickString(def: unknown, key: string): string {
  if (!def || typeof def !== "object") return "";
  const v = (def as Record<string, unknown>)[key];
  return typeof v === "string" ? v : "";
}

function pickStringArray(def: unknown, key: string): string[] {
  if (!def || typeof def !== "object") return [];
  const v = (def as Record<string, unknown>)[key];
  if (!Array.isArray(v)) return [];
  return v.filter((x): x is string => typeof x === "string");
}

function pickStringMap(def: unknown, key: string): Record<string, string> {
  if (!def || typeof def !== "object") return {};
  const v = (def as Record<string, unknown>)[key];
  if (!v || typeof v !== "object" || Array.isArray(v)) return {};
  const out: Record<string, string> = {};
  for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
    if (typeof val === "string") out[k] = val;
  }
  return out;
}

// explainServerError unwraps the jsonFetch thrown-Error message —
// `<status> <statusText>: {"code":"tool_refused","error":"...","tool":"X"}`
// — and maps the inner human-text to a friendlier message via substring
// matching. Falls back to the raw text when the pattern doesn't match.
//
// Pattern-matching the text (instead of a discrete `code` field) is the
// pragmatic choice: the substrate tools deliberately surface human text
// today (internal/tools/builtin/agentdef.go etc. use errResult(string));
// matching on stable substrings ("matches a static cfg.", "not allowed",
// etc.) keeps the UI useful without forcing a substrate-side error-code
// taxonomy redesign.
function explainServerError(e: unknown): string {
  const raw = e instanceof Error ? e.message : String(e);
  // jsonFetch throws "<status> <statusText>: <body>"; pull the JSON body
  const jsonIdx = raw.indexOf("{");
  let innerText = raw;
  if (jsonIdx >= 0) {
    const jsonPart = raw.slice(jsonIdx);
    try {
      const parsed = JSON.parse(jsonPart);
      if (parsed && typeof parsed.error === "string") {
        innerText = parsed.error;
      }
    } catch {
      // Body wasn't JSON or was truncated; fall through with raw.
    }
  }
  // Substring patterns surfaced by the substrate tools (verified
  // against internal/tools/builtin/{agentdef,skilldef,mcpserverdef}.go).
  if (innerText.includes("matches a static cfg.")) {
    return "An entry with this name is defined in yaml. Pick a different name.";
  }
  if (innerText.includes("not configured (no Store backend)")) {
    return "Substrate is not configured. Set up a store backend in loomcycle.yaml.";
  }
  if (innerText.includes("not configured (no Config")) {
    return "Substrate is not configured. Operator's root config is missing.";
  }
  if (innerText.includes("not allowed for dynamic registration")) {
    return "MCP server transport must be http or streamable-http. Use yaml for stdio servers.";
  }
  if (innerText.includes("allowed_tools") && innerText.includes("widen")) {
    return "Fork can't add tools beyond the calling agent's ceiling. Trim allowed_tools.";
  }
  if (innerText.includes("body is required") || innerText.includes("empty")) {
    return "Body cannot be empty.";
  }
  if (innerText.includes("not a valid http")) {
    return "URL is not a valid http(s) URI.";
  }
  if (innerText.includes("not in") && innerText.includes("HOST_ALLOWLIST")) {
    return "URL host is not in LOOMCYCLE_HTTP_HOST_ALLOWLIST. Add it to operator yaml first.";
  }
  if (innerText.includes("missing required field")) {
    return innerText;
  }
  // Fallback — surface the raw inner text from the substrate tool.
  return innerText || raw;
}
