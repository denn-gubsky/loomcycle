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

// postOntologyJSON drives one of the RFC CA operator actions.
func (s *Server) postOntologyJSON(t *testing.T, path, tenant string, payload any, wantCode int) ontologyResponse {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, path+"?tenant="+tenant, strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	switch path {
	case "/v1/_ontology/proposals":
		s.handleOntologyProposal(rec, req)
	case "/v1/_ontology/adopt":
		s.handleOntologyAdopt(rec, req)
	default:
		t.Fatalf("unknown path %q", path)
	}
	if rec.Code != wantCode {
		t.Fatalf("POST %s %v = %d, want %d: %s", path, payload, rec.Code, wantCode, rec.Body.String())
	}
	var got ontologyResponse
	if wantCode == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
	}
	return got
}

// TestOntologyAdopt_CopiesTheStandardTypeWithItsFieldsAndProvenance (RFC CA §3).
//
// The transcription this removes is the point: the operator had to read the field names
// off the panel and retype them to override a standard type. The fields therefore come
// from the seed SERVER-SIDE — a client-supplied list could disagree with the type it
// claims to copy, and the disagreement would be invisible in the document.
func TestOntologyAdopt_CopiesTheStandardTypeWithItsFieldsAndProvenance(t *testing.T) {
	s, mi := ontologyFixture(t)
	s.buildVersion = "v9.9.9"
	s.applyMemoryInjection(context.Background(), config.AgentDef{SystemPrompt: "{{memory:ontology}}"}, mi)

	before := s.ontologyStateOrFail(t, mi.Tenant)
	if !containsName(before.Adoptable, "event") {
		t.Fatalf("a standard type the document does not declare must be adoptable: %v", before.Adoptable)
	}

	after := s.postOntologyJSON(t, "/v1/_ontology/adopt", mi.Tenant, map[string]string{"name": "event"}, http.StatusOK)

	var adopted *meminject.OntologyTerm
	for i := range after.Terms {
		if after.Terms[i].Name == "event" {
			adopted = &after.Terms[i]
		}
	}
	if adopted == nil {
		names := []string{}
		for _, tm := range after.Terms {
			names = append(names, tm.Name)
		}
		t.Fatalf("event was not adopted into the document — have %v", names)
	}
	// The seed's fields, not an empty shell the operator still has to fill in.
	var seed meminject.OntologyTerm
	for _, tm := range meminject.BaseSeedOntology() {
		if tm.Name == "event" {
			seed = tm
		}
	}
	if len(seed.Fields) == 0 {
		t.Fatal("the seed `event` has no fields — this test would be vacuous")
	}
	if strings.Join(adopted.Fields, ",") != strings.Join(seed.Fields, ",") {
		t.Errorf("adopted fields %v, want the seed's %v", adopted.Fields, seed.Fields)
	}
	// It is a ROOT, so the operator can nest beneath it — the whole reason to adopt.
	if adopted.Parent != "" {
		t.Errorf("an adopted type must be a root, got parent %q", adopted.Parent)
	}
	// And it is no longer offered for adoption.
	if containsName(after.Adoptable, "event") {
		t.Errorf("event is still adoptable after being adopted: %v", after.Adoptable)
	}
	// PROVENANCE (§8.3) is in the chunk's structured fields, not its body — asserted by
	// reading the column, because the response deliberately does not carry it and a claim
	// nothing checks is a claim that quietly stops being true. Without this, §3.2's freeze
	// (an adopted type never sees a later seed field) is undiscoverable after the fact.
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(context.Background(), mi)
	// The fields JSON travels with the BODY in the k/v plane, not in a SQL column, so
	// get_chunk is where it surfaces.
	q, _ := json.Marshal(map[string]any{
		"op": "query_chunks", "scope": "tenant",
		"sql": "SELECT id FROM chunks WHERE title = 'event' LIMIT 1",
	})
	qres, qerr := doc.Execute(dctx, q)
	if qerr != nil || qres.IsError {
		t.Fatalf("locate the adopted chunk: %v %s", qerr, qres.Text)
	}
	var qout struct {
		Rows [][]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(qres.Text), &qout); err != nil || len(qout.Rows) == 0 {
		t.Fatalf("no adopted chunk row: %v %s", err, qres.Text)
	}
	adoptedID, _ := qout.Rows[0][0].(string)
	g, _ := json.Marshal(map[string]any{"op": "get_chunk", "scope": "tenant", "id": adoptedID})
	gres, gerr := doc.Execute(dctx, g)
	if gerr != nil || gres.IsError {
		t.Fatalf("get_chunk: %v %s", gerr, gres.Text)
	}
	var chunk struct {
		Fields map[string]any `json:"fields"`
	}
	if err := json.Unmarshal([]byte(gres.Text), &chunk); err != nil {
		t.Fatalf("decode chunk: %v (%s)", err, gres.Text)
	}
	prov := chunk.Fields
	if prov == nil {
		t.Fatalf("the adopted chunk carries no provenance fields: %s", gres.Text)
	}
	if prov["adopted_from"] != "event" || prov["adopted_from_source"] != "base" {
		t.Errorf("provenance = %v, want it to name what was copied and from where", prov)
	}
	if prov["adopted_at_version"] != "v9.9.9" {
		t.Errorf("provenance version = %v, want the running build's", prov["adopted_at_version"])
	}
	// And the body stays the OPERATOR'S — the machine-written note must not be a field
	// bullet, or it would parse as one.
	if strings.Contains(strings.Join(adopted.Fields, ","), "Adopted") {
		t.Errorf("the provenance note leaked into the parsed fields: %v", adopted.Fields)
	}

	// Adopting twice must not silently produce two `event` sections.
	s.postOntologyJSON(t, "/v1/_ontology/adopt", mi.Tenant, map[string]string{"name": "event"}, http.StatusConflict)
	// A name that is not a standard type is refused rather than invented.
	s.postOntologyJSON(t, "/v1/_ontology/adopt", mi.Tenant, map[string]string{"name": "nonesuch"}, http.StatusBadRequest)
}

// TestOntologyProposal_AcceptPutsItInForceInPlace_RejectKeepsATombstone (RFC CA §2.3, §8.1).
func TestOntologyProposal_AcceptPutsItInForceInPlace_RejectKeepsATombstone(t *testing.T) {
	s, mi := ontologyFixture(t)
	s.applyMemoryInjection(context.Background(), config.AgentDef{SystemPrompt: "{{memory:ontology}}"}, mi)
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(context.Background(), mi)

	// A proposal nested under the template's `incident`, as a curator would file it.
	q, _ := json.Marshal(map[string]any{
		"op": "query_chunks", "scope": "tenant",
		"sql": "SELECT id, document_id FROM chunks WHERE title = 'incident' LIMIT 1",
	})
	qres, _ := doc.Execute(dctx, q)
	var qout struct {
		Rows [][]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(qres.Text), &qout); err != nil || len(qout.Rows) == 0 {
		t.Fatalf("no incident chunk: %v %s", err, qres.Text)
	}
	parentID, docID := qout.Rows[0][0].(string), qout.Rows[0][1].(string)
	mkProposal := func(title string) string {
		c, _ := json.Marshal(map[string]any{
			"op": "create_chunk", "scope": "tenant", "document_id": docID, "parent_id": parentID,
			"title": title, "status": meminject.OntologyStatusProposed,
			"body": "- `severity_scale`\n\nSeen 9 times.\n",
		})
		res, err := doc.Execute(dctx, c)
		if err != nil || res.IsError {
			t.Fatalf("create proposal: %v %s", err, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		id, _ := out["id"].(string)
		return id
	}
	acceptID, rejectID := mkProposal("sev1-incident"), mkProposal("bogus-incident")

	state := s.ontologyStateOrFail(t, mi.Tenant)
	if len(state.Proposals) != 2 {
		t.Fatalf("want both proposals reported, got %+v", state.Proposals)
	}
	for _, tm := range state.Terms {
		if tm.Name == "sev1-incident" {
			t.Fatal("a proposal was in force before it was accepted")
		}
	}

	// ACCEPT: in force, in place — still under `incident`, nothing moved.
	after := s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant,
		map[string]string{"chunk_id": acceptID, "action": "accept"}, http.StatusOK)
	var accepted *meminject.OntologyTerm
	for i := range after.Terms {
		if after.Terms[i].Name == "sev1-incident" {
			accepted = &after.Terms[i]
		}
	}
	if accepted == nil {
		t.Fatalf("accepting did not put the entity in force: %+v", after.Terms)
	}
	if accepted.Parent != "incident" {
		t.Errorf("accepted.Parent = %q — accepting must not move the entity", accepted.Parent)
	}
	if len(accepted.Inherited) == 0 {
		t.Error("an accepted subclass should inherit its parent's fields like any other")
	}

	// REJECT: kept as a tombstone, not deleted, so a curator stops re-proposing it.
	after = s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant,
		map[string]string{"chunk_id": rejectID, "action": "reject"}, http.StatusOK)
	var tomb *meminject.OntologyProposal
	for i := range after.Proposals {
		if after.Proposals[i].ChunkID == rejectID {
			tomb = &after.Proposals[i]
		}
	}
	if tomb == nil || tomb.Status != meminject.OntologyStatusRejected {
		t.Errorf("a rejection must survive as a tombstone, got %+v", after.Proposals)
	}
	for _, tm := range after.Terms {
		if tm.Name == "bogus-incident" {
			t.Error("a rejected entity reached the in-force set")
		}
	}
	// Accepting a rejection is a conflict, not a silent un-reject.
	s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant,
		map[string]string{"chunk_id": rejectID, "action": "accept"}, http.StatusConflict)
}

// TestOntologyProposal_RefusesAChunkThatIsNotAProposal is the narrowing (RFC CA §8.2).
//
// The route must not be usable as a general status-writer: accepting is meant to be a
// strictly smaller capability than writing the document. A live entity's id, and the
// document ROOT's id (whose status is the confirm gate itself), must both be refused —
// otherwise "accept a proposal" could confirm the whole ontology.
func TestOntologyProposal_RefusesAChunkThatIsNotAProposal(t *testing.T) {
	s, mi := ontologyFixture(t)
	s.applyMemoryInjection(context.Background(), config.AgentDef{SystemPrompt: "{{memory:ontology}}"}, mi)
	state := s.ontologyStateOrFail(t, mi.Tenant)

	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(context.Background(), mi)
	q, _ := json.Marshal(map[string]any{
		"op": "query_chunks", "scope": "tenant",
		"sql": "SELECT id FROM chunks WHERE title = 'project' LIMIT 1",
	})
	qres, _ := doc.Execute(dctx, q)
	var qout struct {
		Rows [][]any `json:"rows"`
	}
	_ = json.Unmarshal([]byte(qres.Text), &qout)
	if len(qout.Rows) == 0 {
		t.Fatal("no live `project` chunk to try")
	}
	liveID := qout.Rows[0][0].(string)

	for _, id := range []string{liveID, state.RootChunkID, "no-such-chunk"} {
		s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant,
			map[string]string{"chunk_id": id, "action": "accept"}, http.StatusNotFound)
	}
	// The confirm gate is untouched by the attempt on the root.
	if got := s.ontologyStateOrFail(t, mi.Tenant); got.Status != state.Status {
		t.Errorf("the document status changed from %q to %q", state.Status, got.Status)
	}
}

// containsName is a local helper — the package already has a containsString.
func containsName(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// ontologyStateOrFail reads the state directly, without going through a mutation.
func (s *Server) ontologyStateOrFail(t *testing.T, tenant string) ontologyResponse {
	t.Helper()
	resp, err := s.ontologyState(context.Background(), tenant)
	if err != nil {
		t.Fatalf("ontologyState: %v", err)
	}
	return resp
}
