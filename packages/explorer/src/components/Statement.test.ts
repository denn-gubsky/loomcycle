import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { StatementFields } from "../types";
import { StatementMeta } from "./Statement";

const html = (statement: StatementFields) => renderToStaticMarkup(createElement(StatementMeta, { statement }));

describe("StatementMeta evidence", () => {
  it("labels a quote of a chunk's title, and only that one", () => {
    const out = html({
      evidence: [
        { id: "e1", chunk_id: "c1", quote: "Quarterly field report", part: "title", created_at: 1 },
        { id: "e2", chunk_id: "c2", quote: "the survey found", created_at: 1 },
      ],
      evidence_count: 2,
    });
    expect(out).toContain('“Quarterly field report”<span class="doc-stmt-part"> (title)</span>');
    expect(out).toContain("“the survey found”</li>");
    expect(out.match(/\(title\)/g)).toHaveLength(1);
  });

  it("labels nothing on a body quote, as before", () => {
    const out = html({ evidence: [{ id: "e1", chunk_id: "c1", quote: "the survey found", part: "body", created_at: 1 }] });
    expect(out).not.toContain("(title)");
  });
});
