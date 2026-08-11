// ontology.go — SERVER side of the layered entity-tier ontology (RFC BL P4c PR 3).
//
// The pure layering + seed live in internal/memory; this file provisions the
// tenant's ontology document, reads its terms back, and composes the effective
// ontology for {{memory:ontology}}.
//
// PROVISIONING IS LAZY-ON-FIRST-REFERENCE, not eager at tenant creation, and that
// is a deliberate departure from how it was specified.
//
// There IS no tenant-creation event to hang it on: a tenant exists the moment a
// token names one, and nothing in the runtime observes that transition. Hooking
// token mint would have covered tenants minted from then on and MISSED every
// tenant that already exists — including, on the reference deployment, the only
// one. Lazy provisioning covers both, and it reuses the race-safety already proven
// on the user-root path: an in-process singleflight collapses a concurrent burst,
// and a PG advisory lock admits one replica in a cluster.
//
// The operator-visible behaviour is the same either way — the document is there
// the first time anything looks — and until its status is `confirmed` it changes
// nothing, so provisioning early buys no correctness.
package http

import (
	"context"
	"encoding/json"
	"strings"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// renderOntology composes the {{memory:ontology}} body for a run: the base seed
// layered with the tenant's terms when — and only when — the tenant has confirmed
// them.
//
// Best-effort throughout. No SQL Memory, no tenant, an unreadable document: the
// run continues on the base seed rather than failing, because an ontology is
// guidance and a run that dies for want of it is worse than a run using the
// standard types.
func (s *Server) renderOntology(ctx context.Context, mi memInject) string {
	terms, confirmed, _ := s.tenantOntologyTerms(ctx, mi)
	return meminject.RenderOntology(meminject.EffectiveOntology(terms, confirmed), confirmed)
}

// tenantOntologyTerms reads the tenant's ontology document, provisioning it on
// first reference, and reports its terms plus whether the operator has confirmed
// them.
//
// A tenant layer is returned ONLY alongside confirmed=true; an unconfirmed
// document's terms are still returned so a caller can show them, but
// EffectiveOntology discards them. Keeping the discard in one place means no caller
// can accidentally honour a draft.
func (s *Server) tenantOntologyTerms(ctx context.Context, mi memInject) (terms []meminject.OntologyTerm, confirmed bool, note string) {
	if s.store == nil || s.sqlMem == nil {
		return nil, false, ""
	}
	s.ensureOntologyDoc(ctx, mi)

	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.docToolCtx(ctx, mi)
	// The ontology lives at TENANT scope, so the read has to be granted it. This is
	// a server-side read of operator-authored config on the run's own tenant, not an
	// agent reaching tenant memory, so the policy is stamped here rather than
	// requiring every agent that renders the placeholder to hold the tenant grant.
	dctx = tools.WithMemoryPolicy(dctx, tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}})
	dctx = tools.WithSqlMemPolicy(dctx, tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}})

	// The GATE is the root chunk's status, which get_document reports directly.
	// (It does NOT return a chunk list — an earlier draft of this read expected one
	// and silently found zero terms, which the wiring test caught.)
	req, _ := json.Marshal(map[string]any{
		"op": "get_document", "scope": "tenant", "path": meminject.OntologyPath,
	})
	res, _ := doc.Execute(dctx, req)
	if res.IsError {
		return nil, false, ""
	}
	var meta struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(res.Text), &meta); err != nil {
		return nil, false, ""
	}
	confirmed = strings.EqualFold(strings.TrimSpace(meta.Status), meminject.OntologyConfirmedStatus)

	// The TERMS come from the CHUNK TREE, not from the exported Markdown.
	//
	// The export flattens chunk depth into "#" repetition, which makes a subclass's
	// title byte-identical to a heading inside a body — so the tree read is not an
	// optimisation, it is the only way to tell a nested TYPE from a nested COMMENT.
	// It also ends a silent data loss: the markdown parser matched only "## ", so
	// every nested type an operator wrote was dropped without a word.
	tree, terr := doc.OntologyTermsFromTree(dctx, "tenant", meminject.OntologyPath)
	if terr == nil && len(tree) > 0 {
		return tree, confirmed, ""
	}

	// FALLBACK, deliberately narrow: only when the tree yielded NOTHING.
	//
	// A document whose entities were typed into one chunk's body — the root's, say,
	// replacing the provisioned structure — has headings but no child chunks, and the
	// tree read would correctly report zero while the operator sees a full document.
	// Emptying someone's ontology on upgrade is a worse failure than carrying a second
	// reader for one case, so the flat parse still runs, and it reports itself: a
	// silent fallback would leave the operator's hierarchy quietly inert, which is the
	// exact bug being fixed here wearing a different hat.
	exp, _ := json.Marshal(map[string]any{
		"op": "export_md", "scope": "tenant", "path": meminject.OntologyPath,
		"include_metadata": false,
	})
	eres, _ := doc.Execute(dctx, exp)
	if eres.IsError {
		return nil, confirmed, ""
	}
	var body struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal([]byte(eres.Text), &body); err != nil {
		return nil, confirmed, ""
	}
	flat := meminject.ParseOntologyMarkdown(body.Markdown)
	if len(flat) == 0 {
		return nil, confirmed, ""
	}
	return flat, confirmed, "This ontology was read as flat Markdown because the " +
		"document has no per-entity chunks. Split each entity into its own chunk to " +
		"use subclasses — a child chunk is a subclass of its parent."
}

// ensureOntologyDoc provisions the tenant's ontology document from the embedded
// template on first reference. Mirrors ensureUserRootDoc's race safety: a
// singleflight per tenant collapses a concurrent burst, and a PG advisory lock
// admits one replica in a cluster.
//
// Best-effort and idempotent — a failure leaves nothing cached, so the next
// reference retries.
func (s *Server) ensureOntologyDoc(ctx context.Context, mi memInject) {
	if s.store == nil || s.sqlMem == nil {
		return
	}
	_, _, _ = s.userRootProvisionSF.Do("ontology\x00"+mi.Tenant, func() (any, error) {
		s.provisionOntologyDoc(ctx, mi)
		return nil, nil
	})
}

func (s *Server) provisionOntologyDoc(ctx context.Context, mi memInject) {
	if s.sessionLockPG != nil {
		// NUL-free lock id (pg text params reject 0x00). Keyed on the tenant alone:
		// the ontology is one document per tenant, unlike the per-principal user root.
		release, ok := s.sessionLockPG.TryLock(ctx, "memory:ontology:"+mi.Tenant)
		if !ok {
			return
		}
		defer release()
	}

	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.docToolCtx(ctx, mi)
	dctx = tools.WithMemoryPolicy(dctx, tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}})
	dctx = tools.WithSqlMemPolicy(dctx, tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}})

	probe, _ := json.Marshal(map[string]any{
		"op": "get_document", "scope": "tenant", "path": meminject.OntologyPath,
	})
	if res, _ := doc.Execute(dctx, probe); !res.IsError {
		return // already provisioned, here or by another replica
	}

	create, _ := json.Marshal(map[string]any{
		"op": "import_md", "scope": "tenant", "path": meminject.OntologyPath,
		"markdown": meminject.OntologyTemplate(),
	})
	res, _ := doc.Execute(dctx, create)
	if res.IsError {
		return // best-effort; the next reference retries
	}
	// Stamp the root chunk `draft`. import_md leaves status unset, and an unset
	// status is NOT confirmed — so the gate already holds — but writing it makes the
	// state legible to an operator opening the document, who would otherwise have to
	// know that blank means draft.
	var created struct {
		RootChunkID string `json:"root_chunk_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &created); err != nil || created.RootChunkID == "" {
		return
	}
	stamp, _ := json.Marshal(map[string]any{
		"op": "update_chunk", "scope": "tenant", "id": created.RootChunkID,
		"status": meminject.OntologyDraftStatus, "revision": 1,
	})
	_, _ = doc.Execute(dctx, stamp)
}
