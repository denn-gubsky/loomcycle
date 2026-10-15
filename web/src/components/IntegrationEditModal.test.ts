import { describe, expect, it } from "vitest";
import {
  buildWebhookOverlay,
  initWebhook,
  validateWebhook,
  buildMemoryBackendOverlay,
  initMemoryBackend,
  validateMemoryBackend,
  type MemoryBackendFormState,
} from "./IntegrationEditModal";

// RFC CD Part B — the Integrations memory-backend form gained a kind selector
// (inprocess | remote) that reveals the peer-loomcycle config (base_url /
// api_key_env / api_version). These cover the pure init → validate → build
// helpers the modal drives.

const remoteState = (over: Partial<MemoryBackendFormState> = {}): MemoryBackendFormState => ({
  kind: "remote",
  baseUrl: "https://peer.example:8787",
  apiVersion: "",
  apiKeyEnv: "LOOMCYCLE_PEER_MEMORY_KEY",
  tenancyKind: "",
  envPattern: "",
  prefixPattern: "",
  fallbackOnError: "",
  healthCheckIntervalSeconds: "",
  ...over,
});

describe("initMemoryBackend", () => {
  it("reads a remote def's kind + peer config", () => {
    const s = initMemoryBackend({
      kind: "remote",
      config: {
        base_url: "https://peer.example:8787",
        api_version: "v1",
        api_key_env: "LOOMCYCLE_PEER_MEMORY_KEY",
      },
    });
    expect(s.kind).toBe("remote");
    expect(s.baseUrl).toBe("https://peer.example:8787");
    expect(s.apiVersion).toBe("v1");
    expect(s.apiKeyEnv).toBe("LOOMCYCLE_PEER_MEMORY_KEY");
  });

  it("normalises an unknown/removed kind (e.g. mem9) to inprocess", () => {
    const s = initMemoryBackend({ kind: "mem9" });
    expect(s.kind).toBe("inprocess");
    expect(s.baseUrl).toBe("");
  });
});

describe("buildMemoryBackendOverlay", () => {
  it("emits the config block for a remote backend", () => {
    const ov = buildMemoryBackendOverlay(
      remoteState({ apiVersion: "v1", fallbackOnError: "inprocess" }),
      "peer B",
    );
    expect(ov.kind).toBe("remote");
    expect(ov.config).toEqual({
      base_url: "https://peer.example:8787",
      api_version: "v1",
      api_key_env: "LOOMCYCLE_PEER_MEMORY_KEY",
    });
    expect(ov.fallback_on_error).toBe("inprocess");
    expect(ov.description).toBe("peer B");
  });

  it("omits api_version/api_key_env when blank", () => {
    const ov = buildMemoryBackendOverlay(remoteState({ apiVersion: "", apiKeyEnv: "" }), "");
    expect(ov.config).toEqual({ base_url: "https://peer.example:8787" });
  });

  it("writes NO config block for an inprocess backend (back-compat)", () => {
    const ov = buildMemoryBackendOverlay(initMemoryBackend({ kind: "inprocess" }), "");
    expect(ov.kind).toBe("inprocess");
    expect(ov.config).toBeUndefined();
  });
});

describe("validateMemoryBackend", () => {
  it("requires base_url for a remote backend", () => {
    expect(validateMemoryBackend(remoteState({ baseUrl: "" }))).toMatch(/base_url/);
  });
  it("rejects a non-http(s) or unparseable base_url", () => {
    expect(validateMemoryBackend(remoteState({ baseUrl: "ftp://peer" }))).toMatch(/http/);
    // A genuinely host-less URL (new URL throws) is caught client-side. The
    // subtler "http:///v1" case (new URL normalises the host to "v1") is left
    // to the server's stricter url.Parse — validation is server-authoritative.
    expect(validateMemoryBackend(remoteState({ baseUrl: "http://" }))).toMatch(/host/);
  });
  it("rejects shared_key_with_prefix for a remote backend", () => {
    expect(
      validateMemoryBackend(remoteState({ tenancyKind: "shared_key_with_prefix" })),
    ).toMatch(/shared_key_with_prefix/);
  });
  it("accepts a well-formed remote backend", () => {
    expect(validateMemoryBackend(remoteState())).toBeNull();
    expect(
      validateMemoryBackend(
        remoteState({ tenancyKind: "key_per_tenant", envPattern: "LOOMCYCLE_{tenant_id}_KEY" }),
      ),
    ).toBeNull();
  });
  it("still validates inprocess tenancy rules", () => {
    const s = initMemoryBackend({ kind: "inprocess" });
    expect(
      validateMemoryBackend({ ...s, tenancyKind: "key_per_tenant", envPattern: "no-token" }),
    ).toMatch(/env_pattern/);
  });
});

// RFC CE — the DocumentSourceDef surface in Integrations.
import {
  buildDocumentSourceOverlay,
  initDocumentSource,
  validateDocumentSource,
  type DocumentSourceFormState,
} from "./IntegrationEditModal";

const docState = (over: Partial<DocumentSourceFormState> = {}): DocumentSourceFormState => ({
  baseUrl: "https://peer.example:8787",
  apiVersion: "",
  apiKeyEnv: "LOOMCYCLE_PEER_DOC_KEY",
  tenancyKind: "",
  envPattern: "",
  ...over,
});

describe("initDocumentSource", () => {
  it("reads a def's config + tenancy", () => {
    const s = initDocumentSource({
      config: { base_url: "https://peer:8787", api_version: "v1", api_key_env: "LOOMCYCLE_PEER_DOC_KEY" },
      tenancy_strategy: { kind: "key_per_tenant", env_pattern: "LOOMCYCLE_{tenant_id}_KEY" },
    });
    expect(s.baseUrl).toBe("https://peer:8787");
    expect(s.apiVersion).toBe("v1");
    expect(s.apiKeyEnv).toBe("LOOMCYCLE_PEER_DOC_KEY");
    expect(s.tenancyKind).toBe("key_per_tenant");
    expect(s.envPattern).toBe("LOOMCYCLE_{tenant_id}_KEY");
  });
});

describe("buildDocumentSourceOverlay", () => {
  it("always emits config.base_url and omits blank optionals", () => {
    const ov = buildDocumentSourceOverlay(docState({ apiVersion: "" }), "peer docs");
    expect(ov.config).toEqual({
      base_url: "https://peer.example:8787",
      api_key_env: "LOOMCYCLE_PEER_DOC_KEY",
    });
    expect(ov.description).toBe("peer docs");
    expect(ov.tenancy_strategy).toBeUndefined();
  });
  it("emits tenancy_strategy when set", () => {
    const ov = buildDocumentSourceOverlay(
      docState({ tenancyKind: "key_per_tenant", envPattern: "LOOMCYCLE_{tenant_id}_KEY" }),
      "",
    );
    expect(ov.tenancy_strategy).toEqual({
      kind: "key_per_tenant",
      env_pattern: "LOOMCYCLE_{tenant_id}_KEY",
    });
  });
});

describe("validateDocumentSource", () => {
  it("requires a valid http(s) base_url with a host", () => {
    expect(validateDocumentSource(docState({ baseUrl: "" }))).toMatch(/base_url/);
    expect(validateDocumentSource(docState({ baseUrl: "ftp://peer" }))).toMatch(/http/);
    expect(validateDocumentSource(docState({ baseUrl: "http://" }))).toMatch(/host/);
  });
  it("requires {tenant_id} in a key_per_tenant env_pattern", () => {
    expect(
      validateDocumentSource(docState({ tenancyKind: "key_per_tenant", envPattern: "no-token" })),
    ).toMatch(/env_pattern/);
  });
  it("accepts a well-formed source", () => {
    expect(validateDocumentSource(docState())).toBeNull();
    expect(
      validateDocumentSource(docState({ tenancyKind: "key_per_tenant", envPattern: "K_{tenant_id}" })),
    ).toBeNull();
  });
});

// RFC EK — the webhook form reads and writes the forge fields: provider,
// events, a stored-credential secret, user_bearer, dedup_key, filters and
// event_shapes, and delivery team.
describe("webhook form", () => {
  const forgeDef = {
    enabled: true,
    delivery: "spawn",
    agent: "reviewer",
    provider: "github",
    events: ["push", "pull_request"],
    auth: { secret: "$cred:hook", previous_secret: "$cred:hook-old" },
    user_bearer: "$cred:forge-token",
    user_tier: "high",
    dedup_key: ["$.repository.id", "$.after"],
    dedup_ttl_seconds: 86400,
    filters: [{ path: "$.ref", op: "glob", value: "refs/heads/*" }],
    event_shapes: { star: ["$.action", "$.starred_at"] },
  };

  it("reads a forge definition and builds the same overlay back", () => {
    const s = initWebhook(forgeDef);
    expect(s.provider).toBe("github");
    expect(s.authKind).toBe("");
    expect(s.secretSource).toBe("stored");
    expect(validateWebhook(s)).toBeNull();
    const ov = buildWebhookOverlay(s, "");
    expect(ov.provider).toBe("github");
    expect(ov.events).toEqual(["push", "pull_request"]);
    expect(ov.auth).toEqual({ secret: "$cred:hook", previous_secret: "$cred:hook-old" });
    expect(ov.user_bearer).toBe("$cred:forge-token");
    expect(ov.user_tier).toBe("high");
    expect(ov.dedup_key).toEqual(["$.repository.id", "$.after"]);
    expect(ov.dedup_ttl_seconds).toBe(86400);
    expect(ov.filters).toEqual(forgeDef.filters);
    expect(ov.event_shapes).toEqual(forgeDef.event_shapes);
  });

  it("leaves auth.kind out when the provider decides it", () => {
    const s = { ...initWebhook({ delivery: "spawn", agent: "a", provider: "gitlab" }), secretSource: "env" as const, bearerTokenEnv: "LOOMCYCLE_GL_TOKEN" };
    expect(validateWebhook(s)).toBeNull();
    expect(buildWebhookOverlay(s, "").auth).toEqual({ bearer_token_env: "LOOMCYCLE_GL_TOKEN" });
  });

  it("keeps a team webhook a team webhook", () => {
    const s = initWebhook({ delivery: "team", team: "pr-review", vars: { repo: "$.repository.full_name" }, auth: { signing_secret_env: "LOOMCYCLE_S" } });
    expect(s.delivery).toBe("team");
    expect(validateWebhook(s)).toBeNull();
    const ov = buildWebhookOverlay(s, "");
    expect(ov.delivery).toBe("team");
    expect(ov.team).toBe("pr-review");
    expect(ov.vars).toEqual({ repo: "$.repository.full_name" });
    expect(ov.agent).toBeUndefined();
  });

  it("refuses what the server would refuse", () => {
    const ok = initWebhook(forgeDef);
    for (const [bad, want] of [
      [{ secret: "literal-secret" }, "auth.secret"],
      [{ previousSecret: "old" }, "previous_secret"],
      [{ userBearer: "ghp_literal" }, "user_bearer"],
      [{ events: "push, bad event!" }, "events"],
      [{ dedupTTLSeconds: "7776001" }, "90 days"],
      [{ dedupKey: "", dedupTTLSeconds: "60" }, "needs a dedup_key"],
      [{ filtersJSON: "{not json" }, "filters is not JSON"],
      [{ filtersJSON: '{"path":"$.ref"}' }, "JSON list"],
      [{ eventShapesJSON: '{"star": "$.action"}' }, "list of rule strings"],
      [{ secretSource: "env" as const, signingSecretEnv: "" }, "signing_secret_env"],
    ] as const) {
      const msg = validateWebhook({ ...ok, ...bad });
      expect(msg, JSON.stringify(bad)).toContain(want);
    }
    expect(validateWebhook({ ...ok, delivery: "channel", channel: "c" })).toContain("user_bearer");
  });
});
