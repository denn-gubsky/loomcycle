import { useEffect, useRef, useState } from "react";
import { createDef, forkDef, type DefRow, type SubstrateKind } from "../api";

// IntegrationEditModal — v0.24.0 manual-management UI for the four
// "Integrations" substrate families: WebhookDef, A2AServerCardDef,
// A2AAgentDef, MemoryBackendDef. Sibling of LibraryEditModal rather than
// an extension of it: these families share no fields with agent/skill/mcp
// (no tier/models/code-body) and each carries nested repeatable rows, so
// folding them into LibraryEditModal would nearly double it with
// never-co-active state. This keeps each modal cohesive while reusing the
// same .modal-* / .library-form-* CSS, the createDef/forkDef flow, and the
// 422-refusal "tool_refused" error-unwrap pattern.
//
// All four use the same op-discriminated envelope as agent/skill/mcp, so
// createDef/forkDef (api.ts) work unchanged once SubstrateKind is widened.
// Field coverage maps to each family's mergedXDef overlay struct
// (internal/tools/builtin/{webhookdef,a2aservercarddef,a2aagentdef,
// memorybackenddef}.go). Validation is server-authoritative; the local
// checks here only cover the common mistakes for a clearer message.

export type IntegrationKind =
  | "webhook"
  | "a2a-server-card"
  | "a2a-agent"
  | "memory-backend";
export type ModalMode = "create" | "fork";

export interface IntegrationEditModalProps {
  kind: IntegrationKind;
  mode: ModalMode;
  forkSource?: DefRow;
  existingNames?: string[];
  onClose: () => void;
  onSaved: (row: DefRow) => void;
}

// Valid env-var name (mirrors envVarNameRe on the server side).
const ENV_VAR_RE = /^[A-Z][A-Z0-9_]*$/;

type KV = { key: string; value: string };

// ---- per-family form state shapes (UI-friendly: number fields as
// strings so an empty input reads as "unset / inherit") ----

interface WebhookFormState {
  enabled: boolean;
  delivery: "spawn" | "channel";
  agent: string;
  channel: string;
  authKind: "hmac" | "bearer" | "none";
  algorithm: string;
  header: string;
  signingSecretEnv: string;
  deliveryIdHeader: string;
  bearerTokenEnv: string;
  requestsPerMinute: string;
  burst: string;
  bodySizeLimitBytes: string;
  userCredentialsFromEnv: KV[];
  payloadMapping: KV[];
  syncEnabled: boolean;
  syncTimeoutMs: string;
}

interface ExposedAgentRow {
  agentName: string;
  skillId: string;
  skillName: string;
  description: string;
  tags: string; // comma-separated
  inputModes: string; // comma-separated
  outputModes: string; // comma-separated
}

interface A2AServerCardFormState {
  providerOrg: string;
  providerUrl: string;
  capStreaming: boolean;
  capPush: boolean;
  capExtended: boolean;
  exposedAgents: ExposedAgentRow[];
  securitySchemes: { kind: string; scheme: string }[];
  signWithKeyEnv: string;
}

interface A2AAgentFormState {
  reach: "card" | "direct";
  agentCardUrl: string;
  endpoint: string;
  binding: "jsonrpc" | "grpc" | "rest";
  authScheme: string; // "" | http | apiKey | oauth2 | mtls
  bearerCredentialRef: string;
  expectedSkills: { id: string; required: boolean }[];
  verifySignedCard: boolean;
}

interface MemoryBackendFormState {
  kind: "inprocess";
  tenancyKind: "" | "key_per_tenant" | "shared_key_with_prefix";
  envPattern: string;
  prefixPattern: string;
  fallbackOnError: "" | "inprocess";
  healthCheckIntervalSeconds: string;
}

export default function IntegrationEditModal({
  kind,
  mode,
  forkSource,
  existingNames,
  onClose,
  onSaved,
}: IntegrationEditModalProps) {
  const [submitting, setSubmitting] = useState(false);
  const [submitErr, setSubmitErr] = useState<string | null>(null);

  const [name, setName] = useState(forkSource?.name ?? "");
  const [description, setDescription] = useState<string>(
    pickString(forkSource?.definition, "description"),
  );
  // Promote default: ON for create, OFF for fork (review before activating).
  const [promote, setPromote] = useState(mode === "create");

  // Hooks must run unconditionally, so all four state objects exist; only
  // the active `kind`'s state is read at build/validate time. Each is
  // seeded from forkSource when forking that kind.
  const [webhook, setWebhook] = useState<WebhookFormState>(() =>
    initWebhook(kind === "webhook" ? forkSource?.definition : undefined),
  );
  const [serverCard, setServerCard] = useState<A2AServerCardFormState>(() =>
    initServerCard(
      kind === "a2a-server-card" ? forkSource?.definition : undefined,
    ),
  );
  const [a2aAgent, setA2AAgent] = useState<A2AAgentFormState>(() =>
    initA2AAgent(kind === "a2a-agent" ? forkSource?.definition : undefined),
  );
  const [memBackend, setMemBackend] = useState<MemoryBackendFormState>(() =>
    initMemoryBackend(
      kind === "memory-backend" ? forkSource?.definition : undefined,
    ),
  );

  const titlePrefix = mode === "create" ? "Create" : "Edit (fork)";
  const titleLabel = INTEGRATION_LABEL[kind];

  // ESC closes (unless submitting). Holding onClose in a ref keeps the
  // listener registered once across parent re-renders (mirrors
  // LibraryEditModal).
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
    if (mode === "create" && existingNames?.includes(name.trim())) {
      return `An entry named "${name.trim()}" already exists. Use Edit (fork) on its row instead.`;
    }
    switch (kind) {
      case "webhook":
        return validateWebhook(webhook);
      case "a2a-server-card":
        return validateServerCard(serverCard);
      case "a2a-agent":
        return validateA2AAgent(a2aAgent);
      case "memory-backend":
        return validateMemoryBackend(memBackend);
    }
  };

  const buildOverlay = (): Record<string, unknown> => {
    switch (kind) {
      case "webhook":
        return buildWebhookOverlay(webhook, description);
      case "a2a-server-card":
        return buildServerCardOverlay(serverCard, description);
      case "a2a-agent":
        return buildA2AAgentOverlay(a2aAgent, description);
      case "memory-backend":
        return buildMemoryBackendOverlay(memBackend, description);
    }
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
      const substrateKind = KIND_TO_SUBSTRATE[kind];
      const overlay = buildOverlay();
      let row: DefRow;
      if (mode === "create") {
        row = await createDef(substrateKind, name.trim(), overlay, promote);
      } else {
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
      <div className="modal library-modal" onClick={(e) => e.stopPropagation()}>
        <h3>
          {titlePrefix} {titleLabel}
          {mode === "fork" && forkSource?.def_id?.startsWith("static:") && (
            <span
              className="library-modal-bootstrap-hint"
              title="The first fork of a yaml-static entry auto-bootstraps a v1 lineage root from cfg before attaching this fork as v2."
            >
              {" "}
              — forks from yaml
            </span>
          )}
        </h3>

        <div className="library-form-row">
          <label htmlFor="int-name">name</label>
          <input
            id="int-name"
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={mode === "fork" || submitting}
            placeholder={INTEGRATION_NAME_PLACEHOLDER[kind]}
            autoFocus={mode === "create"}
          />
        </div>

        <div className="library-form-row">
          <label htmlFor="int-desc">description</label>
          <input
            id="int-desc"
            type="text"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            disabled={submitting}
            placeholder="short summary"
          />
        </div>

        {kind === "webhook" && (
          <WebhookFields state={webhook} set={setWebhook} submitting={submitting} />
        )}
        {kind === "a2a-server-card" && (
          <ServerCardFields
            state={serverCard}
            set={setServerCard}
            submitting={submitting}
          />
        )}
        {kind === "a2a-agent" && (
          <A2AAgentFields state={a2aAgent} set={setA2AAgent} submitting={submitting} />
        )}
        {kind === "memory-backend" && (
          <MemoryBackendFields
            state={memBackend}
            set={setMemBackend}
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
            {submitting ? "Saving…" : mode === "create" ? "Create" : "Save fork"}
          </button>
        </div>
      </div>
    </div>
  );
}

// ===================== Webhook =====================

function WebhookFields(props: {
  state: WebhookFormState;
  set: (v: WebhookFormState) => void;
  submitting: boolean;
}) {
  const { state: s, set, submitting } = props;
  const patch = (p: Partial<WebhookFormState>) => set({ ...s, ...p });
  return (
    <>
      <div className="library-form-row library-form-row-checkbox">
        <label>
          <input
            type="checkbox"
            checked={s.enabled}
            onChange={(e) => patch({ enabled: e.target.checked })}
            disabled={submitting}
          />{" "}
          enabled
          <span className="library-modal-field-hint">
            {" "}
            — an overlay can only turn this ON. To disable an already-enabled
            webhook, edit its yaml (a fork with this unchecked won't disable it).
          </span>
        </label>
      </div>

      <div className="library-form-row">
        <label>delivery</label>
        <div className="library-radio-group">
          <label>
            <input
              type="radio"
              name="int-wh-delivery"
              checked={s.delivery === "spawn"}
              onChange={() => patch({ delivery: "spawn" })}
              disabled={submitting}
            />{" "}
            spawn (run an agent)
          </label>
          <label>
            <input
              type="radio"
              name="int-wh-delivery"
              checked={s.delivery === "channel"}
              onChange={() => patch({ delivery: "channel" })}
              disabled={submitting}
            />{" "}
            channel (publish)
          </label>
        </div>
      </div>

      {s.delivery === "spawn" ? (
        <div className="library-form-row">
          <label htmlFor="int-wh-agent">agent</label>
          <input
            id="int-wh-agent"
            type="text"
            value={s.agent}
            onChange={(e) => patch({ agent: e.target.value })}
            disabled={submitting}
            placeholder="agent name to spawn on delivery"
          />
        </div>
      ) : (
        <div className="library-form-row">
          <label htmlFor="int-wh-channel">channel</label>
          <input
            id="int-wh-channel"
            type="text"
            value={s.channel}
            onChange={(e) => patch({ channel: e.target.value })}
            disabled={submitting}
            placeholder="channel name to publish to"
          />
        </div>
      )}

      <div className="library-form-row">
        <label>auth.kind</label>
        <div className="library-radio-group">
          {(["hmac", "bearer", "none"] as const).map((k) => (
            <label key={k}>
              <input
                type="radio"
                name="int-wh-auth"
                checked={s.authKind === k}
                onChange={() => patch({ authKind: k })}
                disabled={submitting}
              />{" "}
              {k}
            </label>
          ))}
        </div>
      </div>

      {s.authKind === "hmac" && (
        <>
          <div className="library-form-row">
            <label htmlFor="int-wh-secret">
              auth.signing_secret_env
              <span className="library-modal-field-hint">
                {" "}
                — env var holding the HMAC secret (NAME only, e.g.
                LOOMCYCLE_WH_SECRET)
              </span>
            </label>
            <input
              id="int-wh-secret"
              type="text"
              value={s.signingSecretEnv}
              onChange={(e) => patch({ signingSecretEnv: e.target.value })}
              disabled={submitting}
              placeholder="LOOMCYCLE_WEBHOOK_SECRET"
            />
          </div>
          <div className="library-form-row library-form-row-quad">
            <label htmlFor="int-wh-algo">algorithm</label>
            <input
              id="int-wh-algo"
              type="text"
              value={s.algorithm}
              onChange={(e) => patch({ algorithm: e.target.value })}
              disabled={submitting}
              placeholder="sha256 (only sha256 is implemented)"
            />
            <label htmlFor="int-wh-header">signature header</label>
            <input
              id="int-wh-header"
              type="text"
              value={s.header}
              onChange={(e) => patch({ header: e.target.value })}
              disabled={submitting}
              placeholder="X-Hub-Signature-256"
            />
          </div>
        </>
      )}
      {s.authKind === "bearer" && (
        <div className="library-form-row">
          <label htmlFor="int-wh-bearer">
            auth.bearer_token_env
            <span className="library-modal-field-hint">
              {" "}
              — env var NAME holding the expected bearer token
            </span>
          </label>
          <input
            id="int-wh-bearer"
            type="text"
            value={s.bearerTokenEnv}
            onChange={(e) => patch({ bearerTokenEnv: e.target.value })}
            disabled={submitting}
            placeholder="LOOMCYCLE_WEBHOOK_BEARER"
          />
        </div>
      )}

      <div className="library-form-row library-form-row-quad">
        <label htmlFor="int-wh-rpm">rate_limit.requests_per_minute</label>
        <input
          id="int-wh-rpm"
          type="number"
          min="0"
          value={s.requestsPerMinute}
          onChange={(e) => patch({ requestsPerMinute: e.target.value })}
          disabled={submitting}
          placeholder="0 = unset"
        />
        <label htmlFor="int-wh-burst">rate_limit.burst</label>
        <input
          id="int-wh-burst"
          type="number"
          min="0"
          value={s.burst}
          onChange={(e) => patch({ burst: e.target.value })}
          disabled={submitting}
          placeholder="0 = unset"
        />
      </div>

      <div className="library-form-row">
        <label htmlFor="int-wh-bodysize">body_size_limit_bytes</label>
        <input
          id="int-wh-bodysize"
          type="number"
          min="0"
          value={s.bodySizeLimitBytes}
          onChange={(e) => patch({ bodySizeLimitBytes: e.target.value })}
          disabled={submitting}
          placeholder="0 = unset"
        />
      </div>

      {s.delivery === "spawn" && (
        <KVRows
          label="user_credentials_from_env"
          hint="map credential ref → env var NAME (value is the env name, not a secret)"
          rows={s.userCredentialsFromEnv}
          setRows={(rows) => patch({ userCredentialsFromEnv: rows })}
          keyPlaceholder="jobs"
          valuePlaceholder="LOOMCYCLE_JOBS_BEARER"
          submitting={submitting}
        />
      )}

      <KVRows
        label="payload_mapping"
        hint="map target field → JSONPath into the inbound payload"
        rows={s.payloadMapping}
        setRows={(rows) => patch({ payloadMapping: rows })}
        keyPlaceholder="repo"
        valuePlaceholder="$.repository.full_name"
        submitting={submitting}
      />

      <div className="library-form-row library-form-row-checkbox">
        <label>
          <input
            type="checkbox"
            checked={s.syncEnabled}
            onChange={(e) => patch({ syncEnabled: e.target.checked })}
            disabled={submitting}
          />{" "}
          sync_response.enabled
        </label>
      </div>
      {s.syncEnabled && (
        <div className="library-form-row">
          <label htmlFor="int-wh-sync-timeout">sync_response.timeout_ms</label>
          <input
            id="int-wh-sync-timeout"
            type="number"
            min="1"
            max="60000"
            value={s.syncTimeoutMs}
            onChange={(e) => patch({ syncTimeoutMs: e.target.value })}
            disabled={submitting}
            placeholder="1..60000"
          />
        </div>
      )}
    </>
  );
}

function initWebhook(def: unknown): WebhookFormState {
  const auth = pickObject(def, "auth");
  const rl = pickObject(def, "rate_limit");
  const sync = pickObject(def, "sync_response");
  const deliveryRaw = pickString(def, "delivery");
  const authKindRaw = pickString(auth, "kind");
  return {
    enabled: pickBool(def, "enabled"),
    delivery: deliveryRaw === "channel" ? "channel" : "spawn",
    agent: pickString(def, "agent"),
    channel: pickString(def, "channel"),
    authKind:
      authKindRaw === "bearer"
        ? "bearer"
        : authKindRaw === "none"
          ? "none"
          : "hmac",
    algorithm: pickString(auth, "algorithm"),
    header: pickString(auth, "header"),
    signingSecretEnv: pickString(auth, "signing_secret_env"),
    deliveryIdHeader: pickString(auth, "delivery_id_header"),
    bearerTokenEnv: pickString(auth, "bearer_token_env"),
    requestsPerMinute: pickNumStr(rl, "requests_per_minute"),
    burst: pickNumStr(rl, "burst"),
    bodySizeLimitBytes: pickNumStr(def, "body_size_limit_bytes"),
    userCredentialsFromEnv: kvFromMap(def, "user_credentials_from_env"),
    payloadMapping: kvFromMap(def, "payload_mapping"),
    syncEnabled: pickBool(sync, "enabled"),
    syncTimeoutMs: pickNumStr(sync, "timeout_ms"),
  };
}

function validateWebhook(s: WebhookFormState): string | null {
  if (s.delivery === "spawn") {
    if (!s.agent.trim()) return "delivery=spawn requires an agent.";
  } else {
    if (!s.channel.trim()) return "delivery=channel requires a channel.";
    if (s.userCredentialsFromEnv.some((r) => r.key.trim()))
      return "delivery=channel forbids user_credentials_from_env (channel mode has no run identity).";
  }
  if (s.authKind === "hmac") {
    if (!s.signingSecretEnv.trim())
      return "auth.kind=hmac requires auth.signing_secret_env.";
    if (!ENV_VAR_RE.test(s.signingSecretEnv.trim()))
      return `signing_secret_env "${s.signingSecretEnv.trim()}" is not a valid env-var name ([A-Z][A-Z0-9_]*).`;
    const a = s.algorithm.trim().toLowerCase();
    if (a && a !== "sha256")
      return `auth.algorithm "${s.algorithm.trim()}" unsupported (only sha256 is implemented).`;
  } else if (s.authKind === "bearer") {
    if (!s.bearerTokenEnv.trim())
      return "auth.kind=bearer requires auth.bearer_token_env.";
    if (!ENV_VAR_RE.test(s.bearerTokenEnv.trim()))
      return `bearer_token_env "${s.bearerTokenEnv.trim()}" is not a valid env-var name ([A-Z][A-Z0-9_]*).`;
  }
  for (const r of s.userCredentialsFromEnv) {
    if (r.key.trim() && r.value.trim() && !ENV_VAR_RE.test(r.value.trim()))
      return `user_credentials_from_env["${r.key.trim()}"] = "${r.value.trim()}" is not a valid env-var name.`;
  }
  if (s.syncEnabled) {
    const t = Number(s.syncTimeoutMs);
    if (!Number.isFinite(t) || t <= 0 || t > 60000)
      return "sync_response.timeout_ms must be in (0, 60000] when sync_response.enabled.";
  }
  return null;
}

function buildWebhookOverlay(
  s: WebhookFormState,
  description: string,
): Record<string, unknown> {
  const ov: Record<string, unknown> = {};
  if (description.trim()) ov.description = description.trim();
  if (s.enabled) ov.enabled = true;
  ov.delivery = s.delivery;
  if (s.delivery === "spawn") {
    if (s.agent.trim()) ov.agent = s.agent.trim();
  } else if (s.channel.trim()) ov.channel = s.channel.trim();

  const auth: Record<string, unknown> = { kind: s.authKind };
  if (s.authKind === "hmac") {
    if (s.signingSecretEnv.trim())
      auth.signing_secret_env = s.signingSecretEnv.trim();
    if (s.algorithm.trim()) auth.algorithm = s.algorithm.trim();
    if (s.header.trim()) auth.header = s.header.trim();
    if (s.deliveryIdHeader.trim())
      auth.delivery_id_header = s.deliveryIdHeader.trim();
  } else if (s.authKind === "bearer") {
    if (s.bearerTokenEnv.trim()) auth.bearer_token_env = s.bearerTokenEnv.trim();
  }
  ov.auth = auth;

  const rl: Record<string, unknown> = {};
  const rpm = posInt(s.requestsPerMinute);
  if (rpm !== null) rl.requests_per_minute = rpm;
  const burst = posInt(s.burst);
  if (burst !== null) rl.burst = burst;
  if (Object.keys(rl).length > 0) ov.rate_limit = rl;

  const bsl = posInt(s.bodySizeLimitBytes);
  if (bsl !== null) ov.body_size_limit_bytes = bsl;

  if (s.delivery === "spawn") {
    const creds = mapFromKV(s.userCredentialsFromEnv);
    if (Object.keys(creds).length > 0) ov.user_credentials_from_env = creds;
  }
  const mapping = mapFromKV(s.payloadMapping);
  if (Object.keys(mapping).length > 0) ov.payload_mapping = mapping;

  if (s.syncEnabled) {
    const t = posInt(s.syncTimeoutMs);
    ov.sync_response = { enabled: true, timeout_ms: t ?? 0 };
  }
  return ov;
}

// ===================== A2A Server Card =====================

function ServerCardFields(props: {
  state: A2AServerCardFormState;
  set: (v: A2AServerCardFormState) => void;
  submitting: boolean;
}) {
  const { state: s, set, submitting } = props;
  const patch = (p: Partial<A2AServerCardFormState>) => set({ ...s, ...p });
  const updateAgent = (i: number, p: Partial<ExposedAgentRow>) => {
    const next = [...s.exposedAgents];
    next[i] = { ...next[i]!, ...p };
    patch({ exposedAgents: next });
  };
  const updateScheme = (i: number, p: Partial<{ kind: string; scheme: string }>) => {
    const next = [...s.securitySchemes];
    next[i] = { ...next[i]!, ...p };
    patch({ securitySchemes: next });
  };
  return (
    <>
      <div className="library-form-row library-form-row-quad">
        <label htmlFor="int-sc-org">provider.organization</label>
        <input
          id="int-sc-org"
          type="text"
          value={s.providerOrg}
          onChange={(e) => patch({ providerOrg: e.target.value })}
          disabled={submitting}
          placeholder="Acme Inc"
        />
        <label htmlFor="int-sc-url">provider.url</label>
        <input
          id="int-sc-url"
          type="text"
          value={s.providerUrl}
          onChange={(e) => patch({ providerUrl: e.target.value })}
          disabled={submitting}
          placeholder="https://acme.example.com"
        />
      </div>

      <div className="library-form-row">
        <label>capabilities</label>
        <div className="library-checkbox-group">
          <label>
            <input
              type="checkbox"
              checked={s.capStreaming}
              onChange={(e) => patch({ capStreaming: e.target.checked })}
              disabled={submitting}
            />{" "}
            streaming
          </label>
          <label>
            <input
              type="checkbox"
              checked={s.capPush}
              onChange={(e) => patch({ capPush: e.target.checked })}
              disabled={submitting}
            />{" "}
            push_notifications
          </label>
          <label>
            <input
              type="checkbox"
              checked={s.capExtended}
              onChange={(e) => patch({ capExtended: e.target.checked })}
              disabled={submitting}
            />{" "}
            extended_agent_card
          </label>
        </div>
      </div>

      <div className="library-form-row">
        <label>
          exposed_agents (at least one)
          <button
            type="button"
            className="library-schema-hint-toggle"
            onClick={() =>
              patch({
                exposedAgents: [...s.exposedAgents, emptyExposedAgent()],
              })
            }
            disabled={submitting}
          >
            + add agent
          </button>
        </label>
        <div className="library-headers-grid">
          {s.exposedAgents.map((a, i) => (
            <div key={i} className="library-models-tier-row">
              <div className="library-form-row-quad">
                <label>agent_name</label>
                <input
                  type="text"
                  value={a.agentName}
                  onChange={(e) => updateAgent(i, { agentName: e.target.value })}
                  disabled={submitting}
                  placeholder="researcher"
                />
                <label>skill_id</label>
                <input
                  type="text"
                  value={a.skillId}
                  onChange={(e) => updateAgent(i, { skillId: e.target.value })}
                  disabled={submitting}
                  placeholder="optional"
                />
              </div>
              <div className="library-form-row-quad">
                <label>skill_name</label>
                <input
                  type="text"
                  value={a.skillName}
                  onChange={(e) => updateAgent(i, { skillName: e.target.value })}
                  disabled={submitting}
                  placeholder="optional"
                />
                <label>description</label>
                <input
                  type="text"
                  value={a.description}
                  onChange={(e) =>
                    updateAgent(i, { description: e.target.value })
                  }
                  disabled={submitting}
                  placeholder="optional"
                />
              </div>
              <input
                type="text"
                value={a.tags}
                onChange={(e) => updateAgent(i, { tags: e.target.value })}
                disabled={submitting}
                placeholder="tags (comma-separated)"
              />
              <input
                type="text"
                value={a.inputModes}
                onChange={(e) => updateAgent(i, { inputModes: e.target.value })}
                disabled={submitting}
                placeholder="input_modes (comma-separated)"
              />
              <input
                type="text"
                value={a.outputModes}
                onChange={(e) =>
                  updateAgent(i, { outputModes: e.target.value })
                }
                disabled={submitting}
                placeholder="output_modes (comma-separated)"
              />
              <button
                type="button"
                onClick={() =>
                  patch({
                    exposedAgents: s.exposedAgents.filter((_, idx) => idx !== i),
                  })
                }
                disabled={submitting}
                title="remove agent"
              >
                × remove
              </button>
            </div>
          ))}
        </div>
      </div>

      <div className="library-form-row">
        <label>
          security_schemes
          <button
            type="button"
            className="library-schema-hint-toggle"
            onClick={() =>
              patch({
                securitySchemes: [
                  ...s.securitySchemes,
                  { kind: "http", scheme: "" },
                ],
              })
            }
            disabled={submitting}
          >
            + add scheme
          </button>
        </label>
        <div className="library-headers-grid">
          {s.securitySchemes.map((sc, i) => (
            <div key={i} className="library-header-row">
              <select
                value={sc.kind}
                onChange={(e) => updateScheme(i, { kind: e.target.value })}
                disabled={submitting}
              >
                {["http", "apiKey", "oauth2", "mtls"].map((k) => (
                  <option key={k} value={k}>
                    {k}
                  </option>
                ))}
              </select>
              <input
                type="text"
                value={sc.scheme}
                onChange={(e) => updateScheme(i, { scheme: e.target.value })}
                disabled={submitting}
                placeholder="scheme (e.g. bearer)"
              />
              <button
                type="button"
                onClick={() =>
                  patch({
                    securitySchemes: s.securitySchemes.filter(
                      (_, idx) => idx !== i,
                    ),
                  })
                }
                disabled={submitting}
                title="remove"
              >
                ×
              </button>
            </div>
          ))}
        </div>
      </div>

      <div className="library-form-row">
        <label htmlFor="int-sc-signkey">
          sign_with_key_env
          <span className="library-modal-field-hint">
            {" "}
            — env var NAME of the signing key (optional)
          </span>
        </label>
        <input
          id="int-sc-signkey"
          type="text"
          value={s.signWithKeyEnv}
          onChange={(e) => patch({ signWithKeyEnv: e.target.value })}
          disabled={submitting}
          placeholder="LOOMCYCLE_A2A_CARD_SIGNING_KEY"
        />
      </div>
    </>
  );
}

function emptyExposedAgent(): ExposedAgentRow {
  return {
    agentName: "",
    skillId: "",
    skillName: "",
    description: "",
    tags: "",
    inputModes: "",
    outputModes: "",
  };
}

function initServerCard(def: unknown): A2AServerCardFormState {
  const provider = pickObject(def, "provider");
  const caps = pickObject(def, "capabilities");
  const exposed = pickObjectArray(def, "exposed_agents").map((e) => ({
    agentName: pickString(e, "agent_name"),
    skillId: pickString(e, "skill_id"),
    skillName: pickString(e, "skill_name"),
    description: pickString(e, "description"),
    tags: pickStringArray(e, "tags").join(", "),
    inputModes: pickStringArray(e, "input_modes").join(", "),
    outputModes: pickStringArray(e, "output_modes").join(", "),
  }));
  const schemes = pickObjectArray(def, "security_schemes").map((sc) => ({
    kind: pickString(sc, "kind") || "http",
    scheme: pickString(sc, "scheme"),
  }));
  return {
    providerOrg: pickString(provider, "organization"),
    providerUrl: pickString(provider, "url"),
    capStreaming: pickBool(caps, "streaming"),
    capPush: pickBool(caps, "push_notifications"),
    capExtended: pickBool(caps, "extended_agent_card"),
    exposedAgents: exposed.length > 0 ? exposed : [emptyExposedAgent()],
    securitySchemes: schemes,
    signWithKeyEnv: pickString(def, "sign_with_key_env"),
  };
}

function validateServerCard(s: A2AServerCardFormState): string | null {
  const named = s.exposedAgents.filter((a) => a.agentName.trim());
  if (named.length === 0)
    return "exposed_agents: at least one agent with an agent_name is required.";
  if (s.signWithKeyEnv.trim() && !ENV_VAR_RE.test(s.signWithKeyEnv.trim()))
    return `sign_with_key_env "${s.signWithKeyEnv.trim()}" is not a valid env-var name.`;
  return null;
}

function buildServerCardOverlay(
  s: A2AServerCardFormState,
  description: string,
): Record<string, unknown> {
  const ov: Record<string, unknown> = {};
  if (description.trim()) ov.description = description.trim();
  const provider: Record<string, unknown> = {};
  if (s.providerOrg.trim()) provider.organization = s.providerOrg.trim();
  if (s.providerUrl.trim()) provider.url = s.providerUrl.trim();
  if (Object.keys(provider).length > 0) ov.provider = provider;

  // All-false capabilities marshals to the zero struct server-side and is
  // ignored (inherits parent); any true value applies the block.
  ov.capabilities = {
    streaming: s.capStreaming,
    push_notifications: s.capPush,
    extended_agent_card: s.capExtended,
  };

  ov.exposed_agents = s.exposedAgents
    .filter((a) => a.agentName.trim())
    .map((a) => {
      const o: Record<string, unknown> = { agent_name: a.agentName.trim() };
      if (a.skillId.trim()) o.skill_id = a.skillId.trim();
      if (a.skillName.trim()) o.skill_name = a.skillName.trim();
      if (a.description.trim()) o.description = a.description.trim();
      const tags = parseCommaList(a.tags);
      if (tags.length > 0) o.tags = tags;
      const im = parseCommaList(a.inputModes);
      if (im.length > 0) o.input_modes = im;
      const om = parseCommaList(a.outputModes);
      if (om.length > 0) o.output_modes = om;
      return o;
    });

  const schemes = s.securitySchemes
    .filter((sc) => sc.scheme.trim() || sc.kind.trim())
    .map((sc) => ({ kind: sc.kind, scheme: sc.scheme.trim() }));
  if (schemes.length > 0) ov.security_schemes = schemes;

  if (s.signWithKeyEnv.trim()) ov.sign_with_key_env = s.signWithKeyEnv.trim();
  return ov;
}

// ===================== A2A Agent (peer) =====================

function A2AAgentFields(props: {
  state: A2AAgentFormState;
  set: (v: A2AAgentFormState) => void;
  submitting: boolean;
}) {
  const { state: s, set, submitting } = props;
  const patch = (p: Partial<A2AAgentFormState>) => set({ ...s, ...p });
  const updateSkill = (i: number, p: Partial<{ id: string; required: boolean }>) => {
    const next = [...s.expectedSkills];
    next[i] = { ...next[i]!, ...p };
    patch({ expectedSkills: next });
  };
  return (
    <>
      <div className="library-form-row">
        <label>reachability</label>
        <div className="library-radio-group">
          <label>
            <input
              type="radio"
              name="int-a2a-reach"
              checked={s.reach === "card"}
              onChange={() => patch({ reach: "card" })}
              disabled={submitting}
            />{" "}
            agent_card_url
          </label>
          <label>
            <input
              type="radio"
              name="int-a2a-reach"
              checked={s.reach === "direct"}
              onChange={() => patch({ reach: "direct" })}
              disabled={submitting}
            />{" "}
            endpoint + binding
          </label>
        </div>
      </div>

      {s.reach === "card" ? (
        <div className="library-form-row">
          <label htmlFor="int-a2a-cardurl">agent_card_url</label>
          <input
            id="int-a2a-cardurl"
            type="text"
            value={s.agentCardUrl}
            onChange={(e) => patch({ agentCardUrl: e.target.value })}
            disabled={submitting}
            placeholder="https://peer.example.com/.well-known/agent-card.json"
          />
        </div>
      ) : (
        <div className="library-form-row library-form-row-quad">
          <label htmlFor="int-a2a-endpoint">endpoint</label>
          <input
            id="int-a2a-endpoint"
            type="text"
            value={s.endpoint}
            onChange={(e) => patch({ endpoint: e.target.value })}
            disabled={submitting}
            placeholder="https://peer.example.com/a2a"
          />
          <label htmlFor="int-a2a-binding">binding</label>
          <select
            id="int-a2a-binding"
            value={s.binding}
            onChange={(e) =>
              patch({ binding: e.target.value as A2AAgentFormState["binding"] })
            }
            disabled={submitting}
          >
            {(["jsonrpc", "grpc", "rest"] as const).map((b) => (
              <option key={b} value={b}>
                {b}
              </option>
            ))}
          </select>
        </div>
      )}

      <div className="library-form-row library-form-row-quad">
        <label htmlFor="int-a2a-scheme">auth.scheme</label>
        <select
          id="int-a2a-scheme"
          value={s.authScheme}
          onChange={(e) => patch({ authScheme: e.target.value })}
          disabled={submitting}
        >
          <option value="">(none)</option>
          {["http", "apiKey", "oauth2", "mtls"].map((k) => (
            <option key={k} value={k}>
              {k}
            </option>
          ))}
        </select>
        <label htmlFor="int-a2a-credref">auth.bearer_credential_ref</label>
        <input
          id="int-a2a-credref"
          type="text"
          value={s.bearerCredentialRef}
          onChange={(e) => patch({ bearerCredentialRef: e.target.value })}
          disabled={submitting}
          placeholder="peer-token"
        />
      </div>

      <div className="library-form-row">
        <label>
          expected_skills
          <button
            type="button"
            className="library-schema-hint-toggle"
            onClick={() =>
              patch({
                expectedSkills: [
                  ...s.expectedSkills,
                  { id: "", required: false },
                ],
              })
            }
            disabled={submitting}
          >
            + add skill
          </button>
        </label>
        <div className="library-headers-grid">
          {s.expectedSkills.map((sk, i) => (
            <div key={i} className="library-header-row">
              <input
                type="text"
                value={sk.id}
                onChange={(e) => updateSkill(i, { id: e.target.value })}
                disabled={submitting}
                placeholder="skill id"
              />
              <label>
                <input
                  type="checkbox"
                  checked={sk.required}
                  onChange={(e) => updateSkill(i, { required: e.target.checked })}
                  disabled={submitting}
                />{" "}
                required
              </label>
              <button
                type="button"
                onClick={() =>
                  patch({
                    expectedSkills: s.expectedSkills.filter(
                      (_, idx) => idx !== i,
                    ),
                  })
                }
                disabled={submitting}
                title="remove"
              >
                ×
              </button>
            </div>
          ))}
        </div>
      </div>

      <div className="library-form-row library-form-row-checkbox">
        <label>
          <input
            type="checkbox"
            checked={s.verifySignedCard}
            onChange={(e) => patch({ verifySignedCard: e.target.checked })}
            disabled={submitting}
          />{" "}
          verify_signed_card
        </label>
      </div>
    </>
  );
}

function initA2AAgent(def: unknown): A2AAgentFormState {
  const auth = pickObject(def, "auth");
  const cardURL = pickString(def, "agent_card_url");
  const endpoint = pickString(def, "endpoint");
  const bindingRaw = pickString(def, "binding");
  const skills = pickObjectArray(def, "expected_skills").map((sk) => ({
    id: pickString(sk, "id"),
    required: pickBool(sk, "required"),
  }));
  return {
    reach: endpoint || bindingRaw ? "direct" : "card",
    agentCardUrl: cardURL,
    endpoint,
    binding:
      bindingRaw === "grpc" ? "grpc" : bindingRaw === "rest" ? "rest" : "jsonrpc",
    authScheme: pickString(auth, "scheme"),
    bearerCredentialRef: pickString(auth, "bearer_credential_ref"),
    expectedSkills: skills,
    verifySignedCard: pickBool(def, "verify_signed_card"),
  };
}

function validateA2AAgent(s: A2AAgentFormState): string | null {
  if (s.reach === "card") {
    if (!s.agentCardUrl.trim()) return "agent_card_url is required.";
    if (!isHTTPURL(s.agentCardUrl.trim()))
      return "agent_card_url must be a valid http(s) URL.";
  } else {
    if (!s.endpoint.trim()) return "endpoint is required.";
    if (s.binding !== "grpc" && !isHTTPURL(s.endpoint.trim()))
      return "endpoint must be a valid http(s) URL.";
  }
  return null;
}

function buildA2AAgentOverlay(
  s: A2AAgentFormState,
  description: string,
): Record<string, unknown> {
  const ov: Record<string, unknown> = {};
  if (description.trim()) ov.description = description.trim();
  if (s.reach === "card") {
    ov.agent_card_url = s.agentCardUrl.trim();
  } else {
    ov.endpoint = s.endpoint.trim();
    ov.binding = s.binding;
  }
  const auth: Record<string, unknown> = {};
  if (s.authScheme.trim()) auth.scheme = s.authScheme.trim();
  if (s.bearerCredentialRef.trim())
    auth.bearer_credential_ref = s.bearerCredentialRef.trim();
  if (Object.keys(auth).length > 0) ov.auth = auth;
  const skills = s.expectedSkills
    .filter((sk) => sk.id.trim())
    .map((sk) => ({ id: sk.id.trim(), required: sk.required }));
  if (skills.length > 0) ov.expected_skills = skills;
  if (s.verifySignedCard) ov.verify_signed_card = true;
  return ov;
}

// ===================== Memory Backend =====================

function MemoryBackendFields(props: {
  state: MemoryBackendFormState;
  set: (v: MemoryBackendFormState) => void;
  submitting: boolean;
}) {
  const { state: s, set, submitting } = props;
  const patch = (p: Partial<MemoryBackendFormState>) => set({ ...s, ...p });
  return (
    <>
      {/* `inprocess` is the only kind the server accepts — the external
          backend kind was removed — so this group has a single option. */}
      <div className="library-form-row">
        <label>kind</label>
        <div className="library-radio-group">
          <label>
            <input
              type="radio"
              name="int-mb-kind"
              checked
              readOnly
              disabled={submitting}
            />{" "}
            inprocess
          </label>
        </div>
      </div>

      <div className="library-form-row">
        <label htmlFor="int-mb-tenancy">tenancy_strategy.kind</label>
        <select
          id="int-mb-tenancy"
          value={s.tenancyKind}
          onChange={(e) =>
            patch({
              tenancyKind: e.target.value as MemoryBackendFormState["tenancyKind"],
            })
          }
          disabled={submitting}
        >
          <option value="">(default)</option>
          <option value="key_per_tenant">key_per_tenant</option>
          <option value="shared_key_with_prefix">shared_key_with_prefix</option>
        </select>
      </div>
      {s.tenancyKind === "key_per_tenant" && (
        <div className="library-form-row">
          <label htmlFor="int-mb-envpat">
            tenancy_strategy.env_pattern
            <span className="library-modal-field-hint">
              {" "}
              — must contain {"{tenant_id}"} if set
            </span>
          </label>
          <input
            id="int-mb-envpat"
            type="text"
            value={s.envPattern}
            onChange={(e) => patch({ envPattern: e.target.value })}
            disabled={submitting}
            placeholder="LOOMCYCLE_MEMORY_KEY_{tenant_id}"
          />
        </div>
      )}
      {s.tenancyKind === "shared_key_with_prefix" && (
        <div className="library-form-row">
          <label htmlFor="int-mb-prefixpat">
            tenancy_strategy.prefix_pattern
            <span className="library-modal-field-hint">
              {" "}
              — MUST contain {"{tenant_id}"}
            </span>
          </label>
          <input
            id="int-mb-prefixpat"
            type="text"
            value={s.prefixPattern}
            onChange={(e) => patch({ prefixPattern: e.target.value })}
            disabled={submitting}
            placeholder="tenant/{tenant_id}/"
          />
        </div>
      )}

      <div className="library-form-row library-form-row-quad">
        <label htmlFor="int-mb-fallback">fallback_on_error</label>
        <select
          id="int-mb-fallback"
          value={s.fallbackOnError}
          onChange={(e) =>
            patch({
              fallbackOnError: e.target
                .value as MemoryBackendFormState["fallbackOnError"],
            })
          }
          disabled={submitting}
        >
          <option value="">(none)</option>
          <option value="inprocess">inprocess</option>
        </select>
        <label htmlFor="int-mb-health">health_check_interval_seconds</label>
        <input
          id="int-mb-health"
          type="number"
          min="0"
          value={s.healthCheckIntervalSeconds}
          onChange={(e) =>
            patch({ healthCheckIntervalSeconds: e.target.value })
          }
          disabled={submitting}
          placeholder="0 = unset"
        />
      </div>
    </>
  );
}

function initMemoryBackend(def: unknown): MemoryBackendFormState {
  const tenancy = pickObject(def, "tenancy_strategy");
  const tenancyRaw = pickString(tenancy, "kind");
  const fallbackRaw = pickString(def, "fallback_on_error");
  return {
    // A def persisted by an older build may still say kind:mem9. That kind
    // was removed, so editing normalises it to the only accepted value —
    // saving the form is how an operator retires the stale kind.
    kind: "inprocess",
    tenancyKind:
      tenancyRaw === "key_per_tenant"
        ? "key_per_tenant"
        : tenancyRaw === "shared_key_with_prefix"
          ? "shared_key_with_prefix"
          : "",
    envPattern: pickString(tenancy, "env_pattern"),
    prefixPattern: pickString(tenancy, "prefix_pattern"),
    fallbackOnError: fallbackRaw === "inprocess" ? "inprocess" : "",
    healthCheckIntervalSeconds: pickNumStr(def, "health_check_interval_seconds"),
  };
}

function validateMemoryBackend(s: MemoryBackendFormState): string | null {
  if (
    s.tenancyKind === "key_per_tenant" &&
    s.envPattern.trim() &&
    !s.envPattern.includes("{tenant_id}")
  )
    return "tenancy_strategy.env_pattern must contain {tenant_id}.";
  if (
    s.tenancyKind === "shared_key_with_prefix" &&
    !s.prefixPattern.includes("{tenant_id}")
  )
    return "tenancy_strategy.prefix_pattern must contain {tenant_id} (the prefix is the only tenant-isolation mechanism).";
  return null;
}

function buildMemoryBackendOverlay(
  s: MemoryBackendFormState,
  description: string,
): Record<string, unknown> {
  const ov: Record<string, unknown> = {};
  if (description.trim()) ov.description = description.trim();
  ov.kind = s.kind;
  if (s.tenancyKind) {
    const ten: Record<string, unknown> = { kind: s.tenancyKind };
    if (s.tenancyKind === "key_per_tenant" && s.envPattern.trim())
      ten.env_pattern = s.envPattern.trim();
    if (s.tenancyKind === "shared_key_with_prefix" && s.prefixPattern.trim())
      ten.prefix_pattern = s.prefixPattern.trim();
    ov.tenancy_strategy = ten;
  }
  if (s.fallbackOnError) ov.fallback_on_error = s.fallbackOnError;
  const hci = posInt(s.healthCheckIntervalSeconds);
  if (hci !== null) ov.health_check_interval_seconds = hci;
  return ov;
}

// ===================== shared KV-rows editor =====================

function KVRows(props: {
  label: string;
  hint: string;
  rows: KV[];
  setRows: (v: KV[]) => void;
  keyPlaceholder: string;
  valuePlaceholder: string;
  submitting: boolean;
}) {
  const update = (i: number, p: Partial<KV>) => {
    const next = [...props.rows];
    next[i] = { ...next[i]!, ...p };
    props.setRows(next);
  };
  return (
    <div className="library-form-row">
      <label>
        {props.label}
        <span className="library-modal-field-hint"> — {props.hint}</span>
        <button
          type="button"
          className="library-schema-hint-toggle"
          onClick={() => props.setRows([...props.rows, { key: "", value: "" }])}
          disabled={props.submitting}
        >
          + add row
        </button>
      </label>
      <div className="library-headers-grid">
        {props.rows.map((r, i) => (
          <div key={i} className="library-header-row">
            <input
              type="text"
              placeholder={props.keyPlaceholder}
              value={r.key}
              onChange={(e) => update(i, { key: e.target.value })}
              disabled={props.submitting}
            />
            <input
              type="text"
              placeholder={props.valuePlaceholder}
              value={r.value}
              onChange={(e) => update(i, { value: e.target.value })}
              disabled={props.submitting}
            />
            <button
              type="button"
              onClick={() =>
                props.setRows(props.rows.filter((_, idx) => idx !== i))
              }
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

// ===================== constants + helpers =====================

const INTEGRATION_LABEL: Record<IntegrationKind, string> = {
  webhook: "webhook",
  "a2a-server-card": "A2A server card",
  "a2a-agent": "A2A agent",
  "memory-backend": "memory backend",
};

const INTEGRATION_NAME_PLACEHOLDER: Record<IntegrationKind, string> = {
  webhook: "github-push",
  "a2a-server-card": "my-card",
  "a2a-agent": "peer-researcher",
  "memory-backend": "team-memory",
};

const KIND_TO_SUBSTRATE: Record<IntegrationKind, SubstrateKind> = {
  webhook: "webhookdef",
  "a2a-server-card": "a2aservercarddef",
  "a2a-agent": "a2aagentdef",
  "memory-backend": "memorybackenddef",
};

function pickString(def: unknown, key: string): string {
  if (!def || typeof def !== "object") return "";
  const v = (def as Record<string, unknown>)[key];
  return typeof v === "string" ? v : "";
}

function pickBool(def: unknown, key: string): boolean {
  if (!def || typeof def !== "object") return false;
  return (def as Record<string, unknown>)[key] === true;
}

function pickNumStr(def: unknown, key: string): string {
  if (!def || typeof def !== "object") return "";
  const v = (def as Record<string, unknown>)[key];
  return typeof v === "number" && Number.isFinite(v) ? String(v) : "";
}

function pickStringArray(def: unknown, key: string): string[] {
  if (!def || typeof def !== "object") return [];
  const v = (def as Record<string, unknown>)[key];
  if (!Array.isArray(v)) return [];
  return v.filter((x): x is string => typeof x === "string");
}

function pickObject(def: unknown, key: string): unknown {
  if (!def || typeof def !== "object") return undefined;
  const v = (def as Record<string, unknown>)[key];
  return v && typeof v === "object" && !Array.isArray(v) ? v : undefined;
}

function pickObjectArray(def: unknown, key: string): unknown[] {
  if (!def || typeof def !== "object") return [];
  const v = (def as Record<string, unknown>)[key];
  return Array.isArray(v) ? v.filter((x) => x && typeof x === "object") : [];
}

function kvFromMap(def: unknown, key: string): KV[] {
  if (!def || typeof def !== "object") return [];
  const v = (def as Record<string, unknown>)[key];
  if (!v || typeof v !== "object" || Array.isArray(v)) return [];
  return Object.entries(v as Record<string, unknown>)
    .filter(([, val]) => typeof val === "string")
    .map(([k, val]) => ({ key: k, value: val as string }));
}

function mapFromKV(rows: KV[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const { key, value } of rows) {
    const k = key.trim();
    if (k) out[k] = value;
  }
  return out;
}

function parseCommaList(s: string): string[] {
  return s
    .split(",")
    .map((t) => t.trim())
    .filter((t) => t.length > 0);
}

// posInt returns a positive integer or null for empty/zero/invalid — so
// the overlay omits the field (substrate inherits parent / default).
function posInt(s: string): number | null {
  const t = s.trim();
  if (t === "") return null;
  const n = Number(t);
  return Number.isFinite(n) && Number.isInteger(n) && n > 0 ? n : null;
}

function isHTTPURL(s: string): boolean {
  try {
    const u = new URL(s);
    return u.protocol === "http:" || u.protocol === "https:";
  } catch {
    return false;
  }
}

// explainServerError unwraps the jsonFetch "<status>: {tool_refused JSON}"
// message and maps known substrings to friendlier text. Mirrors
// LibraryEditModal.explainServerError but with the integration families'
// refusal phrasings; kept local to avoid touching the shipped modal.
function explainServerError(e: unknown): string {
  const raw = e instanceof Error ? e.message : String(e);
  const jsonIdx = raw.indexOf("{");
  let inner = raw;
  if (jsonIdx >= 0) {
    try {
      const parsed = JSON.parse(raw.slice(jsonIdx));
      if (parsed && typeof parsed.error === "string") inner = parsed.error;
    } catch {
      // not JSON / truncated — fall through with raw
    }
  }
  if (inner.includes("matches a static cfg."))
    return "An entry with this name is defined in yaml. Pick a different name.";
  if (inner.includes("has neither a DB version nor a static"))
    return "No existing version found for this name. Use Create instead of Edit.";
  if (inner.includes("not configured (no Store backend)"))
    return "Substrate is not configured. Set up a store backend in loomcycle.yaml.";
  if (inner.includes("not configured (no Config"))
    return "Substrate is not configured. Operator's root config is missing.";
  // Webhook / A2A / memory-backend validator phrasings are already
  // human-readable; surface them verbatim.
  return inner || raw;
}
