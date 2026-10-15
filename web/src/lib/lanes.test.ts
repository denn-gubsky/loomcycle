import { describe, expect, it } from "vitest";
import type { LaneStat, ProviderGateStat } from "../api";
import {
  laneActive,
  laneQueued,
  laneSummary,
  lanesView,
  modelsInFlight,
  sortedLanes,
  sortedProviders,
} from "./lanes";

const lane = (over: Partial<LaneStat> = {}): LaneStat => ({
  max_concurrent: 4,
  active: 2,
  reserved: 1,
  queued: 3,
  queued_background: 0,
  grants: 10,
  preemptions: 2,
  ...over,
});

describe("lanesView", () => {
  it("reports a 503 as not wired and a 403 as forbidden", () => {
    expect(lanesView({ ok: false, status: 503, body: { code: "concurrency_not_wired" } })).toEqual({
      kind: "not_wired",
    });
    expect(lanesView({ ok: false, status: 403, body: null })).toEqual({ kind: "forbidden" });
  });

  it("reports uncapped when the providers block is absent or empty, keeping the run queue", () => {
    const bare = { active: 1, queued: 2 };
    expect(lanesView({ ok: true, data: bare })).toEqual({ kind: "uncapped", stats: bare });
    const empty = { active: 0, queued: 0, providers: {} };
    expect(lanesView({ ok: true, data: empty }).kind).toBe("uncapped");
  });

  it("reports capped once any provider is listed", () => {
    const data = { active: 0, queued: 0, providers: { ollama: { active: 0, queued: 0 } } };
    expect(lanesView({ ok: true, data }).kind).toBe("capped");
  });

  it("reports any other refusal as an error naming the status", () => {
    expect(lanesView({ ok: false, status: 500, body: null })).toEqual({
      kind: "error",
      message: "HTTP 500",
    });
  });
});

describe("sortedLanes", () => {
  it("orders chat, decision, embedder, then unknown kinds by name", () => {
    const p: ProviderGateStat = {
      active: 0,
      queued: 0,
      lanes: { zeta: lane(), embedder: lane(), alpha: lane(), chat: lane(), decision: lane() },
    };
    expect(sortedLanes(p).map(([k]) => k)).toEqual(["chat", "decision", "embedder", "alpha", "zeta"]);
  });

  it("is empty for a provider that caps only models in flight", () => {
    expect(sortedLanes({ active: 0, queued: 0, models_in_flight: { m: 1 } })).toEqual([]);
  });
});

describe("sortedProviders", () => {
  it("orders providers by id and tolerates an absent block", () => {
    const p = { active: 0, queued: 0 };
    expect(sortedProviders({ openai: p, anthropic: p }).map(([id]) => id)).toEqual([
      "anthropic",
      "openai",
    ]);
    expect(sortedProviders(undefined)).toEqual([]);
  });
});

describe("modelsInFlight", () => {
  it("counts models by identity, busiest first, so the length is the models computing", () => {
    const p: ProviderGateStat = {
      active: 0,
      queued: 0,
      // The same model at another context size is another model.
      models_in_flight: { "qwen3.6:latest@65536": 1, "qwen3.6:latest@32768": 5, "gemma:2b@8192": 1 },
    };
    expect(modelsInFlight(p)).toEqual([
      ["qwen3.6:latest@32768", 5],
      ["gemma:2b@8192", 1],
      ["qwen3.6:latest@65536", 1],
    ]);
    expect(modelsInFlight(p)).toHaveLength(3);
  });

  it("is empty when the field is absent", () => {
    expect(modelsInFlight({ active: 0, queued: 0 })).toEqual([]);
  });
});

describe("lane formatting", () => {
  it("renders occupancy against the cap", () => {
    expect(laneActive(lane())).toBe("2 / 4");
  });

  it("names the background share of the queue only when there is one", () => {
    expect(laneQueued(lane())).toBe("3");
    expect(laneQueued(lane({ queued_background: 1 }))).toBe("3 (1 background)");
  });

  it("summarises a lane on one line", () => {
    expect(laneSummary("chat", lane({ queued_background: 1 }))).toBe(
      "chat: 2 / 4 active, 1 reserved, 3 (1 background) queued; 10 grants, 2 preemptions",
    );
  });
});
