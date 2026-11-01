// RFC FS — ruling on one document's proposals at once, from the document viewer.
// The sequence is runRuling's (dry run first, expect_count, chunks); what is
// here is which documents it is offered for and what it selects.

import type { BrowseScope, DocEdge, DocScope } from "../types";
import type { DocRulingAnswer, DocRulingFilter, ExplorerDataLayer } from "./dataLayer";
import { isProposed } from "./statement";
import { runRuling, type RulingAction, type RulingHooks, type RulingOutcome, type RulingScope } from "./ruling";

// reviewScopeFor is the review scope a document's bulk ruling acts in, or
// undefined for an agent-scope document, which the review routes do not reach.
// A user document is reviewed in the scope of the subject being browsed
// (ruleDocumentProposals sends it as scope_id); the runtime decides whether
// the caller may name that subject.
export function reviewScopeFor(scope: DocScope): RulingScope | undefined {
  return scope === "tenant" || scope === "user" ? scope : undefined;
}

// bulkReviewScope is where the document viewer offers bulk review: the review
// scope, when the data layer can rule in bulk (documentRuleStatements is
// optional — a custom data layer without it gets no bulk action) and the review
// routes reach the document.
export function bulkReviewScope(
  data: Pick<ExplorerDataLayer, "documentRuleStatements">,
  scope: DocScope,
): RulingScope | undefined {
  return data.documentRuleStatements ? reviewScopeFor(scope) : undefined;
}

// documentProposals is the document's own proposals among its edges: those
// filed from it, which is what from_document_id selects on the server.
export function documentProposals(edges: DocEdge[], documentId: string): DocEdge[] {
  return edges.filter((e) => isProposed(e) && !!e.id && e.from_document_id === documentId);
}

// ReviewPanel is what the Connections panel needs to offer bulk review.
export interface ReviewPanel {
  proposals: DocEdge[];
  scope: RulingScope;
  // The edge list stopped at its page cap: proposals past it are not listed.
  incomplete: boolean;
  onChanged: () => void;
}

// reviewPanel decides whether the Connections panel shows bulk review: wherever
// it can rule (scope), even with nothing listed. Hiding it at zero unmounted it
// the moment a ruling took the document's last proposal, and with it the
// ruling's result and its Undo.
export function reviewPanel(
  scope: RulingScope | undefined,
  proposals: DocEdge[],
  incomplete: boolean,
  onChanged: () => void,
): ReviewPanel | undefined {
  return scope ? { proposals, scope, incomplete, onChanged } : undefined;
}

export interface DocumentRuling {
  documentId: string;
  action: RulingAction;
  // The statements picked by hand; empty or absent rules every proposal filed
  // from the document.
  ids?: string[];
  reason?: string;
  scope: RulingScope;
  browse?: BrowseScope;
}

// ruleDocumentProposals rules a document's proposals through the data layer's
// bulk ruling. Rejects when the data layer has none.
export function ruleDocumentProposals(
  data: ExplorerDataLayer,
  r: DocumentRuling,
  hooks?: RulingHooks<DocRulingAnswer>,
): Promise<RulingOutcome> {
  if (!data.documentRuleStatements) {
    return Promise.reject(new Error("this data layer cannot rule in bulk"));
  }
  return runRuling<DocRulingFilter, DocRulingAnswer>(
    // Called through `data` so a class-based data layer keeps `this`.
    { rule: (req) => data.documentRuleStatements!(req, r.browse) },
    {
      action: r.action,
      ...(r.ids && r.ids.length > 0 ? { ids: r.ids } : { filter: { from_document_id: r.documentId } }),
      ...(r.reason ? { reason: r.reason } : {}),
      scope: r.scope,
      // The browsed subject's user documents: the review routes take it as
      // scope_id, as the document reads take it as browse.scopeId.
      ...(r.scope === "user" && r.browse?.scopeId ? { scopeId: r.browse.scopeId } : {}),
    },
    hooks,
  );
}

// rulingNote is the one line a ruling leaves in the panel.
export function rulingNote(action: RulingAction, o: RulingOutcome): string {
  const n = (x: number) => x.toLocaleString("en-US");
  const parts = [`${action === "accept" ? "Accepted" : "Rejected"} ${n(o.ruled)} statement${o.ruled === 1 ? "" : "s"}`];
  if (o.failed > 0) parts.push(`${n(o.failed)} did not take the ruling`);
  if (o.skipped > 0) parts.push(`${n(o.skipped)} skipped: not yours to rule`);
  return parts.join("; ") + ".";
}
