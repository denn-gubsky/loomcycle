import { describe, expect, it } from "vitest";
import {
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
