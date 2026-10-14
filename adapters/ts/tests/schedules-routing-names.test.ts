// adapters/ts/tests/schedules-routing-names.test.ts — client methods for
// routes the server already served with no adapter wrapper: the schedules
// admin surface, the model-alias / routing / live-provider-model views, and the
// per-family def-name listings. Each asserts the method, URL, body and the
// parsed result, the way the Web UI drives the same routes.

import { describe, expect, it } from "vitest";

import { errorResponse, jsonResponse, makeClient } from "./helpers.js";
import { LoomcycleError, NotFoundError } from "../src/index.js";
import type { LoomcycleClient } from "../src/index.js";

const BASE = "http://test-loomcycle:8787";

function call(fetchMock: { mock: { calls: unknown[][] } }, i = 0): { url: string; init: RequestInit } {
  const c = fetchMock.mock.calls[i]!;
  return { url: c[0] as string, init: c[1] as RequestInit };
}

describe("schedules admin", () => {
  it("listAllSchedules GETs list-all and returns the entries", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        entries: [
          { name: "nightly", source: "static-only", in_static: true, in_substrate: false, static_definition: { agent: "a" } },
          {
            name: "weekly",
            source: "dynamic-only",
            in_static: false,
            in_substrate: true,
            version_count: 2,
            active_def_id: "sd_2",
            latest_version: 2,
            last_updated: "2026-10-01T00:00:00Z",
          },
        ],
      }),
    ]);

    const res = await client.listAllSchedules();
    expect(res.entries).toHaveLength(2);
    expect(res.entries[0]!.last_updated).toBeUndefined();
    expect(res.entries[1]!.active_def_id).toBe("sd_2");

    const { url, init } = call(fetchMock);
    expect(url).toBe(`${BASE}/v1/_schedules/list-all`);
    expect(init.method).toBe("GET");
  });

  it("listAllSchedules passes an admin tenant focus as ?tenant=", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ entries: [] })]);
    await client.listAllSchedules({ tenant: "acme corp" });
    expect(call(fetchMock).url).toBe(`${BASE}/v1/_schedules/list-all?tenant=acme%20corp`);
  });

  it("getScheduleState GETs the state of an encoded def id", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        def_id: "sd/1",
        next_run_at: "2026-10-15T09:00:00Z",
        fire_count: 0,
        active_runs: [],
      }),
    ]);

    const res = await client.getScheduleState("sd/1");
    // A never-run, unpaused schedule: no last_run_at / paused_until keys.
    expect(res.last_run_at).toBeUndefined();
    expect(res.paused_until).toBeUndefined();
    expect(res.active_runs).toEqual([]);

    const { url, init } = call(fetchMock);
    expect(url).toBe(`${BASE}/v1/_schedules/sd%2F1/state`);
    expect(init.method).toBe("GET");
  });

  it("getScheduleState carries the active runs and the backlog", async () => {
    const { client } = makeClient([
      jsonResponse({
        def_id: "sd_1",
        last_status: "running",
        next_run_at: "2026-10-15T09:00:00Z",
        fire_count: 3,
        catch_up_until: "2026-10-14T09:00:00Z",
        missed_slots: 2,
        active_runs: [
          { def_id: "sd_1", run_id: "r_1", slot_at: "2026-10-14T08:00:00Z", catch_up: true, started_at: "2026-10-14T08:00:01Z" },
        ],
      }),
    ]);
    const res = await client.getScheduleState("sd_1");
    expect(res.missed_slots).toBe(2);
    expect(res.active_runs[0]!.catch_up).toBe(true);
  });

  it("getScheduleState raises NotFoundError on 404", async () => {
    const { client } = makeClient([
      errorResponse(404, JSON.stringify({ code: "not_found", error: "no run-state row for def_id" })),
    ]);
    await expect(client.getScheduleState("nope")).rejects.toBeInstanceOf(NotFoundError);
  });

  it.each([
    ["runScheduleNow", "run-now", { def_id: "sd_1", scheduled: "next-tick" }],
    ["pauseSchedule", "pause", { def_id: "sd_1", paused: true }],
    ["resumeSchedule", "resume", { def_id: "sd_1", paused: false }],
  ] as const)("%s POSTs %s with no body", async (method, suffix, reply) => {
    const { client, fetchMock } = makeClient([jsonResponse(reply)]);

    const res = await (client[method] as (id: string) => Promise<unknown>).call(client, "sd 1");
    expect(res).toEqual(reply);

    const { url, init } = call(fetchMock);
    expect(url).toBe(`${BASE}/v1/_schedules/sd%201/${suffix}`);
    expect(init.method).toBe("POST");
    expect(init.body).toBeUndefined();
  });
});

describe("models, routing, provider models", () => {
  it("listModels GETs /v1/_models and returns the alias map", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        aliases: {
          "deepseek-pro": { provider: "deepseek", model: "deepseek-v4-pro" },
          judge: { provider: "ollama-local", model: "nimble", kind: "decision" },
        },
      }),
    ]);

    const res = await client.listModels();
    expect(res.aliases["deepseek-pro"]!.model).toBe("deepseek-v4-pro");
    expect(res.aliases["deepseek-pro"]!.kind).toBeUndefined();
    expect(res.aliases.judge!.kind).toBe("decision");

    const { url, init } = call(fetchMock);
    expect(url).toBe(`${BASE}/v1/_models`);
    expect(init.method).toBe("GET");
  });

  it("getRouting GETs /v1/_routing and returns the cascade", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        generated_at: "2026-10-14T10:00:00Z",
        admin: false,
        providers: [{ provider: "anthropic", reachable: true, excluded: false }],
        user_tiers: [
          {
            name: "",
            tiers: [
              {
                tier: "middle",
                cascade: [
                  {
                    provider: "anthropic",
                    model: "claude-x",
                    primary: true,
                    available: true,
                    selected: true,
                    throughput: { multiplier: 1, samples: 4, source: "measured" },
                  },
                ],
              },
            ],
          },
        ],
        operator_key_restricted: true,
      }),
    ]);

    const res = await client.getRouting();
    expect(res.admin).toBe(false);
    expect(res.operator_key_restricted).toBe(true);
    expect(res.providers![0]!.last_error).toBeUndefined();
    const top = res.user_tiers[0]!.tiers[0]!.cascade[0]!;
    expect(top.selected).toBe(true);
    expect(top.throughput!.samples).toBe(4);

    const { url, init } = call(fetchMock);
    expect(url).toBe(`${BASE}/v1/_routing`);
    expect(init.method).toBe("GET");
  });

  it("listProviderModels GETs the encoded provider's live model list", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        provider: "ollama-local",
        fetched_at: "2026-10-14T10:00:00Z",
        duration_ms: 42,
        models: ["qwen3:8b"],
      }),
    ]);

    const res = await client.listProviderModels("ollama-local");
    expect(res.models).toEqual(["qwen3:8b"]);
    expect(res.duration_ms).toBe(42);

    const { url, init } = call(fetchMock);
    expect(url).toBe(`${BASE}/v1/_providers/ollama-local/models`);
    expect(init.method).toBe("GET");
  });

  it("listProviderModels surfaces a tenant token's 403 as status 403", async () => {
    const { client } = makeClient([errorResponse(403, "insufficient scope")]);
    const err = await client.listProviderModels("anthropic").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(403);
  });

  it("listProviderModels keeps the server's code on a failed listing", async () => {
    const { client } = makeClient([
      errorResponse(502, JSON.stringify({ code: "provider_list_failed", error: "upstream 500" })),
    ]);
    const err = await client.listProviderModels("openai").catch((e: unknown) => e);
    expect((err as LoomcycleError).code).toBe("provider_list_failed");
  });
});

describe("def name listings", () => {
  const families: Array<[keyof LoomcycleClient, string]> = [
    ["listAgentDefNames", "agentdef"],
    ["listSkillDefNames", "skilldef"],
    ["listHookDefNames", "hookdef"],
    ["listMcpServerDefNames", "mcpserverdef"],
    ["listScheduleDefNames", "scheduledef"],
    ["listA2AServerCardDefNames", "a2aservercarddef"],
    ["listA2AAgentDefNames", "a2aagentdef"],
    ["listWebhookDefNames", "webhookdef"],
    ["listMemoryBackendDefNames", "memorybackenddef"],
    ["listDocumentSourceDefNames", "documentsourcedef"],
    ["listOperatorTokenDefNames", "operatortokendef"],
  ];

  it.each(families)("%s GETs /v1/_%s/names", async (method, family) => {
    const names = [{ name: "n1", version_count: 1, latest_version: 1, last_updated: "2026-10-01T00:00:00Z" }];
    const { client, fetchMock } = makeClient([jsonResponse({ names })]);

    const fn = client[method] as (opts?: { tenant?: string }) => Promise<{ names: unknown[] }>;
    const res = await fn.call(client);
    expect(res.names).toEqual(names);

    const { url, init } = call(fetchMock);
    expect(url).toBe(`${BASE}/v1/_${family}/names`);
    expect(init.method).toBe("GET");
  });

  it.each(families)("%s passes an admin tenant focus as ?tenant=", async (method, family) => {
    const { client, fetchMock } = makeClient([jsonResponse({ names: [] })]);
    const fn = client[method] as (opts?: { tenant?: string }) => Promise<unknown>;
    await fn.call(client, { tenant: "acme" });
    expect(call(fetchMock).url).toBe(`${BASE}/v1/_${family}/names?tenant=acme`);
  });

  it("listOperatorTokenDefNames returns the token summary shape", async () => {
    const { client } = makeClient([
      jsonResponse({
        names: [
          { name: "ops", tenant_id: "acme", subject: "alice", token_count: 3, has_current: true, last_updated: "2026-10-01T00:00:00Z" },
        ],
      }),
    ]);
    const res = await client.listOperatorTokenDefNames();
    expect(res.names[0]!.subject).toBe("alice");
    expect(res.names[0]!.has_current).toBe(true);
  });
});
