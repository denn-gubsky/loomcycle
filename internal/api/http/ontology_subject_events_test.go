package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/config"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// subjectMessages reads every memory/subjects message the store holds.
func (s *Server) subjectMessages(t *testing.T) []store.ChannelMessage {
	t.Helper()
	all, err := s.store.SnapshotReadChannelMessages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []store.ChannelMessage
	for _, m := range all {
		if m.Channel == "memory/subjects" {
			out = append(out, m)
		}
	}
	return out
}

func subjectPayload(t *testing.T, m store.ChannelMessage) map[string]any {
	t.Helper()
	var ev map[string]any
	if err := json.Unmarshal(m.Payload, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// TestOntologyAPI_ConfirmAnnouncesAdoptedSubjects (RFC FR P0).
//
// Adoption happens only on the operator plane, and its routes built a Document with
// no channel bus, so publishing returned early: memory/subjects never carried a
// message in production while the tool-level tests, which set a bus by hand, passed.
// This drives the route on a server wired as boot wires it (a channel bus) and reads
// the store.
func TestOntologyAPI_ConfirmAnnouncesAdoptedSubjects(t *testing.T) {
	s, mi := ontologyFixture(t)
	s.SetChannelBus(channels.NewBus())
	replaceOntologyWithSections(t, s, mi, instancesLayout())

	got := s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	if got.InstanceAdoption == nil || got.InstanceAdoption.Adopted != 2 {
		t.Fatalf("confirm should adopt both instances: %#v", got.InstanceAdoption)
	}
	msgs := s.subjectMessages(t)
	if len(msgs) != 2 {
		t.Fatalf("memory/subjects messages = %d, want one per adopted instance", len(msgs))
	}
	for _, m := range msgs {
		ev := subjectPayload(t, m)
		if m.TenantID != mi.Tenant || m.Scope != store.MemoryScopeTenant {
			t.Errorf("message landed in tenant %q scope %q, want %q tenant", m.TenantID, m.Scope, mi.Tenant)
		}
		if ev["op"] != "adopted" || ev["document_id"] == nil || ev["root_chunk_id"] == nil {
			t.Errorf("event = %v, want an adoption naming the subject's document and root", ev)
		}
	}
}

// TestOntologyAPI_AcceptingASubjectProposalAnnouncesIt (RFC FR P0): the other
// adoption route.
func TestOntologyAPI_AcceptingASubjectProposalAnnouncesIt(t *testing.T) {
	s, mi := ontologyFixture(t)
	s.SetChannelBus(channels.NewBus())
	s.applyMemoryInjection(context.Background(), config.AgentDef{SystemPrompt: "{{memory:ontology}}"}, mi)

	// An agent files the proposal, as the consolidator would.
	agent := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	actx := withTenantDocGrants(s.docToolCtx(context.Background(), mi))
	actx = tools.WithAgentName(actx, "curator")
	res, err := agent.Execute(actx, json.RawMessage(`{"op":"propose_subject","subject":"Dave",
		"natural_key":"person:dave","path":"dave","body":"Seen in one user's chat."}`))
	if err != nil || res.IsError {
		t.Fatalf("propose_subject: %v %s", err, res.Text)
	}
	state := s.ontologyStateOrFail(t, mi.Tenant)
	var chunkID string
	for _, p := range state.Proposals {
		if strings.EqualFold(p.Name, "Dave") {
			chunkID = p.ChunkID
		}
	}
	if chunkID == "" {
		t.Fatalf("no subject proposal to accept: %+v", state.Proposals)
	}

	after := s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant,
		map[string]string{"chunk_id": chunkID, "action": "accept"}, http.StatusOK)
	if after.NotAnnounced != "" {
		t.Errorf("an unrestricted operator's adoption was not announced: %s", after.NotAnnounced)
	}
	msgs := s.subjectMessages(t)
	if len(msgs) != 1 {
		t.Fatalf("memory/subjects messages = %d, want the one adoption", len(msgs))
	}
	ev := subjectPayload(t, msgs[0])
	if ev["op"] != "adopted" || ev["subject_natural_key"] != "person:dave" || ev["document_id"] == nil {
		t.Errorf("event = %v", ev)
	}
	if msgs[0].TenantID != mi.Tenant {
		t.Errorf("message tenant = %q, want %q", msgs[0].TenantID, mi.Tenant)
	}
}
