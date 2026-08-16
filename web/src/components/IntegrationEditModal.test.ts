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
