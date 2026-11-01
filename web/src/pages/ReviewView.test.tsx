import { createElement, type ReactElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  getJudgeAgreement,
  listReviewQueue,
  listRulings,
  revertRuling,
  ruleStatements,
  type RulingRecord,
  type ReviewStatement,
  type RuleStatementsResponse,
} from "../api";
import { ApiError } from "../lib/apiError";
import { groupsFromSummary } from "../lib/review";
import {
  JudgeAgreement,
  noReviewRights,
  ReviewGroupList,
  ReviewNoRights,
  RecentRulings,
  ReviewRows,
  RuleConfirm,
  UndoToast,
} from "./ReviewView";

afterEach(() => vi.unstubAllGlobals());

const html = (el: ReactElement) => renderToStaticMarkup(createElement(MemoryRouter, null, el));

const row = (over: Partial<ReviewStatement> = {}): ReviewStatement => ({
  id: "s1",
  from_id: "c1",
  to_id: "c2",
  kind: "cites",
  method: "rule:citation",
  actor: "kb/linker",
  confidence: 0.97,
  from_title: "Article 12",
  to_title: "Law 2297-VI",
  from_document_id: "d1",
  to_document_id: "d2",
  evidence: [{ id: "e1", quote: "as provided by Law 2297-VI " + "x".repeat(200) }],
  evidence_count: 3,
  ...over,
});

const none = new Set<string>();

describe("the review rights refusal", () => {
  it("reads a 403 from the review routes as no review rights", () => {
    expect(noReviewRights(new ApiError("403 Forbidden: ruling needs review", 403, "forbidden"))).toBe(true);
    expect(noReviewRights(new ApiError("403 Forbidden: scope", 403, "insufficient_scope"))).toBe(true);
    expect(noReviewRights(new ApiError("409 Conflict: queue changed", 409, "queue_changed"))).toBe(false);
    expect(noReviewRights(new Error("network"))).toBe(false);
  });

  it("says plainly that the viewer has no review rights and what grants them", () => {
    const out = html(createElement(ReviewNoRights, { message: "403 Forbidden: reviewing the tenant's statements is for an operator" }));
    expect(out).toContain("No review rights here");
    expect(out).toContain("<code>review</code>");
    expect(out).toContain("reviewing the tenant&#x27;s statements is for an operator");
    expect(out).not.toContain("403 Forbidden");
  });
});

describe("the queue table", () => {
  it("shows from, kind, to, method and actor, confidence and the evidence side by side", () => {
    const out = html(createElement(ReviewRows, { rows: [row()], scope: "tenant", selected: none, expanded: none }));
    for (const want of ["Article 12", "<code>cites</code>", "Law 2297-VI", "rule:citation", "kb/linker", "0.97", "+2 more"]) {
      expect(out).toContain(want);
    }
    expect(out).toContain('href="/documents/d1?scope=tenant"');
    // Folded, a long quote is clipped (the whole quote is its tooltip).
    expect(out).toContain(`“as provided by Law 2297-VI ${"x".repeat(132)}…”`);
    expect(out).not.toContain(`${"x".repeat(200)}”`);
  });

  it("shows a judge's verdict, and its rationale once the evidence is expanded", () => {
    const judged = row({ attributes: { judge: { verdict: "reject", confidence: 0.91, rationale: "an amending act, not the law" } } });
    const folded = html(createElement(ReviewRows, { rows: [judged], scope: "tenant", selected: none, expanded: none }));
    expect(folded).toContain('class="review-judge reject"');
    expect(folded).toContain("0.91");
    expect(folded).not.toContain('class="review-rationale"');
    const open = html(createElement(ReviewRows, { rows: [judged], scope: "user", selected: none, expanded: new Set(["s1"]) }));
    expect(open).toContain("an amending act, not the law");
    expect(open).toContain(`${"x".repeat(200)}”`);
    expect(open).toContain('href="/documents/d2?scope=user"');
  });

  it("offers Confirm and Overturn on a judge's acceptance awaiting audit, and only there", () => {
    const pending = row({ status: "asserted", attributes: { judge: { verdict: "accept", confidence: 0.95, audited: "pending" } } });
    const settled = row({ id: "s2", status: "asserted", attributes: { judge: { verdict: "accept", audited: "confirmed" } } });
    const props = { scope: "tenant" as const, selected: none, expanded: none, onRuleOne: () => {} };
    const out = html(createElement(ReviewRows, { rows: [pending], ...props }));
    expect(out).toContain('class="review-audit pending"');
    expect(out).toContain(">Confirm<");
    expect(out).toContain(">Overturn…<");
    const done = html(createElement(ReviewRows, { rows: [settled], ...props }));
    expect(done).toContain("audit: confirmed");
    expect(done).not.toContain(">Confirm<");
    // A proposal is ruled with the page's own accept / reject, not as an audit.
    expect(html(createElement(ReviewRows, { rows: [row()], ...props }))).not.toContain("review-audit");
  });

  it("marks the focused and the selected rows", () => {
    const out = html(
      createElement(ReviewRows, {
        rows: [row(), row({ id: "s2" })],
        scope: "tenant",
        focusId: "s2",
        selected: new Set(["s1"]),
        expanded: none,
      }),
    );
    expect(out).toContain('class="review-row selected" data-statement="s1"');
    expect(out).toContain('class="review-row focused" data-statement="s2"');
  });
});

describe("the groups", () => {
  const summary = [
    {
      kind: "cites", method: "rule:citation", from_path_prefix: "/board/regulatory-external", judge_verdict: "", count: 1118,
      filter: { kind_exact: "cites", method_exact: "rule:citation", from_path_prefix: "/board/regulatory-external", has_judge_verdict: false },
    },
    // An older server's group, sent without its filter.
    { kind: "subject", method: "classify", from_path_prefix: "", judge_verdict: "", count: 239 },
  ];

  it("lists the summary's groups folded, with their counts and group actions", () => {
    const groups = groupsFromSummary(summary, {});
    const out = html(
      createElement(ReviewGroupList, {
        groups,
        open: none,
        rows: {},
        rowsProps: { scope: "tenant", selected: none, expanded: none },
      }),
    );
    expect(out).toContain("1,118");
    expect(out).toContain("/board/regulatory-external");
    expect(out).toContain('aria-expanded="false"');
    expect(out).toContain("Accept group");
    // No filter from the server: no group action.
    expect(out).toContain("(not filed)");
    expect(out).toContain("rule from the list view");
    expect(out.match(/Accept group/g)).toHaveLength(1);
    expect(out).not.toContain("<table");
  });

  it("lists an open group's rows", () => {
    const groups = groupsFromSummary(summary, {});
    const out = html(
      createElement(ReviewGroupList, {
        groups,
        open: new Set([groups[0].key]),
        rows: { [groups[0].key]: { rows: [row()], loading: false, next: "cur" } },
        rowsProps: { scope: "tenant", selected: none, expanded: none },
      }),
    );
    expect(out).toContain('aria-expanded="true"');
    expect(out).toContain("Article 12");
    expect(out).toContain(">more<");
    expect(out).toContain("select shown");
  });
});

describe("the confirm", () => {
  const dry = (over: Partial<RuleStatementsResponse> = {}): RuleStatementsResponse => ({
    tenant: "acme", scope: "tenant", action: "accept", dry_run: true,
    matched: 2500, selected: 1000, skipped: 0, to_rule: 1000, ruled: 0, failed: 0,
    breakdown: [{ kind: "cites", method: "rule:citation", count: 1000 }],
    sample: Array.from({ length: 20 }, (_, i) => row({ id: `s${i}`, from_title: `Article ${i}` })),
    ...over,
  });

  it("shows the dry run's count, how many rulings it takes, and a sample", () => {
    const out = html(createElement(RuleConfirm, { action: "accept", dry: dry(), why: "first" }));
    expect(out).toContain("Accept 2,500 statements");
    expect(out).toContain("in 3 rulings of up to 1,000 each");
    expect(out).toContain("Article 0");
    expect(out).toContain("+14 more in the sample");
    expect(out).toContain("<code>cites</code> by rule:citation: 1,000");
  });

  it("says why it asks again, and what will be skipped", () => {
    const out = html(createElement(RuleConfirm, { action: "reject", dry: dry({ matched: 1120, skipped: 4 }), why: "queue_changed" }));
    expect(out).toContain("The queue changed since the last count");
    expect(out).toContain("Reject 1,120 statements");
    expect(out).toContain("4 of the first 1,000 are not");
  });
});

describe("the undo toast", () => {
  it("offers Undo after a ruling and reports the undo once done", () => {
    const before = html(
      createElement(UndoToast, { toast: { text: "Accepted 1,118 statements.", rulingIds: ["r1", "r2"], scope: "tenant" } }),
    );
    expect(before).toContain("Accepted 1,118 statements.");
    expect(before).toContain(">Undo<");
    const after = html(
      createElement(UndoToast, {
        toast: { text: "1,100 statements came back to review; 18 changed since and were left as ruled.", rulingIds: ["r1", "r2"], scope: "tenant", undone: true },
      }),
    );
    expect(after).toContain("18 changed since");
    expect(after).not.toContain(">Undo<");
  });
});

describe("the review calls", () => {
  const stub = (body: unknown = {}) => {
    const f = vi.fn(async (_url: string, _init?: RequestInit) => new Response(JSON.stringify(body), { status: 200 }));
    vi.stubGlobal("fetch", f);
    return f;
  };

  it("asks the queue with the filter, the summary and the tenant focus as query parameters", async () => {
    const f = stub({ statements: [] });
    await listReviewQueue({ kinds: ["cites", "repeals"], min_confidence: 0.9, has_judge_verdict: false }, { summarize: true, tenant: "globex", scope: "user" });
    const url = new URL(f.mock.calls[0][0], "http://x");
    expect(url.pathname).toBe("/v1/_statements/review");
    expect(Object.fromEntries(url.searchParams)).toEqual({
      kinds: "cites,repeals", min_confidence: "0.9", has_judge_verdict: "false", summarize: "true", scope: "user", tenant: "globex",
    });
  });

  it("posts a ruling and its undo with the tenant focus", async () => {
    const f = stub({});
    await ruleStatements({ action: "accept", filter: { kind: "cites" }, dry_run: false, expect_count: 7 }, "globex");
    expect(f.mock.calls[0][0]).toBe("/v1/_statements/rule?tenant=globex");
    expect(JSON.parse(String(f.mock.calls[0][1]?.body))).toEqual({ action: "accept", filter: { kind: "cites" }, dry_run: false, expect_count: 7 });
    await revertRuling("r1", { scope: "tenant", scopeId: "bob" });
    expect(f.mock.calls[1][0]).toBe("/v1/_statements/revert");
    // The tenant scope names no subject.
    expect(JSON.parse(String(f.mock.calls[1][1]?.body))).toEqual({ ruling_id: "r1" });
  });

  it("names another subject's user scope on the queue, a ruling, its undo and the rulings", async () => {
    const f = stub({ rulings: [] });
    await listReviewQueue({}, { scope: "user", scopeId: "bob" });
    expect(Object.fromEntries(new URL(f.mock.calls[0][0], "http://x").searchParams)).toEqual({ scope: "user", scope_id: "bob" });
    await revertRuling("r1", { scope: "user", scopeId: "bob", tenant: "globex" });
    expect(f.mock.calls[1][0]).toBe("/v1/_statements/revert?tenant=globex");
    expect(JSON.parse(String(f.mock.calls[1][1]?.body))).toEqual({ ruling_id: "r1", scope: "user", scope_id: "bob" });
    await listRulings({ limit: 10, after: "c1", scope: "user", scopeId: "bob" });
    const u = new URL(f.mock.calls[2][0], "http://x");
    expect(u.pathname).toBe("/v1/_statements/rulings");
    expect(Object.fromEntries(u.searchParams)).toEqual({ limit: "10", after: "c1", scope: "user", scope_id: "bob" });
    await listRulings({ scope: "tenant", scopeId: "bob" });
    expect(f.mock.calls[3][0]).toBe("/v1/_statements/rulings");
  });
});

describe("the judge's agreement report", () => {
  const counts = (kind: string, over = {}) => ({ kind, accepted: 0, sampled: 0, pending: 0, confirmed: 0, overturned: 0, ...over });

  it("lists each kind and the total, with the rate over the settled sample", () => {
    const out = html(
      createElement(JudgeAgreement, {
        report: {
          tenant: "acme",
          scope: "tenant",
          kinds: [counts("elaborates", { accepted: 12, sampled: 4, pending: 1, confirmed: 2, overturned: 1, agreement: 2 / 3 }), counts("repeals", { accepted: 3 })],
          total: counts("", { accepted: 15, sampled: 4, pending: 1, confirmed: 2, overturned: 1, agreement: 2 / 3 }),
        },
        onAudit: () => {},
      }),
    );
    expect(out).toContain("<code>elaborates</code>");
    expect(out).toContain("67%");
    expect(out).toContain("all kinds");
    expect(out).toContain("Audit the 1 pending");
    // No settled sample yet: no rate, not 0%.
    expect(out).toMatch(/<code>repeals<\/code><\/td><td>3<\/td><td>0<\/td><td>0<\/td><td>0<\/td><td>0<\/td><td>—<\/td>/);
  });

  it("says when the judge has accepted nothing", () => {
    const out = html(createElement(JudgeAgreement, { report: { tenant: "acme", scope: "tenant", kinds: [], total: counts("") } }));
    expect(out).toContain("accepted nothing");
    expect(out).not.toContain("<table");
  });

  it("asks GET /v1/_statements/agreement where the review acts", async () => {
    const f = vi.fn(async () => new Response(JSON.stringify({ kinds: [], total: {} }), { status: 200 }));
    vi.stubGlobal("fetch", f);
    await getJudgeAgreement({ scope: "user", scopeId: "bob", tenant: "globex" });
    const u = new URL(String((f.mock.calls[0] as unknown[])[0]), "http://x");
    expect(u.pathname).toBe("/v1/_statements/agreement");
    expect(Object.fromEntries(u.searchParams)).toEqual({ scope: "user", scope_id: "bob", tenant: "globex" });
  });
});

describe("the recent rulings", () => {
  const rec = (over: Partial<RulingRecord> = {}): RulingRecord => ({
    id: "r1", action: "accept", ruled_by: "operator:ada", actor: "ada", reason: "citations hold",
    selector: { filter: { kind_exact: "cites" } }, matched: 3, ruled: 3, failed: 0, skipped: 0,
    created_at: 1793145600000000000, reverted: false, ...over,
  });

  it("lists each ruling with its selection and an Undo while it can be undone", () => {
    const out = html(createElement(RecentRulings, { rulings: [rec(), rec({ id: "r2", action: "reject", selector: { ids_count: 2 }, ruled: 2 })] }));
    expect(out).toContain("Recent rulings");
    expect(out).toContain("kind_exact=cites");
    expect(out).toContain("2 picked by hand");
    expect(out).toContain("citations hold");
    expect(out.match(/>Undo</g)).toHaveLength(2);
  });

  it("shows an undone ruling as undone and a revert without an Undo", () => {
    const out = html(
      createElement(RecentRulings, {
        rulings: [
          rec({ id: "v1", action: "revert", selector: { reverts: "r1aaaaaaaa" }, ruled: 3, failed: 1 }),
          rec({ reverted: true, reverted_at: 1793145700000000000, reverted_by: "v1" }),
        ],
      }),
    );
    expect(out).not.toContain(">Undo<");
    expect(out).toContain("undid r1aaaaaa");
    expect(out).toContain("1 changed");
    expect(out).toContain("undone 2026-");
    expect(out).toContain('class="review-ruling accept reverted"');
  });
});
