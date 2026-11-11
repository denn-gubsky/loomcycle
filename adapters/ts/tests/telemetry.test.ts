import { describe, expect, it } from "vitest";
import { jsonResponse, makeClient } from "./helpers.js";

// A tenant's own trace export. These tests pin the wire shape (verb, URL,
// body) of the four thin wrappers and that each returns the parsed result.

const BASE = "http://test-loomcycle:8787";

const policy = {
  endpoint: "https://otel.acme.example",
  protocol: "http/protobuf",
  signals: ["traces"],
  sampler_ratio: 0.25,
  updated_at: "2026-11-10T09:00:00Z",
  updated_by: "ops",
};

describe("tenant telemetry", () => {
  it("GETs the policy and status, with the tenant query encoded", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ tenant: "acme", policy, status: { exporting: true, exported_spans: 12, dropped_spans: 0, export_failures: 1 } }),
      jsonResponse({ tenant: "a b&c", policy: null }),
    ]);
    const res = await client.getTelemetry();
    expect(res.policy?.sampler_ratio).toBe(0.25);
    expect(res.status?.exported_spans).toBe(12);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${BASE}/v1/_telemetry`);
    expect(init.method).toBe("GET");

    const none = await client.getTelemetry({ tenant: "a b&c" });
    expect(none.policy).toBeNull();
    expect(fetchMock.mock.calls[1]![0]).toBe(`${BASE}/v1/_telemetry?tenant=a%20b%26c`);
  });

  it("PUTs only the fields given, keeping sampler_ratio 0 and the tenant", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ tenant: "acme", policy }),
      jsonResponse({ tenant: "globex", policy: { ...policy, sampler_ratio: 0 } }),
    ]);
    const res = await client.setTelemetry({ endpoint: "https://otel.acme.example" });
    expect(res.tenant).toBe("acme");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${BASE}/v1/_telemetry`);
    expect(init.method).toBe("PUT");
    // Unset sampler_ratio is omitted: the server reads absent as "send all".
    expect(JSON.parse(init.body as string)).toEqual({ endpoint: "https://otel.acme.example" });

    await client.setTelemetry(
      {
        endpoint: "https://otel.globex.example",
        protocol: "grpc",
        headers_credential: "otel-headers",
        signals: ["traces"],
        sampler_ratio: 0,
        content_capture: "off",
      },
      { tenant: "globex" },
    );
    expect(JSON.parse(fetchMock.mock.calls[1]![1]!.body as string)).toEqual({
      endpoint: "https://otel.globex.example",
      protocol: "grpc",
      headers_credential: "otel-headers",
      signals: ["traces"],
      sampler_ratio: 0,
      content_capture: "off",
      tenant: "globex",
    });
  });

  it("DELETEs and returns the 200 result body", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ tenant: "acme", policy: null, deleted: true }),
      jsonResponse({ tenant: "globex", policy: null }),
    ]);
    const res = await client.deleteTelemetry();
    expect(res.deleted).toBe(true);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${BASE}/v1/_telemetry`);
    expect(init.method).toBe("DELETE");
    const res2 = await client.deleteTelemetry({ tenant: "globex" });
    expect(res2.deleted).toBeUndefined();
    expect(fetchMock.mock.calls[1]![0]).toBe(`${BASE}/v1/_telemetry?tenant=globex`);
  });

  it("POSTs a test with no body and returns its outcome", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ tenant: "acme", policy, test: { ok: false, error: "connection refused", duration_ms: 31 } }),
      jsonResponse({ tenant: "globex", policy, test: { ok: true, duration_ms: 5 } }),
    ]);
    const res = await client.testTelemetry();
    expect(res.test).toEqual({ ok: false, error: "connection refused", duration_ms: 31 });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${BASE}/v1/_telemetry:test`);
    expect(init.method).toBe("POST");
    expect(init.body).toBeUndefined();
    await client.testTelemetry({ tenant: "globex" });
    expect(fetchMock.mock.calls[1]![0]).toBe(`${BASE}/v1/_telemetry:test?tenant=globex`);
  });

  it("surfaces feature_disabled as a typed 404", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "feature_disabled", message: "tenant trace export is not enabled" }, 404),
    ]);
    await expect(client.getTelemetry()).rejects.toMatchObject({ status: 404, code: "feature_disabled" });
  });
});
