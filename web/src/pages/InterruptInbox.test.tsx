import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { AnswerModal } from "./InterruptInbox";
import type { InterruptRow } from "../api";

const row = (over: Partial<InterruptRow>): InterruptRow => ({
  interrupt_id: "intr_1",
  run_id: "run-1",
  kind: "question",
  status: "pending",
  question: "Proceed?",
  options: ["Yes", "No"],
  priority: "normal",
  created_at: "2026-11-27T12:00:00Z",
  ...over,
});

const render = (r: InterruptRow) =>
  renderToStaticMarkup(createElement(AnswerModal, { row: r, onClose: () => {}, onResolved: () => {} }));

describe("AnswerModal", () => {
  it("offers an Other answer box beside the options when the question allows one (RFC HG)", () => {
    const html = render(row({ allow_other: true }));
    expect(html).toContain(">Yes</button>");
    expect(html).toContain('aria-label="Other answer"');
    expect(html).toContain('maxLength="4000"');
  });

  it("offers only the options when the question does not allow another answer", () => {
    const html = render(row({}));
    expect(html).toContain(">Yes</button>");
    expect(html).not.toContain("Other answer");
  });
});
