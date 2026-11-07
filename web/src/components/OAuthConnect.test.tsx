// @vitest-environment happy-dom
import { act, createElement } from "react";
import { type Root, createRoot } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { CredentialMeta, OAuthConnectStart, OAuthState } from "../api";
import type { ConnectDeps } from "../lib/oauthConnect";
import { OAuthConnect } from "./OAuthConnect";
import OAuthReauthBanner from "./OAuthReauthBanner";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

function meta(state: OAuthState, extra: Partial<NonNullable<CredentialMeta["oauth"]>> = {}): CredentialMeta {
  return { name: "docs", scope: "user", usage: "oauth", oauth: { resource: "https://docs.example.com/mcp", state, ...extra } };
}

function startWith(code: string): OAuthConnectStart {
  return {
    name: "docs",
    scope: "user",
    status: "pending_connect",
    user_code: code,
    verification_uri: "https://auth.example.com/device",
    verification_uri_complete: `https://auth.example.com/device?user_code=${code}`,
    expires_in: 300,
    interval: 5,
  };
}

// steppedDeps hands out the codes in order, answers status reads from the
// `status` queue, and holds every sleep until tick() releases it, so the test
// sees each phase rendered.
function steppedDeps(codes: string[], status: CredentialMeta[]) {
  const waiting: (() => void)[] = [];
  let starts = 0;
  const deps: ConnectDeps = {
    start: async () => startWith(codes[Math.min(starts++, codes.length - 1)]),
    status: async () => status.shift() ?? meta("pending_connect"),
    sleep: (_ms, signal) =>
      new Promise<void>((resolve) => {
        waiting.push(resolve);
        signal.addEventListener("abort", () => resolve());
      }),
    now: () => 0,
  };
  return { deps, tick: () => waiting.shift()?.(), starts: () => starts };
}

let mounted: { root: Root; el: HTMLElement } | null = null;

async function mount(node: ReturnType<typeof createElement>) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () => root.render(node));
  await settle();
  mounted = { root, el };
  return el;
}

async function settle() {
  for (let i = 0; i < 5; i++) await act(async () => new Promise((r) => setTimeout(r, 0)));
}

function button(el: HTMLElement, text: string): HTMLButtonElement {
  const b = [...el.querySelectorAll("button")].find((x) => x.textContent === text);
  if (!b) throw new Error(`no button "${text}" in: ${el.textContent}`);
  return b;
}

afterEach(() => {
  if (mounted) act(() => mounted!.root.unmount());
  mounted?.el.remove();
  mounted = null;
  vi.unstubAllGlobals();
});

describe("OAuthConnect", () => {
  it("shows the code, the link and the hint, then the account it connected as", async () => {
    const s = steppedDeps(["ABCD-EFGH"], [meta("pending_connect"), meta("connected", { connected_as: "ada@example.com" })]);
    const onChanged = vi.fn();
    const el = await mount(createElement(OAuthConnect, { meta: meta("pending_connect"), scope: "user", onChanged, deps: s.deps }));
    expect(el.textContent).toContain("OAuth · pending connect");

    await act(async () => button(el, "Connect account").click());
    await settle();
    expect(el.querySelector("code.token-secret")?.textContent).toBe("ABCD-EFGH");
    expect(el.textContent).toContain("Codes last 5 minutes; use the newest.");
    const link = el.querySelector("a[target=_blank]") as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("https://auth.example.com/device?user_code=ABCD-EFGH");
    expect(link.getAttribute("rel")).toContain("noopener");

    await act(async () => s.tick()); // still pending
    await settle();
    expect(el.textContent).toContain("Waiting for the approval");
    expect(onChanged).not.toHaveBeenCalled();

    await act(async () => s.tick()); // connected
    await settle();
    expect(el.textContent).toContain("Connected as ada@example.com.");
    expect(el.querySelector("code.token-secret")).toBeNull();
    expect(onChanged).toHaveBeenCalledTimes(1);
  });

  it("after a code expires, a new start shows only the newest code", async () => {
    const s = steppedDeps(["OLD1-CODE", "NEW2-CODE"], [meta("needs_reauth", { last_error: "device code expired; start again" })]);
    const el = await mount(createElement(OAuthConnect, { meta: meta("needs_reauth"), scope: "user", onChanged: () => {}, deps: s.deps }));
    expect(el.textContent).toContain("OAuth · needs reauth");

    await act(async () => button(el, "Reconnect").click());
    await settle();
    expect(el.querySelector("code.token-secret")?.textContent).toBe("OLD1-CODE");
    await act(async () => s.tick());
    await settle();
    expect(el.textContent).toContain("The code expired before it was approved.");

    const again = [...el.querySelectorAll(".settings-error button")].find((b) => b.textContent === "Get a new code") as HTMLButtonElement;
    await act(async () => again.click());
    await settle();
    const codes = [...el.querySelectorAll("code.token-secret")].map((c) => c.textContent);
    expect(codes).toEqual(["NEW2-CODE"]);
    expect(s.starts()).toBe(2);
  });

  it("asks before disconnecting, and does nothing when declined", async () => {
    const fetchMock = vi.fn(async () => new Response(JSON.stringify(meta("revoked")), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const confirmMock = vi.fn(() => false);
    vi.stubGlobal("confirm", confirmMock);
    const onChanged = vi.fn();
    const el = await mount(
      createElement(OAuthConnect, { meta: meta("connected", { connected_as: "ada@example.com" }), scope: "user", onChanged }),
    );
    expect(el.textContent).toContain("connected as ada@example.com");

    await act(async () => button(el, "Disconnect").click());
    expect(confirmMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).not.toHaveBeenCalled();

    confirmMock.mockReturnValue(true);
    await act(async () => button(el, "Disconnect").click());
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const body = JSON.parse(String((fetchMock.mock.calls[0] as unknown as [string, RequestInit])[1].body));
    expect(body).toEqual({ op: "oauth_disconnect", scope: "user", name: "docs" });
    expect(onChanged).toHaveBeenCalledTimes(1);
  });
});

describe("OAuthReauthBanner", () => {
  function stubLists(tenant: CredentialMeta[] | number, user: CredentialMeta[]) {
    const fetchMock = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      const { scope } = JSON.parse(String(init?.body)) as { scope: string };
      if (scope === "tenant") {
        if (typeof tenant === "number") return new Response('{"error":"not permitted"}', { status: tenant });
        return new Response(JSON.stringify({ scope, credentials: tenant }), { status: 200 });
      }
      return new Response(JSON.stringify({ scope, credentials: user }), { status: 200 });
    });
    vi.stubGlobal("fetch", fetchMock);
    return fetchMock;
  }

  const banner = (canManageTenant: boolean) =>
    createElement(MemoryRouter, null, createElement(OAuthReauthBanner, { canManageTenant }));

  it("names each credential in needs_reauth with a Reconnect link to its page", async () => {
    stubLists(
      [{ ...meta("needs_reauth", { last_error: "invalid_grant" }), name: "shared", scope: "tenant" }, { name: "KEY", scope: "tenant" }],
      [meta("needs_reauth"), { ...meta("connected"), name: "fine" }],
    );
    const el = await mount(banner(true));
    const alert = el.querySelector("[role=alert]");
    expect(alert?.textContent).toContain("Account connections need to be connected again.");
    expect(alert?.textContent).toContain("shared (tenant) — invalid_grant");
    expect(alert?.textContent).toContain("docs (user)");
    expect(alert?.textContent).not.toContain("fine");
    const hrefs = [...el.querySelectorAll("a")].map((a) => a.getAttribute("href"));
    expect(hrefs).toEqual(["/credentials", "/my-credentials"]);
  });

  it("reads only the viewer's own scope when they do not operate the tenant", async () => {
    const fetchMock = stubLists([{ ...meta("needs_reauth"), name: "shared" }], []);
    const el = await mount(banner(false));
    expect(el.querySelector("[role=alert]")).toBeNull();
    const scopes = fetchMock.mock.calls.map((c) => JSON.parse(String((c as unknown as [string, RequestInit])[1].body)).scope);
    expect(scopes).toEqual(["user"]);
  });

  it("still shows the viewer's own credentials when the tenant list is refused", async () => {
    stubLists(403, [meta("needs_reauth")]);
    const el = await mount(banner(true));
    expect(el.querySelector("[role=alert]")?.textContent).toContain("An account connection needs to be connected again.");
  });

  it("renders nothing while every connection is healthy", async () => {
    stubLists([], [meta("connected")]);
    const el = await mount(banner(true));
    expect(el.textContent).toBe("");
  });
});
