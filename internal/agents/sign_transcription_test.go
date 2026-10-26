package agents

import (
	"encoding/json"
	"testing"
)

// TestSign_Transcription_IsContentIdentifyingAndAbsentIsStable — an agent's
// transcription block changes what its runs do and spend, so two agents that
// differ only there are not the same agent; an agent without it (and the empty
// object a substrate read can produce) must hash exactly as before the field
// existed, so no stored row changes its content_sha256.
func TestSign_Transcription_IsContentIdentifyingAndAbsentIsStable(t *testing.T) {
	base := AgentContent{
		Name:         "researcher",
		SystemPrompt: "be thorough",
		Tools:        []string{"WebFetch", "Read"},
		MaxTokens:    8192,
	}
	// The TestSign_KnownVector pin: an agent without the block is unchanged.
	const pinned = "sha256:e03953f0f2b43b6e9521b0c5e3f31cdddad6c3d3e907deb8172cd4c959de105e"
	if got := Sign(base); got != pinned {
		t.Fatalf("an agent without transcription re-hashed: %s", got)
	}
	empty := base
	empty.Transcription = &Transcription{}
	if got := Sign(empty); got != pinned {
		t.Errorf("an empty transcription block hashes %s, want the absent hash %s", got, pinned)
	}
	read, err := FromOverlay(json.RawMessage(`{"name":"researcher","system_prompt":"be thorough","tools":["WebFetch","Read"],"max_tokens":8192,"transcription":{}}`))
	if err != nil {
		t.Fatalf("FromOverlay: %v", err)
	}
	if got := Sign(read); got != pinned {
		t.Errorf(`a persisted "transcription":{} hashes %s, want %s`, got, pinned)
	}

	opted := base
	opted.Transcription = &Transcription{OCR: "auto"}
	lowered := base
	lowered.Transcription = &Transcription{OCR: "auto", MaxPagesPerRun: 10}
	if Sign(opted) == pinned || Sign(lowered) == Sign(opted) {
		t.Error("a transcription field did not change the hash")
	}
}
