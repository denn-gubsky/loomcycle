import { describe, expect, it, vi } from "vitest";
import { codeFromHash, minLength, redeemInvite, refusalText, signInWithPassword } from "./passwordGate";

const code = "lci_" + "A".repeat(43);

describe("codeFromHash", () => {
  it("reads an invite or reset code from the fragment", () => {
    expect(codeFromHash("#" + code)).toBe(code);
    expect(codeFromHash("#lcr_" + "b".repeat(43))).toBe("lcr_" + "b".repeat(43));
  });
  it("refuses anything that is not a code", () => {
    expect(codeFromHash("")).toBe("");
    expect(codeFromHash("#lct_" + "A".repeat(43))).toBe("");
    expect(codeFromHash("#" + code + "&x=<script>")).toBe("");
  });
});

describe("calls", () => {
  it("posts the password form as JSON with the UI header, never in the URL", async () => {
    const f = vi.fn(async () => new Response(null, { status: 204 }));
    const r = await signInWithPassword("local", "alice", "secret pass", "", f as unknown as typeof fetch);
    expect(r.ok).toBe(true);
    const [url, init] = f.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("/v1/_auth/complete/local");
    expect(url).not.toContain("secret");
    expect(new Headers(init.headers).get("X-Loomcycle-UI")).toBe("1");
    expect(JSON.parse(String(init.body))).toEqual({ login: "alice", password: "secret pass", otp: "" });
  });
  it("carries the invite code in the body", async () => {
    const f = vi.fn(async () => new Response(JSON.stringify({ code: "login_taken", error: "x" }), { status: 409 }));
    const r = await redeemInvite({ code, login: "a", password: "p" }, f as unknown as typeof fetch);
    const [url, init] = f.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("/v1/_auth/invite");
    expect(JSON.parse(String(init.body)).code).toBe(code);
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.refusal.code).toBe("login_taken");
  });
});

describe("refusalText", () => {
  it("names the minimum length when the server gives it", () => {
    expect(refusalText({ code: "password_too_short", min_length: 15 })).toContain("15");
  });
  it("answers an unknown login and a wrong password alike", () => {
    expect(refusalText({ code: "invalid_credentials" })).toBe("That login and password were not accepted.");
  });
  it("has a fallback", () => {
    expect(refusalText({ code: "something_new" })).toBe("That did not work. Try again.");
  });
});

describe("minLength", () => {
  it("is 15 alone and 8 with an authenticator", () => {
    const info = { min_length: 15, min_length_with_mfa: 8 };
    expect(minLength(info, false)).toBe(15);
    expect(minLength(info, true)).toBe(8);
  });
});
