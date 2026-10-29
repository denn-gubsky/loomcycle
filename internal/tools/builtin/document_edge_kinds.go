package builtin

// reservedEdgeKinds are the relation kinds only the runtime writes, each mapped to
// what writes it. A caller-made edge of one of these kinds would be read as the
// runtime's: a manual `supersedes` makes priorSuperseder refuse a real
// correction, a manual `references` is mistaken for (and suppresses) an inline
// [[name]] link, and `mentions` / `derived_from` / `supported_by` are the
// document ↔ memory bridge's own records. link_chunks, propose_statement and the
// import / canvas / sync paths refuse them with kind_reserved.
//
// `about` is reserved too, but link_chunks still writes it: the memory
// consolidator — a code-js agent, not runtime Go — files every fact's `about` edge
// through link_chunks. Refusing it there waits until the consolidator writes it
// through the runtime's own path (RFC FN P3); propose_statement and the import /
// canvas / sync paths refuse it now.
var reservedEdgeKinds = map[string]string{
	"about":        "upsert_chunk (the consolidator)",
	"supersedes":   "supersede_chunk",
	"references":   "[[name]] links in chunk bodies",
	"mentions":     "the runtime's mention links",
	"derived_from": "upsert_chunk with a source chunk",
	"supported_by": "upsert_chunk with a source chunk",
}

// isReservedEdgeKind reports whether kind is written only by the runtime.
func isReservedEdgeKind(kind string) bool {
	_, ok := reservedEdgeKinds[kind]
	return ok
}
