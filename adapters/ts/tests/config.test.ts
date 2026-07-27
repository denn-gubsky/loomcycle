// adapters/ts/tests/config.test.ts — GET /v1/config: instance configuration +
// the live provider/model/search cascade. Mirrors library.test.ts shape.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient } from "./helpers.js";
import { type ConfigResponse } from "../src/index.js";

const adminReport: ConfigResponse = {
  generated_at: "2026-07-27T10:00:00Z",
  view: "admin",
  instance: {
    version: "v1.38.0",
    commit: "abc1234",
    build_time: "2026-07-27T09:00:00Z",
    url: "https://loomcycle.cloud",
  },
  features: {
    bash: { available: true },
    storage: { backend: "postgres" },
  },
  providers: [
    { provider: "deepseek", active: true },
    { provider: "anthropic", active: false },
  ],
  models: [
    {
      provider: "deepseek",
      model: "deepseek-v4-pro",
      tiers: ["middle"],
      active: true,
      selected: true,
    },
    {
      provider: "anthropic",
      model: "claude-sonnet-4-6",
      tiers: ["middle"],
      active: false,
      selected: false,
    },
  ],
  search: [{ provider: "brave", active: true, primary: true }],
  user_tiers: ["default", "free"],
  limits: { max_request_bytes: 16777216 },
};

describe("getConfig", () => {
  it("GETs /v1/config and returns the typed report", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(adminReport)]);

    const got = await client.getConfig();

    expect(got.view).toBe("admin");
    expect(got.instance.version).toBe("v1.38.0");
    // The live cascade is what a consumer renders: active discriminates, and
    // selected marks what actually runs.
    expect(got.providers.map((p) => p.active)).toEqual([true, false]);
    expect(got.models[0]!.selected).toBe(true);
    expect(got.models[1]!.active).toBe(false);
    expect(got.search[0]!.primary).toBe(true);
    expect(got.limits?.max_request_bytes).toBe(16777216);

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/config");
    expect((call[1] as RequestInit).method).toBe("GET");
  });

  it("reads the public view with no bearer, where features are plain booleans", async () => {
    // A deployment running LOOMCYCLE_PUBLIC_CONFIG=1 serves this to an
    // unauthenticated caller — the landing-page case. Its features are reduced to
    // booleans server-side (a whitelist by shape), so the union type in
    // ConfigResponse has to accept both forms.
    const publicReport: ConfigResponse = {
      generated_at: "2026-07-27T10:00:00Z",
      view: "public",
      instance: { version: "v1.38.0" },
      features: { bash: false, search: true },
      providers: [{ provider: "deepseek", active: true }],
      models: [
        {
          provider: "deepseek",
          model: "deepseek-v4-pro",
          tiers: ["middle"],
          active: true,
          selected: true,
        },
      ],
      search: [{ provider: "brave", active: true, primary: true }],
    };
    const { client } = makeClient([jsonResponse(publicReport)]);

    const got = await client.getConfig();

    expect(got.view).toBe("public");
    expect(got.features.search).toBe(true);
    // Build provenance and the plan roster are absent at this level, not empty.
    expect(got.instance.commit).toBeUndefined();
    expect(got.user_tiers).toBeUndefined();
    expect(got.limits).toBeUndefined();
  });

  it("forwards bearer auth in the Authorization header", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(adminReport)]);
    await client.getConfig();
    const headers = (fetchMock.mock.calls[0]![1] as RequestInit)
      .headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-bearer");
  });
});
