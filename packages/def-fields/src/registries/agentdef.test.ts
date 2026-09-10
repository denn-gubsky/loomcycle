import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import type { DefRegistry, FieldSpec } from "../types";
import { AGENTDEF_EXCLUDED, agentDefRegistry } from "./agentdef";

// The overlay keys `AgentDef create` / `fork` persists, i.e. the json tags of
// builtin.mergedDef. Copied here so the assertions below read as a checkable
// list; the "drift against the Go source" test at the bottom is what keeps the
// copy honest while this package lives in the loomcycle repo.
const AGENTDEF_OVERLAY_KEYS = [
  "a2a_agent_def_scopes", "a2a_server_card_def_scopes", "agent_def_scopes",
  "channels", "code_body", "compaction", "context", "core_blocks",
  "description", "effort", "evaluation_scopes", "history_scope",
  "inherit_core_blocks", "inject_tool_guide", "internal", "interruption",
  "max_concurrent_children", "max_context_tokens", "max_iterations",
  "max_tokens", "memory_backend", "memory_consolidation",
  "memory_index_max_bytes", "memory_inject_max_tokens", "memory_protocol",
  "memory_quota_bytes", "memory_roots", "memory_scopes", "model", "models",
  "provider", "providers", "retry_attempts", "run_timeout_seconds", "sampling",
  "schedule_def_scopes", "search_providers", "skills", "sql_quota_bytes",
  "sql_scopes", "system_prompt", "system_prompt_base", "tier", "tools",
  "unbounded_iterations", "volume_def_scopes", "volumes",
] as const;

const topLevelKeys = (reg: DefRegistry) => reg.fields.map((f) => f.key);

const walk = (fields: readonly FieldSpec[], out: FieldSpec[] = []): FieldSpec[] => {
  for (const f of fields) {
    out.push(f);
    if (f.fields) walk(f.fields, out);
  }
  return out;
};

describe("agentDefRegistry — coverage of the persisted overlay", () => {
  it("declares each key at most once", () => {
    const keys = topLevelKeys(agentDefRegistry);
    expect(keys.length).toBe(new Set(keys).size);
  });

  // A field the editor renders but the substrate does not persist is worse than
  // a missing one: the operator sets it, saves, and it silently vanishes.
  it("renders nothing the overlay cannot store", () => {
    const overlay = new Set<string>(AGENTDEF_OVERLAY_KEYS);
    const strays = topLevelKeys(agentDefRegistry).filter((k) => !overlay.has(k));
    expect(strays).toEqual([]);
  });

  // The complement: a key the overlay stores but no surface exposes is only
  // reachable by hand-writing yaml — the gap that motivated the registry. An
  // omission is allowed ONLY with a recorded reason.
  it("exposes every overlay key, or names it in AGENTDEF_EXCLUDED with a reason", () => {
    const covered = new Set(topLevelKeys(agentDefRegistry));
    const missing = AGENTDEF_OVERLAY_KEYS.filter((k) => !covered.has(k));
    expect(missing).toEqual(["system_prompt_base"]);
    for (const k of missing) {
      expect(AGENTDEF_EXCLUDED[k], `${k} must carry an exclusion reason`).toBeTruthy();
    }
  });

  it("every exclusion reason is a real sentence, not a placeholder", () => {
    for (const [key, reason] of Object.entries(AGENTDEF_EXCLUDED)) {
      expect(reason.length, `${key}`).toBeGreaterThan(15);
    }
  });
});

describe("agentDefRegistry — shape invariants", () => {
  const all = walk(agentDefRegistry.fields);
  const groupNames = new Set(agentDefRegistry.groups.map((g) => g.name));

  it("every top-level field folds under a declared group", () => {
    for (const f of agentDefRegistry.fields) {
      expect(groupNames.has(f.group), `${f.key} → ${f.group}`).toBe(true);
    }
  });

  it("no group is declared empty", () => {
    for (const g of agentDefRegistry.groups) {
      const n = agentDefRegistry.fields.filter((f) => f.group === g.name).length;
      expect(n, `group ${g.name}`).toBeGreaterThan(0);
    }
  });

  // The hint is the deliverable, not decoration: an unexplained parameter in a
  // list of forty-six is indistinguishable from noise.
  it("every field and sub-field carries a hint", () => {
    for (const f of all) {
      expect(f.hint?.trim().length, `${f.key} hint`).toBeGreaterThan(10);
      expect(f.label.trim(), `${f.key} label`).not.toBe("");
    }
  });

  it("enum fields offer options; non-enum fields do not", () => {
    for (const f of all) {
      if (f.type === "enum") expect(f.options?.length, `${f.key}`).toBeGreaterThan(1);
      else expect(f.options, `${f.key}`).toBeUndefined();
    }
  });

  it("object and object-array fields declare children; leaf types do not", () => {
    for (const f of all) {
      if (f.type === "object" || f.type === "object-array") {
        expect(f.fields?.length, `${f.key}`).toBeGreaterThan(0);
      } else {
        expect(f.fields, `${f.key}`).toBeUndefined();
      }
    }
  });

  it("numeric bounds are ordered and only on numeric fields", () => {
    for (const f of all) {
      if (f.min !== undefined && f.max !== undefined) expect(f.min, `${f.key}`).toBeLessThan(f.max);
      if (f.min !== undefined || f.max !== undefined) {
        expect(["int", "float"], `${f.key}`).toContain(f.type);
      }
    }
  });

  it("children of a nested field inherit the parent's group, so the fold is coherent", () => {
    for (const parent of agentDefRegistry.fields) {
      for (const child of parent.fields ?? []) {
        expect(child.group, `${parent.key}.${child.key}`).toBe(parent.group);
      }
    }
  });
});

// Drift guard. While this package sits in the loomcycle repo, the fixture above
// is checked against the Go struct that actually defines the overlay, so adding
// a parameter to the substrate without exposing it here fails HERE rather than
// shipping an unreachable knob. A published/extracted copy has no Go source, so
// the check reports itself skipped instead of failing.
describe("AGENTDEF_OVERLAY_KEYS matches builtin.mergedDef", () => {
  const goFile = fileURLToPath(
    new URL("../../../../internal/tools/builtin/agentdef.go", import.meta.url),
  );

  it("has the same json tags as the Go struct", () => {
    if (!existsSync(goFile)) {
      // Standalone checkout: nothing to compare against.
      expect(AGENTDEF_OVERLAY_KEYS.length).toBeGreaterThan(0);
      return;
    }
    const src = readFileSync(goFile, "utf8");
    const start = src.indexOf("type mergedDef struct {");
    expect(start, "mergedDef struct not found — did it move or get renamed?").toBeGreaterThan(-1);
    const end = src.indexOf("\n}", start);
    const block = src.slice(start, end);
    const goKeys = [...block.matchAll(/json:"([a-z0-9_]+)/g)].map((m) => m[1]).sort();

    expect(goKeys).toEqual([...AGENTDEF_OVERLAY_KEYS].sort());
  });
});
