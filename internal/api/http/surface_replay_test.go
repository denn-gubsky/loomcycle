package http

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A page the run exposed is told to its user, never to its model: a
// conversation rebuilt from a transcript is the same with surface and
// surface_closed rows on it as without (RFC GW D24).
func TestReplayTranscript_SkipsSurfaceRows(t *testing.T) {
	user := mkEvent("user_input", []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}})
	text := mkEvent("text", providers.Event{Type: providers.EventText, Text: "shown"})
	opened := mkEvent(string(providers.EventSurface), providers.Event{Type: providers.EventSurface,
		Surface: &providers.SurfaceInfo{ExposureID: "x1", Workspace: "web", Title: "Chart", Kind: "panel"}})
	closed := mkEvent(string(providers.EventSurfaceClosed), providers.Event{Type: providers.EventSurfaceClosed,
		Surface: &providers.SurfaceInfo{ExposureID: "x1", Reason: "idle"}})
	without := replayTranscript([]store.Event{user, text})
	with := replayTranscript([]store.Event{user, opened, text, closed})
	if len(without) == 0 || !reflect.DeepEqual(with, without) {
		t.Errorf("replay with surface rows = %+v\nwithout = %+v", with, without)
	}
}
