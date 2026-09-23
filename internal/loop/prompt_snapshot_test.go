package loop

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC DI: the loop records what its FIRST model call was sent, once, even when
// the run goes on to make more calls — and it records the request exactly as
// built, so it is what the model saw rather than what a caller composed.
func TestRun_RecordsThePromptOfItsFirstModelCallOnce(t *testing.T) {
	prov := &scriptedProvider{toolCalls: []providers.ToolUse{
		{ID: "call_1", Name: "WebFetch", Input: json.RawMessage(`{"url":"https://example.com"}`)},
	}}
	tool := &fakeWebFetch{result: tools.Result{Text: "page"}}
	var mu sync.Mutex
	var snaps []*providers.PromptSnapshotInfo
	_, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{tool},
		Dispatcher: tools.NewDispatcher([]tools.Tool{tool}),
		Segments: []PromptSegment{
			{Role: "system", Content: []PromptContentBlock{{Type: "trusted-text", Text: "You are a reviewer."}}},
			{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "review this"}}},
		},
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventPromptSnapshot {
				mu.Lock()
				snaps = append(snaps, ev.PromptSnapshot)
				mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.requests) < 2 {
		t.Fatalf("fixture made %d model calls; the once-only rule needs at least two", len(prov.requests))
	}
	if len(snaps) != 1 || snaps[0] == nil {
		t.Fatalf("got %d prompt snapshots, want exactly one", len(snaps))
	}
	first := prov.requests[0]
	if len(snaps[0].System) != len(first.System) || snaps[0].System[0].Text != first.System[0].Text {
		t.Errorf("snapshot system = %+v, want the first request's system %+v", snaps[0].System, first.System)
	}
	if len(snaps[0].Input) != 1 || snaps[0].Input[0].Text != "review this" {
		t.Errorf("snapshot input = %+v, want the run's user input", snaps[0].Input)
	}
}

// A snapshot is for reading what the model was asked; a base64 image would make
// it as large as the image. The media type survives, the bytes do not — and
// the caller's request is not modified by building it.
func TestNewPromptSnapshot_DropsImageBytesKeepsTheRequestIntact(t *testing.T) {
	msgs := []providers.Message{
		{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "old turn"}}},
		{Role: "assistant", Content: []providers.ContentBlock{{Type: "text", Text: "reply"}}},
		{Role: "user", Content: []providers.ContentBlock{
			{Type: "text", Text: "what is this?"},
			{Type: "image", MediaType: "image/png", Data: "iVBORw0KGgo="},
		}},
	}
	snap := providers.NewPromptSnapshot(nil, msgs)
	if len(snap.Input) != 2 || snap.Input[0].Text != "what is this?" {
		t.Fatalf("input = %+v, want the LAST user turn", snap.Input)
	}
	if snap.Input[1].MediaType != "image/png" || snap.Input[1].Data != "" {
		t.Errorf("image block = %+v, want media type kept and bytes dropped", snap.Input[1])
	}
	if msgs[2].Content[1].Data == "" {
		t.Error("building the snapshot emptied the image in the live request")
	}
}

// The stateful loop builds its own requests, so it records its own snapshot —
// once, and with the system its state instructions ride in, which the main
// loop's path never sees.
func TestRunStateful_RecordsThePromptOfItsFirstModelCallOnce(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{"count":0},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{"count":1},"done":true,"final":"count is 1"}`,
	}}
	echo := &echoTool{reply: "observed"}
	var snaps []*providers.PromptSnapshotInfo
	_, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventPromptSnapshot {
				snaps = append(snaps, ev.PromptSnapshot)
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.requests) < 2 {
		t.Fatalf("fixture made %d model calls; the once-only rule needs at least two", len(prov.requests))
	}
	if len(snaps) != 1 || snaps[0] == nil {
		t.Fatalf("got %d prompt snapshots from a stateful run, want exactly one", len(snaps))
	}
	if len(snaps[0].System) == 0 || len(snaps[0].Input) == 0 {
		t.Errorf("stateful snapshot = %+v, want the first request's system and input", snaps[0])
	}
}
