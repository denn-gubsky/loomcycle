package builtin

// document_ontology_entry_proposal.go — a class or relation ADD proposal that carries
// structured data (RFC GQ P6.3 and P6.4; plan 5.4; D12, D15). propose_entity without
// change, given any of meaning, directives, evidence or signal.
//
// THE SHAPE is the change proposal's (#2729, RFC GQ 9b C5): one proposed chunk, filed
// where a plain add is filed, its title the new entry's name and its body the filer's
// evidence with every directive line quoted. What accepting would put in force travels
// as STRUCTURED, INERT data in the chunk's fields under a fourth mark beside
// scheme_proposal, concept_proposal and change_proposal:
//
//	{"entry_proposal": memrank.ProposedEntry{kind, signal?, meaning?, directives?,
//	                                         evidence?, plan, filed_by?}}
//
// A plain add proposal (none of the four fields) is filed exactly as before, with no
// mark.
//
// DIRECTIVES are limited to what a new entry may safely carry before an operator has
// read it: a relation's domain and range, nothing for a class. The plan names this the
// run's allow-list; it is applied to EVERY caller here, operator plane included, so a
// mark can never carry more than it and the accept can re-check it without trusting
// who filed it. locked, acl, retention, memory_scope, auto_assert and identity are
// refused by name; an operator adds them by editing the entry after accepting it.
//
// THE CAP (D15, P6.4): a run already holding entryProposalCap open entry proposals
// that carry its run id is refused with proposal_cap. Only marked proposals carry a
// run id, so only they count; an off-run caller is not capped.
//
// THE PLAN AT FILING is computed here, never taken from the caller: the ontology's
// fingerprint and the tenant-scope rows that already use the name. Only its risk is
// returned to the caller. User and agent scopes are not walked at filing (that walk
// has a 20-second budget, too much for a tool call); the console plans the proposal
// again, live, by its id.
//
// ACCEPTING (AcceptEntryProposal, from the operator's proposals route) re-checks the
// stored mark under the per-tenant accept lock and refuses one that does not pass or is
// not what filing would have stored (ErrEntryProposalInvalid, 422 on the route). The
// entry's body becomes the checked directive lines above the quoted meaning — never the
// proposal's evidence text — the mark leaves its fields and its status clears, in one
// update at the revision read. The chunk keeps its id.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	entryProposalField = "entry_proposal"
	// entryProposalCap is D15's ten proposals per pass, held per run.
	entryProposalCap = 10
	// entryEvidenceMax is the five pieces of evidence RFC GQ 5.2 allows a proposal.
	entryEvidenceMax = 5
	proposalCapCode  = "proposal_cap"
)

// entrySignals are the building signals of RFC GQ 5.1 an add proposal can answer.
var entrySignals = map[string]bool{"undeclared_type": true, "undeclared_kind": true, "invalid_ends": true}

// entryDirectives are the directives an add proposal may carry, per kind of entry.
var entryDirectives = map[string]map[string]bool{
	planKindClass:    {},
	planKindRelation: directiveSet("domain", "range"),
}

// ErrEntryProposalInvalid: a stored entry proposal fails its checks now, or its mark is
// not what filing would store. Nothing was written.
var ErrEntryProposalInvalid = errors.New("invalid_proposal")

// wantsStructuredAdd reports an add proposal carrying structured data.
func wantsStructuredAdd(in docInput) bool {
	return in.Meaning != nil || directivesGiven(in) || len(in.Evidence) > 0 || strings.TrimSpace(in.Signal) != ""
}

// entryData checks an add proposal's signal, meaning and directives and returns them as
// the mark stores them. Filing and accepting both run it. The evidence is filing's own
// check: it is shown to the reviewer, never written to the entry.
func entryData(read OntologyRead, kind, name, signal string, meaning *string, rawDirs json.RawMessage) (memrank.ProposedEntry, *tools.Result) {
	mark := memrank.ProposedEntry{Kind: kind, Signal: strings.TrimSpace(signal)}
	refuse := func(msg, fix string) (memrank.ProposedEntry, *tools.Result) {
		r := errValidation("propose_entity: "+msg, fix)
		return mark, &r
	}
	if mark.Signal != "" && !entrySignals[mark.Signal] {
		return refuse("signal "+strconv.Quote(mark.Signal)+" is not a building signal", "Pass undeclared_type, undeclared_kind or invalid_ends, or omit it.")
	}
	if meaning != nil {
		m := strings.TrimSpace(*meaning)
		if m == "" {
			return refuse("meaning is empty; omit it, or say what the new entry means", "")
		}
		if len(m) > proposalBodyMax {
			return refuse(fmt.Sprintf("meaning is %d bytes, over the %d-byte limit", len(m), proposalBodyMax), "")
		}
		mark.Meaning = neutraliseDirectives(m)
	}
	if len(rawDirs) == 0 || string(rawDirs) == "null" {
		return mark, nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(rawDirs, &m) != nil {
		return refuse("directives: not an object", "Pass an object of directive name to value.")
	}
	allowed := entryDirectives[kind]
	var named []string
	for raw := range m {
		if n := strings.TrimPrefix(strings.TrimSpace(raw), "@"); !allowed[n] {
			named = append(named, "@"+n)
		}
	}
	if len(named) > 0 {
		sort.Strings(named)
		have := "none"
		if len(allowed) > 0 {
			have = strings.Join(directiveNames(allowed), ", ")
		}
		return refuse("directives: "+strings.Join(named, ", ")+" may not be set by an add proposal (a new "+kind+
			" may carry: "+have+")", "Omit it; an operator adds it to the entry after accepting.")
	}
	dirs, r := checkChangeDirectives(kind, name, rawDirs)
	if r != nil {
		return mark, r
	}
	// A new relation's domain or range may not name a retired class (RFC FO 3.1a), as
	// an edit's may not.
	tg := changeTarget{read: read, entry: planEntry{OntologyPlanEntry: connector.OntologyPlanEntry{Name: name, Kind: kind}}}
	if r := tg.retiredReference(dirs); r != nil {
		return mark, r
	}
	mark.Directives = dirs
	return mark, nil
}

// fileEntryProposal files a class or relation add proposal with its structured data,
// under parentID (where a plain add of the kind is filed). The caller has run the add's
// own checks (the name, duplicates, the parent) on read, under the proposal lock.
func (d *Document) fileEntryProposal(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, read OntologyRead,
	kind, parentID, name string, in docInput) (tools.Result, error) {
	mark, r := entryData(read, kind, name, in.Signal, in.Meaning, in.Directives)
	if r != nil {
		return *r, nil
	}
	if len(in.Evidence) > entryEvidenceMax {
		return errValidation(fmt.Sprintf("propose_entity: at most %d pieces of evidence", entryEvidenceMax),
			"Keep the clearest few."), nil
	}
	for i, ev := range in.Evidence {
		if strings.TrimSpace(ev.ChunkID) == "" {
			return errValidation(fmt.Sprintf("propose_entity: evidence[%d]: an add proposal cites a chunk of the tenant scope by chunk_id", i),
				"Quote a chunk of the tenant scope; a chat turn or another scope is not taken here."), nil
		}
	}
	checked, rr, err := d.checkEvidence(ctx, key, mscope, in.Evidence)
	if err != nil {
		return errFrom("propose_entity: "+err.Error(), err), nil
	}
	if rr != nil {
		rr.Text = "propose_entity: " + rr.Text
		return *rr, nil
	}
	for _, c := range checked {
		mark.Evidence = append(mark.Evidence, memrank.ProposedEvidence{ChunkID: c.ChunkID, Part: c.Part, Quote: c.Quote})
	}
	if runID := tools.RunID(ctx); runID != "" {
		open := 0
		for _, p := range read.Proposals {
			if p.Status == memrank.OntologyStatusProposed && p.Entry != nil && p.Entry.FiledBy != nil && p.Entry.FiledBy.RunID == runID {
				open++
			}
		}
		if open >= entryProposalCap {
			return errBusiness(fmt.Sprintf("propose_entity: %s: this run already has %d open proposals, the most one run may file",
				proposalCapCode, open), "Stop here; an operator reviews them before the next pass."), nil
		}
		mark.FiledBy = &memrank.ProposalFiler{Agent: tools.AgentName(ctx), RunID: runID}
	}
	if mark.Plan, err = d.entryProposalPlan(ctx, key, read, kind, name); err != nil {
		return errFrom("propose_entity: "+err.Error(), err), nil
	}
	fields, _ := json.Marshal(map[string]any{entryProposalField: mark})
	id, err := d.createProposalChunk(ctx, key, mscope, read.DocumentID, parentID, name, in.Body, fields)
	if err != nil {
		return errFrom("propose_entity: "+err.Error(), err), nil
	}
	return jsonResult(map[string]any{
		"proposed":  name,
		"chunk_id":  id,
		"parent_id": parentID,
		"risk":      mark.Plan.Risk,
		"note": "Filed as a suggestion, not in force. An operator accepts or rejects it in " +
			"the Ontology page; nothing changes for any run until they do.",
	})
}

// entryProposalPlan is the plan stored with an entry proposal: the ontology's
// fingerprint and the tenant-scope rows that already use the name (see the file
// comment). Reads only, and never provisions.
func (d *Document) entryProposalPlan(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, kind, name string) (*memrank.ProposedEntryPlan, error) {
	rctx := noWrites(ctx)
	fp, err := d.ontologyPlanFingerprint(rctx, key, read.DocumentID)
	if err != nil {
		return nil, err
	}
	p := &plannedChange{counts: map[string]*connector.OntologyPlanCount{}}
	if kind == planKindClass {
		p.classes = []string{name}
	} else {
		p.kinds = []string{name}
	}
	if err := d.countPlanNames(rctx, key, read.DocumentID, true, p); err != nil {
		return nil, err
	}
	would := make(map[string]int, len(p.counts))
	for n, c := range p.counts {
		would[n] = c.N
	}
	return &memrank.ProposedEntryPlan{Fingerprint: fp, Risk: connector.OntologyRiskNone, WouldDeclare: would}, nil
}

// withEntryMarks sets Entry on each proposal carrying an entry_proposal mark that
// decodes, read from its chunk's fields.
func withEntryMarks(ps []memrank.OntologyProposal, chunkOf func(string) chunkBody) []memrank.OntologyProposal {
	for i := range ps {
		var e memrank.ProposedEntry
		if proposalMark(chunkOf(ps[i].ChunkID).Fields, entryProposalField, &e) {
			ps[i].Entry = &e
		}
	}
	return ps
}

// AcceptEntryProposal accepts a class or relation add proposal carrying an
// entry_proposal mark (see the file comment). handled is false for a chunk with no such
// mark, which the caller accepts as before. Refusals wrap ErrEntryProposalInvalid, or
// ErrEntryRetired when a directive names an entry retired since filing; nothing is
// written for either.
func (d *Document) AcceptEntryProposal(ctx context.Context, scope, chunkID string) (handled bool, err error) {
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return false, err
	}
	// The scheme and change accepts' lock: one accept at a time per tenant ontology.
	// Within this process; the revision the update carries guards across replicas.
	unlock, err := d.editions.lock(ctx, key, "ontology_accept", memrank.OntologyPath)
	if err != nil {
		return false, err
	}
	defer unlock()
	self, ok, err := d.getChunkRow(ctx, key, chunkID)
	if err != nil || !ok {
		return false, err
	}
	b, err := d.readBody(ctx, mscope, key, chunkID)
	if err != nil {
		return false, err
	}
	var marks map[string]json.RawMessage
	if len(b.Fields) == 0 || json.Unmarshal(b.Fields, &marks) != nil {
		return false, nil
	}
	raw, marked := marks[entryProposalField]
	if !marked {
		return false, nil
	}
	read, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath, nil)
	if err != nil {
		return true, err
	}
	body, err := checkEntryAccept(read, self, raw)
	if err != nil {
		return true, err
	}
	r, err := d.execOpResult(ctx, map[string]any{"op": "update_chunk", "scope": "tenant", "id": chunkID,
		"revision": self.Revision, "body": body, "fields": withoutMark(b.Fields, entryProposalField), "status": ""})
	if err != nil {
		return true, err
	}
	if r.IsError {
		if r.Error != nil && (r.Error.Category == tools.CategoryBusiness || r.Error.Category == tools.CategoryValidation) {
			return true, fmt.Errorf("%w: %s", ErrEntryProposalInvalid, r.Text)
		}
		return true, errors.New(r.Text)
	}
	return true, nil
}

// checkEntryAccept re-checks a stored entry proposal against the ontology as it reads
// now and returns the entry's new body: its directive lines, then its quoted meaning.
func checkEntryAccept(read OntologyRead, self chunkRow, raw json.RawMessage) (string, error) {
	invalid := func(why string) (string, error) {
		return "", fmt.Errorf("%w: %s", ErrEntryProposalInvalid, why)
	}
	if strings.ToLower(strings.TrimSpace(self.Status)) != memrank.OntologyStatusProposed {
		return invalid("it is not an open proposal")
	}
	if self.DocumentID != read.DocumentID {
		return invalid("it is not in this tenant's ontology")
	}
	var stored memrank.ProposedEntry
	if json.Unmarshal(raw, &stored) != nil {
		return invalid("its entry_proposal mark does not decode")
	}
	var listed *memrank.OntologyProposal
	for i := range read.Proposals {
		if read.Proposals[i].ChunkID == self.ID {
			listed = &read.Proposals[i]
		}
	}
	if listed == nil {
		return invalid("it is not listed as a proposal of this tenant's ontology")
	}
	kind := planKindClass
	switch listed.Section {
	case "", memrank.SectionClasses:
	case memrank.SectionRelations:
		kind = planKindRelation
	default:
		return invalid("an entry_proposal mark belongs on a class or relation proposal, not one in the " + listed.Section + " section")
	}
	if stored.Kind != kind {
		return invalid("its mark says " + strconv.Quote(stored.Kind) + " but it is filed as a " + kind)
	}
	if len(stored.Evidence) > entryEvidenceMax {
		return invalid(fmt.Sprintf("it carries more than %d pieces of evidence", entryEvidenceMax))
	}
	var meaning *string
	if stored.Meaning != "" {
		m := stored.Meaning
		meaning = &m
	}
	var rawDirs json.RawMessage
	if len(stored.Directives) > 0 {
		in, err := changeInputOf(memrank.ProposedChange{SetsDirectives: true, Directives: stored.Directives})
		if err != nil {
			return invalid(err.Error())
		}
		rawDirs = in.Directives
	}
	rebuilt, r := entryData(read, kind, listed.Name, stored.Signal, meaning, rawDirs)
	if r != nil {
		why := strings.TrimPrefix(r.Text, "propose_entity: ")
		if strings.Contains(why, "entry_retired:") {
			return "", fmt.Errorf("%w: %s", ErrEntryRetired, why)
		}
		return invalid(why)
	}
	if rebuilt.Signal != stored.Signal || rebuilt.Meaning != stored.Meaning ||
		!reflect.DeepEqual(rebuilt.Directives, stored.Directives) {
		return invalid("its mark is not what filing stores (edited after filing); reject it and propose again")
	}
	return composeChangeBody(kind, "", memrank.ProposedChange{Meaning: rebuilt.Meaning, SetsDirectives: true,
		Directives: rebuilt.Directives}), nil
}
