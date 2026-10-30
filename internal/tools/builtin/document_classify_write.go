package builtin

// document_classify_write.go — classify's neighbourhood vote, its writes, and
// accept_classification (RFC FO P5a). See document_classify.go.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// classifyVote is step 2: the classes and concepts of the document's neighbours.
//
// Neighbours are the documents `related` finds from the first section, inside the
// document's own Path folder, that the caller may read and that already carry a declared
// document class. Unreadable ones are dropped before anything is counted, so a vote
// never tells a confined caller what a document it cannot see is about.
func (d *Document) classifyVote(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, acl *docACL,
	read OntologyRead, classes map[string]memrank.OntologyTerm, cd classifyDoc, tenantScope bool) (classifyStep, []classifyConcept, error) {
	step := classifyStep{Step: "vote", Candidates: []classifyCandidate{}}
	concepts := []classifyConcept{}
	if d.Embedder == nil {
		step.Reason = "no embedder is configured, so there are no neighbours to ask"
		return step, concepts, nil
	}
	folder := ""
	if paths := d.documentPaths(ctx, key)[cd.id]; len(paths) > 0 {
		if i := strings.LastIndex(paths[0], "/"); i > 0 {
			folder = paths[0][:i]
		}
	}
	var neighbourDocs []string
	seen := map[string]bool{cd.id: true}
	for _, seed := range cd.sectionIDs {
		res, err := d.related(ctx, key, mscope, docInput{ID: seed, UnderPath: folder, ExcludeDocumentIDs: []string{cd.id}, Limit: classifyNeighbours})
		if err != nil {
			return step, concepts, err
		}
		if res.IsError {
			step.Reason = "the neighbours could not be found: " + res.Text
			return step, concepts, nil
		}
		var hits struct {
			Related []struct {
				DocumentID string `json:"document_id"`
			} `json:"related"`
		}
		_ = json.Unmarshal([]byte(res.Text), &hits)
		for _, h := range hits.Related {
			if h.DocumentID != "" && !seen[h.DocumentID] {
				seen[h.DocumentID] = true
				neighbourDocs = append(neighbourDocs, h.DocumentID)
			}
		}
		if len(neighbourDocs) > 0 {
			break // the first section with neighbours is the seed
		}
	}
	if acl != nil {
		readable := neighbourDocs[:0]
		for _, id := range neighbourDocs {
			if ok, err := acl.readable(ctx, id); err != nil {
				return step, concepts, err
			} else if ok {
				readable = append(readable, id)
			}
		}
		neighbourDocs = readable
	}
	if len(neighbourDocs) == 0 {
		step.Reason = "no neighbouring document"
		if folder != "" {
			step.Reason += " under " + folder
		}
		return step, concepts, nil
	}

	ph, args := inPlaceholders(neighbourDocs)
	res, err := d.query(ctx, key, `SELECT doc.root_chunk_id, c.type FROM documents doc JOIN chunks c ON c.id = doc.root_chunk_id
		WHERE doc.id IN (`+ph+`)`, args...)
	if err != nil {
		return step, concepts, err
	}
	classOf := map[string]string{} // root chunk → declared class
	votes := map[string]int{}
	for _, r := range res.Rows {
		if c, ok := classes[strings.ToLower(strings.TrimSpace(asStr(r[1])))]; ok {
			classOf[asStr(r[0])] = c.Name
			votes[c.Name]++
		}
	}
	total := len(classOf)
	step.Neighbours = total
	if total == 0 {
		step.Reason = "no neighbouring document is classified yet"
		return step, concepts, nil
	}
	for name, n := range votes {
		step.Candidates = append(step.Candidates, classifyCandidate{Class: name, Votes: n, Share: float64(n) / float64(total)})
	}
	sort.Slice(step.Candidates, func(i, j int) bool {
		a, b := step.Candidates[i], step.Candidates[j]
		if a.Votes != b.Votes {
			return a.Votes > b.Votes
		}
		return a.Class < b.Class
	})
	if top := step.Candidates[0]; top.Votes >= classifyMinVotes && top.Share >= classifyMinShare &&
		(len(step.Candidates) == 1 || step.Candidates[1].Votes < top.Votes) {
		step.Chosen = top.Class
	} else {
		step.Reason = "the neighbours do not agree"
	}

	byChunk, byTag := map[string]memrank.OntologyConcept{}, map[string]memrank.OntologyConcept{}
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			byChunk[c.ChunkID], byTag[c.Tag] = c, c
		}
	}
	roots := make([]string, 0, len(classOf))
	for r := range classOf {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	type tally struct {
		docs map[string]bool
		kind string
		c    memrank.OntologyConcept
	}
	tallies := map[string]*tally{}
	count := func(root string, c memrank.OntologyConcept, kind string) {
		t := tallies[c.Tag]
		if t == nil {
			t = &tally{docs: map[string]bool{}, kind: kind, c: c}
			tallies[c.Tag] = t
		}
		t.docs[root] = true
	}
	ph, args = inPlaceholders(roots)
	if tenantScope {
		subjectKinds, _ := d.expandKindFilter(ctx, []string{"subject"})
		filed, _ := d.expandKindFilter(ctx, []string{"classified_as"})
		kindOf := map[string]string{}
		for _, k := range subjectKinds {
			kindOf[k] = "subject"
		}
		for _, k := range filed {
			kindOf[k] = "classified_as"
		}
		res, err = d.query(ctx, key, `SELECT from_id, to_id, kind FROM chunk_edges WHERE from_id IN (`+ph+`) AND status = ?`,
			append(args, edgeStatusAsserted)...)
		if err != nil {
			return step, concepts, err
		}
		for _, r := range res.Rows {
			if kind, ok := kindOf[asStr(r[2])]; ok {
				if c, ok := byChunk[asStr(r[1])]; ok {
					count(asStr(r[0]), c, kind)
				}
			}
		}
	} else {
		res, err = d.query(ctx, key, `SELECT chunk_id, tag FROM chunk_tags WHERE chunk_id IN (`+ph+`)`, args...)
		if err != nil {
			return step, concepts, err
		}
		for _, r := range res.Rows {
			if c, ok := byTag[asStr(r[1])]; ok {
				count(asStr(r[0]), c, "")
			}
		}
	}
	for _, t := range tallies {
		share := float64(len(t.docs)) / float64(total)
		if len(t.docs) >= classifyMinVotes && share >= classifyMinShare {
			concepts = append(concepts, classifyConcept{Concept: t.c.Name, Tag: t.c.Tag, Kind: t.kind, ChunkID: t.c.ChunkID, Share: share})
		}
	}
	sort.Slice(concepts, func(i, j int) bool { return concepts[i].Tag < concepts[j].Tag })
	return step, concepts, nil
}

// classifyIssuerSubject is the subject chunk an instance was adopted as, by the same
// trust rule adoption and describe_ontology use; "" when it is not adopted.
func (d *Document) classifyIssuerSubject(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, read OntologyRead, name string) string {
	for _, inst := range read.Instances {
		if inst.Name != name || inst.RegistryKey == "" {
			continue
		}
		class := ""
		for _, c := range memrank.EffectiveClasses(read.Terms, true) {
			if strings.EqualFold(c.Name, inst.Class) {
				class = c.Name
			}
		}
		if class == "" {
			return ""
		}
		lookup := func(nk string) (string, error) { return d.chunkIDByNaturalKey(ctx, key, nk) }
		subjectKey, trusted := d.instanceKey(ctx, key, mscope, inst, class, lookup)
		if !trusted {
			return ""
		}
		id, _ := lookup(subjectKey)
		return id
	}
	return ""
}

// classifyStatement writes one statement from the document's root, asking the caller's
// rules about both ends first. A refusal is a warning, not a failure: the rest of the
// classification still stands.
func (d *Document) classifyStatement(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, acl *docACL,
	docID, from, to, kind, status, quote string) (*edgeWriteResult, string) {
	if to == "" || kind == "" {
		return nil, ""
	}
	verb := "propose"
	if status == edgeStatusAsserted {
		verb = "link"
	}
	toRow, ok, err := d.getChunkRow(ctx, key, to)
	if err != nil || !ok {
		return nil, kind + " not written: its target could not be read"
	}
	for _, doc := range []string{docID, toRow.DocumentID} {
		if acl == nil {
			break
		}
		if r, err := acl.document(ctx, "classify", verb, doc); err != nil || r != nil {
			return nil, kind + " not written: the access-control rules do not allow it"
		}
	}
	w := edgeWrite{from: from, to: to, kind: kind, method: edgeMethodClassify, status: status, proposeIfInvalid: true}
	if quote != "" {
		w.attributes = map[string]any{"quote": quote}
	}
	res, refuse, err := d.writeStatement(ctx, key, mscope, w)
	if err != nil {
		return nil, kind + " not written: " + err.Error()
	}
	if refuse != nil {
		return nil, kind + " not written: " + refuse.Text
	}
	d.publishEdgeChange(ctx, key, mscope, "classify", res.ID, from, to, kind, res.Status)
	return &res, ""
}

// classifyWriteRoot writes the record (and, when setType is set, the class and its
// identity fields) onto the root through update_chunk, with the class rules the
// dispatcher runs for any write: an identity another chunk holds refuses the class, and
// the class is then proposed instead.
func (d *Document) classifyWriteRoot(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, acl *docACL,
	cd classifyDoc, setType string, idFields map[string]any, rec classification, out *classifyAnswer) *tools.Result {
	fields := make(map[string]any, len(cd.fields)+len(idFields)+1)
	for k, v := range cd.fields {
		fields[k] = v
	}
	if setType != "" {
		for k, v := range idFields {
			fields[k] = v
		}
	}
	fields[classifyField] = rec
	fb, err := json.Marshal(fields)
	if err != nil {
		r := errFrom("classify: "+err.Error(), err)
		return &r
	}
	rev := cd.rootRevision
	upd := docInput{Op: "update_chunk", ID: cd.rootID, Revision: &rev, Fields: fb}
	present := map[string]any{"op": "update_chunk", "id": cd.rootID, "revision": rev, "fields": json.RawMessage(fb)}
	if setType != "" {
		upd.Type, present["type"] = setType, setType
	}
	raw, _ := json.Marshal(present)
	cr, refused := d.applyClassRules(ctx, key, mscope, acl, &upd, raw)
	if refused != nil && setType != "" {
		// The class cannot be applied (its identity is held): propose it instead.
		out.Warnings = append(out.Warnings, "the class was not applied: "+refused.Text)
		out.Live, out.Result = false, "proposed"
		rec.Applied, rec.Proposed, rec.ProposedFields = "", setType, idFields
		if cd.record != nil {
			rec.Applied = cd.record.Applied
		}
		return d.classifyWriteRoot(ctx, key, mscope, acl, cd, "", nil, rec, out)
	}
	if refused != nil {
		return refused
	}
	res, err := d.updateChunk(ctx, key, mscope, upd, raw)
	if err != nil {
		r := errFrom("classify: "+err.Error(), err)
		return &r
	}
	if res.IsError {
		res.Text = "classify: " + res.Text
		return &res
	}
	res = d.finishClassRules(ctx, key, upd, cr, res)
	var wrote struct {
		Warnings []string `json:"warnings"`
	}
	_ = json.Unmarshal([]byte(res.Text), &wrote)
	out.Warnings = append(out.Warnings, wrote.Warnings...)
	return nil
}

// acceptClassification is the operator's ruling on a pending classification: the
// proposed class becomes the type, the proposed statements are accepted, and outside
// the tenant scope the proposed concepts become the root's tags. Not reachable from
// inside a run, like accept_statement.
func (d *Document) acceptClassification(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if !authorIsOperatorPlane(ctx) {
		return errPermission("accept_classification: only an operator accepts a classification — not an agent inside a run",
			"Leave it pending; an operator reviews it in the console."), nil
	}
	docID, err := d.docIDFromInput(ctx, key, in)
	if err != nil {
		return docFail("accept_classification: ", err), nil
	}
	cd, found, err := d.classifyRead(ctx, key, mscope, docID)
	if err != nil {
		return errFrom("accept_classification: "+err.Error(), err), nil
	}
	if !found {
		return errNotFound("accept_classification: no such document: "+docID, "Find the document with op=query_documents in the same scope."), nil
	}
	rec := cd.record
	if rec == nil || (rec.Proposed == "" && len(rec.Statements) == 0 && len(rec.ProposedTags) == 0) {
		return errBusiness("accept_classification: nothing_pending: the document has no pending classification",
			"Run op=classify first; a rule match it applied needs no acceptance."), nil
	}
	// THE RECORD IS A CLAIM, NOT AN AUTHORITY. It lives in the root's fields, which
	// anyone who may write the document may set, so a writer could stage one that names
	// another document's proposals, a rejected statement or an arbitrary tag for an
	// operator to accept unawares. Everything it names is re-checked here against what
	// classify itself could have written; what fails is skipped and reported.
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || !read.Confirmed {
		return errBusiness("accept_classification: the tenant ontology is not confirmed",
			"Confirm the ontology, then classify the document again."), nil
	}
	acl := d.newDocACL(ctx, key)
	out := map[string]any{"document_id": docID}
	var accepted, skipped []string
	for _, id := range rec.Statements {
		s, err := d.statementByID(ctx, key, id)
		if err != nil {
			return errFrom("accept_classification: "+err.Error(), err), nil
		}
		// Only what classify filed from THIS document's root, still awaiting review:
		// never another document's proposal, never a person's, and never a rejection,
		// which accept_statement would otherwise reverse.
		if s == nil || s.Method != edgeMethodClassify || s.FromID != cd.rootID || s.Status != edgeStatusProposed {
			skipped = append(skipped, id)
			continue
		}
		if acl != nil {
			denied := false
			for _, end := range []string{s.FromID, s.ToID} {
				row, found, err := d.getChunkRow(ctx, key, end)
				if err != nil {
					return errFrom("accept_classification: "+err.Error(), err), nil
				}
				if !found {
					denied = true
					break
				}
				if r, err := acl.document(ctx, "accept_classification", "link", row.DocumentID); err != nil {
					return errFrom("accept_classification: "+err.Error(), err), nil
				} else if r != nil {
					denied = true
					break
				}
			}
			if denied {
				skipped = append(skipped, id)
				continue
			}
		}
		r, err := d.reviewStatement(ctx, key, mscope, docInput{ID: id}, true)
		if err != nil {
			return errFrom("accept_classification: "+err.Error(), err), nil
		}
		if r.IsError {
			skipped = append(skipped, id)
			continue
		}
		accepted = append(accepted, id)
	}
	conceptTags := map[string]bool{}
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			conceptTags[c.Tag] = true
		}
	}
	var tags, skippedTags []string
	for _, tag := range rec.ProposedTags {
		if conceptTags[tag] {
			tags = append(tags, tag)
		} else {
			skippedTags = append(skippedTags, tag)
		}
	}
	if len(skippedTags) > 0 {
		out["skipped_tags"] = skippedTags
	}
	if len(tags) > 0 {
		r, err := d.addTags(ctx, key, docInput{ID: cd.rootID, Tags: tags})
		if err != nil {
			return errFrom("accept_classification: "+err.Error(), err), nil
		}
		if r.IsError {
			return r, nil
		}
		out["tags"] = tags
	}
	next := classification{By: edgeMethodClassify, At: rec.At, Applied: rec.Applied}
	setType := ""
	var setFields map[string]any
	if rec.Proposed != "" {
		// A declared document class only, and only that class's own fields.
		var class *memrank.OntologyTerm
		for _, c := range memrank.EffectiveClasses(read.Terms, true) {
			if c.IsDocumentClass && strings.EqualFold(c.Name, rec.Proposed) {
				class = &c
				break
			}
		}
		if class == nil {
			out["skipped_type"] = rec.Proposed
		} else {
			setType, next.Applied, setFields = class.Name, class.Name, identityGroups(*class, rec.ProposedFields)
			out["type"] = class.Name
		}
	}
	cd.record = rec
	var ans classifyAnswer
	if r := d.classifyWriteRoot(ctx, key, mscope, acl, cd, setType, setFields, next, &ans); r != nil {
		return *r, nil
	}
	if setType != "" && !ans.Live && ans.Result == "proposed" {
		// The identity is held by another chunk: the class stays pending, so it is not
		// reported as applied.
		delete(out, "type")
		out["type_pending"] = true
	}
	out["accepted_statements"] = nonNil(accepted)
	if len(skipped) > 0 {
		out["skipped_statements"] = skipped
	}
	if len(ans.Warnings) > 0 {
		out["warnings"] = ans.Warnings
	}
	return jsonResult(out)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
