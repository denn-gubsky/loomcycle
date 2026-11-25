package builtin

// document_ontology_plan_proposal.go — planning an open proposal by its chunk id (RFC GQ
// P4.3): {"op": "proposal", "id": <chunk id>} in a plan's changes.
//
// A CHANGE OR RETIRE PROPOSAL plans as the direct changes accepting it makes. Those are
// found exactly as the accept finds them — checkChangeAccept rebuilds the change from the
// mark, re-runs filing's checks against the ontology as it stands, refuses a mark that is
// not the change they rebuild, and composeChangeAccept turns it into retire / rename /
// edit / move — so the plan an operator confirms is the plan the accept's own gate
// computes, with the same fingerprint. A proposal that would be refused at accept is
// refused here too (invalid_request), rather than planned as something it cannot become.
//
// AN ADD PROPOSAL (a proposed class, relation, concept, scheme or instance, with no
// change mark) plans as op add, risk none: accepting it puts a new entry in force, and
// nothing stored rests on an entry that is not yet in force. It is not put in the
// overlay: the issues a plan reports are those of the ontology the direct changes leave.
//
// Proposals are read from the caller's tenant ontology only (the scope key the plan
// resolved), and anything that is not an OPEN proposal there — an unknown id, an entry
// in force, a rejected or accepted proposal, a chunk of another document, another
// tenant's proposal — is one refusal with one message shape (entry_not_found), so a
// caller cannot tell another tenant's proposal from an unknown id.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// planItem is one change of a plan after proposals are expanded: a direct change to
// resolve (change, at the request's index), or a planned change already final (fixed:
// an add proposal, or a change proposal that changes nothing).
type planItem struct {
	index    int
	change   connector.OntologyChange
	proposal string
	fixed    *connector.OntologyPlannedChange
}

// expandPlanProposals replaces each proposal of changes with what planning it means, in
// place; direct changes pass through. The expanded request is bounded as a direct one
// is, so a set of proposals does no more counting work than ontologyPlanMaxChanges.
func (d *Document) expandPlanProposals(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope,
	changes []connector.OntologyChange, before OntologyRead) ([]planItem, error) {
	items := make([]planItem, 0, len(changes))
	direct := 0
	for i, ch := range changes {
		if ch.Op != connector.OntologyPlanProposal {
			items = append(items, planItem{index: i, change: ch})
			direct++
			continue
		}
		id := strings.TrimSpace(ch.ID)
		expanded, fixed, err := d.planProposal(ctx, key, mscope, i, id, before)
		if err != nil {
			return nil, err
		}
		if fixed != nil {
			items = append(items, planItem{index: i, proposal: id, fixed: fixed})
			continue
		}
		for _, c := range expanded {
			items = append(items, planItem{index: i, change: c, proposal: id})
			direct++
		}
	}
	if direct > ontologyPlanMaxChanges {
		return nil, connector.WithMessage(connector.ErrOntologyPlanInvalid,
			"the proposals make "+strconv.Itoa(direct)+" changes in all; a plan takes at most "+strconv.Itoa(ontologyPlanMaxChanges))
	}
	return items, nil
}

// planProposal is what planning proposal id (changes[i]) means: the direct changes
// accepting it makes, or a planned change already final.
func (d *Document) planProposal(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, i int, id string,
	before OntologyRead) ([]connector.OntologyChange, *connector.OntologyPlannedChange, error) {
	at := "changes[" + strconv.Itoa(i) + "]"
	notFound := connector.WithMessage(connector.ErrOntologyPlanNotFound, at+": "+strconv.Quote(id)+" is not an open proposal of the tenant ontology")
	var listed *memrank.OntologyProposal
	for k := range before.Proposals {
		if p := &before.Proposals[k]; p.ChunkID == id && p.Status == memrank.OntologyStatusProposed {
			listed = p
			break
		}
	}
	if listed == nil {
		return nil, nil, notFound
	}
	self, ok, err := d.getChunkRow(ctx, key, id)
	if err != nil {
		return nil, nil, planReadErr(err)
	}
	if !ok {
		return nil, nil, notFound
	}
	b, err := d.readBody(ctx, mscope, key, id)
	if err != nil {
		return nil, nil, planReadErr(err)
	}
	// The mark is looked up on the chunk itself, not on the read's decoded view: a
	// change_proposal mark that no longer decodes is a broken change proposal, never an
	// add proposal planned as risk none.
	var marks map[string]json.RawMessage
	if len(b.Fields) > 0 && json.Unmarshal(b.Fields, &marks) == nil {
		if raw, marked := marks[changeProposalField]; marked {
			return d.planChangeProposal(ctx, key, mscope, at, self, raw)
		}
		// An add carrying structured data is planned as an add, once its mark passes the
		// accept's checks: one the accept would refuse is refused here too.
		if raw, marked := marks[entryProposalField]; marked {
			if _, err := checkEntryAccept(before, self, raw); err != nil {
				return nil, nil, connector.WithMessage(connector.ErrOntologyPlanInvalid,
					at+": proposal "+strconv.Quote(self.ID)+" cannot be accepted as it stands: "+err.Error())
			}
		}
	}
	return nil, addProposalPlan(*listed), nil
}

// planChangeProposal is a change or retire proposal's direct changes, as the accept
// composes them. A proposal the accept would refuse is refused (invalid_request) with
// the accept's reason; a store fault is returned as it is, so the transport hides it.
func (d *Document) planChangeProposal(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, at string,
	self chunkRow, raw json.RawMessage) ([]connector.OntologyChange, *connector.OntologyPlannedChange, error) {
	a, err := d.checkChangeAccept(ctx, key, mscope, self, raw)
	if err != nil {
		if errors.Is(err, ErrChangeProposalInvalid) || errors.Is(err, ErrChangeEntryLocked) {
			return nil, nil, connector.WithMessage(connector.ErrOntologyPlanInvalid,
				at+": proposal "+strconv.Quote(self.ID)+" cannot be accepted as it stands: "+err.Error())
		}
		return nil, nil, planReadErr(err)
	}
	if len(a.changes) == 0 {
		return nil, &connector.OntologyPlannedChange{Op: a.mark.Op, Entry: a.tg.entry.OntologyPlanEntry,
			Risk: connector.OntologyRiskNone, Counts: map[string]connector.OntologyPlanCount{}, Proposal: self.ID,
			Notes: []string{"accepting it changes nothing in the entry; only the proposal is removed"}}, nil
	}
	return a.changes, nil, nil
}

// proposalResolveErr is the plan's refusal of a change a proposal stands for. The
// proposal's checks ran on their own read of the ontology, the change is resolved
// against the plan's earlier one, and an edit between the two can make it refuse the
// entry or a move's parent. For a proposal item that is invalid_request naming the
// proposal, as the accept would refuse it: entry_not_found means only "not an open
// proposal". A direct change's refusal is returned as it is.
func proposalResolveErr(it planItem, err error) error {
	if it.proposal == "" || !(errors.Is(err, connector.ErrOntologyPlanNotFound) || errors.Is(err, connector.ErrOntologyPlanInvalid)) {
		return err
	}
	return connector.WithMessage(connector.ErrOntologyPlanInvalid, "changes["+strconv.Itoa(it.index)+"]: proposal "+
		strconv.Quote(it.proposal)+" cannot be accepted as it stands: "+err.Error())
}

// addProposalPlan is an add proposal's plan: a new entry, which nothing rests on.
func addProposalPlan(p memrank.OntologyProposal) *connector.OntologyPlannedChange {
	section := p.Section
	if section == "" {
		section = memrank.SectionClasses
	}
	kind := planKindClass
	switch section {
	case memrank.SectionRelations:
		kind = planKindRelation
	case memrank.SectionInstances:
		kind = planKindInstance
	case memrank.SectionSchemes:
		kind = planKindConcept
		if p.Scheme != nil {
			kind = planKindScheme
		}
	}
	return &connector.OntologyPlannedChange{Op: connector.OntologyPlanAdd,
		Entry: connector.OntologyPlanEntry{ChunkID: p.ChunkID, Name: p.Name, Section: section, Kind: kind},
		Risk:  connector.OntologyRiskNone, Counts: map[string]connector.OntologyPlanCount{}, Proposal: p.ChunkID,
		Notes: []string{"accepting it puts a new entry in force; nothing stored rests on it yet"}}
}
