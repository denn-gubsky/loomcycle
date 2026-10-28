import { describe, expect, it, vi } from "vitest";
import { proxyRefusalText, signInThroughProxy } from "./headerGate";

describe("signInThroughProxy", () => {
  it("posts to the gate's complete route with the UI header and no body", async () => {
    const f = vi.fn(async () => new Response(null, { status: 204 }));
    expect(await signInThroughProxy("cloudflare", f as unknown as typeof fetch)).toBe("");
    const [url, init] = f.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("/v1/_auth/complete/cloudflare");
    expect(init.method).toBe("POST");
    expect(init.credentials).toBe("same-origin");
    expect(init.body).toBeUndefined();
    expect(new Headers(init.headers).get("X-Loomcycle-UI")).toBe("1");
  });
  it("encodes the gate name into the path", async () => {
    const f = vi.fn(async () => new Response(null, { status: 204 }));
    await signInThroughProxy("a/../b", f as unknown as typeof fetch);
    expect((f.mock.calls[0] as unknown as [string])[0]).toBe("/v1/_auth/complete/a%2F..%2Fb");
  });
  it("returns the server's refusal code", async () => {
    const f = vi.fn(async () => new Response(JSON.stringify({ code: "no_assertion" }), { status: 401 }));
    expect(await signInThroughProxy("cf", f as unknown as typeof fetch)).toBe("no_assertion");
    const broken = vi.fn(async () => {
      throw new Error("down");
    });
    expect(await signInThroughProxy("cf", broken as unknown as typeof fetch)).toBe("network");
  });
});

describe("proxyRefusalText", () => {
  it("names the proxy", () => {
    expect(proxyRefusalText("no_assertion", "Cloudflare Access")).toContain("Cloudflare Access");
    expect(proxyRefusalText("weird", "x")).toBe("Sign-in failed — try again.");
  });
});
