import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

// A colour token no theme defines falls back to whatever literal the rule
// carries — the same in every theme. .login-card once fell back to a dark
// background while its text followed the light theme's --fg, and the invite
// page rendered black on black. So every var(--x) in the Web UI must name a
// custom property something declares.
const root = fileURLToPath(new URL(".", import.meta.url));

function sources(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = join(dir, e.name);
    if (e.isDirectory()) return e.name === "node_modules" ? [] : sources(p);
    return /\.(css|tsx?)$/.test(e.name) && !/\.test\.tsx?$/.test(e.name)
      ? [p]
      : [];
  });
}

const files = sources(root).map((p) => ({
  path: p.slice(root.length),
  text: readFileSync(p, "utf8"),
}));

// Declared: `--x:` in CSS; `"--x":` (a style object) or setProperty("--x", …)
// in TS/TSX.
const declared = new Set(
  files.flatMap((f) => {
    const patterns = f.path.endsWith(".css")
      ? [/(--[\w-]+)\s*:/g]
      : [/["'](--[\w-]+)["']\s*:/g, /setProperty\(\s*["'](--[\w-]+)["']/g];
    return patterns.flatMap((re) => [...f.text.matchAll(re)].map((m) => m[1]));
  }),
);

const refs = files.flatMap((f) =>
  [...f.text.matchAll(/var\((--[\w-]+)/g)].map((m) => ({
    name: m[1],
    at: `${f.path}:${f.text.slice(0, m.index).split("\n").length}`,
  })),
);

describe("theme tokens", () => {
  it("finds the sign-in card's tokens", () => {
    expect(
      refs.some((r) => r.at.startsWith("styles.css") && r.name === "--fg"),
    ).toBe(true);
    expect(declared.has("--bg-soft")).toBe(true);
  });

  it("every var() names a declared custom property", () => {
    const undeclared = refs
      .filter((r) => !declared.has(r.name))
      .map((r) => `${r.at} ${r.name}`);
    expect(undeclared).toEqual([]);
  });
});
