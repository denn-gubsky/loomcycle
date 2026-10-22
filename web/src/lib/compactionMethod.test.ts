import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { CONTEXT_DEFAULTS, contextMethod, contextMethodFromSpec } from "./compactionMethod";

// Produced by internal/config/context_defaults_web_fixture_test.go from the
// constants the loop applies at use time.
const fixture = JSON.parse(
  readFileSync(new URL("./__fixtures__/context-defaults.json", import.meta.url), "utf8"),
);

describe("CONTEXT_DEFAULTS", () => {
  it("are the runtime's own defaults", () => {
    expect(CONTEXT_DEFAULTS).toEqual(fixture);
  });
});

describe("contextMethod", () => {
  it("labels an agent that configured nothing as append, compacting at the default threshold", () => {
    const m = contextMethod({});
    expect(m.label).toBe("append · auto-compact at 80%");
    // Unset `enabled` still compacts — as the safety net — and the tooltip says so.
    expect(m.details.join("\n")).toContain("compaction.enabled");
    expect(m.details.join("\n")).toContain("last 4 messages");
    expect(m.details.join("\n")).toContain("~10%");
  });

  it("says manual when auto-compaction is explicitly off", () => {
    const m = contextMethod({ compaction: { enabled: false } });
    expect(m.label).toBe("append · manual compact");
    expect(m.details.join("\n")).toContain("Compact");
  });

  it("reads the run's own threshold and summary settings", () => {
    const m = contextMethod({
      compaction: { enabled: true, autocompact_at_pct: 70, keep_last_n: 2, target_percentage: 25, model: "small", memory_flush: true },
    });
    expect(m.label).toBe("append · auto-compact at 70%");
    const d = m.details.join("\n");
    expect(d).toContain("last 2 messages");
    expect(d).toContain("~25%");
    expect(d).toContain("small");
    expect(d).toContain("memory flush");
  });

  it("labels recap by its own trigger and names compaction as the fallback", () => {
    const m = contextMethod({ context: { mode: "recap", autorecap_at_pct: 85, reasoning: "drop" }, compaction: { autocompact_at_pct: 90 } });
    expect(m.label).toBe("recap · auto at 85%");
    const d = m.details.join("\n");
    expect(d).toContain("drop");
    expect(d).toContain("last 6 messages");
    expect(d).toContain("90%");
  });

  it("labels stateful by when it evicts, or plainly when eviction is off", () => {
    expect(contextMethod({ context: { mode: "stateful" } }).label).toBe("stateful · evicts at 80%");
    expect(contextMethod({ context: { mode: "stateful" }, compaction: { enabled: false } }).label).toBe("stateful");
  });

  it("leaves auto unresolved unless the run is interactive, which always takes recap", () => {
    expect(contextMethod({ context: { mode: "auto" } }).label).toBe("auto");
    expect(contextMethod({ context: { mode: "auto" } }).details.join("\n")).toContain("local");
    expect(contextMethod({ context: { mode: "auto" } }, { interactive: true }).label).toBe("auto → recap · auto at 80%");
  });
});

describe("contextMethodFromSpec", () => {
  it("reads the run's recorded blocks and ignores what is not one", () => {
    expect(contextMethodFromSpec({ compaction: { enabled: false } })?.label).toBe("append · manual compact");
    expect(contextMethodFromSpec({ context: "nonsense", compaction: 7 })?.label).toBe("append · auto-compact at 80%");
    expect(contextMethodFromSpec(undefined)).toBeNull();
    // A no-trace run's record holds its retention and nothing else, so it says
    // nothing about the run's context method — no label rather than a guess.
    expect(contextMethodFromSpec({ retention: "none" })).toBeNull();
  });
});
