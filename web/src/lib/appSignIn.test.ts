import { describe, expect, it } from "vitest";
import { afterSignInHref, appReturnTo, appSignInRequest, mayAutoContinue } from "./appSignIn";

const q = "response_type=code&client_id=board&redirect_uri=https%3A%2F%2Fboard.example%2Fcb&state=s";

describe("app sign-in", () => {
  it("reads the carried authorize query", () => {
    expect(appSignInRequest("?" + new URLSearchParams({ oauth: q }).toString())).toBe(q);
    expect(appSignInRequest("?error=x")).toBe("");
  });

  it("continues only to the fixed authorize path", () => {
    expect(afterSignInHref(q)).toBe("/v1/_oauth/authorize?" + q);
    expect(afterSignInHref("")).toBe("/ui");
    // Whatever the query holds, the path stays the server's own.
    expect(afterSignInHref("x=//evil.example")).toBe("/v1/_oauth/authorize?x=//evil.example");
  });

  it("returns a redirect gate to the login page with the request", () => {
    const back = appReturnTo(q);
    expect(back.startsWith("/ui/login?oauth=")).toBe(true);
    expect(new URLSearchParams(back.slice("/ui/login".length)).get("oauth")).toBe(q);
    expect(appReturnTo("")).toBe("");
  });

  it("auto-continues once per request", () => {
    const m = new Map<string, string>();
    const s = { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v) };
    expect(mayAutoContinue(q, s)).toBe(true);
    expect(mayAutoContinue(q, s)).toBe(false);
    expect(mayAutoContinue(q + "&x=1", s)).toBe(true);
    expect(mayAutoContinue("", s)).toBe(false);
    expect(mayAutoContinue(q, null)).toBe(true);
  });
});
