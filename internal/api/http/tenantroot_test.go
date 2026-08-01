package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
)

// TestTenantInfo_ReachesTheAssembledPromptAndProvisions is the wiring, asserted on
// the prompt a run actually gets.
//
// `tenant_info` was an ACCEPTED-BUT-EMPTY variant for four phases: the name
// validated, the placeholder expanded, and it always rendered nothing because the
// document behind it was never built. A prompt referencing it looked correct and
// carried no content — which is why this asserts on the assembled prompt rather
// than on the renderer.
func TestTenantInfo_ReachesTheAssembledPromptAndProvisions(t *testing.T) {
	s, mi := ontologyFixture(t)
	def := config.AgentDef{SystemPrompt: "You are CHAT.\n\n{{memory:tenant_info}}"}

	got, _ := s.applyMemoryInjection(context.Background(), def, mi)

	if strings.Contains(got.SystemPrompt, "{{memory:") {
		t.Errorf("placeholder left unexpanded:\n%s", got.SystemPrompt)
	}
	// The template's own headings, so this fails if the document was provisioned
	// from something else or not at all.
	for _, want := range []string{"Deployment Context", "Systems and vocabulary", "Standing conventions"} {
		if !strings.Contains(got.SystemPrompt, want) {
			t.Errorf("missing %q from the assembled prompt — tenant_info rendered empty:\n%s", want, got.SystemPrompt)
		}
	}
	// FRAMED, unlike the ontology: this is operator-authored prose, not a schema
	// the model applies, so it goes in as content the model should treat as data.
	if !strings.Contains(got.SystemPrompt, "<memory source=\"tenant_info\">") {
		t.Errorf("tenant_info must be framed as untrusted content:\n%s", got.SystemPrompt)
	}
}

// TestTenantInfo_NotReferencedNeverProvisions: rendering PROVISIONS a document, so a
// prompt that never mentions it must not create one as a side effect of being
// assembled. The same rule user_info and ontology follow.
func TestTenantInfo_NotReferencedNeverProvisions(t *testing.T) {
	s, mi := ontologyFixture(t)
	def := config.AgentDef{SystemPrompt: "A prompt with no placeholders."}

	got, _ := s.applyMemoryInjection(context.Background(), def, mi)
	if got.SystemPrompt != def.SystemPrompt {
		t.Errorf("the fast path should return byte-identical, got:\n%s", got.SystemPrompt)
	}
	if s.readTenantRootMarkdown(context.Background(), mi) != "" {
		t.Error("an unreferenced tenant root must not be provisioned")
	}
}

// TestTenantInfo_IsTenantScopedNotPerUser: two users of one tenant must read the
// SAME document. If it were provisioned per-principal the deployment's vocabulary
// would fork silently, one copy per person, and an operator editing "the" document
// would fix it for exactly one of them.
func TestTenantInfo_IsTenantScopedNotPerUser(t *testing.T) {
	s, mi := ontologyFixture(t)
	def := config.AgentDef{SystemPrompt: "{{memory:tenant_info}}"}

	s.applyMemoryInjection(context.Background(), def, mi) // provisions for alice

	other := mi
	other.UserID = "bob"
	body := s.readTenantRootMarkdown(context.Background(), other)
	if body == "" {
		t.Fatal("a second user of the same tenant reads nothing — the document forked per user")
	}
	if !strings.Contains(body, meminject.TenantRootTitle) {
		t.Errorf("bob read something other than the tenant root:\n%s", body)
	}
}

// TestTenantInfo_NeedsNoTenantGrantOnTheAgent: the chat agents that consume this are
// deliberately user-scoped (`memory_scopes: [user]`). The read stamps the tenant
// grants server-side because it is operator-authored config on the run's own
// tenant, not the agent reaching tenant memory — so requiring the grant would mean
// widening every consuming agent to tenant scope to read a paragraph of prose.
func TestTenantInfo_NeedsNoTenantGrantOnTheAgent(t *testing.T) {
	s, mi := ontologyFixture(t)
	// No policy stamped on the context at all — the weakest possible caller.
	if body := s.renderTenantInfo(context.Background(), mi); body == "" {
		t.Error("tenant_info must render for an agent holding no tenant grant")
	}
}
