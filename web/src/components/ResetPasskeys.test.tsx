import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { ResetPasskeys } from "./PasswordGateParts";

const at = "2026-11-09T10:00:00Z";
const render = (passkeys: { name: string; created_at: string }[], removed: { name: string; created_at: string }[]) =>
  renderToStaticMarkup(createElement(ResetPasskeys, { passkeys, removed, onDone: () => {} }));

describe("ResetPasskeys", () => {
  it("names the passkeys the reset removed and the ones it kept", () => {
    const html = render([{ name: "laptop", created_at: at }], [{ name: "intruder", created_at: at }]);
    expect(html).toContain("the reset removed them");
    expect(html).toContain("<strong>intruder</strong>");
    expect(html).toContain("were kept");
    expect(html).toContain("<strong>laptop</strong>");
    expect(html.indexOf("intruder")).toBeLessThan(html.indexOf("were kept"));
  });

  it("says nothing about removed passkeys when none was removed", () => {
    const html = render([{ name: "laptop", created_at: at }], []);
    expect(html).not.toContain("removed them");
    expect(html).toContain("<strong>laptop</strong>");
  });

  it("says nothing about kept passkeys when every one was removed", () => {
    const html = render([], [{ name: "", created_at: at }]);
    expect(html).not.toContain("were kept");
    expect(html).toContain("Unnamed passkey");
  });
});
