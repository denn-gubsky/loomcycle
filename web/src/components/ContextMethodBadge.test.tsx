import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import ContextMethodBadge, { RunContextMethod } from "./ContextMethodBadge";

describe("ContextMethodBadge", () => {
  it("shows the label with the details as its tooltip", () => {
    const html = renderToStaticMarkup(
      createElement(ContextMethodBadge, { method: { label: "append · manual compact", details: ["one", "two"] } }),
    );
    expect(html).toContain("append · manual compact");
    expect(html).toContain('title="one\ntwo"');
  });

  it("renders nothing without a method", () => {
    expect(renderToStaticMarkup(createElement(ContextMethodBadge, { method: null }))).toBe("");
  });
});

describe("RunContextMethod", () => {
  it("labels a run from its recorded spec", () => {
    const html = renderToStaticMarkup(
      createElement(RunContextMethod, { run: { spec: { compaction: { autocompact_at_pct: 70 } }, interactive: false } }),
    );
    expect(html).toContain("append · auto-compact at 70%");
  });

  it("resolves auto to recap for an interactive run", () => {
    const html = renderToStaticMarkup(
      createElement(RunContextMethod, { run: { spec: { context: { mode: "auto" } }, interactive: true } }),
    );
    expect(html).toContain("auto → recap");
  });

  it("renders nothing for a run with no spec", () => {
    expect(renderToStaticMarkup(createElement(RunContextMethod, { run: {} }))).toBe("");
  });
});
