import { useEffect, useMemo, useRef, useState } from "react";
import { load as yamlLoad } from "js-yaml";
import type { DefRow, LibraryEntry, SubstrateKind } from "../types";
import { useLibraryData } from "../lib/dataLayer";

// LibraryEditModal — Library admin UI.
//
// Hybrid form for create / fork on the three substrate flavors. The
// common, high-value overlay fields render as structured inputs. The
// v0.10.4 JSON catch-all for the WHOLE agent overlay was removed in
// v0.11.6 because operators hit two real pain points:
//
//   1. Raw newlines inside the agent's `system_prompt` produced
//      invalid JSON and surfaced as a confusing "JSON parse error"
//      on submit.
//   2. A single missing comma anywhere in the JSON body sunk the
//      whole submit, with no per-field validation.
//
// The AgentDef overlay keeps growing (sampling, channels, interruption,
// the *_def_scopes family, …) and dedicated controls can't keep pace
// with every field. So a SCOPED advanced editor is back (agents only):
// a collapsible JSON/YAML textarea that holds ONLY keys not covered by
// a structured control, shallow-merged over the structured overlay at
// submit. The two old pain points don't apply: `system_prompt` stays in
// its own textarea (no newlines-in-JSON), and an EMPTY advanced box
// never blocks submit (a malformed NON-empty box does, with an inline
// error). This deliberately differs from the removed whole-overlay
// catch-all — the structured fields remain authoritative for the common
// case; the box is the escape hatch for the long tail.
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
  // RFC AU: whether the runtime permits dynamic stdio MCP servers
  // (LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO). When false, the stdio transport
  // option is hidden — a stdio server would 422 anyway. From
  // principal.capabilities.mcp_allow_dynamic_stdio.
  stdioAllowed?: boolean;
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
  stdioAllowed,
  onClose,
  onSaved,
}: LibraryEditModalProps) {
  const data = useLibraryData();
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
  // RFC J inline code-js body. Shown only when provider is "code-js".
  const [codeBody, setCodeBody] = useState(
    pickString(forkSource?.definition, "code_body"),
  );
  const [allowedTools, setAllowedTools] = useState(
    pickStringArray(forkSource?.definition, "tools").join(", "),
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
  const [maxConcurrentChildren, setMaxConcurrentChildren] = useState(
    pickNumberAsString(forkSource?.definition, "max_concurrent_children"),
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

  // --- Sampling (v0.28.0 per-agent LLM sampling block). String-typed
  // state, mirroring the number-as-string pattern above: blank = unset
  // (inherit provider/agent default), "0" = an EXPLICIT value. This is
  // load-bearing for `temperature` — 0.0 is deterministic, distinct from
  // "use the default" (the substrate's Sampling uses pointer fields for
  // exactly this reason), so the sampling parser must NOT drop "0".
  const [sampTemperature, setSampTemperature] = useState(
    pickNestedNumberAsString(forkSource?.definition, "sampling", "temperature"),
  );
  const [sampTopP, setSampTopP] = useState(
    pickNestedNumberAsString(forkSource?.definition, "sampling", "top_p"),
  );
  const [sampTopK, setSampTopK] = useState(
    pickNestedNumberAsString(forkSource?.definition, "sampling", "top_k"),
  );
  const [sampFrequencyPenalty, setSampFrequencyPenalty] = useState(
    pickNestedNumberAsString(forkSource?.definition, "sampling", "frequency_penalty"),
  );
  const [sampPresencePenalty, setSampPresencePenalty] = useState(
    pickNestedNumberAsString(forkSource?.definition, "sampling", "presence_penalty"),
  );
  const [sampSeed, setSampSeed] = useState(
    pickNestedNumberAsString(forkSource?.definition, "sampling", "seed"),
  );
  const [sampStop, setSampStop] = useState(
    pickSamplingStop(forkSource?.definition).join(", "),
  );

  // --- Advanced overlay (agents only). A free-form JSON/YAML object
  // for overlay keys without a dedicated control (channels, interruption,
  // evaluation_scopes, memory_backend, compaction, retry_attempts,
  // unbounded_iterations, the *_def_scopes family, …). PRE-FILLED on
  // fork/edit with the source's current advanced keys (values), so a saved
  // overlay is visible AND editable when the editor is reopened — it used to
  // start empty and surface saved values only as a key-name hint, which read
  // as "the overlay was never saved". Opens expanded when there's content.
  const initialAdvancedText = useMemo(
    () => initialAdvancedOverlayText(forkSource?.definition),
    [forkSource],
  );
  const [advancedOpen, setAdvancedOpen] = useState(initialAdvancedText !== "");
  const [advancedFormat, setAdvancedFormat] = useState<"json" | "yaml">("json");
  const [advancedText, setAdvancedText] = useState(initialAdvancedText);
  const [advancedErr, setAdvancedErr] = useState<string | null>(null);
  // Keys present on the fork source that no structured control maps. They're
  // pre-filled into the box above; the hint names them (and notes the
  // remove-falls-back-to-inherited subtlety of the substrate's additive merge).
  const inheritedUnknownKeys = useMemo(
    () => unknownSourceKeys(forkSource?.definition),
    [forkSource],
  );

  // --- Skill-flavor specific
  const [skillBody, setSkillBody] = useState<string>(
    pickString(forkSource?.definition, "body"),
  );
  const [skillAllowedTools, setSkillAllowedTools] = useState<string>(
    pickStringArray(forkSource?.definition, "tools").join(", "),
  );

  // --- MCP-flavor specific
  type Transport = "http" | "streamable-http" | "stdio";
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
  // stdio transport (RFC AU / F31). Command runs on the loomcycle host — the
  // transport option is only shown when stdioAllowed.
  const [mcpCommand, setMcpCommand] = useState<string>(
    pickString(forkSource?.definition, "command"),
  );
  const [mcpArgs, setMcpArgs] = useState<string>(
    pickStringArray(forkSource?.definition, "args").join(", "),
  );
  const [mcpEnv, setMcpEnv] = useState<{ key: string; value: string }[]>(() => {
    const e = pickStringMap(forkSource?.definition, "env");
    const entries = Object.entries(e);
    return entries.length === 0
      ? [{ key: "", value: "" }]
      : entries.map(([key, value]) => ({ key, value }));
  });

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
      // A name is reclaimable when it has NO live active version and isn't
      // static — every version retired (the active pointer was cleared on
      // retire), so a fresh create just allocates the next version. Block
      // only a name that's static or has a live, non-retired active def
      // (mirrors the backend: AgentDefCreate refuses static names, but
      // versions onward otherwise). This is the soft-reclaim fix: a retired
      // agent no longer blocks recreating its name (e.g. to grant more tools).
      const reclaimable =
        hit &&
        !hit.in_static &&
        (!hit.active_def_id || hit.active_retired === true);
      if (hit && !reclaimable) {
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
        ["max_concurrent_children", maxConcurrentChildren],
      ];
      for (const [label, raw] of numChecks) {
        if (raw.trim() === "") continue; // empty = unset = OK
        const n = Number(raw);
        if (!Number.isFinite(n) || n < 0 || !Number.isInteger(n)) {
          return `${label} must be a non-negative integer (got "${raw}").`;
        }
      }
      // A code-js agent's behaviour IS its inline body — refuse an empty
      // one locally (the substrate would reject it at create too, but a
      // local check is a clearer message). Only enforced for code-js so
      // LLM agents are unaffected.
      if (provider.trim() === "code-js" && !codeBody.trim()) {
        return "code_body is required for a code-js agent.";
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
      // Sampling range checks — mirror config.Sampling.Validate() so the
      // operator gets a per-field message instead of a 422 from the
      // substrate. These are guards, not authority: the provider API is
      // the final arbiter of per-model ranges. A blank field is unset
      // (skipped); "0" is a real value and IS range-checked.
      const sampErr = validateSampling({
        temperature: sampTemperature,
        top_p: sampTopP,
        top_k: sampTopK,
        frequency_penalty: sampFrequencyPenalty,
        presence_penalty: sampPresencePenalty,
        seed: sampSeed,
        stop: sampStop,
      });
      if (sampErr) return sampErr;
    }
    if (kind === "skill") {
      if (!skillBody.trim()) return "Skill body is required (substrate refuses empty bodies).";
    }
    if (kind === "mcp-server") {
      if (mcpTransport === "stdio") {
        if (!mcpCommand.trim()) return "Command is required for a stdio server.";
      } else {
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
      // code_body: raw source preserved verbatim (whitespace is
      // hash-significant). Omit when empty so a fork of an LLM agent
      // doesn't write a "" body.
      if (codeBody.trim()) ov.code_body = codeBody;
      const tools = parseCommaList(allowedTools);
      if (tools.length > 0) ov.tools = tools;
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
      const mcc = intOrSkip(maxConcurrentChildren);
      if (mcc !== null && mcc > 0) ov.max_concurrent_children = mcc;
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
      // sampling: emit only the fields the operator set. `num()` keeps an
      // explicit "0" (temperature 0.0 is deterministic — see the state
      // comment) and omits a blank field so the substrate inherits the
      // parent/agent default. top_k + seed are emitted as integers.
      const num = (s: string): number | null => {
        const t = s.trim();
        if (t === "") return null;
        const n = Number(t);
        return Number.isFinite(n) ? n : null;
      };
      const sampling: Record<string, unknown> = {};
      const tv = num(sampTemperature);
      if (tv !== null) sampling.temperature = tv;
      const pv = num(sampTopP);
      if (pv !== null) sampling.top_p = pv;
      const kv = num(sampTopK);
      if (kv !== null) sampling.top_k = kv;
      const fv = num(sampFrequencyPenalty);
      if (fv !== null) sampling.frequency_penalty = fv;
      const ppv = num(sampPresencePenalty);
      if (ppv !== null) sampling.presence_penalty = ppv;
      const sv = num(sampSeed);
      if (sv !== null) sampling.seed = sv;
      const stop = parseCommaList(sampStop);
      if (stop.length > 0) sampling.stop = stop;
      if (Object.keys(sampling).length > 0) ov.sampling = sampling;
      return ov;
    }
    if (kind === "skill") {
      const ov: Record<string, unknown> = { body: skillBody };
      if (description.trim()) ov.description = description.trim();
      const tools = parseCommaList(skillAllowedTools);
      if (tools.length > 0) ov.tools = tools;
      return ov;
    }
    // mcp-server
    if (mcpTransport === "stdio") {
      const env: Record<string, string> = {};
      mcpEnv.forEach(({ key, value }) => {
        const k = key.trim();
        if (k) env[k] = value;
      });
      const ov: Record<string, unknown> = {
        transport: "stdio",
        command: mcpCommand.trim(),
      };
      const args = parseCommaList(mcpArgs);
      if (args.length > 0) ov.args = args;
      if (Object.keys(env).length > 0) ov.env = env;
      if (description.trim()) ov.description = description.trim();
      return ov;
    }
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
    // Advanced overlay (agents only): parse-on-submit. An EMPTY box never
    // blocks; a non-empty box that won't parse blocks with an inline
    // error. Parsed keys shallow-merge OVER the structured overlay
    // (advanced wins per-key) — the collision warning in the UI tells
    // the operator which structured fields they're shadowing.
    let advancedOv: Record<string, unknown> = {};
    if (kind === "agent" && advancedText.trim()) {
      const { obj, err } = parseAdvancedOverlay(advancedText, advancedFormat);
      if (err || !obj) {
        setAdvancedErr(err ?? "advanced overlay must be a JSON/YAML object");
        setAdvancedOpen(true);
        setSubmitErr("Fix the advanced overlay before saving.");
        return;
      }
      advancedOv = obj;
    }
    setAdvancedErr(null);
    setSubmitErr(null);
    setSubmitting(true);
    try {
      const substrateKind = kindToSubstrate(kind);
      const overlay = { ...buildOverlay(), ...advancedOv };
      let row: DefRow;
      if (mode === "create") {
        row = await data.createDef(substrateKind, name.trim(), overlay, promote);
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
        row = await data.forkDef(
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
            codeBody={codeBody}
            setCodeBody={setCodeBody}
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
            maxConcurrentChildren={maxConcurrentChildren}
            setMaxConcurrentChildren={setMaxConcurrentChildren}
            memoryScopes={memoryScopes}
            setMemoryScopes={setMemoryScopes}
            agentProviders={agentProviders}
            setAgentProviders={setAgentProviders}
            modelsByTier={modelsByTier}
            setModelsByTier={setModelsByTier}
            droppedCustomTiers={droppedCustomTiers}
            sampTemperature={sampTemperature}
            setSampTemperature={setSampTemperature}
            sampTopP={sampTopP}
            setSampTopP={setSampTopP}
            sampTopK={sampTopK}
            setSampTopK={setSampTopK}
            sampFrequencyPenalty={sampFrequencyPenalty}
            setSampFrequencyPenalty={setSampFrequencyPenalty}
            sampPresencePenalty={sampPresencePenalty}
            setSampPresencePenalty={setSampPresencePenalty}
            sampSeed={sampSeed}
            setSampSeed={setSampSeed}
            sampStop={sampStop}
            setSampStop={setSampStop}
            submitting={submitting}
          />
        )}

        {kind === "agent" && (
          <AgentAdvancedOverlay
            open={advancedOpen}
            setOpen={setAdvancedOpen}
            format={advancedFormat}
            setFormat={setAdvancedFormat}
            text={advancedText}
            setText={(v) => {
              setAdvancedText(v);
              if (advancedErr) setAdvancedErr(null);
            }}
            err={advancedErr}
            inheritedUnknownKeys={inheritedUnknownKeys}
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
            command={mcpCommand}
            setCommand={setMcpCommand}
            args={mcpArgs}
            setArgs={setMcpArgs}
            env={mcpEnv}
            setEnv={setMcpEnv}
            stdioAllowed={stdioAllowed === true}
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
  codeBody: string;
  setCodeBody: (v: string) => void;
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
  maxConcurrentChildren: string;
  setMaxConcurrentChildren: (v: string) => void;
  memoryScopes: { agent: boolean; user: boolean };
  setMemoryScopes: (v: { agent: boolean; user: boolean }) => void;
  agentProviders: string;
  setAgentProviders: (v: string) => void;
  modelsByTier: Record<TierSlot, ModelCandidate[]>;
  setModelsByTier: (v: Record<TierSlot, ModelCandidate[]>) => void;
  droppedCustomTiers: string[];
  sampTemperature: string;
  setSampTemperature: (v: string) => void;
  sampTopP: string;
  setSampTopP: (v: string) => void;
  sampTopK: string;
  setSampTopK: (v: string) => void;
  sampFrequencyPenalty: string;
  setSampFrequencyPenalty: (v: string) => void;
  sampPresencePenalty: string;
  setSampPresencePenalty: (v: string) => void;
  sampSeed: string;
  setSampSeed: (v: string) => void;
  sampStop: string;
  setSampStop: (v: string) => void;
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
      {props.droppedCustomTiers.length > 0 && (
        // v0.11.7 — hoisted to the top of AgentFields so it's visible
        // before the operator scrolls. Burying it inside the models
        // grid (v0.11.6) meant an operator forking just to tweak
        // system_prompt would Save without ever seeing the warning.
        <div className="library-models-warning">
          ⚠ This definition has custom tier name
          {props.droppedCustomTiers.length === 1 ? "" : "s"}{" "}
          <code>{props.droppedCustomTiers.join(", ")}</code> that this
          modal does not render. The substrate's overlay merge does a
          full replacement of <code>models</code>, so if any standard
          tier (low / middle / high) below has a candidate, those
          custom tiers will be dropped from the fork. Edit via yaml to
          preserve them.
        </div>
      )}
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

      {props.provider.trim() === "code-js" && (
        <div className="library-form-row">
          <label htmlFor="lib-code-body">
            code body
            <span className="library-modal-field-hint">
              {" "}— inline JavaScript orchestrator (RFC J); requires
              LOOMCYCLE_CODE_AGENTS_ENABLED on the sidecar
            </span>
          </label>
          <textarea
            id="lib-code-body"
            className="library-prompt-textarea mono"
            value={props.codeBody}
            onChange={(e) => props.setCodeBody(e.target.value)}
            disabled={props.submitting}
            rows={14}
            spellCheck={false}
            placeholder={"function run(input) {\n  return { final_text: \"…\" };\n}"}
          />
        </div>
      )}

      <div className="library-form-row">
        <label htmlFor="lib-allowed-tools">
          tools
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
        <label htmlFor="lib-max-concurrent-children">
          max_concurrent_children
          <span className="library-modal-field-hint">
            {" "}— cap on Agent.parallel_spawn fan-out from this agent;
            0 = use runtime default (4). Sequential Agent.spawn is
            unaffected.
          </span>
        </label>
        <input
          id="lib-max-concurrent-children"
          type="number"
          min="0"
          value={props.maxConcurrentChildren}
          onChange={(e) => props.setMaxConcurrentChildren(e.target.value)}
          disabled={props.submitting}
          placeholder="0 = default"
        />
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
            {" "}— per-tier candidate list; model may be a literal model name
            or an alias from the top-level models: map (e.g. local-gemma);
            leave a tier empty to inherit from the library Tiers map
          </span>
        </label>
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
                    placeholder="model or alias"
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

      <div className="library-form-row">
        <label>
          sampling
          <span className="library-modal-field-hint">
            {" "}— per-agent LLM sampling; blank = provider default,
            0 = explicit (temperature 0 is deterministic)
          </span>
        </label>
      </div>
      <div className="library-form-row library-form-row-quad">
        <label htmlFor="lib-samp-temperature">temperature</label>
        <input
          id="lib-samp-temperature"
          type="number"
          step="0.1"
          value={props.sampTemperature}
          onChange={(e) => props.setSampTemperature(e.target.value)}
          disabled={props.submitting}
          placeholder="blank = default; 0 = deterministic"
        />
        <label htmlFor="lib-samp-top-p">top_p</label>
        <input
          id="lib-samp-top-p"
          type="number"
          step="0.05"
          value={props.sampTopP}
          onChange={(e) => props.setSampTopP(e.target.value)}
          disabled={props.submitting}
          placeholder="0–1"
        />
      </div>
      <div className="library-form-row library-form-row-quad">
        <label htmlFor="lib-samp-top-k">top_k</label>
        <input
          id="lib-samp-top-k"
          type="number"
          step="1"
          min="1"
          value={props.sampTopK}
          onChange={(e) => props.setSampTopK(e.target.value)}
          disabled={props.submitting}
          placeholder="≥1 (Anthropic/Gemini/Ollama)"
        />
        <label htmlFor="lib-samp-seed">seed</label>
        <input
          id="lib-samp-seed"
          type="number"
          step="1"
          value={props.sampSeed}
          onChange={(e) => props.setSampSeed(e.target.value)}
          disabled={props.submitting}
          placeholder="reproducibility (OpenAI/Gemini/Ollama)"
        />
      </div>
      <div className="library-form-row library-form-row-quad">
        <label htmlFor="lib-samp-freq">frequency_penalty</label>
        <input
          id="lib-samp-freq"
          type="number"
          step="0.1"
          value={props.sampFrequencyPenalty}
          onChange={(e) => props.setSampFrequencyPenalty(e.target.value)}
          disabled={props.submitting}
          placeholder="-2–2 (OpenAI/DeepSeek)"
        />
        <label htmlFor="lib-samp-pres">presence_penalty</label>
        <input
          id="lib-samp-pres"
          type="number"
          step="0.1"
          value={props.sampPresencePenalty}
          onChange={(e) => props.setSampPresencePenalty(e.target.value)}
          disabled={props.submitting}
          placeholder="-2–2 (OpenAI/DeepSeek)"
        />
      </div>
      <div className="library-form-row">
        <label htmlFor="lib-samp-stop">
          stop
          <span className="library-modal-field-hint">
            {" "}— comma-separated stop sequences (max 8)
          </span>
        </label>
        <input
          id="lib-samp-stop"
          type="text"
          value={props.sampStop}
          onChange={(e) => props.setSampStop(e.target.value)}
          disabled={props.submitting}
          placeholder="END, \n\nHuman:"
        />
      </div>
    </>
  );
}

// AgentAdvancedOverlay — the scoped escape hatch for overlay keys that
// have no dedicated control. JSON or YAML; parsed on submit by the
// parent (parseAdvancedOverlay). Empty = no-op (never blocks); a
// non-empty malformed body blocks with the inline `err`.
function AgentAdvancedOverlay(props: {
  open: boolean;
  setOpen: (v: boolean) => void;
  format: "json" | "yaml";
  setFormat: (v: "json" | "yaml") => void;
  text: string;
  setText: (v: string) => void;
  err: string | null;
  inheritedUnknownKeys: string[];
  submitting: boolean;
}) {
  // Live, non-blocking collision detection: which structured-control keys
  // would the advanced body shadow? Parsed best-effort for display only.
  const collisions = useMemo(() => {
    if (!props.text.trim()) return [];
    const { obj } = parseAdvancedOverlay(props.text, props.format);
    if (!obj) return [];
    return Object.keys(obj).filter((k) => STRUCTURED_AGENT_KEYS.has(k));
  }, [props.text, props.format]);

  return (
    <div className="library-form-row library-advanced-overlay">
      <button
        type="button"
        className="library-schema-hint-toggle"
        onClick={() => props.setOpen(!props.open)}
        disabled={props.submitting}
      >
        {props.open ? "▾" : "▸"} advanced (raw overlay) — channels,
        interruption, *_def_scopes, …
      </button>
      {props.open && (
        <>
          {props.inheritedUnknownKeys.length > 0 && (
            <div className="library-modal-field-hint">
              pre-filled from the source: <code>{props.inheritedUnknownKeys.join(", ")}</code>.
              Edit a value to change it; a key you delete falls back to the
              source's value (the substrate merge is additive — it can't unset).
            </div>
          )}
          <div className="library-advanced-format">
            <label>
              <input
                type="radio"
                name="adv-format"
                checked={props.format === "json"}
                onChange={() => props.setFormat("json")}
                disabled={props.submitting}
              />{" "}
              JSON
            </label>
            <label>
              <input
                type="radio"
                name="adv-format"
                checked={props.format === "yaml"}
                onChange={() => props.setFormat("yaml")}
                disabled={props.submitting}
              />{" "}
              YAML
            </label>
          </div>
          <textarea
            className="library-prompt-textarea mono"
            value={props.text}
            onChange={(e) => props.setText(e.target.value)}
            disabled={props.submitting}
            rows={8}
            spellCheck={false}
            placeholder={
              props.format === "json"
                ? '{\n  "interruption": { "enabled": true },\n  "retry_attempts": 3\n}'
                : "interruption:\n  enabled: true\nretry_attempts: 3"
            }
          />
          <div className="library-modal-field-hint">
            Keys not covered by the fields above. Merged over them
            (advanced wins). allowed_hosts is intentionally NOT settable
            here — it's a caller-authoritative trust boundary, set per-run.
          </div>
          {collisions.length > 0 && (
            <div className="library-models-warning">
              ⚠ overrides structured field
              {collisions.length === 1 ? "" : "s"}:{" "}
              <code>{collisions.join(", ")}</code> — the value(s) above are
              ignored for these.
            </div>
          )}
          {props.err && <div className="modal-err">{props.err}</div>}
        </>
      )}
    </div>
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
          tools
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

// KVGrid renders an editable key/value grid (used for both MCP http headers
// and stdio env). Empty rows are dropped at buildOverlay time.
function KVGrid(props: {
  label: string;
  rows: { key: string; value: string }[];
  setRows: (v: { key: string; value: string }[]) => void;
  keyPlaceholder: string;
  valuePlaceholder: string;
  submitting: boolean;
}) {
  const update = (i: number, patch: Partial<{ key: string; value: string }>) => {
    const next = [...props.rows];
    next[i] = { ...next[i]!, ...patch };
    props.setRows(next);
  };
  const add = () => props.setRows([...props.rows, { key: "", value: "" }]);
  const remove = (i: number) => {
    if (props.rows.length === 1) {
      props.setRows([{ key: "", value: "" }]);
      return;
    }
    props.setRows(props.rows.filter((_, idx) => idx !== i));
  };
  return (
    <div className="library-form-row">
      <label>
        {props.label}
        <button
          type="button"
          className="library-schema-hint-toggle"
          onClick={add}
          disabled={props.submitting}
        >
          + add row
        </button>
      </label>
      <div className="library-headers-grid">
        {props.rows.map((h, i) => (
          <div key={i} className="library-header-row">
            <input
              type="text"
              placeholder={props.keyPlaceholder}
              value={h.key}
              onChange={(e) => update(i, { key: e.target.value })}
              disabled={props.submitting}
            />
            <input
              type="text"
              placeholder={props.valuePlaceholder}
              value={h.value}
              onChange={(e) => update(i, { value: e.target.value })}
              disabled={props.submitting}
            />
            <button
              type="button"
              onClick={() => remove(i)}
              disabled={props.submitting}
              title="remove"
            >
              ×
            </button>
          </div>
        ))}
      </div>
    </div>
  );
}

function McpFields(props: {
  transport: "http" | "streamable-http" | "stdio";
  setTransport: (v: "http" | "streamable-http" | "stdio") => void;
  url: string;
  setUrl: (v: string) => void;
  headers: { key: string; value: string }[];
  setHeaders: (v: { key: string; value: string }[]) => void;
  command: string;
  setCommand: (v: string) => void;
  args: string;
  setArgs: (v: string) => void;
  env: { key: string; value: string }[];
  setEnv: (v: { key: string; value: string }[]) => void;
  stdioAllowed: boolean;
  submitting: boolean;
}) {
  const isStdio = props.transport === "stdio";
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
          {props.stdioAllowed ? (
            <label>
              <input
                type="radio"
                name="lib-mcp-transport"
                value="stdio"
                checked={isStdio}
                onChange={() => props.setTransport("stdio")}
                disabled={props.submitting}
              />{" "}
              stdio
            </label>
          ) : (
            <span className="library-radio-note">
              stdio disabled (operator sets LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO=1)
            </span>
          )}
        </div>
      </div>
      {isStdio ? (
        <>
          <div className="library-form-row">
            <label htmlFor="lib-mcp-command">
              command
              <span className="library-modal-field-hint">
                {" "}— runs on the loomcycle host (arbitrary command, RCE-class trust)
              </span>
            </label>
            <input
              id="lib-mcp-command"
              type="text"
              value={props.command}
              onChange={(e) => props.setCommand(e.target.value)}
              disabled={props.submitting}
              placeholder="npx"
            />
          </div>
          <div className="library-form-row">
            <label htmlFor="lib-mcp-args">
              args
              <span className="library-modal-field-hint"> — comma-separated</span>
            </label>
            <input
              id="lib-mcp-args"
              type="text"
              value={props.args}
              onChange={(e) => props.setArgs(e.target.value)}
              disabled={props.submitting}
              placeholder="-y, @scope/server, ${LOOMCYCLE_ROOT}"
            />
          </div>
          <KVGrid
            label="env"
            rows={props.env}
            setRows={props.setEnv}
            keyPlaceholder="ENV_NAME"
            valuePlaceholder="value (e.g. ${LOOMCYCLE_X_TOKEN})"
            submitting={props.submitting}
          />
        </>
      ) : (
        <>
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
          <KVGrid
            label="headers"
            rows={props.headers}
            setRows={props.setHeaders}
            keyPlaceholder="header-name"
            valuePlaceholder="value (e.g. Bearer ${LOOMCYCLE_X_TOKEN})"
            submitting={props.submitting}
          />
        </>
      )}
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

// Overlay keys owned by a dedicated structured control. The advanced
// overlay warns when it shadows one of these, and unknownSourceKeys()
// excludes them from the "inherited" hint.
const STRUCTURED_AGENT_KEYS = new Set<string>([
  "description",
  "provider",
  "model",
  "tier",
  "effort",
  "system_prompt",
  "code_body",
  "tools",
  "skills",
  "providers",
  "max_tokens",
  "max_iterations",
  "memory_quota_bytes",
  "max_concurrent_children",
  "memory_scopes",
  "models",
  "sampling",
]);

// Server-set / immutable / derived keys that are NOT operator overlay
// fields — excluded from the "inherited unknown keys" hint so we don't
// suggest re-typing def_id/version/etc.
const NON_OVERLAY_KEYS = new Set<string>([
  "name",
  "def_id",
  "version",
  "parent_def_id",
  "created_at",
  "created_by_agent_id",
  "created_by_run_id",
  "bootstrapped_from_static",
  "content_sha256",
  "retired",
  "system_prompt_base", // derived: pre-skill-bake snapshot
]);

// pickNestedNumberAsString reads def[parent][key] for sampling pre-fill.
// Returns "" for unset/non-numeric; returns "0" for an explicit 0 (the
// 0-vs-unset distinction the substrate's pointer fields preserve).
function pickNestedNumberAsString(
  def: unknown,
  parent: string,
  key: string,
): string {
  if (!def || typeof def !== "object") return "";
  const p = (def as Record<string, unknown>)[parent];
  if (!p || typeof p !== "object" || Array.isArray(p)) return "";
  const v = (p as Record<string, unknown>)[key];
  if (typeof v === "number" && Number.isFinite(v)) return String(v);
  return "";
}

// pickSamplingStop reads def.sampling.stop as a string[].
function pickSamplingStop(def: unknown): string[] {
  if (!def || typeof def !== "object") return [];
  const s = (def as Record<string, unknown>)["sampling"];
  if (!s || typeof s !== "object" || Array.isArray(s)) return [];
  const stop = (s as Record<string, unknown>)["stop"];
  if (!Array.isArray(stop)) return [];
  return stop.filter((x): x is string => typeof x === "string");
}

// unknownSourceKeys returns the source definition's keys that no
// structured control maps and that aren't server-set — i.e. overlay
// fields the operator set via the advanced box (channels, interruption,
// retry_attempts, *_def_scopes, …). The substrate's per-field merge
// carries them forward unchanged on a fork, so we only hint at them.
function unknownSourceKeys(def: unknown): string[] {
  if (!def || typeof def !== "object") return [];
  const out: string[] = [];
  for (const key of Object.keys(def as Record<string, unknown>)) {
    if (STRUCTURED_AGENT_KEYS.has(key) || NON_OVERLAY_KEYS.has(key)) continue;
    out.push(key);
  }
  return out;
}

// unknownSourceOverlay returns the {key: value} object for the source's
// advanced-overlay keys (the unknownSourceKeys set) — used to pre-fill the
// advanced box so a saved overlay round-trips visibly in the editor.
function unknownSourceOverlay(def: unknown): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (!def || typeof def !== "object") return out;
  const d = def as Record<string, unknown>;
  for (const key of unknownSourceKeys(def)) {
    out[key] = d[key];
  }
  return out;
}

// initialAdvancedOverlayText pretty-prints unknownSourceOverlay as JSON for the
// advanced box's initial value; "" when the source has no such keys.
function initialAdvancedOverlayText(def: unknown): string {
  const ov = unknownSourceOverlay(def);
  return Object.keys(ov).length > 0 ? JSON.stringify(ov, null, 2) : "";
}

// validateSampling mirrors config.Sampling.Validate() for per-field
// client-side messages. Blank = unset (skipped); "0" is a real value.
// Returns null when all set fields are in range.
function validateSampling(v: {
  temperature: string;
  top_p: string;
  top_k: string;
  frequency_penalty: string;
  presence_penalty: string;
  seed: string;
  stop: string;
}): string | null {
  const range = (
    raw: string,
    label: string,
    lo: number,
    hi: number,
  ): string | null => {
    const t = raw.trim();
    if (t === "") return null;
    const n = Number(t);
    if (!Number.isFinite(n)) return `${label} must be a number (got "${raw}").`;
    if (n < lo || n > hi) return `${label} must be between ${lo} and ${hi}.`;
    return null;
  };
  const intMin = (
    raw: string,
    label: string,
    lo: number,
  ): string | null => {
    const t = raw.trim();
    if (t === "") return null;
    const n = Number(t);
    if (!Number.isInteger(n)) return `${label} must be an integer (got "${raw}").`;
    if (n < lo) return `${label} must be ≥ ${lo}.`;
    return null;
  };
  return (
    range(v.temperature, "temperature", 0, 2) ??
    range(v.top_p, "top_p", 0, 1) ??
    intMin(v.top_k, "top_k", 1) ??
    range(v.frequency_penalty, "frequency_penalty", -2, 2) ??
    range(v.presence_penalty, "presence_penalty", -2, 2) ??
    (v.seed.trim() === "" || Number.isInteger(Number(v.seed))
      ? null
      : `seed must be an integer (got "${v.seed}").`) ??
    (parseCommaList(v.stop).length > 8
      ? "stop accepts at most 8 sequences."
      : null)
  );
}

// parseAdvancedOverlay parses the advanced box as JSON or YAML and
// requires a plain object (not array / null / scalar). Used both by the
// submit path (the merge) and the live collision-detection display.
function parseAdvancedOverlay(
  text: string,
  format: "json" | "yaml",
): { obj: Record<string, unknown> | null; err: string | null } {
  let parsed: unknown;
  try {
    parsed = format === "json" ? JSON.parse(text) : yamlLoad(text);
  } catch (e) {
    return { obj: null, err: e instanceof Error ? e.message : String(e) };
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    return {
      obj: null,
      err: `advanced overlay must be a ${format.toUpperCase()} object, not an array or scalar`,
    };
  }
  return { obj: parsed as Record<string, unknown>, err: null };
}

// parseCommaList splits a comma-separated string into trimmed,
// non-empty entries. Handles whitespace + trailing commas + duplicate
// spaces. Used for tools, skills, and providers.
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
export function explainServerError(e: unknown): string {
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
    return "stdio MCP servers require the operator to set LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO=1 (host RCE opt-in), or declare the server in yaml mcp_servers:.";
  }
  if (innerText.includes("tools") && innerText.includes("widen")) {
    return "Fork can't add tools beyond the calling agent's ceiling. Trim tools.";
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
