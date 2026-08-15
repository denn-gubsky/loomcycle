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

// ontologistRunHref stages a curation pass in the run terminal.
//
// A LINK, not a button that starts a run: a settings click that spends tokens is a
// surprise, and the operator should see which agent and which prompt before anything
// runs. The user id is carried because the survey reads ONE user's stored facts — the
// curator looks at the run's own scope, so which user the run belongs to IS the scope.
export function ontologistRunHref(userId?: string): string {
  const prompt = userId
    ? `Review the stored facts for user ${userId} and suggest ontology improvements.`
    : "Review the stored facts in this scope and suggest ontology improvements.";
  return `/run?agent=${encodeURIComponent("memory/ontologist")}&prompt=${encodeURIComponent(prompt)}`;
}

// consolidationRunHref stages a consolidation pass in the run terminal.
//
// A LINK, for the same reason the curation one is: a settings click that spends tokens
// is a surprise, and a consolidation pass is the most expensive thing this UI can start
// — one extractor call per chat read, plus the judge's calls when verification is on.
// The operator should see the agent and the prompt before any of that runs.
//
// THE USER ID IS THE SCOPE, not decoration. The pass consolidates whichever user the RUN
// belongs to (its scope is "user"), and the run form seeds that from the same topbar
// picker this href reads — so the prompt naming a user and the run targeting one cannot
// disagree. The prompt still names them, because an operator reviewing a staged run
// should be able to see the target without inspecting the form.
export function consolidationRunHref(userId?: string): string {
  const prompt = userId
    ? `Run one consolidation pass for ${userId}.`
    : "Run one consolidation pass for your assigned memory target.";
  return `/run?agent=${encodeURIComponent("memory/consolidator")}&prompt=${encodeURIComponent(prompt)}`;
}
