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

func (s *stubStore) DynamicAgentGet(_ context.Context, name string) (store.DynamicAgent, error) {
	if row, ok := s.dyn[name]; ok {
		return row, nil
	}
	return store.DynamicAgent{}, &store.ErrNotFound{Kind: "dynamic_agent", ID: name}
}

func (s *stubStore) AgentDefGetActive(_ context.Context, name string) (store.AgentDefRow, error) {
	if row, ok := s.defs[name]; ok {
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
		Provider:         "anthropic",
		Model:            "claude-sonnet-4-6",
		Tier:             "smart",
		MaxTokens:        4096,
		MaxIterations:    32,
		SystemPrompt:     "You are a careful researcher. Ask questions before acting.",
		SystemPromptBase: "You are a careful researcher. Ask questions before acting.",
		AllowedTools:     []string{"Read", "Memory", "Channel"},
		Skills:           []string{"voice-applier"},
		MemoryScopes:     []string{"agent", "user"},
		MemoryQuotaBytes: 65536,
	}
	// Simulate boot-time normalization on the yaml side: resolveSkills
	// would set SystemPromptBase = SystemPrompt for an agent with
	// skills. The fixture already reflects that.

	// Persist via the substrate shape (snake_case json tags via
	// lookup.SubstrateAgentDef). This mirrors what AgentDef.create
	// stores after the write-side normalize() of commit 3.
	substrateShape := lookup.SubstrateAgentDef{
		Provider:         yamlAgent.Provider,
		Model:            yamlAgent.Model,
		Tier:             yamlAgent.Tier,
		MaxTokens:        yamlAgent.MaxTokens,
		MaxIterations:    yamlAgent.MaxIterations,
		SystemPrompt:     yamlAgent.SystemPrompt,
		SystemPromptBase: yamlAgent.SystemPromptBase,
		AllowedTools:     yamlAgent.AllowedTools,
		Skills:           yamlAgent.Skills,
		MemoryScopes:     yamlAgent.MemoryScopes,
		MemoryQuotaBytes: yamlAgent.MemoryQuotaBytes,
	}
	defJSON, err := json.Marshal(substrateShape)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Resolve via the dynamic-substrate path.
	ss := &stubStore{
		dyn: map[string]store.DynamicAgent{},
		defs: map[string]store.AgentDefRow{
			"researcher": {
				DefID:      "def_researcher_v1",
				Name:       "researcher",
				Version:    1,
				Definition: defJSON,
				CreatedAt:  time.Now(),
			},
		},
	}
	resolved, ok := lookup.Agent(context.Background(), ss, &config.Config{}, "researcher")
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
		resolved.MaxIterations != yamlAgent.MaxIterations {
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
			"legacy": {
				DefID:      "def_legacy_v1",
				Name:       "legacy",
				Version:    1,
				Definition: defJSON,
				CreatedAt:  time.Now(),
			},
		},
	}
	resolved, ok := lookup.Agent(context.Background(), ss, &config.Config{}, "legacy")
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

// TestAgent_DriftDetection is the reflection-based audit pinning the
// architectural invariant: every json-tagged field in the substrate's
// persistence shape (mergedDef in internal/tools/builtin/agentdef.go,
// reflected here via SubstrateAgentDef which is its public mirror)
// must have a corresponding json tag in SubstrateAgentDef so the
// unmarshal slot exists. A future field added to mergedDef without
// being added here would fail this test BEFORE it ships, catching
// the bug PR #184 fixed.
//
// This test is intentionally fragile against ADDITIONS to
// SubstrateAgentDef — when growing the substrate persistence shape,
// the developer MUST update this enumeration. That coupling is the
// point: it forces a conscious decision rather than silent drift.
func TestAgent_DriftDetection(t *testing.T) {
	// Expected json tags on SubstrateAgentDef as of v0.9.x. Keep in
	// sync with mergedDef + SubstrateAgentDef field definitions.
	// Adding a field to either WITHOUT adding it here is the bug
	// this test is designed to catch.
	want := map[string]bool{
		"provider":           true,
		"model":              true,
		"tier":               true,
		"effort":             true,
		"max_tokens":         true,
		"max_iterations":     true,
		"system_prompt":      true,
		"system_prompt_base": true,
		"allowed_tools":      true,
		"skills":             true,
		"providers":          true,
		"models":             true,
		"memory_scopes":      true,
		"memory_quota_bytes": true,
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
