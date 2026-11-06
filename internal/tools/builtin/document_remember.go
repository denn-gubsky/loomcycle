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

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// rememberedFactsPath is where operator-authored facts land: the same document the
// consolidation pass mirrors its entity graph into, so one subject's facts stay in one
// place regardless of who wrote them.
const rememberedFactsPath = memrank.EntitiesPath

// entitiesDocumentTitle is the entity document's title, which the runtime supplies.
const entitiesDocumentTitle = "Entities"

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
	// A remembered fact is filed as a PERSON's statement — evidential, citing
	// itself, quotable by verbatim_answer — so a run may file one only when its
	// agent is granted statement_methods: [remember] (RFC FZ Part C, D5). A
	// person's own off-run call needs no grant.
	if !tools.IsOffRunPlane(ctx) && !contains(tools.PolicyOf(ctx).StatementMethods, statementRememberGrant) {
		return errPermission("remember: this agent may not file a fact as something a person said (its definition's statement_methods does not grant remember); propose what you learned with propose_statement, or record what you derived from a source with upsert_chunk and its source_quote",
			"Use propose_statement or upsert_chunk instead; an operator grants remember in the agent's statement_methods."), nil
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return errValidation("remember: missing required field: text (the statement to remember, "+
			"as one self-contained sentence)", ""), nil
	}
	if len(text) > maxRememberChars {
		return errValidation("remember: that is longer than a fact — store it as a document instead", "Shorten it to one sentence, or store it with op=create_document."), nil
	}

	// Get-or-create the scope's entity document, mirroring what the consolidation pass
	// does. A remembered fact belongs in the same place a distilled one does, or the
	// graph a recall walks would have two disconnected halves.
	docID, err := d.entityDocumentID(ctx, key, mscope, in.Scope)
	if err != nil {
		return errFrom("remember: "+err.Error(), err), nil
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
	// THE EDGE A DISTILLED FACT GETS. A remembered fact named its subject and was
	// still unreachable from it: the consolidator writes `about` to the subject's
	// node, remember did not. Linked when the scope has a node for the subject;
	// remember does not mint one.
	if target := d.rememberAboutTarget(ctx, key, next.Subject, next.Type); target != "" {
		next.AboutIDs = []string{target}
	}
	return d.upsertChunk(ctx, key, mscope, next)
}

// refuseForgedRemember holds upsert_chunk from a run to the remember grant for
// the two shapes only remember writes (RFC FZ Part C, D5): a key in the
// remembered namespace, and a self-citing piece of source material — class
// evidential, its source_quote its own body, no source_chunk_id — which a judge
// trivially supports and verbatim_answer would quote. A fact distilled from a
// source (a quote that is not the body, or a derived fact from a chat) is left
// alone, as is any person's off-run call.
func refuseForgedRemember(ctx context.Context, in docInput) *tools.Result {
	if tools.IsOffRunPlane(ctx) || contains(tools.PolicyOf(ctx).StatementMethods, statementRememberGrant) {
		return nil
	}
	quote := strings.TrimSpace(in.SourceQuote)
	selfCiting := in.Class == "evidential" && in.SourceChunkID == "" && quote != "" && quote == strings.TrimSpace(in.Body)
	if !strings.HasPrefix(strings.TrimSpace(in.NaturalKey), rememberedKeyPrefix) && !selfCiting {
		return nil
	}
	r := errPermission("upsert_chunk: this fact has the shape of something a person said (a remembered key, or evidential text citing itself), which only remember files, and this agent is not granted remember; record what you derived with a source_quote taken from its source, or propose it with propose_statement",
		"Use a key of your own and the source's quote, or ask an operator to grant remember in the agent's statement_methods.")
	return &r
}

// entityDocumentID resolves (and on first use creates) the scope's entity document.
func (d *Document) entityDocumentID(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, scope string) (string, error) {
	// The runtime chooses this name, not the caller, so it may give the reserved
	// /memory/entities in the tenant scope (RFC FY Part C) — and only this name.
	ctx = context.WithValue(ctx, reservedProvisionKey{}, rememberedFactsPath)
	// The document AT the name, and only that one. A fallback to any document BELOW
	// the name (documentsUnderPath) never found this document — so every call made a
	// new one — and adopted whatever an agent filed at /memory/entities/<anything> as
	// the document every placed fact lands in, leaving the reserved name unregistered.
	if id, err := d.documentAtName(ctx, key, rememberedFactsPath); err != nil {
		return "", err
	} else if id != "" {
		return id, nil
	}
	res, cerr := d.createDocument(ctx, key, mscope, docInput{
		Scope: scope, Title: entitiesDocumentTitle, Path: rememberedFactsPath,
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

// entitiesEnsureKeys are the only fields a create_document at /memory/entities may
// carry from inside a run: the shape of the memory bundle's consolidator call.
func entitiesEnsureKey(k string) bool {
	switch k {
	case "op", "scope", "path", "title":
		return true
	}
	return false
}

// ensureEntitiesDocument answers a run's create_document at the reserved tenant name
// /memory/entities (RFC FY Part C, review of P2).
//
// The memory bundle's consolidator creates that document from inside its run, and the
// name is reserved, so the plain create would be refused and every placed fact's graph
// chunk dropped. The call is therefore read as what it is — an ENSURE of the runtime's
// own empty entities document, the one remember files into — and served by the same
// get-or-create. The runtime supplies the title; a call carrying anything of its own
// (a body, status, type, tags, fields, another title) is refused, because that would be
// an agent authoring the document every placed fact lands in. An existing document
// answers with its id and created:false.
//
// handled is false for every other call, which then meets the ordinary gates.
func (d *Document) ensureEntitiesDocument(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput, raw json.RawMessage) (tools.Result, bool) {
	if in.Op != "create_document" || key.Scope != "tenant" || reservedNameWriter(ctx) || len(raw) == 0 {
		return tools.Result{}, false
	}
	if canonical, err := normalizePath(in.Path); err != nil || canonical != memrank.EntitiesPath {
		return tools.Result{}, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return tools.Result{}, false
	}
	refuse := errPermission("create_document: "+memrank.EntitiesPath+" is a reserved name in the tenant scope; "+
		"a run may only ensure the runtime's own entities document there, so the call may carry op, scope, "+
		"path and title \"Entities\" and nothing else.",
		"Drop the other fields, or file facts with op=remember.")
	for k := range fields {
		if !entitiesEnsureKey(k) {
			return refuse, true
		}
	}
	if t := strings.TrimSpace(in.Title); t != "" && t != entitiesDocumentTitle {
		return refuse, true
	}
	existing, _ := d.documentAtName(ctx, key, rememberedFactsPath)
	docID, err := d.entityDocumentID(ctx, key, mscope, in.Scope)
	if err != nil {
		return errFrom("create_document: "+err.Error(), err), true
	}
	resp := map[string]any{"document_id": docID, "title": entitiesDocumentTitle, "path": memrank.EntitiesPath,
		"created": existing == ""}
	if res, qerr := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ? LIMIT 1`, docID); qerr == nil &&
		len(res.Rows) > 0 && len(res.Rows[0]) > 0 {
		resp["root_chunk_id"] = asStr(res.Rows[0][0])
	}
	res, _ := jsonResult(resp)
	return res, true
}
