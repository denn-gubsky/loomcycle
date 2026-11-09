import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, it, expect } from "vitest";
import type { Annotation } from "../types";
import AnnotationPanel, { AnnotationBrief, type AnnotationPanelProps } from "./AnnotationPanel";

const rec = (over: Partial<Annotation> = {}): Annotation => ({
  chunk_id: "c1",
  document_id: "d1",
  level: "document",
  brief: "The brief.",
  abstract: "The abstract paragraph.",
  language: "en",
  model: "local-medium",
  written_at: "2026-11-09T22:00:00Z",
  written_by: "run:r1",
  ...over,
});

const render = (props: Partial<AnnotationPanelProps>) =>
  renderToStaticMarkup(
    createElement(AnnotationPanel, { records: [], newLevel: "document", defaultOpen: true, ...props }),
  );

describe("AnnotationPanel", () => {
  it("shows the abstract with its level, language, model, writer and time", () => {
    const html = render({ records: [rec()] });
    expect(html).toContain("The abstract paragraph.");
    expect(html).toContain(">document<");
    expect(html).toContain(">en<");
    expect(html).toContain(">local-medium<");
    expect(html).toContain("run r1");
    expect(html).toContain('title="2026-11-09T22:00:00Z"');
    expect(html).not.toContain("stale");
    expect(html).not.toContain("manual");
  });

  it("marks a stale record and a hand-written one instead of hiding them", () => {
    const html = render({ records: [rec({ stale: true, manual: true })] });
    expect(html).toContain("annotation-stale");
    expect(html).toContain(">stale<");
    expect(html).toContain(">manual<");
    expect(html).toContain("written by hand");
  });

  it("renders nothing for a reader who cannot write when there is no record", () => {
    expect(render({ records: [] })).toBe("");
  });

  it("offers Edit only with a writer and Regenerate only with the host's callback", () => {
    const save = async () => {};
    const regen = async () => {};
    expect(render({ records: [rec()] })).not.toContain(">edit<");
    expect(render({ records: [rec()], onSave: save })).toContain(">edit<");
    expect(render({ records: [rec()], onSave: save })).not.toContain(">regenerate<");
    expect(render({ records: [rec()], onRegenerate: regen })).toContain(">regenerate<");
  });

  it("offers an editor a way to write one when there is none, without inventing text", () => {
    const html = render({ records: [], onSave: async () => {} });
    expect(html).toContain(">none<");
    expect(html).toContain(">write<");
    expect(html).not.toContain("annotation-abstract");
  });

  it("renders the abstract as text, not markup", () => {
    const html = render({ records: [rec({ abstract: "<img src=x onerror=alert(1)>" })] });
    expect(html).not.toContain("<img");
    expect(html).toContain("&lt;img");
  });
});

describe("AnnotationBrief", () => {
  it("shows the brief with a stale marker when stale, and nothing without a record", () => {
    const html = renderToStaticMarkup(createElement(AnnotationBrief, { record: rec({ stale: true }) }));
    expect(html).toContain("The brief.");
    expect(html).toContain(">stale<");
    expect(renderToStaticMarkup(createElement(AnnotationBrief, {}))).toBe("");
  });
});
