// @vitest-environment happy-dom
import { act, createElement } from "react";
import { type Root, createRoot } from "react-dom/client";
import { afterEach, describe, expect, it, vi } from "vitest";
import AccountView from "./AccountView";

// The account page is mounted for real (effects run) because the sign-in
// methods panel decides what to offer from three fetches: who is signed in,
// which gates the server has, and which identities are linked already.

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const passwordGate = { name: "password", kind: "password", label: "Password" };
const ssoGate = { name: "example-sso", kind: "oidc", label: "Sign in with Example SSO" };

function stubServer(gates: object[]) {
  const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200 });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/v1/_auth/account")
        return json({ gate: "password", login: "ada", has_totp: false, recovery_codes_left: 0, require_mfa: false });
      if (url === "/v1/_me") return json({ subject: "u-ada", auth: { gate: "password" } });
      if (url === "/ui/login-config.json") return json({ gates });
      if (url === "/v1/_users/u-ada/identities") return json({ subject: "u-ada", identities: [] });
      if (url.startsWith("/v1/_auth/passkeys")) return json({ passkeys: [] });
      return new Response("{}", { status: 404 });
    }),
  );
}

async function mount(): Promise<{ root: Root; el: HTMLElement }> {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () => root.render(createElement(AccountView)));
  // Let every fetch chain settle and re-render.
  for (let i = 0; i < 5; i++) await act(async () => new Promise((r) => setTimeout(r, 0)));
  return { root, el };
}

describe("AccountView for a password-gate session", () => {
  let mounted: { root: Root; el: HTMLElement } | null = null;
  afterEach(() => {
    if (mounted) act(() => mounted!.root.unmount());
    mounted?.el.remove();
    mounted = null;
    vi.unstubAllGlobals();
  });

  it("offers to link a configured identity-provider gate", async () => {
    stubServer([passwordGate, ssoGate]);
    mounted = await mount();
    const text = mounted.el.textContent ?? "";
    // The password branch rendered, not the identity-provider one.
    expect(text).toContain("Signed in as ada");
    expect(text.match(/Sign-in methods/g)).toHaveLength(1);
    const links = [...mounted.el.querySelectorAll("button")].filter((b) => b.textContent === "Link Sign in with Example SSO");
    expect(links).toHaveLength(1);
  });

  it("offers no link when no identity-provider gate is configured", async () => {
    stubServer([passwordGate]);
    mounted = await mount();
    const text = mounted.el.textContent ?? "";
    expect(text).toContain("Signed in as ada");
    expect(text).not.toMatch(/\bLink /);
  });
});
