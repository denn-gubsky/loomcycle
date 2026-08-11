package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestOntology_ReachesTheAssembledPromptAndStartsInert is the wiring plus the gate,
// in one pass over the path a run actually takes.
//
// A capability with no caller passes every test it has — this subsystem has shipped
// that mistake twice — so the assertion is on the prompt the run gets, not on the
// renderer in isolation.
func TestOntology_ReachesTheAssembledPromptAndStartsInert(t *testing.T) {
	s, mi := ontologyFixture(t)
	def := config.AgentDef{SystemPrompt: "You extract entities.\n\n{{memory:ontology}}"}

	got, _ := s.applyMemoryInjection(context.Background(), def, mi)

	// The seed reached the prompt.
	for _, want := range []string{"Entity types", "person", "organization", "preference", "fact"} {
		if !strings.Contains(got.SystemPrompt, want) {
			t.Errorf("missing %q in the assembled prompt:\n%s", want, got.SystemPrompt)
		}
	}
	if strings.Contains(got.SystemPrompt, "{{memory:") {
		t.Errorf("placeholder left unexpanded:\n%s", got.SystemPrompt)
	}
	// UNFRAMED — it is a schema to apply, not data to distrust.
	if strings.Contains(got.SystemPrompt, `<memory source="ontology">`) {
		t.Errorf("ontology must render unframed:\n%s", got.SystemPrompt)
	}
	// And a freshly provisioned deployment is INERT: the document exists, its terms
	// are not applied, and the prompt says so.
	if !strings.Contains(got.SystemPrompt, "has not confirmed") {
		t.Errorf("a newly provisioned tenant should read as unconfirmed:\n%s", got.SystemPrompt)
	}
	if strings.Contains(got.SystemPrompt, "incident") {
		t.Errorf("the template's sample terms applied while still draft:\n%s", got.SystemPrompt)
	}
}

// TestOntology_ProvisionsTheDocumentOnFirstReference: lazily, because there is no
// tenant-creation event to hang it on — a tenant exists the moment a token names
// one, and nothing observes that transition. Hooking token mint would have missed
// every tenant that already exists.
func TestOntology_ProvisionsTheDocumentOnFirstReference(t *testing.T) {
	s, mi := ontologyFixture(t)
	def := config.AgentDef{SystemPrompt: "{{memory:ontology}}"}
	s.applyMemoryInjection(context.Background(), def, mi)

	terms, confirmed, _ := s.tenantOntologyTerms(context.Background(), mi)
	if confirmed {
		t.Error("a provisioned document must start unconfirmed")
	}
	// The template's three worked examples are present as terms, ready to edit —
	// they are just not in force yet.
	names := map[string]bool{}
	for _, term := range terms {
		names[strings.ToLower(term.Name)] = true
	}
	for _, want := range []string{"project", "incident", "constraint"} {
		if !names[want] {
			t.Errorf("the provisioned document should carry the %q sample term (have %v)", want, names)
		}
	}
}

// TestOntology_NotReferencedNeverProvisions: rendering PROVISIONS a document, so a
// prompt that never mentions the ontology must not create one as a side effect of
// being assembled. The same rule user_info follows.
func TestOntology_NotReferencedNeverProvisions(t *testing.T) {
	s, mi := ontologyFixture(t)
	def := config.AgentDef{SystemPrompt: "A prompt with no placeholders."}

	got, _ := s.applyMemoryInjection(context.Background(), def, mi)
	if got.SystemPrompt != def.SystemPrompt {
		t.Errorf("the fast path should return byte-identical, got:\n%s", got.SystemPrompt)
	}
	if s.ontologyDocExists(t, mi) {
		t.Error("an unreferenced ontology must not be provisioned")
	}
}

// TestOntology_ConfirmedLayerApplies closes the loop: flip the root chunk to
// confirmed and the operator's terms take effect. Without this the gate is only
// proven in one direction, which is how a gate that never opens ships.
func TestOntology_ConfirmedLayerApplies(t *testing.T) {
	s, mi := ontologyFixture(t)
	def := config.AgentDef{SystemPrompt: "{{memory:ontology}}"}
	s.applyMemoryInjection(context.Background(), def, mi) // provision

	s.confirmOntology(t, mi)

	got, _ := s.applyMemoryInjection(context.Background(), def, mi)
	if strings.Contains(got.SystemPrompt, "has not confirmed") {
		t.Errorf("a confirmed deployment should not read as unconfirmed:\n%s", got.SystemPrompt)
	}
	for _, want := range []string{"project", "incident", "constraint"} {
		if !strings.Contains(got.SystemPrompt, want) {
			t.Errorf("confirmed tenant term %q missing from the prompt:\n%s", want, got.SystemPrompt)
		}
	}
	// The seed survives alongside the tenant layer — this is a layering, not a
	// replacement.
	if !strings.Contains(got.SystemPrompt, "person") {
		t.Errorf("the base seed was lost when the tenant layer activated:\n%s", got.SystemPrompt)
	}
}

// ---- fixture + helpers ----

// ontologyFixture builds a Server with a real store and a real SQL Memory manager
// — the ontology lives in a tenant-scope Document, so a stub would prove nothing
// about the path a run takes.
func ontologyFixture(t *testing.T) (*Server, memInject) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	s := &Server{store: st, sqlMem: mgr}
	return s, memInject{Tenant: "acme", UserID: "alice", AgentName: "curator"}
}

// ontologyDocExists reports whether the tenant's ontology document is present,
// WITHOUT provisioning it — a probe that provisioned would make
// TestOntology_NotReferencedNeverProvisions vacuous.
func (s *Server) ontologyDocExists(t *testing.T, mi memInject) bool {
	t.Helper()
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := tools.WithMemoryPolicy(s.docToolCtx(context.Background(), mi),
		tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}})
	dctx = tools.WithSqlMemPolicy(dctx, tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}})
	req, _ := json.Marshal(map[string]any{
		"op": "get_document", "scope": "tenant", "path": meminject.OntologyPath,
	})
	res, _ := doc.Execute(dctx, req)
	return !res.IsError
}

// confirmOntology flips the root chunk to `confirmed`, which is what an operator
// does in the Web UI — and now literally so: it drives POST /v1/_ontology rather
// than re-deriving the get_document → get_chunk → update_chunk sequence itself. A
// helper with its own copy of that sequence would keep passing after the real route
// broke, which is the wrong way round for the path an operator depends on.
func (s *Server) confirmOntology(t *testing.T, mi memInject) {
	t.Helper()
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
}

// TestOntology_NestedChunkReachesTheEffectiveOntology is the bug, end to end.
//
// The unit test proves the tree reader; this proves the SERVER read uses it. Those are
// different claims, and this subsystem has already shipped the gap between them twice —
// a correct component wired to nothing passes all of its own tests.
//
// FAILS before the change: the read went through export_md, which flattens chunk depth
// into "#" repetition, and the parser matched only "## ".
func TestOntology_NestedChunkReachesTheEffectiveOntology(t *testing.T) {
	s, mi := ontologyFixture(t)
	// Provision by rendering once, exactly as a run does.
	s.applyMemoryInjection(context.Background(), config.AgentDef{
		SystemPrompt: "{{memory:ontology}}",
	}, mi)

	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(context.Background(), mi)

	// Find the sample `project` term's chunk and hang a subclass off it.
	q, _ := json.Marshal(map[string]any{
		"op": "query_chunks", "scope": "tenant",
		"sql": "SELECT id, document_id FROM chunks WHERE title = 'project' LIMIT 1",
	})
	qres, err := doc.Execute(dctx, q)
	if err != nil || qres.IsError {
		t.Fatalf("query_chunks: %v %s", err, qres.Text)
	}
	var qout struct {
		Rows [][]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(qres.Text), &qout); err != nil || len(qout.Rows) == 0 {
		t.Fatalf("no `project` chunk to nest under: %v %s", err, qres.Text)
	}
	parentID, docID := qout.Rows[0][0].(string), qout.Rows[0][1].(string)

	c, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "scope": "tenant", "document_id": docID,
		"parent_id": parentID, "title": "internal-project",
		"body": "- `cost_center` — who pays\n",
	})
	if cres, cerr := doc.Execute(dctx, c); cerr != nil || cres.IsError {
		t.Fatalf("create_chunk: %v %s", cerr, cres.Text)
	}

	terms, _, notes := s.tenantOntologyTerms(context.Background(), mi)
	var found *meminject.OntologyTerm
	for i := range terms {
		if terms[i].Name == "internal-project" {
			found = &terms[i]
		}
	}
	if found == nil {
		names := []string{}
		for _, tm := range terms {
			names = append(names, tm.Name)
		}
		t.Fatalf("the nested type never reached the server read — have %v", names)
	}
	if found.Parent != "project" {
		t.Errorf("internal-project.Parent = %q, want project", found.Parent)
	}
	if len(found.Fields) != 1 || found.Fields[0] != "cost_center" {
		t.Errorf("fields = %v, want [cost_center]", found.Fields)
	}
	// The tree read succeeded, so the flat-Markdown fallback must NOT have reported
	// itself — a note here would mean the tree path silently lost and the caveat is
	// the only thing telling anyone.
	if len(notes) != 0 {
		t.Errorf("unexpected notes: %v", notes)
	}
}

// TestOntology_HierarchyReachesTheAssembledPrompt closes the loop phase 2 opened.
//
// A renderer that emits a beautiful tree into nothing is the failure mode this
// subsystem has shipped twice — the entity tier with no producer, graph_recall with
// nothing to walk. So the assertion is on the system prompt a run is actually given:
// the subclass is indented, it carries the field it inherited without the operator
// restating it, and the specificity instruction is present to make the ladder get used.
func TestOntology_HierarchyReachesTheAssembledPrompt(t *testing.T) {
	s, mi := ontologyFixture(t)
	def := config.AgentDef{SystemPrompt: "You extract entities.\n\n{{memory:ontology}}"}
	s.applyMemoryInjection(context.Background(), def, mi) // provisions

	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(context.Background(), mi)

	q, _ := json.Marshal(map[string]any{
		"op": "query_chunks", "scope": "tenant",
		"sql": "SELECT id, document_id FROM chunks WHERE title = 'incident' LIMIT 1",
	})
	qres, qerr := doc.Execute(dctx, q)
	if qerr != nil || qres.IsError {
		t.Fatalf("query_chunks: %v %s", qerr, qres.Text)
	}
	var qout struct {
		Rows [][]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(qres.Text), &qout); err != nil || len(qout.Rows) == 0 {
		t.Fatalf("no `incident` chunk in the template: %v %s", err, qres.Text)
	}
	parentID, docID := qout.Rows[0][0].(string), qout.Rows[0][1].(string)

	// A subclass declaring exactly ONE field, so an inherited field showing up in the
	// prompt can only have come from inheritance.
	c, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "scope": "tenant", "document_id": docID,
		"parent_id": parentID, "title": "security-incident",
		"body": "- `cve` — the identifier, if there is one\n",
	})
	if cres, cerr := doc.Execute(dctx, c); cerr != nil || cres.IsError {
		t.Fatalf("create_chunk: %v %s", cerr, cres.Text)
	}
	// CONFIRM, because a draft is inert by design and an unconfirmed document would
	// make this test pass for the wrong reason.
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)

	got, _ := s.applyMemoryInjection(context.Background(), def, mi)

	if !strings.Contains(got.SystemPrompt, "  - **security-incident**") {
		t.Errorf("the subclass is not indented in the assembled prompt:\n%s", got.SystemPrompt)
	}
	// `cause` is declared on the template's `incident`, never on the subclass.
	if !strings.Contains(got.SystemPrompt, "cve") ||
		!strings.Contains(got.SystemPrompt, "cause") {
		t.Errorf("the subclass line lacks its own or its inherited fields:\n%s", got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, "MOST SPECIFIC") {
		t.Errorf("the specificity instruction never reached the prompt:\n%s", got.SystemPrompt)
	}
}
