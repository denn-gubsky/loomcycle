import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { LoomcycleClient } from "@loomcycle/client";
import type { DocEdge } from "../types";
import { bulkReviewScope, documentProposals, reviewPanel, reviewScopeFor, ruleDocumentProposals, rulingNote } from "./bulkReview";
import { jsonPostFromConnection, RequestError } from "./createClient";
import { dataLayerFromClient, ExplorerDataProvider, type DocRulingAnswer, type ExplorerDataLayer } from "./dataLayer";
import type { RulingRequest } from "./ruling";
import ProposalReview, { BulkConfirm } from "../components/ProposalReview";

afterEach(() => vi.unstubAllGlobals());

const edge = (over: Partial<DocEdge> = {}): DocEdge => ({
  id: "s1",
  from_id: "c1",
  to_id: "c9",
  kind: "cites",
  status: "proposed",
  from_document_id: "d1",
  to_document_id: "d2",
  from_title: "Article 12",
  to_title: "Law 2297-VI",
  ...over,
});

// A data layer whose bulk ruling answers like a queue of `size` proposals
// filed from the document, recording each call.
function rulingLayer(size: number) {
  let left = size;
  const calls: RulingRequest<{ from_document_id: string }>[] = [];
  const documentRuleStatements = vi.fn(async (req: RulingRequest<{ from_document_id: string }>): Promise<DocRulingAnswer> => {
    calls.push(req);
    const selected = Math.min(left, 1000);
    const base = { dry_run: req.dry_run, matched: left, selected, skipped: 0, to_rule: selected, ruled: 0, failed: 0 };
    if (req.dry_run) return base;
    if (req.expect_count !== left) throw Object.assign(new Error("queue changed"), { code: "queue_changed" });
    left -= selected;
    return { ...base, ruled: selected, ruling_id: `r${calls.length}` };
  });
  return { calls, data: { documentRuleStatements } as unknown as ExplorerDataLayer };
}

describe("where bulk review is offered", () => {
  it("offers it for tenant documents and the viewer's own user documents only", () => {
    const me = { subject: "ada" };
    expect(reviewScopeFor("tenant", { scopeId: "bob" }, me)).toBe("tenant");
    expect(reviewScopeFor("user", undefined, me)).toBe("user");
    expect(reviewScopeFor("user", { scopeId: "ada" }, me)).toBe("user");
    // The review routes rule in the caller's own user scope, not the one browsed.
    expect(reviewScopeFor("user", { scopeId: "bob" }, me)).toBeUndefined();
    expect(reviewScopeFor("agent", undefined, me)).toBeUndefined();
  });

  it("offers no bulk action when the data layer has no bulk ruling", () => {
    expect(bulkReviewScope({}, "tenant")).toBeUndefined();
    expect(bulkReviewScope({ documentRuleStatements: vi.fn() }, "tenant")).toBe("tenant");
  });

  it("keeps the panel once the document's last proposal is ruled, so its result and Undo stay", () => {
    const changed = () => {};
    expect(reviewPanel("tenant", [], false, changed)).toEqual({ proposals: [], scope: "tenant", incomplete: false, onChanged: changed });
    expect(reviewPanel(undefined, [edge()], false, changed)).toBeUndefined();
  });

  it("takes the document's own proposals from its edges", () => {
    const got = documentProposals(
      [
        edge(),
        edge({ id: "s2", status: "asserted" }),
        edge({ id: "s3", from_document_id: "d7" }),
        edge({ id: undefined }),
        edge({ id: "s4" }),
      ],
      "d1",
    );
    expect(got.map((e) => e.id)).toEqual(["s1", "s4"]);
  });
});

describe("ruling on a document's proposals", () => {
  it("rules every proposal filed from the document: dry run first, then expect_count", async () => {
    const { calls, data } = rulingLayer(37);
    const confirm = vi.fn(async () => true);
    const out = await ruleDocumentProposals(
      data,
      { documentId: "d1", action: "accept", scope: "tenant", browse: { tenant: "globex" } },
      { confirm },
    );
    expect(calls).toEqual([
      { action: "accept", filter: { from_document_id: "d1" }, dry_run: true },
      { action: "accept", filter: { from_document_id: "d1" }, dry_run: false, expect_count: 37 },
    ]);
    expect((data.documentRuleStatements as ReturnType<typeof vi.fn>).mock.calls[0][1]).toEqual({ tenant: "globex" });
    expect(confirm).toHaveBeenCalledTimes(1);
    expect(out).toMatchObject({ ruled: 37, rulingIds: ["r2"] });
    expect(rulingNote("accept", out)).toBe("Accepted 37 statements.");
  });

  it("rules the rows picked by hand by id, in the user scope", async () => {
    const { calls, data } = rulingLayer(2);
    await ruleDocumentProposals(data, { documentId: "d1", action: "reject", ids: ["s1", "s4"], reason: "wrong act", scope: "user" });
    expect(calls[0]).toEqual({ action: "reject", ids: ["s1", "s4"], reason: "wrong act", scope: "user", dry_run: true });
    expect(calls[1]).toMatchObject({ ids: ["s1", "s4"], dry_run: false, expect_count: 2 });
  });

  it("refuses without a bulk ruling in the data layer", async () => {
    await expect(ruleDocumentProposals({} as ExplorerDataLayer, { documentId: "d1", action: "accept", scope: "tenant" })).rejects.toThrow(
      /cannot rule in bulk/,
    );
  });
});

describe("the default data layer's bulk ruling", () => {
  const bareClient = () => ({ path: vi.fn(), document: vi.fn() }) as unknown as LoomcycleClient;

  it("has none over a client without the review methods and no connection", () => {
    const dl = dataLayerFromClient(bareClient());
    expect(dl.documentRuleStatements).toBeUndefined();
    expect(dl.documentRevertRuling).toBeUndefined();
  });

  it("posts to the review routes over the connection, with the tenant focus", async () => {
    const post = vi.fn(async () => ({ matched: 0 }));
    const dl = dataLayerFromClient(bareClient(), undefined, post);
    const req = { action: "accept" as const, filter: { from_document_id: "d1" }, dry_run: true };
    await dl.documentRuleStatements!(req, { tenant: "globex", scopeId: "ada" });
    expect(post).toHaveBeenCalledWith("/v1/_statements/rule?tenant=globex", req);
    await dl.documentRevertRuling!("r1", "user");
    expect(post).toHaveBeenLastCalledWith("/v1/_statements/revert", { ruling_id: "r1", scope: "user" });
  });

  it("uses a newer client's own review methods when it has them", async () => {
    const ruleStatements = vi.fn(async () => ({}));
    const revertRuling = vi.fn(async () => ({}));
    const client = { ...bareClient(), ruleStatements, revertRuling } as unknown as LoomcycleClient;
    const post = vi.fn();
    const dl = dataLayerFromClient(client, undefined, post);
    await dl.documentRuleStatements!(
      { action: "reject", ids: ["s1"], reason: "no", dry_run: false, expect_count: 1, scope: "tenant" },
      { tenant: "globex" },
    );
    expect(ruleStatements).toHaveBeenCalledWith("reject", { ids: ["s1"] }, {
      reason: "no", dryRun: false, expectCount: 1, max: undefined, scope: "tenant", tenant: "globex",
    });
    await dl.documentRevertRuling!("r1", "tenant", { tenant: "globex" });
    expect(revertRuling).toHaveBeenCalledWith("r1", { scope: "tenant", tenant: "globex" });
    expect(post).not.toHaveBeenCalled();
  });
});

describe("jsonPostFromConnection", () => {
  it("sends JSON with the bearer and turns a refusal into an error with its code", async () => {
    const f = vi.fn(
      async (_url: RequestInfo | URL, _init?: RequestInit) =>
        new Response(JSON.stringify({ code: "queue_changed", error: "the selection now matches 3" }), { status: 409 }),
    );
    const post = jsonPostFromConnection({ baseUrl: "http://lc", token: "t0k", fetch: f as typeof fetch });
    const err = await post("/v1/_statements/rule", { action: "accept" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(RequestError);
    expect(err).toMatchObject({ status: 409, code: "queue_changed", message: "the selection now matches 3" });
    const [url, init] = f.mock.calls[0];
    expect(url).toBe("http://lc/v1/_statements/rule");
    expect(init?.method).toBe("POST");
    expect((init?.headers as Record<string, string>).Authorization).toBe("Bearer t0k");
    expect(JSON.parse(String(init?.body))).toEqual({ action: "accept" });
  });
});

describe("ProposalReview", () => {
  const render = (proposals: DocEdge[], data: Partial<ExplorerDataLayer> = {}) =>
    renderToStaticMarkup(
      createElement(ExplorerDataProvider, {
        value: data as ExplorerDataLayer,
        children: createElement(ProposalReview, {
          documentId: "d1",
          proposals,
          scope: "tenant",
          onChanged: () => {},
          onSelectChunk: () => {},
        }),
      }),
    );

  it("lists the document's proposals with a checkbox each and the bulk actions", () => {
    const out = render([edge(), edge({ id: "s4", to_title: "Law 1" })]);
    expect(out).toContain("Proposals from this document (2)");
    expect(out.match(/type="checkbox"/g)).toHaveLength(3);
    expect(out).toContain("Accept all from this document");
    expect(out).toContain("Reject all…");
    expect(out).toContain("Accept selected (0)");
    expect(out).toContain("Law 2297-VI");
  });

  it("disables the all-of-the-document actions when none is listed and the list is complete", () => {
    const out = render([]);
    expect(out).toContain("Proposals from this document (0)");
    expect(out).toMatch(/<button type="button" disabled="">Accept all from this document<\/button>/);
    const cut = renderToStaticMarkup(
      createElement(ExplorerDataProvider, {
        value: {} as ExplorerDataLayer,
        children: createElement(ProposalReview, {
          documentId: "d1", proposals: [], incomplete: true, scope: "tenant", onChanged: () => {}, onSelectChunk: () => {},
        }),
      }),
    );
    expect(cut).toMatch(/<button type="button">Accept all from this document<\/button>/);
  });

  it("confirms with the dry run's count and sample before ruling", () => {
    const out = renderToStaticMarkup(
      createElement(BulkConfirm, {
        step: {
          action: "reject",
          all: true,
          changed: true,
          dry: { dry_run: true, matched: 1200, selected: 1000, skipped: 2, to_rule: 998, ruled: 0, failed: 0, sample: [edge()] },
        },
        onAnswer: () => {},
      }),
    );
    expect(out).toContain("counted again");
    expect(out).toContain("Reject 1200 statements filed from this document, in 2 rulings of up to 1,000?");
    expect(out).toContain("2 of them are not yours to rule");
    expect(out).toContain("Article 12");
  });
});
