import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

/**
 * Every public name a source module declares must be re-exported by the
 * package entry point.
 *
 * WHY. src/index.ts re-exports by explicit name lists (`export type { … } from
 * "./types.js"`), so a type added to types.ts without a matching line in
 * index.ts compiles, passes every test, and is unreachable to a consumer: a
 * method signature references it, but `import type { X } from
 * "@loomcycle/client"` fails. 23 such types accumulated before this guard —
 * found by a downstream package (n8n-nodes-loomcycle) that had to restate
 * them by hand.
 *
 * HOW. Text, not runtime: types are erased, so `import * as idx` cannot see
 * them. Each module's declared names are read from its source and compared
 * with the names index.ts lists against that module's specifier. Nothing here
 * restates either list — both are derived from the files they guard.
 */

const SRC = resolve(__dirname, "..", "src");

/** Modules whose every top-level export is public surface. client.ts is
 *  covered by its single class; fetch-helpers.ts and stream.ts are internal. */
const PUBLIC_MODULES = ["types", "errors", "client-tools", "interactive", "signature", "oauth"];

function read(file: string): string {
  return readFileSync(resolve(SRC, file), "utf8");
}

/** Names a module declares at top level. `\s+` spans newlines, so a
 *  declaration split across lines is still read. */
function declaredExports(source: string): string[] {
  const re =
    /^export\s+(?:declare\s+)?(?:abstract\s+)?(?:async\s+)?(?:interface|type|enum|const|class|function)\s+([A-Za-z_$][\w$]*)/gm;
  return [...new Set([...source.matchAll(re)].map((m) => m[1]!))].sort();
}

/** Names index.ts re-exports from one module, across every `export { … }` and
 *  `export type { … }` statement naming it. Comments inside a list are
 *  stripped; `A as B` counts as A (the source name). */
function reexportedFrom(index: string, module: string): Set<string> {
  const re = new RegExp(
    String.raw`export\s+(?:type\s+)?\{([^}]*)\}\s*from\s*["']\./${module}\.js["']`,
    "g",
  );
  const names = new Set<string>();
  for (const m of index.matchAll(re)) {
    const body = m[1]!.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");
    for (const item of body.split(",")) {
      const name = item.trim().replace(/^type\s+/, "").split(/\s+as\s+/)[0];
      if (name) names.add(name);
    }
  }
  return names;
}

describe("index.ts re-exports every public declaration", () => {
  const index = read("index.ts");

  for (const module of PUBLIC_MODULES) {
    it(`exports every name declared in src/${module}.ts`, () => {
      const declared = declaredExports(read(`${module}.ts`));
      // A parser that silently matched nothing would pass vacuously.
      expect(declared.length).toBeGreaterThan(0);
      const listed = reexportedFrom(index, module);
      const missing = declared.filter((n) => !listed.has(n));
      expect(missing).toEqual([]);
    });
  }

  it("lists no name its module does not declare", () => {
    // The reverse direction: tsc already fails on a stale name, so this only
    // keeps the parser above honest — a regex that read too much would show
    // up here, not as a false pass.
    for (const module of PUBLIC_MODULES) {
      const declared = new Set(declaredExports(read(`${module}.ts`)));
      const stale = [...reexportedFrom(index, module)].filter((n) => !declared.has(n));
      expect(stale).toEqual([]);
    }
  });
});
