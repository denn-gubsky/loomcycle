import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { Principal } from "../api";
import OpenModeBanner from "./OpenModeBanner";

const principal = (extra: Partial<Principal>): Principal =>
  ({ tenant_id: "default", subject: "default", scopes: [], is_admin: true, legacy: false, ...extra }) as Principal;

describe("OpenModeBanner", () => {
  it("says the server is unauthenticated when /v1/_me reports open mode", () => {
    const html = renderToStaticMarkup(createElement(OpenModeBanner, { principal: principal({ open_mode: true }) }));
    expect(html).toContain("Unauthenticated");
    expect(html).toContain("LOOMCYCLE_AUTH_TOKEN");
  });

  it("renders nothing for an authenticated principal or before identity loads", () => {
    expect(renderToStaticMarkup(createElement(OpenModeBanner, { principal: principal({}) }))).toBe("");
    expect(renderToStaticMarkup(createElement(OpenModeBanner, { principal: undefined }))).toBe("");
    expect(renderToStaticMarkup(createElement(OpenModeBanner, { principal: null }))).toBe("");
  });
});
