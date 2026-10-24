import { describe, expect, it, vi } from "vitest";
import { endSession, endSessionWith, parseLoginConfig, signOut, UI_REQUEST_HEADER, withUIHeader } from "./uiSession";

const origin = "http://loomcycle.test:8787";

function recorder() {
  const calls: { input: RequestInfo | URL; init?: RequestInit }[] = [];
  const impl = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ input, init });
    return new Response(null, { status: 204 });
  });
  return { calls, impl: impl as unknown as typeof fetch };
}

describe("withUIHeader", () => {
  it("adds the UI header to relative and same-origin requests, keeping their own headers", async () => {
    const { calls, impl } = recorder();
    const f = withUIHeader(impl, origin);
    await f("/v1/runs", { method: "POST", headers: { Accept: "application/json" } });
    await f(origin + "/v1/_me");
    await f(new URL("/v1/x", origin));
    for (const c of calls) {
      expect(new Headers(c.init?.headers).get(UI_REQUEST_HEADER)).toBe("1");
    }
    expect(new Headers(calls[0].init?.headers).get("Accept")).toBe("application/json");
    expect(calls[0].init?.method).toBe("POST");
  });

  it("keeps a Request's own headers", async () => {
    const { calls, impl } = recorder();
    await withUIHeader(impl, origin)(new Request(origin + "/v1/x", { headers: { "X-Other": "y" } }));
    const h = new Headers(calls[0].init?.headers);
    expect(h.get("X-Other")).toBe("y");
    expect(h.get(UI_REQUEST_HEADER)).toBe("1");
  });

  it("leaves cross-origin requests untouched", async () => {
    const { calls, impl } = recorder();
    await withUIHeader(impl, origin)("https://elsewhere.example/api", { headers: { A: "b" } });
    expect(new Headers(calls[0].init?.headers).has(UI_REQUEST_HEADER)).toBe(false);
  });
});

describe("endSession", () => {
  it("posts to the logout route with the UI header", async () => {
    const { calls, impl } = recorder();
    expect(await endSession(impl)).toBe(true);
    expect(calls[0].input).toBe("/v1/_auth/logout");
    expect(calls[0].init?.method).toBe("POST");
    expect(new Headers(calls[0].init?.headers).get(UI_REQUEST_HEADER)).toBe("1");
  });

  it("follows an OIDC gate's end-session URL, and only an http(s) one", async () => {
    const answer = (body: unknown) =>
      (async () => new Response(JSON.stringify(body), { status: 200 })) as unknown as typeof fetch;
    const went: string[] = [];
    const go = (u: string) => went.push(u);
    expect(await signOut(answer({ end_session_url: "https://idp.example/logout?client_id=c" }), go)).toBe(true);
    expect(await signOut(answer({ end_session_url: "javascript:alert(1)" }), go)).toBe(true);
    expect(await signOut((async () => new Response(null, { status: 204 })) as unknown as typeof fetch, go)).toBe(true);
    expect(went).toEqual(["https://idp.example/logout?client_id=c", "/ui/login", "/ui/login"]);
    expect((await endSessionWith(answer({}))).next).toBe("/ui/login");
  });

  it("reports a failed logout", async () => {
    const failing = (async () => {
      throw new Error("offline");
    }) as unknown as typeof fetch;
    expect(await endSession(failing)).toBe(false);
  });
});

describe("parseLoginConfig", () => {
  it("reads mode, gates, origins and the session refusal", () => {
    const c = parseLoginConfig({
      mode: "strict",
      gates: [{ name: "paste", kind: "token", label: "Access token" }, { name: 7 }],
      login_origins: ["https://loomcycle.cloud", 3],
      session_refused: "insecure_transport",
    });
    expect(c).toEqual({
      mode: "strict",
      gates: [{ name: "paste", kind: "token", label: "Access token" }],
      origins: ["https://loomcycle.cloud"],
      sessionRefused: "insecure_transport",
    });
  });

  it("treats a missing or malformed document as empty", () => {
    expect(parseLoginConfig(null)).toEqual({ mode: "", gates: [], origins: [], sessionRefused: "" });
    expect(parseLoginConfig({ gates: "x", login_origins: {} }).gates).toEqual([]);
  });
});
