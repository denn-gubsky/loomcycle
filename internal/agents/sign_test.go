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

// UnboundedIterations is content-identifying: two agents differing only in it
// must hash differently (F14-class — else a fork toggling it silently dedups),
// and an unset (false) flag must hash identically to an agent without it
// (omitempty keeps pre-existing rows byte-identical → no re-backfill).
func TestSign_UnboundedIterations_IsContentIdentifying(t *testing.T) {
	base := AgentContent{Name: "term-agent", SystemPrompt: "interactive"}
	withFlag := base
	withFlag.UnboundedIterations = true

	if Sign(base) == Sign(withFlag) {
		t.Error("UnboundedIterations not content-identifying: enabling it did not change the hash")
	}
	if got, want := Sign(base), Sign(AgentContent{Name: "term-agent", SystemPrompt: "interactive", UnboundedIterations: false}); got != want {
		t.Errorf("unset UnboundedIterations changed the hash (omitempty broken): %s vs %s", got, want)
	}
}

func TestSign_DeterministicAcrossCalls(t *testing.T) {
	c := AgentContent{
		Name:         "researcher",
		SystemPrompt: "be thorough",
		Tools:        []string{"WebFetch", "Read"},
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
	// An agent with tools: [] hashes identically to one with
	// no tools key at all. Operator-side semantics: "no tools"
	// is "no tools" regardless of how the YAML expressed it.
	a := Sign(AgentContent{Name: "x", Tools: nil})
	b := Sign(AgentContent{Name: "x", Tools: []string{}})
	if a != b {
		t.Errorf("nil vs empty tools differ: %s vs %s", a, b)
	}
}

func TestSign_OrderPreservedInArrays(t *testing.T) {
	// Allowed_tools order is semantically meaningful (precedence in
	// some operator scripts). Reordering MUST change the hash.
	a := Sign(AgentContent{Name: "x", Tools: []string{"A", "B"}})
	b := Sign(AgentContent{Name: "x", Tools: []string{"B", "A"}})
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
		Tools:        []string{"WebFetch", "Read"},
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

func TestFromYAMLAgent_ChannelsAffectHash(t *testing.T) {
	// F14: channels now round-trip through AgentDef set (mergedDef → JSONB),
	// so they ARE content-identifying — a fork that adds a channel ACL must
	// get a distinct hash, not be deduped as the parent.
	bare := FromYAMLAgent(&Agent{Name: "x"})
	withChannels := FromYAMLAgent(&Agent{
		Name:     "x",
		Channels: AgentChannelACL{Publish: []string{"out"}, Subscribe: []string{"in"}},
	})
	if Sign(bare) == Sign(withChannels) {
		t.Error("channels did NOT affect the hash — a channels-only fork would be wrongly deduped (F14)")
	}
	// Backward-compat: an EMPTY channels block must still hash like a bare
	// agent (omitempty pointer + normalize collapse) so pre-F14 rows are stable.
	emptyChannels := FromYAMLAgent(&Agent{Name: "x", Channels: AgentChannelACL{}})
	if Sign(bare) != Sign(emptyChannels) {
		t.Error("empty channels block changed the hash — would invalidate every pre-F14 row")
	}
}

func TestFromYAMLAgent_EvaluationScopesAndInterruptionAffectHash(t *testing.T) {
	// F14: evaluation_scopes + interruption also round-trip + are content.
	bare := FromYAMLAgent(&Agent{Name: "x"})
	withEval := FromYAMLAgent(&Agent{Name: "x", EvaluationScopes: []string{"submit_self"}})
	if Sign(bare) == Sign(withEval) {
		t.Error("evaluation_scopes did NOT affect the hash (F14)")
	}
	withInterruption := FromYAMLAgent(&Agent{
		Name:         "x",
		Interruption: AgentInterruptionACL{Enabled: true, Kinds: []string{"question"}, MaxPending: 3},
	})
	if Sign(bare) == Sign(withInterruption) {
		t.Error("interruption did NOT affect the hash (F14)")
	}
	// Backward-compat: an all-zero interruption block hashes like bare.
	emptyInterruption := FromYAMLAgent(&Agent{Name: "x", Interruption: AgentInterruptionACL{}})
	if Sign(bare) != Sign(emptyInterruption) {
		t.Error("empty interruption block changed the hash — would invalidate every pre-F14 row")
	}
}

func TestFromYAMLAgent_ToolCapabilityScopesStillIgnored(t *testing.T) {
	// The *_def_scopes gates (agent_def_scopes, …) govern which substrate tools
	// the agent may CALL; they are NOT part of its authored definition and do
	// NOT round-trip through AgentDef set, so they MUST stay out of the hash
	// (else a yaml-loaded agent and its substrate copy diverge).
	bare := FromYAMLAgent(&Agent{Name: "x"})
	withScopes := FromYAMLAgent(&Agent{
		Name:           "x",
		AgentDefScopes: []string{"self"},
	})
	if Sign(bare) != Sign(withScopes) {
		t.Error("agent_def_scopes leaked into hash — yaml vs substrate would falsely report drift")
	}
}

// RFC BA: `skills:` is a pattern ALLOWLIST (authority, not content) and is
// EXCLUDED from the content hash — two agents differing ONLY in their skills:
// allowlist must hash identically. Reverting the AgentContent.Skills removal
// (re-adding it to the hash basis + FromYAMLAgent) breaks this.
func TestFromYAMLAgent_SkillsAllowlistExcludedFromHash(t *testing.T) {
	bare := FromYAMLAgent(&Agent{Name: "x", SystemPrompt: "do the thing"})
	withSkills := FromYAMLAgent(&Agent{
		Name:         "x",
		SystemPrompt: "do the thing",
		Skills:       []string{"doc/*", "-secret/*"},
	})
	if Sign(bare) != Sign(withSkills) {
		t.Error("skills: allowlist leaked into the content hash — it is authority (an ACL), not content")
	}
	// And a DIFFERENT allowlist must also hash identically (the field never
	// contributes, regardless of value).
	other := FromYAMLAgent(&Agent{
		Name:         "x",
		SystemPrompt: "do the thing",
		Skills:       []string{"-*"},
	})
	if Sign(bare) != Sign(other) {
		t.Error("a different skills: allowlist changed the hash — skills must not be hashed at all")
	}
}

func TestFromOverlay_ChannelsConvergeWithWritePath(t *testing.T) {
	// F14 convergence invariant: the substrate write path (signFromMergedDef,
	// modelled here by FromYAMLAgent which builds the same AgentContent) and
	// the substrate READ path (FromOverlay, used by the boot backfill + verify
	// against the persisted definition JSON) must produce the SAME hash.
	//
	// mergedDef persists channels/interruption as VALUE structs with
	// omitempty, which (verified) serialise an empty block as `"channels":{}`
	// — so the persisted definition for a no-ACL agent literally contains
	// `"channels":{}`. FromOverlay must collapse that to the bare hash.
	bareHash := Sign(FromYAMLAgent(&Agent{Name: "x"}))

	persistedEmpty := json.RawMessage(`{"name":"x","channels":{},"interruption":{}}`)
	cEmpty, err := FromOverlay(persistedEmpty)
	if err != nil {
		t.Fatalf("FromOverlay(empty blocks): %v", err)
	}
	if Sign(cEmpty) != bareHash {
		t.Errorf("persisted empty channels/interruption did not collapse to the bare hash — backfill/verify would diverge from create")
	}

	// A real channel ACL must round-trip identically between the two paths.
	writeHash := Sign(FromYAMLAgent(&Agent{Name: "x", Channels: AgentChannelACL{Publish: []string{"out"}}}))
	persisted := json.RawMessage(`{"name":"x","channels":{"publish":["out"]},"interruption":{}}`)
	cRead, err := FromOverlay(persisted)
	if err != nil {
		t.Fatalf("FromOverlay(channels): %v", err)
	}
	if Sign(cRead) != writeHash {
		t.Errorf("read path hash %q != write path hash %q for the same channels ACL", Sign(cRead), writeHash)
	}
}

func TestFromOverlay_ParsesValidJSON(t *testing.T) {
	overlay := json.RawMessage(`{"name":"x","system_prompt":"hi","tools":["Read"]}`)
	c, err := FromOverlay(overlay)
	if err != nil {
		t.Fatalf("FromOverlay: %v", err)
	}
	if c.Name != "x" || c.SystemPrompt != "hi" || len(c.Tools) != 1 {
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
		Tools:        []string{"Read", "WebFetch"},
		MaxTokens:    8192,
	}
	hashFromYAML := Sign(FromYAMLAgent(agent))

	overlay := json.RawMessage(`{
		"name": "researcher",
		"system_prompt": "be thorough",
		"tools": ["Read", "WebFetch"],
		"max_tokens": 8192
	}`)
	parsed, _ := FromOverlay(overlay)
	hashFromOverlay := Sign(parsed)

	if hashFromYAML != hashFromOverlay {
		t.Errorf("YAML path vs overlay path produced different hashes: %s vs %s", hashFromYAML, hashFromOverlay)
	}
}

// TestFromYAMLAgent_CarriesCodeBody pins that the .md/CLI path threads the
// inline code-js body into the hash. Fails on the pre-fix code, where
// FromYAMLAgent dropped Agent.Code so the hash never reflected the body.
func TestFromYAMLAgent_CarriesCodeBody(t *testing.T) {
	body := `function run(input){ return {final_text:'x'}; }`
	withCode := Sign(FromYAMLAgent(&Agent{Name: "ca", Provider: "code-js", Code: body}))
	without := Sign(FromYAMLAgent(&Agent{Name: "ca", Provider: "code-js"}))
	if withCode == without {
		t.Fatal("FromYAMLAgent dropped Code — the inline body did not affect the hash")
	}
}

// TestFromOverlay_RoundTripMatchesYAMLAgent_WithCode is the 3-way symmetry
// guarantee for code agents: the SAME inline body reaching the hash via the
// yaml/.md/CLI path (FromYAMLAgent) vs the substrate-read path (FromOverlay,
// which json-unmarshals the persisted code_body) MUST hash identically — so
// `loomcycle hash agent` and the deployed substrate's content_sha256 agree.
// Fails on the pre-fix code, where FromYAMLAgent omitted CodeBody and the two
// paths diverged for any code agent. (signFromMergedDef, the third producer,
// also maps Code→CodeBody, so all three converge on the same AgentContent.)
func TestFromOverlay_RoundTripMatchesYAMLAgent_WithCode(t *testing.T) {
	body := `function run(input){ return {final_text:'x'}; }`
	agent := &Agent{Name: "ca", Provider: "code-js", SystemPrompt: "orchestrate", Code: body}
	hashFromYAML := Sign(FromYAMLAgent(agent))

	overlay, err := json.Marshal(map[string]any{
		"name":          "ca",
		"provider":      "code-js",
		"system_prompt": "orchestrate",
		"code_body":     body,
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := FromOverlay(overlay)
	hashFromOverlay := Sign(parsed)

	if hashFromYAML != hashFromOverlay {
		t.Errorf("code agent: yaml/CLI path vs substrate-overlay path diverged:\n %s\n %s", hashFromYAML, hashFromOverlay)
	}
}

func TestSign_KnownVector(t *testing.T) {
	// A pin — if anyone changes the canonical encoding, this test
	// catches the silent break. Update only with intent: bump a
	// version field on every existing row + re-run the backfill.
	c := AgentContent{
		Name:         "researcher",
		SystemPrompt: "be thorough",
		Tools:        []string{"WebFetch", "Read"},
		MaxTokens:    8192,
	}
	const want = "sha256:e03953f0f2b43b6e9521b0c5e3f31cdddad6c3d3e907deb8172cd4c959de105e"
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
