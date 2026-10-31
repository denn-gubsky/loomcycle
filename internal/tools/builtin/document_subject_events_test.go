package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// subjectEvents returns the memory/subjects payloads the writer recorded.
func (w *recordingWriter) subjectEvents(t *testing.T) []map[string]any {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []map[string]any
	for _, m := range w.msgs {
		if m.Channel != "memory/subjects" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal(m.Payload, &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// TestSubjectEvent_CarriesTheSubjectsDocumentIDs (RFC FR §2, P0).
//
// A consumer selects statements by document, so an adoption names the subject's
// document and root chunk, not only its natural key. Both adoption paths: an
// accepted subject proposal and a confirmed ontology instance.
func TestSubjectEvent_CarriesTheSubjectsDocumentIDs(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	w := &recordingWriter{}
	d.Writer = w
	seedTenantOntology(t, d, ctx)
	if _, r := docExec(t, d, ctx, `{"op":"propose_subject","subject":"Dave",
		"natural_key":"person:dave","path":"dave","body":"Seen in one user's chat."}`); r.IsError {
		t.Fatalf("propose_subject: %s", r.Text)
	}
	read, _ := d.OntologyTermsFromTree(grantedTenantCtx(ctx), "tenant", memrank.OntologyPath)
	var chunkID string
	for _, p := range read.Proposals {
		if strings.EqualFold(p.Name, "Dave") {
			chunkID = p.ChunkID
		}
	}
	_, f, evidence := d.SubjectProposal(grantedTenantCtx(ctx), "tenant", chunkID)
	docID, why, err := d.AdoptSubjectProposalInScope(grantedTenantCtx(ctx), "tenant", f, evidence)
	if err != nil || why != "" {
		t.Fatalf("adopt: %v (not announced: %q)", err, why)
	}
	got, _ := docExec(t, d, grantedTenantCtx(ctx), `{"op":"get_document","scope":"tenant","id":"`+docID+`"}`)
	evs := w.subjectEvents(t)
	if len(evs) != 1 {
		t.Fatalf("events = %v, want one adoption", evs)
	}
	ev := evs[0]
	if ev["op"] != "adopted" || ev["subject_natural_key"] != "person:dave" ||
		ev["document_id"] != docID || ev["root_chunk_id"] != got["root_chunk_id"] || ev["root_chunk_id"] == nil {
		t.Errorf("proposal adoption event = %v, want document %s root %v", ev, docID, got["root_chunk_id"])
	}

	d2, base, _ := documentFixture(t)
	w2 := &recordingWriter{}
	d2.Writer = w2
	ictx := installTenantOntology(t, d2, base, instancesOntologyMD, memrank.OntologyConfirmedStatus)
	rep, err := d2.AdoptInstances(ictx, false)
	if err != nil || rep.Adopted != 4 || rep.NotAnnounced != "" {
		t.Fatalf("adopt instances = %#v, %v", rep, err)
	}
	key := mustTenantKey(t, d2, ictx)
	for _, ev := range w2.subjectEvents(t) {
		root, _ := d2.chunkIDByNaturalKey(ictx, key, asStr(ev["subject_natural_key"]))
		res, qerr := d2.query(ictx, key, `SELECT document_id FROM chunks WHERE id = ?`, root)
		if qerr != nil || len(res.Rows) == 0 {
			t.Fatalf("no subject for %v: %v", ev, qerr)
		}
		if ev["root_chunk_id"] != root || ev["document_id"] != asStr(res.Rows[0][0]) {
			t.Errorf("instance adoption event = %v, want root %s document %s", ev, root, asStr(res.Rows[0][0]))
		}
	}
}

// TestSubjectEvent_WithheldWhenReadingIsPartitioned (RFC FR §2, P0).
//
// Everyone on the scope's channel reads the event and it names the subject, so it is
// not published when the caller's rules hide part of the scope from reading — and the
// adoption says why rather than staying silent.
func TestSubjectEvent_WithheldWhenReadingIsPartitioned(t *testing.T) {
	d, base, _ := documentFixture(t)
	w := &recordingWriter{}
	d.Writer = w
	ctx := installTenantOntology(t, d, base, instancesOntologyMD, memrank.OntologyConfirmedStatus)
	limited := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}})
	})
	rep, err := d.AdoptInstances(limited, false)
	if err != nil || rep.Adopted != 4 {
		t.Fatalf("adopt = %#v, %v", rep, err)
	}
	if rep.NotAnnounced == "" {
		t.Error("the adoption gives no reason for not announcing")
	}
	if evs := w.subjectEvents(t); len(evs) != 0 {
		t.Errorf("announced under a partitioned scope: %v", evs)
	}
}
