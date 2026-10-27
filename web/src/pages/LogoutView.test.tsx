import { readFileSync } from "node:fs";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it, vi } from "vitest";
import LogoutView from "./LogoutView";
import { handleLogoutClick } from "../components/AccountMenu";
import { signOut } from "../lib/uiSession";

// /ui/logout is reachable by a link from any site, and the page's own
// same-origin POST would carry the session cookie, so loading it must not
// sign anyone out. Only a click does.

describe("LogoutView", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("renders a confirmation and calls no endpoint", () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    const html = renderToStaticMarkup(createElement(LogoutView));
    expect(html).toContain("Sign out?");
    expect(html).toContain("<button");
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it("has no effect hook that could sign out on load", () => {
    // The render above runs no effects (server rendering), so this is what
    // keeps a mount-time logout from coming back.
    const src = readFileSync(new URL("./LogoutView.tsx", import.meta.url), "utf8");
    expect(src).not.toMatch(/\buse(Layout)?Effect\b/);
  });
});

describe("signing out from the menu", () => {
  it("posts the logout with the UI header and goes to the login page", async () => {
    const calls: { input: RequestInfo | URL; init?: RequestInit }[] = [];
    const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ input, init });
      return new Response(null, { status: 204 });
    }) as unknown as typeof fetch;
    const go = vi.fn();
    const preventDefault = vi.fn();
    await handleLogoutClick({ preventDefault }, "/ui/logout", () => signOut(fetchImpl, go), go);
    expect(preventDefault).toHaveBeenCalled();
    expect(calls).toHaveLength(1);
    expect(calls[0].input).toBe("/v1/_auth/logout");
    expect(calls[0].init?.method).toBe("POST");
    expect(new Headers(calls[0].init?.headers).get("X-Loomcycle-UI")).toBe("1");
    expect(go).toHaveBeenCalledWith("/ui/login");
  });

  it("falls back to the confirm page when the server could not end the session", async () => {
    const go = vi.fn();
    await handleLogoutClick({ preventDefault() {} }, "/ui/logout", async () => false, go);
    expect(go).toHaveBeenCalledWith("/ui/logout");
  });
});
