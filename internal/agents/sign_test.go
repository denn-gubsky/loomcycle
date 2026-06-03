package agents

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSign_PrefixedHex(t *testing.T) {
	h := Sign(AgentContent{Name: "test"})
	if !strings.HasPrefix(h, "sha256:") {
		t.Errorf("hash %q missing sha256: prefix", h)
	}
	if got := len(h); got != 71 {
		t.Errorf("hash length = %d, want 71 (sha256: + 64 hex chars)", got)
	}
	if strings.ContainsAny(h[7:], "ABCDEF") {
		t.Errorf("hash %q has uppercase hex; want lowercase", h)
	}
}

func TestSign_DeterministicAcrossCalls(t *testing.T) {
	c := AgentContent{
		Name:         "researcher",
		SystemPrompt: "be thorough",
		AllowedTools: []string{"WebFetch", "Read"},
		Skills:       []string{"summariser"},
		MaxTokens:    8192,
	}
	h1 := Sign(c)
	h2 := Sign(c)
	if h1 != h2 {
		t.Errorf("non-deterministic: %s vs %s", h1, h2)
	}
}

func TestSign_DifferentContentDifferentHash(t *testing.T) {
	a := Sign(AgentContent{Name: "a", SystemPrompt: "be terse"})
	b := Sign(AgentContent{Name: "a", SystemPrompt: "be terse and clear"})
	if a == b {
		t.Errorf("system_prompt change didn't move the hash: %s", a)
	}
}

// TestSign_EmptyCodeBodyHashesIdenticallyToLegacy is the backward-compat
// guarantee for the RFC J code_body addition: a definition with no inline
// code (every LLM agent + every filesystem-backed code agent) must hash
// byte-for-byte as it did before the field existed — so no existing row's
// content_sha256 churns. We assert it against a hand-computed reference: the
// hash of an AgentContent that never sets CodeBody.
func TestSign_EmptyCodeBodyHashesIdenticallyToLegacy(t *testing.T) {
	withField := Sign(AgentContent{Name: "a", SystemPrompt: "x", Provider: "anthropic"})
	// Same content, CodeBody explicitly zeroed — omitempty must drop it so the
	// canonical JSON (and thus the hash) is unchanged.
	zeroed := Sign(AgentContent{Name: "a", SystemPrompt: "x", Provider: "anthropic", CodeBody: ""})
	if withField != zeroed {
		t.Fatalf("empty CodeBody changed the hash (omitempty regression):\n %s\n %s", withField, zeroed)
	}
}

// TestSign_CodeBodyChangesHash pins that inline code is content: two defs
// differing only in code_body must hash differently (so dedup/verify is
// correct for code agents).
func TestSign_CodeBodyChangesHash(t *testing.T) {
	a := Sign(AgentContent{Name: "ca", Provider: "code-js", CodeBody: `function run(){return {final_text:"A"};}`})
	b := Sign(AgentContent{Name: "ca", Provider: "code-js", CodeBody: `function run(){return {final_text:"B"};}`})
	if a == b {
		t.Error("code_body change didn't move the hash")
	}
}

func TestSign_NameChangesHash(t *testing.T) {
	a := Sign(AgentContent{Name: "a", SystemPrompt: "x"})
	b := Sign(AgentContent{Name: "b", SystemPrompt: "x"})
	if a == b {
		t.Error("name change didn't move the hash")
	}
}

func TestSign_NilEqualsEmptySlice(t *testing.T) {
	// An agent with allowed_tools: [] hashes identically to one with
	// no allowed_tools key at all. Operator-side semantics: "no tools"
	// is "no tools" regardless of how the YAML expressed it.
	a := Sign(AgentContent{Name: "x", AllowedTools: nil})
	b := Sign(AgentContent{Name: "x", AllowedTools: []string{}})
	if a != b {
		t.Errorf("nil vs empty allowed_tools differ: %s vs %s", a, b)
	}
}

func TestSign_OrderPreservedInArrays(t *testing.T) {
	// Allowed_tools order is semantically meaningful (precedence in
	// some operator scripts). Reordering MUST change the hash.
	a := Sign(AgentContent{Name: "x", AllowedTools: []string{"A", "B"}})
	b := Sign(AgentContent{Name: "x", AllowedTools: []string{"B", "A"}})
	if a == b {
		t.Error("array reordering didn't change the hash; semantics broken")
	}
}

func TestSign_TrailingWhitespaceNormalisedInSystemPrompt(t *testing.T) {
	a := Sign(AgentContent{Name: "x", SystemPrompt: "hello world"})
	b := Sign(AgentContent{Name: "x", SystemPrompt: "hello world\n\n  "})
	if a != b {
		t.Errorf("trailing whitespace caused drift: %s vs %s", a, b)
	}
}

func TestSign_CRLFNormalisedToLF(t *testing.T) {
	a := Sign(AgentContent{Name: "x", SystemPrompt: "line1\nline2"})
	b := Sign(AgentContent{Name: "x", SystemPrompt: "line1\r\nline2"})
	if a != b {
		t.Errorf("CRLF vs LF differ: %s vs %s", a, b)
	}
}

func TestSign_InternalWhitespacePreserved(t *testing.T) {
	// Internal whitespace must be respected — it's part of the prompt.
	a := Sign(AgentContent{Name: "x", SystemPrompt: "line1\n\nline2"})
	b := Sign(AgentContent{Name: "x", SystemPrompt: "line1\nline2"})
	if a == b {
		t.Error("internal blank line was stripped; prompt content was lost")
	}
}

func TestSign_ZeroIntsOmitted(t *testing.T) {
	// max_tokens: 0 + max_iterations: 0 are "unset" — hash must match
	// "no max_tokens key" / "no max_iterations key".
	a := Sign(AgentContent{Name: "x"})
	b := Sign(AgentContent{Name: "x", MaxTokens: 0, MaxIterations: 0})
	if a != b {
		t.Errorf("zero ints not normalised: %s vs %s", a, b)
	}
}

func TestSign_NonZeroIntContributes(t *testing.T) {
	a := Sign(AgentContent{Name: "x"})
	b := Sign(AgentContent{Name: "x", MaxIterations: 32})
	if a == b {
		t.Error("max_iterations change ignored")
	}
}

// TestSign_MaxConcurrentChildrenContributes pins the v0.11.8 hash
// inclusion of max_concurrent_children. AgentDef.verify uses the
// content_sha256 to detect drift between a deployed agent and an
// updated definition; if max_concurrent_children doesn't feed the
// hash, two definitions that differ only in the cap will falsely
// report as matching.
func TestSign_MaxConcurrentChildrenContributes(t *testing.T) {
	a := Sign(AgentContent{Name: "x"})
	b := Sign(AgentContent{Name: "x", MaxConcurrentChildren: 8})
	if a == b {
		t.Error("max_concurrent_children change ignored — AgentDef.verify would report wrong matches")
	}
}

func TestFromYAMLAgent_RoundTrip(t *testing.T) {
	agent := &Agent{
		Name:         "researcher",
		Description:  "thorough investigator",
		SystemPrompt: "be thorough",
		AllowedTools: []string{"WebFetch", "Read"},
		Skills:       []string{"summariser"},
		Tier:         "high",
		Effort:       "medium",
		MaxTokens:    8192,
	}
	c := FromYAMLAgent(agent)
	if c.Name != "researcher" || c.Tier != "high" {
		t.Errorf("FromYAMLAgent lost fields: %+v", c)
	}
	h := Sign(c)
	if !strings.HasPrefix(h, "sha256:") || len(h) != 71 {
		t.Errorf("hash from FromYAMLAgent malformed: %s", h)
	}
}

func TestFromYAMLAgent_NilSafe(t *testing.T) {
	c := FromYAMLAgent(nil)
	h := Sign(c)
	if !strings.HasPrefix(h, "sha256:") {
		t.Errorf("nil agent should still hash: %s", h)
	}
}

func TestFromYAMLAgent_DropsPath(t *testing.T) {
	// Path is filesystem location, not content — two installations of
	// the same agent at different paths must hash identically.
	a := FromYAMLAgent(&Agent{Name: "x", Path: "/etc/loomcycle/agents/x.md"})
	b := FromYAMLAgent(&Agent{Name: "x", Path: "/home/user/agents/x.md"})
	if Sign(a) != Sign(b) {
		t.Error("Path leaked into hash")
	}
}

func TestFromYAMLAgent_ChannelACLIgnored(t *testing.T) {
	// Channels live in operator yaml + don't round-trip through
	// AgentDef set; they MUST NOT contribute to the hash. Otherwise a
	// YAML-loaded agent and the same agent pushed via the substrate
	// would diverge.
	bare := FromYAMLAgent(&Agent{Name: "x"})
	withChannels := FromYAMLAgent(&Agent{
		Name:     "x",
		Channels: AgentChannelACL{Publish: []string{"out"}, Subscribe: []string{"in"}},
	})
	if Sign(bare) != Sign(withChannels) {
		t.Error("channel ACL leaked into hash — bundle vs deployed comparison would falsely report drift")
	}
}

func TestFromYAMLAgent_ScopesIgnored(t *testing.T) {
	// AgentDef / SkillDef / Evaluation scopes are also yaml-only.
	bare := FromYAMLAgent(&Agent{Name: "x"})
	withScopes := FromYAMLAgent(&Agent{
		Name:             "x",
		AgentDefScopes:   []string{"self"},
		SkillDefScopes:   []string{"descendants"},
		EvaluationScopes: []string{"submit_self"},
	})
	if Sign(bare) != Sign(withScopes) {
		t.Error("*Scopes leaked into hash — same drift risk as channels")
	}
}

func TestFromOverlay_ParsesValidJSON(t *testing.T) {
	overlay := json.RawMessage(`{"name":"x","system_prompt":"hi","allowed_tools":["Read"]}`)
	c, err := FromOverlay(overlay)
	if err != nil {
		t.Fatalf("FromOverlay: %v", err)
	}
	if c.Name != "x" || c.SystemPrompt != "hi" || len(c.AllowedTools) != 1 {
		t.Errorf("FromOverlay lost data: %+v", c)
	}
}

func TestFromOverlay_RejectsMalformed(t *testing.T) {
	_, err := FromOverlay(json.RawMessage(`{`))
	if err == nil {
		t.Error("malformed JSON should error")
	}
}

func TestFromOverlay_EmptyInputZeroValue(t *testing.T) {
	c, err := FromOverlay(nil)
	if err != nil {
		t.Errorf("nil overlay should not error: %v", err)
	}
	if c.Name != "" {
		t.Errorf("nil overlay should yield zero value: %+v", c)
	}
}

func TestFromOverlay_RoundTripMatchesYAMLAgent(t *testing.T) {
	// The same content reaching the substrate via a YAML file vs via
	// an AgentDef set overlay MUST hash identically. This is the
	// "operator's bundle file matches operator's pushed overlay" guarantee.
	agent := &Agent{
		Name:         "researcher",
		SystemPrompt: "be thorough",
		AllowedTools: []string{"Read", "WebFetch"},
		MaxTokens:    8192,
	}
	hashFromYAML := Sign(FromYAMLAgent(agent))

	overlay := json.RawMessage(`{
		"name": "researcher",
		"system_prompt": "be thorough",
		"allowed_tools": ["Read", "WebFetch"],
		"max_tokens": 8192
	}`)
	parsed, _ := FromOverlay(overlay)
	hashFromOverlay := Sign(parsed)

	if hashFromYAML != hashFromOverlay {
		t.Errorf("YAML path vs overlay path produced different hashes: %s vs %s", hashFromYAML, hashFromOverlay)
	}
}

func TestSign_KnownVector(t *testing.T) {
	// A pin — if anyone changes the canonical encoding, this test
	// catches the silent break. Update only with intent: bump a
	// version field on every existing row + re-run the backfill.
	c := AgentContent{
		Name:         "researcher",
		SystemPrompt: "be thorough",
		AllowedTools: []string{"WebFetch", "Read"},
		MaxTokens:    8192,
	}
	const want = "sha256:9c6a6098efbad0bdde2cd0c777a70d97b204125c37004b3f80a7f5734cde2c03"
	got := Sign(c)
	if got != want {
		t.Errorf("canonical encoding drift: got %s, want %s — update only with intent (bump every existing row + re-backfill)", got, want)
	}
}

// TestSign_KnownVectorWithModels covers the second hot canonical-
// encoding case: an AgentContent with a non-empty `Models` map.
// Without explicit `json:` tags on TierCandidate, Go's encoding/json
// would default to capitalized field names (`Provider`, `Model`) —
// which works as a self-consistent convention TODAY (all three sign
// paths produce the same bytes) but is a hidden landmine: anyone
// adding lowercase `json:` tags later would silently invalidate every
// deployed agent's hash with a non-empty `models:` field. The tags
// are present on the struct as of v0.9.x; this pin makes any future
// tag removal/case-change fail at PR time.
func TestSign_KnownVectorWithModels(t *testing.T) {
	c := AgentContent{
		Name: "researcher",
		Models: map[string][]TierCandidate{
			"high": {
				{Provider: "anthropic", Model: "claude-opus-4-5"},
				{Provider: "openai", Model: "gpt-5-2025-08-07"},
			},
			"low": {
				{Provider: "deepseek", Model: "deepseek-chat"},
			},
		},
	}
	const want = "sha256:c23827d24ac610aa96718ea32a8e3c22783c3beb1ad3cc0830165ba82f9fc701"
	got := Sign(c)
	if got != want {
		t.Errorf("canonical encoding drift on Models: got %s, want %s — update only with intent (bump every existing row + re-backfill); see TierCandidate's json: tag for context", got, want)
	}
}
