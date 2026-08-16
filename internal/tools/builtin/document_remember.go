package builtin

// document_remember.go — storing something a PERSON said to remember (RFC CF §6).
//
// THE RISK THIS IS SHAPED AGAINST. A box labelled "remember X" is a natural-language
// write path into memory, which is the exact thing the verified-writes line was built to
// constrain. Written naively, the fact it produces has no span and no verdict:
// permanently unverifiable, indistinguishable from a model's output once stored, and
// counted forever in the `unverifiable_no_span` column.
//
// THE REFRAMING THAT RESOLVES IT: an operator's instruction is a SOURCE, not a claim.
// The sentence they typed IS the evidence, so the fact is stored citing itself — its
// span is its own text — and marked `evidential`, the class already meaning "source
// material, exempt from age-based pruning". Operator-authored memory then becomes the
// best-evidenced kind rather than the worst, and it needs no judge: a claim whose span
// is itself is trivially entailed.
//
// WRITTEN DIRECTLY, NOT ROUTED THROUGH THE EXTRACTOR. Treating the instruction as a
// one-message transcript and letting the extractor distil it would be more consistent
// with the pipeline and is the wrong choice: the point of a human instruction is that it
// needs no interpretation. Passing it through a model buys multi-fact distillation — which
// an operator can do by typing two sentences — in exchange for distorting a statement
// somebody made precisely, plus a call and a wait for what should be instant.
//
// ADDITIVE ONLY. There is no "forget X" here. An instruction that deletes on a fuzzy
// match is how data disappears quietly; removal is a verdict (judge_fact, recoverable)
// or erasure (explicit, targeted, and not on this surface).

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// rememberedFactsPath is where operator-authored facts land: the same document the
// consolidation pass mirrors its entity graph into, so one subject's facts stay in one
// place regardless of who wrote them.
const rememberedFactsPath = "/memory/entities"

// rememberedKeyPrefix namespaces an operator's fact away from the pass's.
//
// DISTINCT ON PURPOSE. The consolidator keys a distilled fact `memory/fact/<slug>`, and
// an operator typing the same sentence must not silently overwrite one — the two are
// different claims about the same subject (a machine's reading, and a person's
// statement) and collapsing them would lose whichever was written second.
const rememberedKeyPrefix = "memory/operator/"

// maxRememberChars bounds one instruction. A fact is a sentence; anything longer is a
// document, and the Document ops are how you store one.
const maxRememberChars = 1000

// rememberFact stores a statement as a self-citing evidential fact.
func (d *Document) rememberFact(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return errResult("remember: missing required field: text (the statement to remember, " +
			"as one self-contained sentence)"), nil
	}
	if len(text) > maxRememberChars {
		return errResult("remember: that is longer than a fact — store it as a document instead"), nil
	}

	// Get-or-create the scope's entity document, mirroring what the consolidation pass
	// does. A remembered fact belongs in the same place a distilled one does, or the
	// graph a recall walks would have two disconnected halves.
	docID, err := d.entityDocumentID(ctx, key, mscope, in.Scope)
	if err != nil {
		return errResult("remember: " + err.Error()), nil
	}

	// Composed as an upsert rather than duplicating its write path: one place still
	// stamps origin, mirrors the k/v tier and applies the ontology gate.
	next := docInput{
		Scope:      in.Scope,
		DocumentID: docID,
		NaturalKey: rememberedKeyPrefix + slugForKey(text),
		Title:      truncateForEvent(text, 80),
		Body:       text,
		// THE SELF-CITATION IS THE SERVER'S, not the caller's. A client supplying its own
		// span could point a claim at text that does not contain it; here the span IS the
		// statement, by construction, which is what makes it checkable at all.
		SourceQuote: text,
		// Source material rather than a distillation — exempt from age-based pruning,
		// because a person said it and nothing else records that they did.
		Class:   "evidential",
		Type:    strings.TrimSpace(in.Type),
		Subject: strings.TrimSpace(in.Subject),
	}
	return d.upsertChunk(ctx, key, mscope, next)
}

// entityDocumentID resolves (and on first use creates) the scope's entity document.
func (d *Document) entityDocumentID(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, scope string) (string, error) {
	ids, err := d.documentsUnderPath(ctx, key, rememberedFactsPath)
	if err == nil && len(ids) > 0 {
		return ids[0], nil
	}
	res, cerr := d.createDocument(ctx, key, mscope, docInput{
		Scope: scope, Title: "Entities", Path: rememberedFactsPath,
	})
	if cerr != nil {
		return "", cerr
	}
	if res.IsError {
		return "", errString(res.Text)
	}
	var out struct {
		DocumentID string `json:"document_id"`
	}
	if uerr := json.Unmarshal([]byte(res.Text), &out); uerr != nil || out.DocumentID == "" {
		return "", errString("could not resolve the entity document")
	}
	return out.DocumentID, nil
}

// slugForKey renders a statement as a natural key.
//
// Bounded to [a-z0-9-] and a fixed length: this value is interpolated into SQL by the
// consolidation pass's key lookup, which guards on exactly that character class. A slug
// that could contain a quote would turn a remembered sentence into an injection site.
func slugForKey(text string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(text) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
		if b.Len() >= 60 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// errString is an error from a message, for the two spots here that surface a Result's
// text as one.
type errString string

func (e errString) Error() string { return string(e) }
