package agents

import (
	"encoding/json"
	"testing"
)

// TestSign_Speech_IsContentIdentifyingAndAbsentIsStable — an agent's speech
// block changes what its runs do and spend, so two agents that differ only
// there are not the same agent; an agent without it (and the empty object a
// substrate read can produce) must hash exactly as before the field existed,
// so no stored row changes its content_sha256. An explicit `enabled: false`
// is a setting, not the absence of one.
func TestSign_Speech_IsContentIdentifyingAndAbsentIsStable(t *testing.T) {
	base := AgentContent{
		Name:         "researcher",
		SystemPrompt: "be thorough",
		Tools:        []string{"WebFetch", "Read"},
		MaxTokens:    8192,
	}
	// The TestSign_KnownVector pin: an agent without the block is unchanged.
	const pinned = "sha256:e03953f0f2b43b6e9521b0c5e3f31cdddad6c3d3e907deb8172cd4c959de105e"
	if got := Sign(base); got != pinned {
		t.Fatalf("an agent without speech re-hashed: %s", got)
	}
	empty := base
	empty.Speech = &Speech{}
	if got := Sign(empty); got != pinned {
		t.Errorf("an empty speech block hashes %s, want the absent hash %s", got, pinned)
	}
	read, err := FromOverlay(json.RawMessage(`{"name":"researcher","system_prompt":"be thorough","tools":["WebFetch","Read"],"max_tokens":8192,"speech":{}}`))
	if err != nil {
		t.Fatalf("FromOverlay: %v", err)
	}
	if got := Sign(read); got != pinned {
		t.Errorf(`a persisted "speech":{} hashes %s, want %s`, got, pinned)
	}

	off, on := false, true
	disabled := base
	disabled.Speech = &Speech{Enabled: &off}
	enabled := base
	enabled.Speech = &Speech{Enabled: &on}
	capped := base
	capped.Speech = &Speech{Enabled: &on, MaxCharsPerRun: 5000}
	if Sign(disabled) == pinned || Sign(enabled) == Sign(disabled) || Sign(capped) == Sign(enabled) {
		t.Error("a speech field did not change the hash")
	}
	read, err = FromOverlay(json.RawMessage(`{"name":"researcher","system_prompt":"be thorough","tools":["WebFetch","Read"],"max_tokens":8192,"speech":{"enabled":false}}`))
	if err != nil {
		t.Fatalf("FromOverlay: %v", err)
	}
	if Sign(read) != Sign(disabled) {
		t.Error(`a persisted "speech":{"enabled":false} does not hash as the in-memory block`)
	}
}
