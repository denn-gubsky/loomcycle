package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestOntologyAPI_ReportsTheGateAndWhatWouldChange is the reason the route exists.
//
// The failure it guards against is not a crash: it is an operator who edits the
// ontology, never learns that editing is not enough, and concludes the feature does
// nothing. So the read has to report BOTH halves — the terms they wrote, and the
// types actually in force — because the gap between them is the only visible
// evidence of the gate.
func TestOntologyAPI_ReportsTheGateAndWhatWouldChange(t *testing.T) {
	s, mi := ontologyFixture(t)

	got := s.getOntology(t, mi.Tenant)

	if !got.Provisioned {
		t.Fatal("the read should provision the document on first reference")
	}
	if got.Confirmed {
		t.Error("a freshly provisioned ontology must report unconfirmed")
	}
	if got.Status != meminject.OntologyDraftStatus {
		t.Errorf("status = %q, want %q", got.Status, meminject.OntologyDraftStatus)
	}
	// The tenant's OWN terms are reported even while inert — this is what the
	// operator is about to activate.
	if !hasTerm(got.Terms, "incident") {
		t.Errorf("the template's sample terms should be reported as the tenant layer, got %v", termNames(got.Terms))
	}
	// ...and the EFFECTIVE ontology does not contain them, which is the gate.
	if hasTerm(got.Effective, "incident") {
		t.Errorf("a draft tenant layer must not be in force, got %v", termNames(got.Effective))
	}
	if !hasTerm(got.Effective, "person") {
		t.Errorf("the base seed should always be in force, got %v", termNames(got.Effective))
	}
	for _, term := range got.Effective {
		if term.Source != "base" {
			t.Errorf("while draft every effective term must come from the seed; %q says %q", term.Name, term.Source)
		}
	}
}

// TestOntologyAPI_ConfirmAndRevertBothTakeEffect exercises the gate in BOTH
// directions.
//
// One direction is not enough: a gate tested only on opening can ship stuck open,
// and a confirm an operator cannot undo is a one-way door on a live extractor.
func TestOntologyAPI_ConfirmAndRevertBothTakeEffect(t *testing.T) {
	s, mi := ontologyFixture(t)
	s.getOntology(t, mi.Tenant) // provision

	after := s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	if !after.Confirmed {
		t.Fatal("confirm did not take")
	}
	if !hasTerm(after.Effective, "incident") {
		t.Errorf("a confirmed tenant layer should be in force, got %v", termNames(after.Effective))
	}
	if !hasTerm(after.Effective, "person") {
		t.Errorf("the seed must survive the layering, got %v", termNames(after.Effective))
	}
	// The response reports WHICH half each type came from, so the UI never has to
	// diff against the seed to know.
	for _, term := range after.Effective {
		if term.Name == "incident" && term.Source != "tenant" {
			t.Errorf("a tenant term should be sourced 'tenant', got %q", term.Source)
		}
		if term.Name == "person" && term.Source != "base" {
			t.Errorf("a seed term should be sourced 'base', got %q", term.Source)
		}
	}

	back := s.setOntologyStatus(t, mi.Tenant, meminject.OntologyDraftStatus, http.StatusOK)
	if back.Confirmed {
		t.Error("reverting to draft should deactivate the tenant layer")
	}
	if hasTerm(back.Effective, "incident") {
		t.Errorf("a reverted layer must leave force, got %v", termNames(back.Effective))
	}
	// Reverting deactivates; it does not erase. The operator's work is still there.
	if !hasTerm(back.Terms, "incident") {
		t.Errorf("revert must not discard the tenant's terms, got %v", termNames(back.Terms))
	}
}

// TestOntologyAPI_RefusesAStatusTheGateWouldIgnore is the entire justification for a
// dedicated route over the generic document editor.
//
// The gate matches one exact word. Through update_chunk an operator can type
// `confirmd` into a free-text status box, see it saved, and get a permanently inert
// ontology with no error anywhere — the invisible failure this subsystem's design
// rejected a per-term approval queue to avoid. A two-value endpoint cannot produce
// that state.
func TestOntologyAPI_RefusesAStatusTheGateWouldIgnore(t *testing.T) {
	s, mi := ontologyFixture(t)
	s.getOntology(t, mi.Tenant)

	for _, bad := range []string{"confirmd", "CONFIRMED!", "active", "", "pending"} {
		s.setOntologyStatus(t, mi.Tenant, bad, http.StatusBadRequest)
	}
	// And nothing moved.
	got := s.getOntology(t, mi.Tenant)
	if got.Status != meminject.OntologyDraftStatus || got.Confirmed {
		t.Errorf("a refused write changed state: status=%q confirmed=%v", got.Status, got.Confirmed)
	}
}

// TestOntologyAPI_CaseAndWhitespaceStillConfirm: the two accepted words are matched
// leniently, because "Confirmed" from a form field is unambiguously the same intent
// and refusing it would be pedantry that reads as a bug.
func TestOntologyAPI_CaseAndWhitespaceStillConfirm(t *testing.T) {
	s, mi := ontologyFixture(t)
	s.getOntology(t, mi.Tenant)

	got := s.setOntologyStatus(t, mi.Tenant, "  Confirmed ", http.StatusOK)
	if !got.Confirmed {
		t.Error("a case/whitespace variant of the accepted word should confirm")
	}
	// Normalized on the way in, so the stored status is what the gate matches
	// exactly — not a variant that happens to work only through this route.
	if got.Status != meminject.OntologyConfirmedStatus {
		t.Errorf("stored status = %q, want the canonical %q", got.Status, meminject.OntologyConfirmedStatus)
	}
}

// TestOntologyAPI_FlipPreservesTheOperatorsMarkdown guards the exact regression that
// blanked a document root once before: a partial chunk write that sends body/fields
// it did not read. Flipping a flag must not cost the operator their ontology.
func TestOntologyAPI_FlipPreservesTheOperatorsMarkdown(t *testing.T) {
	s, mi := ontologyFixture(t)
	before := s.getOntology(t, mi.Tenant)
	md := s.exportOntologyMD(t, mi)
	if !strings.Contains(md, "incident") {
		t.Fatalf("fixture: the provisioned document should carry the template, got:\n%s", md)
	}

	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)

	if after := s.exportOntologyMD(t, mi); after != md {
		t.Errorf("the flip rewrote the document.\nbefore:\n%s\nafter:\n%s", md, after)
	}
	// Same document, not a replacement — a flip that recreated it would orphan every
	// chunk id anything else holds.
	if got := s.getOntology(t, mi.Tenant); got.DocumentID != before.DocumentID {
		t.Errorf("document id changed across the flip: %q → %q", before.DocumentID, got.DocumentID)
	}
}

// TestOntologyAPI_ReadingProvisionsSoItCanBeAuthoredFirst: provisioning used to be
// lazy on the first RUN that referenced the ontology, which left an operator wanting
// to set it up beforehand with no document to open. The read now creates it — as
// draft, so creating it still changes nothing.
func TestOntologyAPI_ReadingProvisionsSoItCanBeAuthoredFirst(t *testing.T) {
	s, mi := ontologyFixture(t)
	if s.ontologyDocExists(t, mi) {
		t.Fatal("fixture: the document should not exist yet")
	}

	s.getOntology(t, mi.Tenant)

	if !s.ontologyDocExists(t, mi) {
		t.Error("reading the ontology should provision the document")
	}
	// Provisioned ≠ active: a run assembled right now still gets the seed alone.
	def := config.AgentDef{SystemPrompt: "{{memory:ontology}}"}
	got, _ := s.applyMemoryInjection(context.Background(), def, mi)
	if !strings.Contains(got.SystemPrompt, "has not confirmed") {
		t.Errorf("provisioning must not activate anything:\n%s", got.SystemPrompt)
	}
}

// TestOntologyAPI_ConfirmReachesTheRunsPrompt closes the loop the operator cares
// about: the button changes what the model is told. Asserting on the handler's own
// response would only prove the handler agrees with itself.
func TestOntologyAPI_ConfirmReachesTheRunsPrompt(t *testing.T) {
	s, mi := ontologyFixture(t)
	s.getOntology(t, mi.Tenant)
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)

	def := config.AgentDef{SystemPrompt: "You extract entities.\n\n{{memory:ontology}}"}
	got, _ := s.applyMemoryInjection(context.Background(), def, mi)

	if !strings.Contains(got.SystemPrompt, "incident") {
		t.Errorf("a confirmed tenant term should reach the prompt:\n%s", got.SystemPrompt)
	}
	if strings.Contains(got.SystemPrompt, "has not confirmed") {
		t.Errorf("the prompt still says unconfirmed after the flip:\n%s", got.SystemPrompt)
	}
}

// TestOntologyAPI_IsTenantScopedNotAdminOnly: the ontology is per-tenant config and
// the tenant operator is the intended actor. Pinning it to substrate:admin would
// hand the only activation control to someone who does not own the vocabulary — and
// the same tenant token can already write this status through /v1/_document, so a
// harder gate here would deny the owner without closing anything.
func TestOntologyAPI_IsTenantScopedNotAdminOnly(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if got := requiredScopeFor(method, "/v1/_ontology"); got != auth.ScopeTenant {
			t.Errorf("requiredScopeFor(%s /v1/_ontology) = %q, want %q", method, got, auth.ScopeTenant)
		}
	}
}

// TestOntologyAPI_DegradesWithoutSQLMemory: no SQL Memory means no document, but a
// deployment in that shape still runs on the base seed. Reporting that honestly beats
// a 503 the UI has to special-case to show a page that is factually correct.
func TestOntologyAPI_DegradesWithoutSQLMemory(t *testing.T) {
	s := &Server{}

	got := s.getOntology(t, "acme")

	if got.Provisioned {
		t.Error("nothing can be provisioned without SQL Memory")
	}
	if !hasTerm(got.Effective, "person") {
		t.Errorf("the seed applies with or without a document, got %v", termNames(got.Effective))
	}
	// The write, by contrast, must fail loudly — silently accepting a flip that
	// cannot persist would report success for nothing.
	s.setOntologyStatus(t, "acme", meminject.OntologyConfirmedStatus, http.StatusServiceUnavailable)
}

// ---- helpers ----

func (s *Server) getOntology(t *testing.T, tenant string) ontologyResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/_ontology?tenant="+tenant, nil)
	rec := httptest.NewRecorder()
	s.handleOntology(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/_ontology = %d: %s", rec.Code, rec.Body.String())
	}
	var got ontologyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return got
}

// setOntologyStatus posts a flip and asserts the status code, returning the decoded
// response on success. wantCode makes the refusal cases assert the code rather than
// only the absence of a change.
func (s *Server) setOntologyStatus(t *testing.T, tenant, status string, wantCode int) ontologyResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"status": status})
	req := httptest.NewRequest(http.MethodPost, "/v1/_ontology?tenant="+tenant, strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	s.handleOntologySetStatus(rec, req)
	if rec.Code != wantCode {
		t.Fatalf("POST /v1/_ontology status=%q = %d, want %d: %s", status, rec.Code, wantCode, rec.Body.String())
	}
	var got ontologyResponse
	if wantCode == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
	}
	return got
}

// exportOntologyMD reads the document's Markdown, so a test can prove a write did
// not disturb it.
func (s *Server) exportOntologyMD(t *testing.T, mi memInject) string {
	t.Helper()
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := tools.WithMemoryPolicy(s.docToolCtx(context.Background(), mi),
		tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}})
	dctx = tools.WithSqlMemPolicy(dctx, tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}})
	req, _ := json.Marshal(map[string]any{
		"op": "export_md", "scope": "tenant", "path": meminject.OntologyPath, "include_metadata": false,
	})
	res, _ := doc.Execute(dctx, req)
	if res.IsError {
		t.Fatalf("export_md: %s", res.Text)
	}
	var body struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal([]byte(res.Text), &body); err != nil {
		t.Fatalf("export_md decode: %v", err)
	}
	return body.Markdown
}

func hasTerm(terms []meminject.OntologyTerm, name string) bool {
	for _, t := range terms {
		if strings.EqualFold(t.Name, name) {
			return true
		}
	}
	return false
}

func termNames(terms []meminject.OntologyTerm) []string {
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		out = append(out, t.Name)
	}
	return out
}
