package http

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// applyAgentDefOverlay is the helper that merges an agent_defs row's
// JSON definition over a static cfg.Agents entry for one sub-run.
// These tests pin the merge semantics described in the v0.8.5 PR 5 RFC:
//   - non-empty / non-zero fields override the static base
//   - nil slices / maps / zero ints leave the base untouched
//   - explicit Model pin clears static Tier (and vice versa) so the
//     Pin XOR Tier invariant survives the overlay
//   - substrate policy fields (agent_def_scopes, evaluation_scopes)
//     are NOT in the merge surface — they stay with the static yaml.

func TestApplyAgentDefOverlay_Empty(t *testing.T) {
	base := config.AgentDef{
		Provider:     "anthropic",
		Model:        "claude-haiku-4-5",
		SystemPrompt: "you are a helpful agent",
		AllowedTools: []string{"Read", "Write"},
	}
	got := applyAgentDefOverlay(base, json.RawMessage{})
	if !reflect.DeepEqual(got, base) {
		t.Errorf("empty overlay should not modify base; got %+v", got)
	}
}

func TestApplyAgentDefOverlay_SystemPromptAndAllowedTools(t *testing.T) {
	base := config.AgentDef{
		Provider:     "anthropic",
		Model:        "claude-haiku-4-5",
		SystemPrompt: "static prompt",
		AllowedTools: []string{"Read", "Write"},
	}
	def := json.RawMessage(`{
		"system_prompt": "forked prompt",
		"allowed_tools": ["Read"]
	}`)
	got := applyAgentDefOverlay(base, def)
	if got.SystemPrompt != "forked prompt" {
		t.Errorf("system_prompt = %q, want forked prompt", got.SystemPrompt)
	}
	if !reflect.DeepEqual(got.AllowedTools, []string{"Read"}) {
		t.Errorf("allowed_tools = %v, want [Read]", got.AllowedTools)
	}
	// Untouched: provider, model.
	if got.Provider != "anthropic" || got.Model != "claude-haiku-4-5" {
		t.Errorf("untouched fields drifted: %+v", got)
	}
}

func TestApplyAgentDefOverlay_TierOverridesStaticModelPin(t *testing.T) {
	// Static had an explicit model pin; fork moves to tier-driven
	// resolution. Pin XOR Tier — the overlay must clear Model.
	base := config.AgentDef{
		Provider: "anthropic",
		Model:    "claude-haiku-4-5",
	}
	def := json.RawMessage(`{"tier": "low"}`)
	got := applyAgentDefOverlay(base, def)
	if got.Tier != "low" {
		t.Errorf("tier = %q, want low", got.Tier)
	}
	if got.Model != "" {
		t.Errorf("model should clear when tier set; got %q", got.Model)
	}
}

func TestApplyAgentDefOverlay_BothModelAndTier_PrefersModel(t *testing.T) {
	// Defensive: a row that somehow carries both model AND tier (which
	// AgentDef.create rejects) must collapse to one choice rather than
	// passing the invariant violation through to the resolver. Prefer
	// the more specific intent (Model wins, Tier cleared).
	base := config.AgentDef{Provider: "anthropic"}
	def := json.RawMessage(`{"model": "claude-haiku-4-5", "tier": "low"}`)
	got := applyAgentDefOverlay(base, def)
	if got.Model != "claude-haiku-4-5" {
		t.Errorf("model = %q, want claude-haiku-4-5", got.Model)
	}
	if got.Tier != "" {
		t.Errorf("tier should clear when model also set; got %q", got.Tier)
	}
}

func TestApplyAgentDefOverlay_ModelOverridesStaticTier(t *testing.T) {
	// Mirror: static was tier-driven; fork pins a specific model.
	base := config.AgentDef{
		Provider: "anthropic",
		Tier:     "low",
	}
	def := json.RawMessage(`{"model": "claude-haiku-4-5"}`)
	got := applyAgentDefOverlay(base, def)
	if got.Model != "claude-haiku-4-5" {
		t.Errorf("model = %q, want claude-haiku-4-5", got.Model)
	}
	if got.Tier != "" {
		t.Errorf("tier should clear when model pinned; got %q", got.Tier)
	}
}

func TestApplyAgentDefOverlay_MaxTokens(t *testing.T) {
	base := config.AgentDef{MaxTokens: 8192}
	def := json.RawMessage(`{"max_tokens": 24576}`)
	got := applyAgentDefOverlay(base, def)
	if got.MaxTokens != 24576 {
		t.Errorf("max_tokens = %d, want 24576", got.MaxTokens)
	}
	// Zero max_tokens in overlay must NOT zero the base — zero means
	// "absent" in the overlay protocol, not "explicit zero".
	got = applyAgentDefOverlay(base, json.RawMessage(`{"max_tokens": 0}`))
	if got.MaxTokens != 8192 {
		t.Errorf("absent max_tokens should keep base; got %d", got.MaxTokens)
	}
}

func TestApplyAgentDefOverlay_MalformedJSONReturnsBase(t *testing.T) {
	base := config.AgentDef{Model: "claude-haiku-4-5"}
	got := applyAgentDefOverlay(base, json.RawMessage(`{not json`))
	if got.Model != "claude-haiku-4-5" {
		t.Errorf("malformed JSON should fall back to base; got %+v", got)
	}
}

func TestApplyAgentDefOverlay_SubstratePolicyFieldsNotMerged(t *testing.T) {
	// agent_def_scopes / evaluation_scopes live on the static yaml
	// only. A row's Definition JSON may contain other keys, but the
	// overlay must not import them — those are the operator's
	// substrate-capability gate and not subject to fork-time mutation.
	base := config.AgentDef{
		AgentDefScopes:   []string{"any"},
		EvaluationScopes: []string{"submit_self", "read_any"},
	}
	def := json.RawMessage(`{
		"agent_def_scopes": ["fork_self"],
		"evaluation_scopes": ["submit_any"]
	}`)
	got := applyAgentDefOverlay(base, def)
	if !reflect.DeepEqual(got.AgentDefScopes, []string{"any"}) {
		t.Errorf("AgentDefScopes drifted: %v", got.AgentDefScopes)
	}
	if !reflect.DeepEqual(got.EvaluationScopes, []string{"submit_self", "read_any"}) {
		t.Errorf("EvaluationScopes drifted: %v", got.EvaluationScopes)
	}
}

func TestApplyAgentDefOverlay_NilSlicesKeepBase(t *testing.T) {
	// Absent slice keys in JSON decode to nil, which the overlay must
	// treat as "keep base", not "zero out base".
	base := config.AgentDef{
		AllowedTools: []string{"Read", "Write"},
		Skills:       []string{"position-relevance"},
	}
	def := json.RawMessage(`{"system_prompt": "new prompt"}`)
	got := applyAgentDefOverlay(base, def)
	if !reflect.DeepEqual(got.AllowedTools, base.AllowedTools) {
		t.Errorf("AllowedTools drifted on partial overlay: %v", got.AllowedTools)
	}
	if !reflect.DeepEqual(got.Skills, base.Skills) {
		t.Errorf("Skills drifted on partial overlay: %v", got.Skills)
	}
}

// v0.9.x — applyAgentDefOverlay must read max_iterations from the
// agent_defs row's definition JSON and surface it on the effective
// AgentDef. Without this the runSubAgent loop.Run call site (which
// reads agentDef.MaxIterations) defaults to 0 → loop default 16,
// which is the exact failure mode PR #168 fixes for yaml-declared
// agents but NOT for dynamic forks.
func TestApplyAgentDefOverlay_MaxIterationsThreadsThrough(t *testing.T) {
	base := config.AgentDef{
		Provider:      "anthropic",
		Model:         "claude-haiku-4-5",
		MaxIterations: 16, // pretend static yaml set this
	}
	def := json.RawMessage(`{"max_iterations": 64}`)
	got := applyAgentDefOverlay(base, def)
	if got.MaxIterations != 64 {
		t.Errorf("MaxIterations = %d, want 64 (overlay must win over static)", got.MaxIterations)
	}
}

// Zero-value overlay must NOT clobber the static yaml's
// max_iterations. Mirrors the MaxTokens contract — only positive
// values override.
func TestApplyAgentDefOverlay_ZeroMaxIterationsFallsThrough(t *testing.T) {
	base := config.AgentDef{
		Provider:      "anthropic",
		Model:         "claude-haiku-4-5",
		MaxIterations: 32,
	}
	// Overlay carries max_tokens but no max_iterations.
	def := json.RawMessage(`{"max_tokens": 8192}`)
	got := applyAgentDefOverlay(base, def)
	if got.MaxIterations != 32 {
		t.Errorf("static MaxIterations should pass through when overlay omits it; got %d, want 32", got.MaxIterations)
	}
	if got.MaxTokens != 8192 {
		t.Errorf("MaxTokens = %d, want 8192", got.MaxTokens)
	}
}
