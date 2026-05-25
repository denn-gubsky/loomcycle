import { useEffect, useMemo, useRef, useState } from "react";
import {
  createDef,
  forkDef,
  type DefRow,
  type LibraryEntry,
  type SubstrateKind,
} from "../api";

// LibraryEditModal — v0.11.6 Library admin UI.
//
// Hybrid form for create / fork on the three substrate flavors. All
// editable fields render as structured inputs — the v0.10.4 JSON
// catch-all for agent overlays was removed in v0.11.6 because
// operators were hitting two real pain points:
//
//   1. Raw newlines inside the agent's `system_prompt` produced
//      invalid JSON and surfaced as a confusing "JSON parse error"
//      on submit.
//   2. A single missing comma anywhere in the JSON body sunk the
//      whole submit, with no per-field validation.
//
// The trade-off: when the AgentDef schema grows a new field, the
// modal must be updated to expose it. That's the same posture the
// MCP-server kind has had since v0.10.4 (fully structured, no JSON)
// and the v0.11.5 channel + memory modals follow.
//
// On submit, calls createDef() or forkDef() depending on `mode`. On
// refusal (substrate returns 422), the thrown jsonFetch error contains
// the `{code:"tool_refused", error:"<human text>", tool:"..."}` envelope
// — explainServerError() pattern-matches the text and renders a friendlier
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

// Per-tier candidate row inside the agent's `models` override map.
// `models` is `Record<TierName, ModelCandidate[]>`. Operators rarely
// touch this — the empty per-tier list is the default and means "use
// the library Tiers map".
interface ModelCandidate {
  provider: string;
  model: string;
}

// Three fixed tier slots in the modal. The substrate doesn't enforce
// these specific names but they match the bundled examples + every
// existing operator yaml; rendering exactly three keeps the form
// scannable. Operators with custom tier names can still edit them via
// yaml (the substrate's overlay merge accepts any tier-name keys).
const AGENT_TIER_SLOTS = ["low", "middle", "high"] as const;
type TierSlot = (typeof AGENT_TIER_SLOTS)[number];

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

  // --- Agent-flavor structured fields (v0.11.6 — every editable
  // overlay field has its own input; no JSON catch-all).
  const [provider, setProvider] = useState(
    pickString(forkSource?.definition, "provider"),
  );
  const [model, setModel] = useState(
    pickString(forkSource?.definition, "model"),
  );
  const [tier, setTier] = useState(
    pickString(forkSource?.definition, "tier"),
  );
  const [effort, setEffort] = useState(
    pickString(forkSource?.definition, "effort"),
  );
  const [systemPrompt, setSystemPrompt] = useState(
    pickString(forkSource?.definition, "system_prompt"),
  );
  const [allowedTools, setAllowedTools] = useState(
    pickStringArray(forkSource?.definition, "allowed_tools").join(", "),
  );
  const [agentSkills, setAgentSkills] = useState(
    pickStringArray(forkSource?.definition, "skills").join(", "),
  );
  const [maxTokens, setMaxTokens] = useState(
    pickNumberAsString(forkSource?.definition, "max_tokens"),
  );
  const [maxIterations, setMaxIterations] = useState(
    pickNumberAsString(forkSource?.definition, "max_iterations"),
  );
  const [memoryQuotaBytes, setMemoryQuotaBytes] = useState(
    pickNumberAsString(forkSource?.definition, "memory_quota_bytes"),
  );
  const [memoryScopes, setMemoryScopes] = useState(() => {
    const arr = pickStringArray(forkSource?.definition, "memory_scopes");
    return { agent: arr.includes("agent"), user: arr.includes("user") };
  });
  const [agentProviders, setAgentProviders] = useState(
    pickStringArray(forkSource?.definition, "providers").join(", "),
  );
  const [modelsByTier, setModelsByTier] = useState<Record<TierSlot, ModelCandidate[]>>(
    () => pickModelsByTier(forkSource?.definition),
  );
  // Custom tier names in the source's `models` map that aren't in the
  // three fixed slots the modal renders. Surfaced as a warning so
  // operators don't silently lose them on fork (the substrate's
  // overlay merge does a FULL replacement of `models`, so emitting
  // any tier from this modal would otherwise drop the custom tiers
  // from the forked def).
  const droppedCustomTiers = useMemo(
    () => pickCustomTierNames(forkSource?.definition),
    [forkSource],
  );

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
  // doesn't get cancelled mid-request. Holding onClose in a ref keeps
  // the keydown listener registered exactly once instead of churning
  // on every parent re-render that recreates the inline onClose arrow
  // (LibraryView re-renders on every poll + every refreshKey bump).
  const onCloseRef = useRef(onClose);
  useEffect(() => {
    onCloseRef.current = onClose;
  });
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !submitting) onCloseRef.current();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [submitting]);

  const validateLocal = (): string | null => {
    if (!name.trim()) return "Name is required.";
    if (mode === "create" && existingNames) {
      const hit = existingNames.find((e) => e.name === name.trim());
      if (hit) {
        return `An entry named "${name.trim()}" already exists. Use Edit (fork) on its row instead.`;
      }
    }
    if (kind === "agent") {
      // Number inputs already refuse non-numeric via type="number";
      // explicit re-check here catches the "Number(undefined) is NaN"
      // edge + the operator who manually typed a negative.
      const numChecks: Array<[string, string]> = [
        ["max_tokens", maxTokens],
        ["max_iterations", maxIterations],
        ["memory_quota_bytes", memoryQuotaBytes],
      ];
      for (const [label, raw] of numChecks) {
        if (raw.trim() === "") continue; // empty = unset = OK
        const n = Number(raw);
        if (!Number.isFinite(n) || n < 0 || !Number.isInteger(n)) {
          return `${label} must be a non-negative integer (got "${raw}").`;
        }
      }
      // Per-tier model rows: trim + drop empty pairs at submit time;
      // here just refuse partial rows (provider without model or
      // vice versa) so the operator sees the issue locally.
      for (const slot of AGENT_TIER_SLOTS) {
        for (const cand of modelsByTier[slot]) {
          const p = cand.provider.trim();
          const m = cand.model.trim();
          if ((p && !m) || (!p && m)) {
            return `tier "${slot}": every model row needs both provider AND model (or leave both blank).`;
          }
        }
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
      const ov: Record<string, unknown> = {};
      if (description.trim()) ov.description = description.trim();
      if (provider.trim()) ov.provider = provider.trim();
      if (model.trim()) ov.model = model.trim();
      if (tier.trim()) ov.tier = tier.trim();
      if (effort.trim()) ov.effort = effort.trim();
      // system_prompt is a freetext string — raw newlines preserved.
      // Omit when empty so the substrate keeps the parent / yaml value
      // instead of overwriting with "".
      if (systemPrompt.trim()) ov.system_prompt = systemPrompt;
      const tools = parseCommaList(allowedTools);
      if (tools.length > 0) ov.allowed_tools = tools;
      const sk = parseCommaList(agentSkills);
      if (sk.length > 0) ov.skills = sk;
      const provs = parseCommaList(agentProviders);
      if (provs.length > 0) ov.providers = provs;
      // Number fields: empty / "0" reads as "use default" — omit from
      // overlay so the substrate keeps the parent value (the
      // substrate's merge treats "missing" as "inherit" and "0" as
      // "explicit zero / inherit default").
      const intOrSkip = (s: string): number | null => {
        const t = s.trim();
        if (t === "") return null;
        const n = Number(t);
        return Number.isFinite(n) ? n : null;
      };
      const mt = intOrSkip(maxTokens);
      if (mt !== null && mt > 0) ov.max_tokens = mt;
      const mi = intOrSkip(maxIterations);
      if (mi !== null && mi > 0) ov.max_iterations = mi;
      const mqb = intOrSkip(memoryQuotaBytes);
      if (mqb !== null && mqb > 0) ov.memory_quota_bytes = mqb;
      // memory_scopes: only emit when at least one box is ticked.
      // Empty array would default-deny on the substrate side which is
      // probably not what the operator wants if they didn't touch the
      // checkboxes — omit instead so the parent value is preserved.
      const scopes: string[] = [];
      if (memoryScopes.agent) scopes.push("agent");
      if (memoryScopes.user) scopes.push("user");
      if (scopes.length > 0) ov.memory_scopes = scopes;
      // models: emit only the per-tier slots that have at least one
      // complete (provider+model) candidate. Empty per-tier lists are
      // dropped so the substrate keeps the library default for that
      // tier; partial rows were rejected by validateLocal already.
      const models: Record<string, ModelCandidate[]> = {};
      for (const slot of AGENT_TIER_SLOTS) {
        const cands = modelsByTier[slot]
          .map((c) => ({ provider: c.provider.trim(), model: c.model.trim() }))
          .filter((c) => c.provider && c.model);
        if (cands.length > 0) models[slot] = cands;
      }
      if (Object.keys(models).length > 0) ov.models = models;
      return ov;
    }
    if (kind === "skill") {
      const ov: Record<string, unknown> = { body: skillBody };
      if (description.trim()) ov.description = description.trim();
      const tools = parseCommaList(skillAllowedTools);
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
        // raced ahead since the modal opened. Static-only forks omit
        // parent_def_id so the substrate's bootstrap-on-first-fork
        // mechanism (v0.8.22) auto-creates a v1 from yaml.
        const parentDefID =
          forkSource?.def_id && !forkSource.def_id.startsWith("static:")
            ? forkSource.def_id
            : undefined;
        row = await forkDef(
          substrateKind,
          name.trim(),
          overlay,
          promote,
          parentDefID,
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
            systemPrompt={systemPrompt}
            setSystemPrompt={setSystemPrompt}
            allowedTools={allowedTools}
            setAllowedTools={setAllowedTools}
            agentSkills={agentSkills}
            setAgentSkills={setAgentSkills}
            maxTokens={maxTokens}
            setMaxTokens={setMaxTokens}
            maxIterations={maxIterations}
            setMaxIterations={setMaxIterations}
            memoryQuotaBytes={memoryQuotaBytes}
            setMemoryQuotaBytes={setMemoryQuotaBytes}
            memoryScopes={memoryScopes}
            setMemoryScopes={setMemoryScopes}
            agentProviders={agentProviders}
            setAgentProviders={setAgentProviders}
            modelsByTier={modelsByTier}
            setModelsByTier={setModelsByTier}
            droppedCustomTiers={droppedCustomTiers}
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

interface AgentFieldsProps {
  provider: string;
  setProvider: (v: string) => void;
  model: string;
  setModel: (v: string) => void;
  tier: string;
  setTier: (v: string) => void;
  effort: string;
  setEffort: (v: string) => void;
  systemPrompt: string;
  setSystemPrompt: (v: string) => void;
  allowedTools: string;
  setAllowedTools: (v: string) => void;
  agentSkills: string;
  setAgentSkills: (v: string) => void;
  maxTokens: string;
  setMaxTokens: (v: string) => void;
  maxIterations: string;
  setMaxIterations: (v: string) => void;
  memoryQuotaBytes: string;
  setMemoryQuotaBytes: (v: string) => void;
  memoryScopes: { agent: boolean; user: boolean };
  setMemoryScopes: (v: { agent: boolean; user: boolean }) => void;
  agentProviders: string;
  setAgentProviders: (v: string) => void;
  modelsByTier: Record<TierSlot, ModelCandidate[]>;
  setModelsByTier: (v: Record<TierSlot, ModelCandidate[]>) => void;
  droppedCustomTiers: string[];
  submitting: boolean;
}

function AgentFields(props: AgentFieldsProps) {
  const addCandidate = (slot: TierSlot) => {
    props.setModelsByTier({
      ...props.modelsByTier,
      [slot]: [...props.modelsByTier[slot], { provider: "", model: "" }],
    });
  };
  const updateCandidate = (
    slot: TierSlot,
    i: number,
    patch: Partial<ModelCandidate>,
  ) => {
    const next = [...props.modelsByTier[slot]];
    next[i] = { ...next[i]!, ...patch };
    props.setModelsByTier({ ...props.modelsByTier, [slot]: next });
  };
  const removeCandidate = (slot: TierSlot, i: number) => {
    props.setModelsByTier({
      ...props.modelsByTier,
      [slot]: props.modelsByTier[slot].filter((_, idx) => idx !== i),
    });
  };

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
        <label htmlFor="lib-system-prompt">
          system prompt
          <span className="library-modal-field-hint">
            {" "}— freetext markdown; raw newlines preserved
          </span>
        </label>
        <textarea
          id="lib-system-prompt"
          className="library-prompt-textarea mono"
          value={props.systemPrompt}
          onChange={(e) => props.setSystemPrompt(e.target.value)}
          disabled={props.submitting}
          rows={10}
          spellCheck={false}
          placeholder="You are a researcher. Follow these rules…"
        />
      </div>

      <div className="library-form-row">
        <label htmlFor="lib-allowed-tools">
          allowed_tools
          <span className="library-modal-field-hint">
            {" "}— comma-separated tool names (Read, WebFetch, Memory…)
          </span>
        </label>
        <input
          id="lib-allowed-tools"
          type="text"
          value={props.allowedTools}
          onChange={(e) => props.setAllowedTools(e.target.value)}
          disabled={props.submitting}
          placeholder="Read, WebFetch, Memory, Channel"
        />
      </div>

      <div className="library-form-row">
        <label htmlFor="lib-skills">
          skills
          <span className="library-modal-field-hint">
            {" "}— comma-separated skill names to bake into system_prompt
          </span>
        </label>
        <input
          id="lib-skills"
          type="text"
          value={props.agentSkills}
          onChange={(e) => props.setAgentSkills(e.target.value)}
          disabled={props.submitting}
          placeholder="briefing-format, citation-style"
        />
      </div>

      <div className="library-form-row library-form-row-quad">
        <label htmlFor="lib-max-tokens">max_tokens</label>
        <input
          id="lib-max-tokens"
          type="number"
          min="0"
          value={props.maxTokens}
          onChange={(e) => props.setMaxTokens(e.target.value)}
          disabled={props.submitting}
          placeholder="0 = default"
        />
        <label htmlFor="lib-max-iterations">max_iterations</label>
        <input
          id="lib-max-iterations"
          type="number"
          min="0"
          value={props.maxIterations}
          onChange={(e) => props.setMaxIterations(e.target.value)}
          disabled={props.submitting}
          placeholder="0 = default"
        />
      </div>

      <div className="library-form-row library-form-row-quad">
        <label htmlFor="lib-memory-quota">memory_quota_bytes</label>
        <input
          id="lib-memory-quota"
          type="number"
          min="0"
          value={props.memoryQuotaBytes}
          onChange={(e) => props.setMemoryQuotaBytes(e.target.value)}
          disabled={props.submitting}
          placeholder="0 = use global default"
        />
        <label>memory_scopes</label>
        <div className="library-checkbox-group">
          <label>
            <input
              type="checkbox"
              checked={props.memoryScopes.agent}
              onChange={(e) =>
                props.setMemoryScopes({
                  ...props.memoryScopes,
                  agent: e.target.checked,
                })
              }
              disabled={props.submitting}
            />{" "}
            agent
          </label>
          <label>
            <input
              type="checkbox"
              checked={props.memoryScopes.user}
              onChange={(e) =>
                props.setMemoryScopes({
                  ...props.memoryScopes,
                  user: e.target.checked,
                })
              }
              disabled={props.submitting}
            />{" "}
            user
          </label>
        </div>
      </div>

      <div className="library-form-row">
        <label htmlFor="lib-providers">
          providers
          <span className="library-modal-field-hint">
            {" "}— comma-separated priority list; overrides library
            ProviderPriority when set
          </span>
        </label>
        <input
          id="lib-providers"
          type="text"
          value={props.agentProviders}
          onChange={(e) => props.setAgentProviders(e.target.value)}
          disabled={props.submitting}
          placeholder="anthropic, openai, deepseek"
        />
      </div>

      <div className="library-form-row">
        <label>
          models (per-tier)
          <span className="library-modal-field-hint">
            {" "}— per-tier candidate list; leave a tier empty to inherit
            from the library Tiers map
          </span>
        </label>
        {props.droppedCustomTiers.length > 0 && (
          <div className="library-models-warning">
            ⚠ This definition has custom tier name
            {props.droppedCustomTiers.length === 1 ? "" : "s"}{" "}
            <code>{props.droppedCustomTiers.join(", ")}</code> that this
            modal does not render. Submitting any change to the models
            grid below will replace the entire models map and drop those
            custom tiers from the fork. Edit via yaml to preserve them.
          </div>
        )}
        <div className="library-models-grid">
          {AGENT_TIER_SLOTS.map((slot) => (
            <div key={slot} className="library-models-tier-row">
              <div className="library-models-tier-header">
                <span className="library-models-tier-name">{slot}</span>
                <button
                  type="button"
                  className="library-schema-hint-toggle"
                  onClick={() => addCandidate(slot)}
                  disabled={props.submitting}
                >
                  + add candidate
                </button>
              </div>
              {props.modelsByTier[slot].length === 0 && (
                <span className="library-modal-field-hint">
                  (inherits library default)
                </span>
              )}
              {props.modelsByTier[slot].map((c, i) => (
                <div key={i} className="library-models-candidate-row">
                  <input
                    type="text"
                    placeholder="provider"
                    value={c.provider}
                    onChange={(e) =>
                      updateCandidate(slot, i, { provider: e.target.value })
                    }
                    disabled={props.submitting}
                  />
                  <input
                    type="text"
                    placeholder="model"
                    value={c.model}
                    onChange={(e) =>
                      updateCandidate(slot, i, { model: e.target.value })
                    }
                    disabled={props.submitting}
                  />
                  <button
                    type="button"
                    onClick={() => removeCandidate(slot, i)}
                    disabled={props.submitting}
                    title="remove"
                  >
                    ×
                  </button>
                </div>
              ))}
            </div>
          ))}
        </div>
      </div>
    </>
  );
}

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
          allowed_tools
          <span className="library-modal-field-hint">
            {" "}— comma-separated; must be a subset of the calling agent's tools
          </span>
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
          className="library-prompt-textarea mono"
          value={props.body}
          onChange={(e) => props.setBody(e.target.value)}
          disabled={props.submitting}
          rows={12}
          spellCheck={false}
          placeholder="# Skill instructions in markdown…"
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

// pickNumberAsString reads a numeric field for pre-fill. Returns "" for
// unset / non-numeric so the <input type="number"> stays empty (which
// the operator reads as "use default" — matches the placeholder text).
function pickNumberAsString(def: unknown, key: string): string {
  if (!def || typeof def !== "object") return "";
  const v = (def as Record<string, unknown>)[key];
  if (typeof v === "number" && Number.isFinite(v)) return String(v);
  return "";
}

// pickModelsByTier reads the agent's `models` map and projects it onto
// the three fixed tier slots the modal renders. Tier names outside
// "low"/"middle"/"high" in the source are silently dropped — the
// operator who needs custom tier names can still edit them via yaml.
function pickModelsByTier(def: unknown): Record<TierSlot, ModelCandidate[]> {
  const out: Record<TierSlot, ModelCandidate[]> = {
    low: [],
    middle: [],
    high: [],
  };
  if (!def || typeof def !== "object") return out;
  const v = (def as Record<string, unknown>)["models"];
  if (!v || typeof v !== "object" || Array.isArray(v)) return out;
  for (const slot of AGENT_TIER_SLOTS) {
    const arr = (v as Record<string, unknown>)[slot];
    if (!Array.isArray(arr)) continue;
    const cands: ModelCandidate[] = [];
    for (const item of arr) {
      if (!item || typeof item !== "object") continue;
      const p = (item as Record<string, unknown>)["provider"];
      const m = (item as Record<string, unknown>)["model"];
      if (typeof p === "string" && typeof m === "string") {
        cands.push({ provider: p, model: m });
      }
    }
    out[slot] = cands;
  }
  return out;
}

// pickCustomTierNames returns tier names present in the source's
// `models` map that AREN'T in the three fixed slots the modal renders.
// Used to surface a warning before the operator submits a fork that
// would otherwise silently drop those tiers (the substrate's overlay
// merge does a full replacement of `models`).
function pickCustomTierNames(def: unknown): string[] {
  if (!def || typeof def !== "object") return [];
  const v = (def as Record<string, unknown>)["models"];
  if (!v || typeof v !== "object" || Array.isArray(v)) return [];
  const out: string[] = [];
  const fixed = new Set<string>(AGENT_TIER_SLOTS);
  for (const key of Object.keys(v as Record<string, unknown>)) {
    if (!fixed.has(key)) out.push(key);
  }
  return out;
}

// parseCommaList splits a comma-separated string into trimmed,
// non-empty entries. Handles whitespace + trailing commas + duplicate
// spaces. Used for allowed_tools, skills, and providers.
function parseCommaList(s: string): string[] {
  return s
    .split(",")
    .map((t) => t.trim())
    .filter((t) => t.length > 0);
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
  // The static-name refusal phrasing differs per substrate — agentdef
  // says "matches a static cfg.Agents entry", mcpserverdef says
  // "matches a static cfg.MCPServers entry", and skilldef says
  // "matches a static SKILL.md entry". Cover all three.
  if (
    innerText.includes("matches a static cfg.") ||
    innerText.includes("matches a static SKILL.md entry")
  ) {
    return "An entry with this name is defined in yaml. Pick a different name.";
  }
  // Fork-with-no-parent surfaces internal env var names + path text
  // ("LOOMCYCLE_SKILLS_ROOT unset", "static cfg.Agents entry") in the
  // raw fallback. Catch it explicitly so operators see Create vs Edit
  // guidance instead of a confusing config dump.
  if (innerText.includes("has neither a DB version nor a static")) {
    return "No existing version found for this name. Use Create instead of Edit.";
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
