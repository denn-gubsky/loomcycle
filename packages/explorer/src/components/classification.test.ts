import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, it, expect, vi } from "vitest";
import type { ClassifyAnswer } from "../types";
import { ExplorerDataProvider, type ExplorerDataLayer } from "../lib/dataLayer";
import Classification, { ClassifyAnswerView, type ClassificationProps } from "./Classification";

// The package tests without a DOM: renderToStaticMarkup renders the first frame
// (effects do not run), which is what decides whether the pending block and the
// panel show at all.
function render(props: Partial<ClassificationProps>, data: Partial<ExplorerDataLayer>) {
  const full: ClassificationProps = {
    documentId: "d1",
    scope: "tenant",
    rootFields: undefined,
    requestKey: 0,
    onClose: () => {},
    onChanged: () => {},
    ...props,
  };
  return renderToStaticMarkup(
    createElement(ExplorerDataProvider, {
      value: data as ExplorerDataLayer,
      children: createElement(Classification, full),
    }),
  );
}

const PENDING = { classification: { by: "classify", at: 1, proposed_type: "Law", statements: ["s1", "s2"], proposed_tags: ["topic/tax"] } };

describe("Classification", () => {
  it("shows a pending classification with Accept", () => {
    const html = render({ rootFields: PENDING }, { documentAcceptClassification: vi.fn() });
    expect(html).toContain("Pending classification");
    expect(html).toContain("<strong>Law</strong>");
    expect(html).toContain("topic/tax");
    expect(html).toContain("2 proposed statements");
    expect(html).toContain(">Accept<");
  });

  it("renders nothing when nothing is pending and the panel is closed", () => {
    const applied = { classification: { by: "classify", at: 1, applied_type: "Law" } };
    expect(render({ rootFields: applied }, { documentAcceptClassification: vi.fn() })).toBe("");
  });

  it("hides the pending block from a data layer that cannot accept", () => {
    expect(render({ rootFields: PENDING }, {})).toBe("");
  });

  it("an open panel says it is classifying until the report lands", () => {
    const html = render({ requestKey: 1 }, { documentClassify: vi.fn() });
    expect(html).toContain("Classify");
    expect(html).toContain("Classifying…");
  });
});

describe("ClassifyAnswerView", () => {
  const answer: ClassifyAnswer = {
    document_id: "d1",
    write: "report",
    result: "classified",
    class: "Law",
    live: true,
    work_key: "law:2297-vi",
    fields: { number: "2297-VI" },
    issued_by: [{ instance: "Verkhovna Rada", quote: "Верховна Рада" }],
    concepts: [{ concept: "Taxation", tag: "topic/tax", share: 0.6 }],
    steps: [
      { step: "rules", candidates: [{ class: "Law", quote: "Закон № 2297-VI", from: "title" }], chosen: "Law" },
      { step: "model", method: "decision", chosen: "Law", reason: "reads as a statute", candidates: [] },
    ],
    warnings: ["the vote abstained: no embedder"],
  };

  it("shows the class, work key, issuer, concepts, steps and warnings", () => {
    const html = renderToStaticMarkup(createElement(ClassifyAnswerView, { answer }));
    expect(html).toContain("would be applied");
    expect(html).toContain("law:2297-vi");
    expect(html).toContain("number=2297-VI");
    expect(html).toContain("Verkhovna Rada");
    expect(html).toContain("Taxation");
    expect(html).toContain("60%");
    expect(html).toContain("1 candidate");
    expect(html).toContain("“Закон № 2297-VI” (title)");
    expect(html).toContain("the vote abstained: no embedder");
  });

  it("renders a model step with its method and reason", () => {
    const html = renderToStaticMarkup(createElement(ClassifyAnswerView, { answer }));
    expect(html).toContain("Model");
    expect(html).toContain("[decision]");
    expect(html).toContain("reads as a statute");
  });

  it("renders answer text as text, never markup", () => {
    const html = renderToStaticMarkup(
      createElement(ClassifyAnswerView, {
        answer: { ...answer, warnings: ["<img src=x onerror=alert(1)>"] },
      }),
    );
    expect(html).not.toContain("<img");
    expect(html).toContain("&lt;img");
  });
});
