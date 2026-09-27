import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { HOOK_EVENT_HINTS } from "../lib/hooks";
import { hookDefRegistry } from "./hookdef";

// Drift guard: the registry must cover exactly what a HookDef stores. The keys
// are read from the Go structs themselves (no copy here to drift with them), so
// a field added to the runtime's definition fails here until the editor has it.
// A published/extracted copy has no Go source, and reports itself skipped.
const goFile = fileURLToPath(new URL("../../../../internal/hooks/def.go", import.meta.url));

function goJSONKeys(src: string, struct: string): string[] {
  const start = src.indexOf(`type ${struct} struct {`);
  expect(start, `${struct} struct not found — did it move or get renamed?`).toBeGreaterThan(-1);
  const block = src.slice(start, src.indexOf("\n}", start));
  return [...block.matchAll(/json:"([a-z0-9_]+)/g)].map((m) => m[1]!).sort();
}

const keysOf = (fields: readonly { key: string }[] | undefined) => (fields ?? []).map((f) => f.key).sort();

describe("hookDefRegistry matches hooks.Def", () => {
  it("has every field of the stored definition, and nothing else", () => {
    if (!existsSync(goFile)) {
      expect(hookDefRegistry.fields.length).toBeGreaterThan(0);
      return;
    }
    const src = readFileSync(goFile, "utf8");
    expect(keysOf(hookDefRegistry.fields)).toEqual(goJSONKeys(src, "Def"));
    const field = (k: string) => hookDefRegistry.fields.find((f) => f.key === k);
    expect(keysOf(field("match")?.fields)).toEqual(goJSONKeys(src, "DefMatch"));
    expect(keysOf(field("body")?.fields)).toEqual(goJSONKeys(src, "DefBody"));
  });

  it("offers every event the runtime's hooks answer, and nothing else", () => {
    // Read from the Phase constants in the runtime, so an event added there
    // fails here until a HookDef can be made for it.
    const typesFile = fileURLToPath(new URL("../../../../internal/hooks/types.go", import.meta.url));
    if (!existsSync(typesFile)) return;
    const phases = [...readFileSync(typesFile, "utf8").matchAll(/^\s*Phase\w+\s+Phase\s*=\s*"([a-z_]+)"/gm)].map((m) => m[1]!);
    expect(phases.length).toBeGreaterThan(10);
    const event = hookDefRegistry.fields.find((f) => f.key === "event") as { options?: readonly string[] } | undefined;
    expect([...(event?.options ?? [])].sort()).toEqual([...phases].sort());
    for (const p of phases) expect(HOOK_EVENT_HINTS[p], `no hint for ${p}`).toBeTruthy();
  });

  it("every field folds under a declared group", () => {
    const groups = new Set(hookDefRegistry.groups.map((g) => g.name));
    for (const f of hookDefRegistry.fields) expect(groups.has(f.group), f.key).toBe(true);
  });
});
