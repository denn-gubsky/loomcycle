import { describe, expect, it } from "vitest";
import { redirectLoginError, redirectLoginHref } from "./oidcGate";

describe("redirectLoginHref", () => {
  it("links to the start route with a /ui return path only", () => {
    expect(redirectLoginHref("google")).toBe("/v1/_auth/login/google");
    expect(redirectLoginHref("google", "/ui/account", true)).toBe(
      "/v1/_auth/login/google?return_to=%2Fui%2Faccount&reauth=1",
    );
    expect(redirectLoginHref("google", "https://evil.example/")).toBe("/v1/_auth/login/google");
    expect(redirectLoginHref("a/b")).toBe("/v1/_auth/login/a%2Fb");
  });
});

describe("redirectLoginError", () => {
  it("maps known codes and hides unknown ones", () => {
    expect(redirectLoginError("")).toBe("");
    expect(redirectLoginError("login_expired")).toMatch(/took too long/);
    expect(redirectLoginError("<script>")).toBe("Sign-in failed — try again.");
  });
});
