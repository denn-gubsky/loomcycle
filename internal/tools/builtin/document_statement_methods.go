package builtin

// document_statement_methods.go — a run names how its statement was made.
//
// Every statement carries a method: manual for a person's (or a model's) write,
// and the runtime's own names for import, sync, canvas and the memory pass. A
// code-js agent's writes are neither: its rules and the extractors it drives
// made them, and the store must say so — auto-assertion is reserved for a rule,
// and a later pass treats a manual statement as a person's, never to be undone.
// The agent definition's statement_methods grant says which prefixes its runs
// may stamp; the run names the rest (rule:<name>) per write.

import (
	"context"
	"regexp"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// statementMethodOps are the ops that write a statement under the caller's
// method. Anything else that writes one (link_chunks) is a person's act, so it
// refuses a method rather than ignoring it.
var statementMethodOps = map[string]bool{
	"propose_statement":  true,
	"maintain_statement": true, // the judge of what it expires, demotes or merges
	"upsert_chunk":       true, // its relation statement; its runtime edges keep method runtime
}

// statementMethodName is the <name> of rule:<name> / extractor:<name>: short,
// lower case and path-like, so it reads as an identifier in the review queue
// and can never carry a sentence.
var statementMethodName = regexp.MustCompile(`^[a-z0-9_./-]{1,64}$`)

// HasStatementMethodGrant reports whether the caller holds a statement_methods
// grant that stamps a method — a run of a code-js agent whose definition lists
// rule or extractor. The operator plane holds none. Ops reserved for the code
// that maintains statements gate on it. The judge grant is not one: a judge
// records verdicts on the proposals routed to it, and maintains nothing.
func HasStatementMethodGrant(ctx context.Context) bool {
	sm := tools.PolicyOf(ctx).StatementMethods
	return contains(sm, "rule") || contains(sm, "extractor")
}

// stampStatementMethod turns the wire `method` into the method the op stamps
// (in.edgeMethod), or refuses it. Every refusal names method_not_granted, the
// code a caller branches on; a malformed name is refused the same way, since
// no grant covers it.
func stampStatementMethod(ctx context.Context, in *docInput) *tools.Result {
	if in.Method == "" {
		return nil
	}
	if !statementMethodOps[in.Op] {
		r := errValidation(in.Op+": method is taken only by propose_statement, maintain_statement and upsert_chunk",
			"Omit method on this op.")
		return &r
	}
	prefix, name, ok := strings.Cut(in.Method, ":")
	if !ok || (prefix != "rule" && prefix != "extractor") || !statementMethodName.MatchString(name) {
		r := errValidation(in.Op+": method_not_granted: method must be rule:<name> or extractor:<name>, the name 1-64 characters of a-z 0-9 _ . / -",
			"Name the rule or extractor that made this statement, e.g. rule:citations, or omit method.")
		return &r
	}
	if !contains(tools.PolicyOf(ctx).StatementMethods, prefix) {
		r := errPermission(in.Op+": method_not_granted: this agent may not stamp "+prefix+": statements",
			"Omit method to write it as manual. Only a code-js agent whose definition lists "+prefix+" in statement_methods may stamp it.")
		return &r
	}
	in.edgeMethod = in.Method
	return nil
}
