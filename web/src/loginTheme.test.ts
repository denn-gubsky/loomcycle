import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

// The sign-in, invite and reset pages render before any app chrome, so their
// card is the first thing a new user sees. A token the themes never define
// falls back to whatever literal the rule carries; .login-card once fell back
// to a dark background while its text followed the light theme's --fg, and the
// invite page rendered black on black.
const styles = readFileSync(new URL("./styles.css", import.meta.url), "utf8");
const tokens = readFileSync(new URL("./tokens.css", import.meta.url), "utf8");

const defined = new Set([...tokens.matchAll(/(--[\w-]+)\s*:/g)].map((m) => m[1]));

// Every rule whose selector names a .login-* class, with its body.
const loginRules = [...styles.matchAll(/([^{}]*\.login-[^{}]*)\{([^}]*)\}/g)].map((m) => ({
  selector: m[1].replace(/\/\*[\s\S]*?\*\//g, "").trim(),
  body: m[2],
}));

describe("sign-in card styles", () => {
  it("finds the login rules it checks", () => {
    expect(loginRules.map((r) => r.selector)).toContain(".login-card");
  });

  it("uses only tokens the themes define", () => {
    const undefinedRefs = loginRules.flatMap((r) =>
      [...r.body.matchAll(/var\((--[\w-]+)/g)]
        .map((m) => m[1])
        .filter((name) => !defined.has(name))
        .map((name) => `${r.selector}: ${name}`),
    );
    expect(undefinedRefs).toEqual([]);
  });
});
