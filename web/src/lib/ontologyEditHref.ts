// ontologyEditHref builds the Settings panel's "Edit ontology →" target.
//
// EXTRACTED BECAUSE THE MISSING PIECE WAS A QUERY PARAM, and a query param is exactly
// what disappears in a template literal without anyone noticing. The panel linked to
// `/documents/<id>` with no scope; the ontology lives at TENANT scope and the viewer
// folds an absent scope to `user`, so the link opened the right document id in the wrong
// store, the read 422'd, and the operator got "No chunks" and no create buttons. The
// affordance existed and led nowhere — most of why the ontology UI was reported unusable.
//
// A function with a test, rather than an inline string, so the scope cannot be dropped
// again silently.
export function ontologyEditHref(documentId: string): string {
  return `/documents/${encodeURIComponent(documentId)}?scope=tenant`;
}
