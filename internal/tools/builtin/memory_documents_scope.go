package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// documents_scope on Memory op=search and op=recall.
//
// An agent keeps its own memory in one scope and reads its reference corpus in
// another: the board assistant's notes are in user, the board files in tenant.
// documents_scope adds a second leg to ONE call — the documents of that scope,
// searched with the same query and ranked with the call's own results in one
// list, one rerank and one top_k — so the agent does not merge two lists by hand.
//
//   - Equal to scope, it is shorthand for adding documents to sources and nothing
//     else: the call is the one it names, byte for byte.
//   - Different, the primary leg is the call as it stands (its own sources, prefix
//     and window); the document leg is that scope's document prose. The document
//     filters (under_paths …) aim at the document leg. Every hit says which scope
//     it came from.
//
// AUTHORITY. The document leg needs what Document op=search in that scope needs —
// its scope gate, through the Document tool — AND this tool's own memory_scopes
// grant for the scope, because the rows are read through Memory. Either alone is
// less than the other would demand: Document's gate checks no memory_scopes for
// agent and user, and memory_scopes alone does not ask for the second tenant
// grant. A refusal of either refuses the whole call, as a permission error naming
// what is missing; a call never answers silently from one scope. The tenant is
// the run's own, never input.
//
// It reads only. Nothing the leg finds is written anywhere.

// documentsScopeResolver is the Document tool's scope gate for the document leg.
type documentsScopeResolver interface {
	ResolveDocumentsScope(ctx context.Context, op string, scope store.MemoryScope) (scopeID string, err error)
}

// DocumentsLegPlan is a resolved documents_scope that differs from the call's
// scope.
type DocumentsLegPlan struct {
	scope   store.MemoryScope
	scopeID string
	// leg is what the backend searches; nil when the document filter matched no
	// document in that scope, so the leg contributes nothing.
	leg *memrank.DocumentsLeg
	// filter is the document filter's plan, when one was given.
	filter *MemoryDocFilterPlan
}

// echo is the response's `filter` block for the document leg, naming its scope.
func (p *DocumentsLegPlan) echo() MemoryDocFilterEcho {
	e := p.filter.Echo()
	e.Scope = string(p.scope)
	return e
}

// Leg is what the backend searches for the document leg; nil when its filter
// matched no document, so the leg contributes nothing.
func (p *DocumentsLegPlan) Leg() *memrank.DocumentsLeg { return p.leg }

// FilterEcho is the response's `filter` block naming the leg's scope, or nil
// when no document filter was given.
func (p *DocumentsLegPlan) FilterEcho() *MemoryDocFilterEcho {
	if p.filter == nil {
		return nil
	}
	e := p.echo()
	return &e
}

// foldEqualDocumentsScope applies documents_scope equal to scope: it adds
// documents to the call's sources and clears the argument, so the call that runs
// is exactly the one with documents in sources. With no sources, a search already
// returns documents (and is left alone); a recall's default is facts and notes,
// so it becomes facts, notes and documents. Any other value is left for
// planDocumentsLeg.
func foldEqualDocumentsScope(in *memoryInput) {
	if in.DocumentsScope == "" || in.DocumentsScope != in.Scope {
		return
	}
	in.DocumentsScope = ""
	if len(in.Sources) == 0 {
		if in.Op == "recall" {
			in.Sources = []string{string(memrank.SourceFacts), string(memrank.SourceNotes), string(memrank.SourceDocuments)}
		}
		return
	}
	for _, s := range in.Sources {
		if strings.EqualFold(strings.TrimSpace(s), string(memrank.SourceDocuments)) {
			return
		}
	}
	in.Sources = append(append([]string(nil), in.Sources...), string(memrank.SourceDocuments))
}

// planDocumentsLeg resolves a documents_scope that differs from the call's scope.
// sources is the primary leg's parsed selector. A refusal comes back as the
// *tools.Result.
func (m *Memory) planDocumentsLeg(ctx context.Context, op string, in memoryInput, sources []memrank.Source) (*DocumentsLegPlan, *tools.Result) {
	switch store.MemoryScope(in.DocumentsScope) {
	case store.MemoryScopeAgent, store.MemoryScopeUser, store.MemoryScopeTenant:
	default:
		r := errValidation(fmt.Sprintf("%s: unknown documents_scope %q (want one of: agent, user, tenant)", op, in.DocumentsScope),
			"Pass documents_scope as agent, user or tenant, or leave it out.")
		return nil, &r
	}
	for _, s := range sources {
		if s == memrank.SourceTraces {
			r := errValidation(op+": documents_scope cannot be combined with sources [traces] — conversation turns are searched on their own",
				"Search traces without documents_scope, or drop traces from sources.")
			return nil, &r
		}
	}
	// The leg is served by this server's own memory: a backend that serves the
	// search from elsewhere would search one scope and ignore the other, and the
	// caller would read a one-scope answer as a two-scope one.
	if _, local := m.backend(ctx).(*inprocess.Backend); !local {
		r := errBusiness(op+": documents_scope applies only to this server's own memory, and this agent's memory is served by another backend",
			"Search without documents_scope, or make a second call with scope set to the documents' scope.")
		return nil, &r
	}
	// This tool's own grant for the scope: memory_scopes, the isolated-member
	// confinement, and the agent name or user id the scope is keyed by.
	legScope, legScopeID, err := m.resolveScope(ctx, in.DocumentsScope)
	if err != nil {
		r := memErr(op+": documents_scope: "+err.Error(), err)
		return nil, &r
	}
	// And Document op=search's gate for it. Read HERE, at use: boot assigns the
	// resolver before it is fully bound.
	return PlanDocumentsLeg(ctx, m.DocumentFilters, op, legScope, legScopeID, in.documentFilter())
}

// PlanDocumentsLeg resolves the document leg of (scope, scopeID) through the
// Document tool's gate for that scope — Document op=search's grants, isolated
// confinement and subject — and, when one is given, its document filter. It is
// the part of a documents_scope the Memory tool and the off-run
// POST /v1/_memory/search share; each checks its own authority for the scope
// first (the tool its memory_scopes grant, the endpoint its caller's token).
// resolver is the Document tool. A refusal comes back as the *tools.Result.
func PlanDocumentsLeg(ctx context.Context, resolver memrank.DocumentFilterResolver, op string,
	scope store.MemoryScope, scopeID string, df memrank.DocumentFilter) (*DocumentsLegPlan, *tools.Result) {
	scoped, ok := resolver.(documentsScopeResolver)
	if !ok || scoped == nil {
		r := errBusiness(op+": documents are not configured on this server, so documents_scope cannot be searched",
			"Search without documents_scope.")
		return nil, &r
	}
	docScopeID, err := scoped.ResolveDocumentsScope(ctx, op, scope)
	if err != nil {
		var rf *documentFilterRefusal
		if errors.As(err, &rf) {
			return nil, &rf.res
		}
		r := errFrom(op+": documents_scope: "+err.Error(), err)
		return nil, &r
	}
	if docScopeID != scopeID {
		// Both sides derive the id from the same identity; a mismatch means
		// the two would read different rows, so nothing is searched.
		err := fmt.Errorf("documents_scope: memory scope %s/%q is not document scope %s/%q", scope, scopeID, scope, docScopeID)
		r := errFrom(op+": "+err.Error(), err)
		return nil, &r
	}
	plan := &DocumentsLegPlan{scope: scope, scopeID: scopeID,
		leg: &memrank.DocumentsLeg{Scope: scope, ScopeID: scopeID}}
	if !df.IsZero() {
		fp, refusal := PlanMemoryDocumentFilter(ctx, resolver, op, scope, scopeID, df,
			memrank.SearchQuery{Sources: []memrank.Source{memrank.SourceDocuments}}, "the documents_scope leg")
		if refusal != nil {
			return nil, refusal
		}
		plan.filter = fp
		if fp.noDocuments {
			plan.leg = nil
		} else {
			plan.leg.IDs, plan.leg.ExcludeIDs = fp.ids, fp.excludeIDs
		}
	}
	return plan, nil
}

// scopeAt is the scope of result i: the backend's report when the leg ran, else
// the call's own scope (a leg that contributed nothing leaves every hit there).
func scopeAt(scopes []store.MemoryScope, i int, own store.MemoryScope) store.MemoryScope {
	if i < len(scopes) && scopes[i] != "" {
		return scopes[i]
	}
	return own
}

// inScope selects the results that came from scope want, for a step applied per
// scope; nil — every result — for a call without a documents leg.
func inScope(legPlan *DocumentsLegPlan, scopes []store.MemoryScope, own, want store.MemoryScope) func(int) bool {
	if legPlan == nil {
		return nil
	}
	return func(i int) bool { return scopeAt(scopes, i, own) == want }
}
