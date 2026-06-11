package lookup_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// stubStore is a minimal in-memory AgentStore for the equivalence
// tests. Sufficient for unit-level coverage of the resolver chain;
// the integration tests in api/http/server_test.go exercise the
// real store-backed paths.
type stubStore struct {
	dyn  map[string]store.DynamicAgent
	defs map[string]store.AgentDefRow // keyed by name (active)
}

// stubKey composes the (tenant, name) map key so the stub honours the
// RFC N tenant axis. Tests that don't care about tenancy use "".
func stubKey(tenantID, name string) string { return tenantID + "\x00" + name }

func (s *stubStore) DynamicAgentGet(_ context.Context, tenantID, name string) (store.DynamicAgent, error) {
	if row, ok := s.dyn[stubKey(tenantID, name)]; ok {
		return row, nil
	}
	return store.DynamicAgent{}, &store.ErrNotFound{Kind: "dynamic_agent", ID: name}
}

func (s *stubStore) AgentDefGetActive(_ context.Context, tenantID, name string) (store.AgentDefRow, error) {
	if row, ok := s.defs[stubKey(tenantID, name)]; ok {
		return row, nil
	}
	return store.AgentDefRow{}, &store.ErrNotFound{Kind: "agent_def_active", ID: name}
}

// TestAgent_EquivalenceYamlVsSubstrate pins the architectural contract:
// loading the same content via the yaml path (cfg.Agents) vs the
// substrate path (agent_defs row, decoded by lookup.Agent) MUST
// produce byte-equivalent config.AgentDef structs after normalization.
//
// This catches the entire class of drift bugs PRs #184 + #186 fixed:
// any future field added to config.AgentDef that needs a static-path
// normalization but not a dynamic one — or vice versa — fails this
// test. The two paths must stay in lockstep.
func TestAgent_EquivalenceYamlVsSubstrate(t *testing.T) {
	// Seed agent — representative of a realistic content shape:
	// non-trivial system_prompt, allowed_tools, skills, model+tier.
	yamlAgent := config.AgentDef{
		Provider:              "anthropic",
		Model:                 "claude-sonnet-4-6",
		Tier:                  "smart",
		MaxTokens:             4096,
		MaxIterations:         32,
		MaxConcurrentChildren: 8,
		SystemPrompt:          "You are a careful researcher. Ask questions before acting.",
		SystemPromptBase:      "You are a careful researcher. Ask questions before acting.",
		AllowedTools:          []string{"Read", "Memory", "Channel"},
		Skills:                []string{"voice-applier"},
		MemoryScopes:          []string{"agent", "user"},
		MemoryQuotaBytes:      65536,
		MemoryBackend:         "team-mem9",
	}
	// Simulate boot-time normalization on the yaml side: resolveSkills
	// would set SystemPromptBase = SystemPrompt for an agent with
	// skills. The fixture already reflects that.

	// Persist via the substrate shape (snake_case json tags via
	// lookup.SubstrateAgentDef). This mirrors what AgentDef.create
	// stores after the write-side normalize() of commit 3.
	substrateShape := lookup.SubstrateAgentDef{
		Provider:              yamlAgent.Provider,
		Model:                 yamlAgent.Model,
		Tier:                  yamlAgent.Tier,
		MaxTokens:             yamlAgent.MaxTokens,
		MaxIterations:         yamlAgent.MaxIterations,
		MaxConcurrentChildren: yamlAgent.MaxConcurrentChildren,
		SystemPrompt:          yamlAgent.SystemPrompt,
		SystemPromptBase:      yamlAgent.SystemPromptBase,
		AllowedTools:          yamlAgent.AllowedTools,
		Skills:                yamlAgent.Skills,
		MemoryScopes:          yamlAgent.MemoryScopes,
		MemoryQuotaBytes:      yamlAgent.MemoryQuotaBytes,
		MemoryBackend:         yamlAgent.MemoryBackend,
	}
	defJSON, err := json.Marshal(substrateShape)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Resolve via the dynamic-substrate path.
	ss := &stubStore{
		dyn: map[string]store.DynamicAgent{},
		defs: map[string]store.AgentDefRow{
			stubKey("", "researcher"): {
				DefID:      "def_researcher_v1",
				Name:       "researcher",
				Version:    1,
				Definition: defJSON,
				CreatedAt:  time.Now(),
			},
		},
	}
	resolved, ok := lookup.Agent(context.Background(), ss, &config.Config{}, "", "researcher")
	if !ok {
		t.Fatal("resolver returned !ok")
	}

	// Equivalence: every field that crosses the wire must arrive intact.
	// (HistoryScope and other yaml-only ACL fields stay at their zero
	// value on the substrate path — that's by design; the substrate's
	// mergedDef deliberately excludes them so a fork can't widen
	// operator-policy fields.)
	if resolved.Provider != yamlAgent.Provider ||
		resolved.Model != yamlAgent.Model ||
		resolved.Tier != yamlAgent.Tier ||
		resolved.MaxTokens != yamlAgent.MaxTokens ||
		resolved.MaxIterations != yamlAgent.MaxIterations ||
		resolved.MaxConcurrentChildren != yamlAgent.MaxConcurrentChildren {
		t.Errorf("scalar mismatch:\n  yaml: %+v\n  resolved: %+v", yamlAgent, resolved)
	}
	if resolved.SystemPrompt != yamlAgent.SystemPrompt {
		t.Errorf("SystemPrompt mismatch:\n  yaml: %q\n  resolved: %q", yamlAgent.SystemPrompt, resolved.SystemPrompt)
	}
	if resolved.SystemPromptBase != yamlAgent.SystemPromptBase {
		t.Errorf("SystemPromptBase mismatch:\n  yaml: %q\n  resolved: %q", yamlAgent.SystemPromptBase, resolved.SystemPromptBase)
	}
	if !reflect.DeepEqual(resolved.AllowedTools, yamlAgent.AllowedTools) {
		t.Errorf("AllowedTools mismatch:\n  yaml: %v\n  resolved: %v", yamlAgent.AllowedTools, resolved.AllowedTools)
	}
	if !reflect.DeepEqual(resolved.Skills, yamlAgent.Skills) {
		t.Errorf("Skills mismatch:\n  yaml: %v\n  resolved: %v", yamlAgent.Skills, resolved.Skills)
	}
	if !reflect.DeepEqual(resolved.MemoryScopes, yamlAgent.MemoryScopes) {
		t.Errorf("MemoryScopes mismatch:\n  yaml: %v\n  resolved: %v", yamlAgent.MemoryScopes, resolved.MemoryScopes)
	}
	if resolved.MemoryQuotaBytes != yamlAgent.MemoryQuotaBytes {
		t.Errorf("MemoryQuotaBytes mismatch:\n  yaml: %d\n  resolved: %d", yamlAgent.MemoryQuotaBytes, resolved.MemoryQuotaBytes)
	}
	if resolved.MemoryBackend != yamlAgent.MemoryBackend {
		t.Errorf("MemoryBackend mismatch:\n  yaml: %q\n  resolved: %q", yamlAgent.MemoryBackend, resolved.MemoryBackend)
	}
}

// TestAgent_LegacyRowGetsSystemPromptBaseFilledOnRead verifies the
// belt-and-suspenders posture: a row persisted BEFORE the
// commit-3 write-side normalize() (i.e., system_prompt_base missing
// from the JSON) is still served correctly because the read-side
// NormalizeAgentDef fills it. This is the production scenario the
// SystemPromptBase backfill exists to close — but it must also
// work on a row that hasn't been backfilled yet.
func TestAgent_LegacyRowGetsSystemPromptBaseFilledOnRead(t *testing.T) {
	// Persist WITHOUT system_prompt_base (legacy shape).
	defJSON, err := json.Marshal(map[string]any{
		"system_prompt": "be helpful",
		"allowed_tools": []string{"Read"},
		"skills":        []string{"summariser"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ss := &stubStore{
		dyn: map[string]store.DynamicAgent{},
		defs: map[string]store.AgentDefRow{
			stubKey("", "legacy"): {
				DefID:      "def_legacy_v1",
				Name:       "legacy",
				Version:    1,
				Definition: defJSON,
				CreatedAt:  time.Now(),
			},
		},
	}
	resolved, ok := lookup.Agent(context.Background(), ss, &config.Config{}, "", "legacy")
	if !ok {
		t.Fatal("resolver returned !ok")
	}
	if resolved.SystemPromptBase != "be helpful" {
		t.Errorf("SystemPromptBase not filled by read-side normalizer: got %q, want %q",
			resolved.SystemPromptBase, "be helpful")
	}
	if resolved.SystemPrompt != "be helpful" {
		t.Errorf("SystemPrompt unexpectedly mutated: %q", resolved.SystemPrompt)
	}
}

// TestAgent_TenantResolutionPrecedence pins the RFC N resolution
// model: a tenant-scoped dynamic registration shadows the shared
// static base by name, while a different tenant resolving the same name
// falls through to the shared static base (it cannot see tenant A's
// private def).
func TestAgent_TenantResolutionPrecedence(t *testing.T) {
	tenantDefJSON, err := json.Marshal(map[string]any{"system_prompt": "tenant-A private"})
	if err != nil {
		t.Fatal(err)
	}
	ss := &stubStore{
		dyn: map[string]store.DynamicAgent{},
		defs: map[string]store.AgentDefRow{
			stubKey("tenant-a", "shared"): {
				DefID:      "def_a_v1",
				Name:       "shared",
				Version:    1,
				Definition: tenantDefJSON,
			},
		},
	}
	cfg := &config.Config{Agents: map[string]config.AgentDef{
		"shared": {SystemPrompt: "operator shared base"},
	}}

	// Tenant A: its dynamic def shadows the shared static base.
	gotA, ok := lookup.Agent(context.Background(), ss, cfg, "tenant-a", "shared")
	if !ok {
		t.Fatal("tenant-a resolve !ok")
	}
	if gotA.SystemPrompt != "tenant-A private" {
		t.Errorf("tenant-a: got %q, want its own dynamic def", gotA.SystemPrompt)
	}

	// Tenant B: no private def → falls through to the shared static base.
	// It must NOT see tenant A's private def.
	gotB, ok := lookup.Agent(context.Background(), ss, cfg, "tenant-b", "shared")
	if !ok {
		t.Fatal("tenant-b resolve !ok")
	}
	if gotB.SystemPrompt != "operator shared base" {
		t.Errorf("tenant-b: got %q, want the shared static base (no cross-tenant leak)", gotB.SystemPrompt)
	}
}

// TestAgent_DefaultTenantPreservesStaticFirstOrder pins the back-compat
// invariant: for the default tenant "", a name present in BOTH static
// cfg.Agents AND the shared dynamic tier resolves to the STATIC one —
// identical to the pre-RFC-N "cfg.Agents first, then dynamic" order.
func TestAgent_DefaultTenantPreservesStaticFirstOrder(t *testing.T) {
	dynJSON, err := json.Marshal(map[string]any{"system_prompt": "dynamic shadow"})
	if err != nil {
		t.Fatal(err)
	}
	ss := &stubStore{
		dyn: map[string]store.DynamicAgent{
			stubKey("", "dual"): {Name: "dual", Definition: dynJSON},
		},
		defs: map[string]store.AgentDefRow{},
	}
	cfg := &config.Config{Agents: map[string]config.AgentDef{
		"dual": {SystemPrompt: "static wins"},
	}}
	got, ok := lookup.Agent(context.Background(), ss, cfg, "", "dual")
	if !ok {
		t.Fatal("resolve !ok")
	}
	if got.SystemPrompt != "static wins" {
		t.Errorf("default tenant precedence broke: got %q, want the static cfg.Agents entry", got.SystemPrompt)
	}
}

// TestAgent_DriftDetection pins the SubstrateAgentDef field set
// against an explicit `want` enumeration. A field added to or removed
// from SubstrateAgentDef without updating this enumeration fails.
//
// This test catches one direction of drift: changes to
// SubstrateAgentDef. The complementary direction — a field added to
// mergedDef in internal/tools/builtin/agentdef.go but accidentally
// NOT mirrored in SubstrateAgentDef — is covered by
// TestMergedDef_DriftDetection_VsLookupSubstrateAgentDef in the
// builtin package, where mergedDef is in-scope for reflection. Both
// tests are needed to close the full drift loop.
func TestAgent_DriftDetection(t *testing.T) {
	// Expected json tags on SubstrateAgentDef as of v0.9.x. Keep in
	// sync with mergedDef + SubstrateAgentDef field definitions.
	// Adding a field to either WITHOUT adding it here is the bug
	// this test is designed to catch.
	want := map[string]bool{
		"provider":                true,
		"model":                   true,
		"code_body":               true, // RFC J inline code-js body
		"tier":                    true,
		"effort":                  true,
		"sampling":                true, // per-agent LLM sampling params (temperature, top_p, …)
		"max_tokens":              true,
		"max_iterations":          true,
		"unbounded_iterations":    true, // lift the iteration soft-cap for interactive LLM agents
		"max_concurrent_children": true,
		"system_prompt":           true,
		"system_prompt_base":      true,
		"allowed_tools":           true,
		"skills":                  true,
		"providers":               true,
		"models":                  true,
		"memory_scopes":           true,
		"memory_quota_bytes":      true,
		"memory_backend":          true,
		"retry_attempts":          true,
		"run_timeout_seconds":     true, // RFC J per-agent code-js budget (operational, not hashed)
		"channels":                true, // F14 — Channel tool ACL on MCP/HTTP-authored agents
		"evaluation_scopes":       true, // F14 — Evaluation tool scope gate
		"interruption":            true, // F14 — Interruption tool gate (enabled/kinds/max_pending)
		// F40 — the *_def_scopes capability gates (substrate-def slice of the
		// F14 closure) so a runtime-authored meta-agent can fork/schedule others.
		"agent_def_scopes":           true,
		"schedule_def_scopes":        true,
		"skill_def_scopes":           true,
		"a2a_server_card_def_scopes": true,
		"a2a_agent_def_scopes":       true,
	}
	have := jsonTagsOf(reflect.TypeOf(lookup.SubstrateAgentDef{}))
	for tag := range want {
		if !have[tag] {
			t.Errorf("SubstrateAgentDef missing json tag %q (must mirror mergedDef in internal/tools/builtin/agentdef.go)", tag)
		}
	}
	for tag := range have {
		if !want[tag] {
			t.Errorf("SubstrateAgentDef has json tag %q not in expected set — if this field was deliberately added, update the `want` map in this test to confirm the addition was conscious", tag)
		}
	}
}

// TestSubstrateAgentDef_UnboundedIterations_ToConfigDef pins the read-side
// projection: a substrate def with unbounded_iterations resolves to a
// config.AgentDef carrying it (catches a dropped ToConfigDef line).
func TestSubstrateAgentDef_UnboundedIterations_ToConfigDef(t *testing.T) {
	got := lookup.SubstrateAgentDef{UnboundedIterations: true}.ToConfigDef()
	if !got.UnboundedIterations {
		t.Error("ToConfigDef dropped UnboundedIterations")
	}
}

// jsonTagsOf walks a struct's exported fields and returns the set of
// json tag names (the part before any "," for `,omitempty` etc.).
func jsonTagsOf(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		// Strip ",omitempty" and similar.
		for j, c := range tag {
			if c == ',' {
				tag = tag[:j]
				break
			}
		}
		out[tag] = true
	}
	return out
}
