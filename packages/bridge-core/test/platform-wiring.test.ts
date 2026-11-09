// A census of the platform bridges beside this package: every call a
// platform makes into the router passes the platform's date for the message
// (MessageOptions), or says on the line above why it has none.
//
// The platforms depend on the PUBLISHED core, so a contract the core adds is
// not proven by either side's own tests: the core's call the router with the
// option directly, and a platform's compile against the old core without it.
// That is how a returning person's message never dropped a kept revoke: the
// core read a sentAt no platform sent. This test reads the platforms' sources
// from the repository, so it fails the core's own suite when a call site
// misses the option.

import { readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

const PACKAGES = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

/** The arguments each router entry point takes when given its options. */
const ARITY: Record<string, number> = { onMessage: 4, onVoice: 5, onCardAction: 4 };

/** The marker that excuses a call: the platform gives no date there. */
const NO_DATE = "census: no date";

function platforms(): string[] {
  try {
    return readdirSync(PACKAGES).filter((d) => d.startsWith("bridge-") && d !== "bridge-core" && statSync(path.join(PACKAGES, d, "src")).isDirectory());
  } catch {
    return [];
  }
}

function sources(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) return sources(p);
    return e.name.endsWith(".ts") ? [p] : [];
  });
}

/** argCount counts the top-level arguments of the call whose "(" is at
 *  open, skipping strings and nested brackets. */
function argCount(src: string, open: number): number {
  let depth = 0;
  let args = 0;
  let sawToken = false;
  for (let i = open; i < src.length; i++) {
    const c = src[i]!;
    if (c === '"' || c === "'" || c === "`") {
      for (i++; i < src.length && src[i] !== c; i++) if (src[i] === "\\") i++;
      sawToken = true;
      continue;
    }
    if (c === "(" || c === "[" || c === "{") {
      depth++;
      if (depth > 1) sawToken = true;
      continue;
    }
    if (c === ")" || c === "]" || c === "}") {
      depth--;
      if (depth === 0) return sawToken ? args + 1 : 0;
      continue;
    }
    if (depth === 1 && c === ",") {
      // A trailing comma ends no argument.
      const rest = src.slice(i + 1).trimStart();
      if (rest.startsWith(")")) continue;
      args++;
      continue;
    }
    if (!/\s/.test(c)) sawToken = true;
  }
  throw new Error("unbalanced call");
}

interface Site {
  file: string;
  line: number;
  method: string;
  args: number;
  excused: boolean;
}

function sites(platform: string): Site[] {
  const out: Site[] = [];
  for (const file of sources(path.join(PACKAGES, platform, "src"))) {
    const src = readFileSync(file, "utf8");
    const lines = src.split("\n");
    for (const m of src.matchAll(/\brouter\.(onMessage|onVoice|onCardAction)\(/g)) {
      const open = m.index! + m[0].length - 1;
      const line = src.slice(0, m.index).split("\n").length;
      const here = lines[line - 1] ?? "";
      const above = lines[line - 2] ?? "";
      out.push({
        file: path.relative(PACKAGES, file),
        line,
        method: m[1]!,
        args: argCount(src, open),
        excused: here.includes(NO_DATE) || above.includes(NO_DATE),
      });
    }
  }
  return out;
}

describe("platform wiring census", () => {
  it("counts top-level arguments", () => {
    const at = (s: string) => argCount(s, s.indexOf("("));
    expect(at("f()")).toBe(0);
    expect(at("f(a)")).toBe(1);
    expect(at('f(a, "x,y", g(1, 2), { a: 1, b: [2, 3] })')).toBe(4);
    expect(at("f(\n  a,\n  b,\n)")).toBe(2);
  });

  const found = platforms();
  it.skipIf(found.length === 0)("finds the platform bridges and their router calls", () => {
    expect(found.length).toBeGreaterThan(0);
    for (const p of found) expect(sites(p).length, p).toBeGreaterThan(0);
  });

  it.skipIf(found.length === 0)("every router call passes the platform's date, or says why it has none", () => {
    const missing = found
      .flatMap(sites)
      .filter((s) => s.args < ARITY[s.method]! && !s.excused)
      .map((s) => `${s.file}:${s.line} ${s.method} has ${s.args} arguments; pass { sentAt } or mark the line above "// ${NO_DATE}: <why>"`);
    expect(missing).toEqual([]);
  });
});
