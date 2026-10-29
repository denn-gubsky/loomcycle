package builtin

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// document_anchor_gate.go — the ontology's hooks for part anchors (RFC FN P2 ↔
// RFC FO). A document class may declare an @anchor_grammar (AKN eId style) that
// keys its parts better than their heading path does. FO owns the bodies; until
// it fills them every document uses the heading-path anchor.

// anchorGrammarFor names the @anchor_grammar of the class of the document whose
// root is rootID ("" = none: heading-path anchors).
func (d *Document) anchorGrammarFor(ctx context.Context, key sqlmem.ScopeKey, rootID string) string {
	return ""
}

// anchorFor applies a grammar to a part's heading path; "" when grammar is "" or
// does not match, and the caller falls back to the heading path.
func (d *Document) anchorFor(ctx context.Context, grammar string, headingPath []string) string {
	return ""
}
