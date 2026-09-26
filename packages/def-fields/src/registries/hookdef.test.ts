import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
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

  it("every field folds under a declared group", () => {
    const groups = new Set(hookDefRegistry.groups.map((g) => g.name));
    for (const f of hookDefRegistry.fields) expect(groups.has(f.group), f.key).toBe(true);
  });
});
