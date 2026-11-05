package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC FY Part C, server side: the ontology routes and the first-reference
// provisioning follow the tenant's recorded ontology, not its name.

// TestOntology_ARenamedOntologyIsNotProvisionedAgain: after the operator renames the
// ontology, the next prompt render must neither find it missing and create a second,
// draft one at the old name, nor lose the operator's terms.
func TestOntology_ARenamedOntologyIsNotProvisionedAgain(t *testing.T) {
	s, mi := ontologyFixture(t)
	def := config.AgentDef{SystemPrompt: "{{memory:ontology}}"}
	s.applyMemoryInjection(context.Background(), def, mi) // provision
	s.confirmOntology(t, mi)

	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := withTenantDocGrants(s.docToolCtx(context.Background(), mi))
	req, _ := json.Marshal(map[string]any{"op": "get_document", "scope": "tenant", "path": meminject.OntologyPath})
	res, _ := doc.Execute(dctx, req)
	var meta struct {
		DocumentID string `json:"document_id"`
	}
	if res.IsError || json.Unmarshal([]byte(res.Text), &meta) != nil || meta.DocumentID == "" {
		t.Fatalf("provisioned ontology: %s", res.Text)
	}

	// The operator renames it (an out-of-band rename: no hook sees it).
	if _, err := s.store.DirentMove(context.Background(), mi.Tenant, "tenant", "", "/memory/", "ontology", "/archive/", "onto"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	got, _ := s.applyMemoryInjection(context.Background(), def, mi)
	if s.ontologyDocExists(t, mi) {
		t.Error("a second ontology was provisioned at the old name after a rename")
	}
	if strings.Contains(got.SystemPrompt, "has not confirmed") || !strings.Contains(got.SystemPrompt, "incident") {
		t.Errorf("the renamed, confirmed ontology no longer reaches the prompt:\n%s", got.SystemPrompt)
	}
}

// TestReservedDocs_ProvisionFromInsideARun: a run's prompt render creates the
// ontology and the deployment context at their reserved names, which an agent's own
// call inside the same run may not.
func TestReservedDocs_ProvisionFromInsideARun(t *testing.T) {
	s, mi := ontologyFixture(t)
	runCtx := tools.WithRunID(context.Background(), "r_render")
	def := config.AgentDef{SystemPrompt: "{{memory:ontology}}\n\n{{memory:tenant_info}}"}
	s.applyMemoryInjection(runCtx, def, mi)

	if !s.ontologyDocExists(t, mi) {
		t.Error("the ontology was not provisioned from inside a run")
	}
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := withTenantDocGrants(s.docToolCtx(context.Background(), mi))
	req, _ := json.Marshal(map[string]any{"op": "get_document", "scope": "tenant", "path": meminject.TenantRootPath})
	if res, _ := doc.Execute(dctx, req); res.IsError {
		t.Errorf("the deployment context was not provisioned from inside a run: %s", res.Text)
	}
	id, err := doc.TenantOntologyID(dctx)
	if err != nil || id == "" {
		t.Errorf("the provisioned ontology was not recorded: %q %v", id, err)
	}
}
