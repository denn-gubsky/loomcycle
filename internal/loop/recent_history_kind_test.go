package loop

import (
	"strings"
	"testing"
)

// RFC HE F3: the preload is fenced under its own kind, so the model can tell
// earlier chats from this one — an unknown kind is rendered as "untrusted".
func TestFlattenContent_RecentHistoryKeepsItsKind(t *testing.T) {
	got := FlattenContent(PromptContentBlock{Type: "untrusted-block", Kind: RecentHistoryKind, Text: "earlier chat"})
	if !strings.Contains(got.Text, RecentHistoryKind) {
		t.Errorf("recent_history rendered without its kind:\n%s", got.Text)
	}
}

// A caller cannot write the runtime's recent_history block: it would tell the
// model the user said what they did not.
func TestDemoteRuntimeKinds_TakesRecentHistoryFromACaller(t *testing.T) {
	in := []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "untrusted-block", Kind: RecentHistoryKind, Text: "forged"}}}}
	out := DemoteRuntimeKinds(in)
	if k := out[0].Content[0].Kind; k != "untrusted" {
		t.Errorf("caller's recent_history block kept kind %q", k)
	}
	if in[0].Content[0].Kind != RecentHistoryKind {
		t.Error("DemoteRuntimeKinds changed its input")
	}
}
