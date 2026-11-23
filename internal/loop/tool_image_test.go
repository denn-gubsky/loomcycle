package loop

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// pictureLoopTool answers every call with a small PNG.
type pictureLoopTool struct{ png []byte }

func (pictureLoopTool) Name() string                 { return "Pic" }
func (pictureLoopTool) Description() string          { return "returns a picture" }
func (pictureLoopTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p pictureLoopTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Text: "the chart", Images: []tools.Image{{MediaType: "image/png", Data: p.png}}}, nil
}

// pictureTurnProvider calls Pic on its first turn and ends on its second,
// keeping the second request's messages.
type pictureTurnProvider struct {
	vision bool
	mu     sync.Mutex
	calls  int
	second []providers.Message
}

func (p *pictureTurnProvider) ID() string                                   { return "pic-prov" }
func (p *pictureTurnProvider) Probe(context.Context) error                  { return nil }
func (p *pictureTurnProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *pictureTurnProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, SupportsVision: p.vision}
}
func (p *pictureTurnProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	if n == 2 {
		p.second = req.Messages
	}
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	if n == 1 {
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu-pic", Name: "Pic", Input: json.RawMessage(`{}`)}}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}
	} else {
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	}
	close(ch)
	return ch, nil
}

func smallPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 6, 4))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func runPictureTurn(t *testing.T, vision bool) (*pictureTurnProvider, []providers.Event, []byte) {
	t.Helper()
	pic := smallPNG(t)
	tool := pictureLoopTool{png: pic}
	prov := &pictureTurnProvider{vision: vision}
	var mu sync.Mutex
	var events []providers.Event
	_, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "m",
		Tools:      []tools.Tool{tool},
		Dispatcher: tools.NewDispatcher([]tools.Tool{tool}),
		Segments:   []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "draw"}}}},
		OnEvent: func(ev providers.Event) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prov.calls != 2 {
		t.Fatalf("provider called %d times, want 2", prov.calls)
	}
	return prov, events, pic
}

func lastToolResultBlock(t *testing.T, msgs []providers.Message) providers.ContentBlock {
	t.Helper()
	for i := len(msgs) - 1; i >= 0; i-- {
		for _, c := range msgs[i].Content {
			if c.Type == "tool_result" {
				return c
			}
		}
	}
	t.Fatalf("no tool_result in %+v", msgs)
	return providers.ContentBlock{}
}

func toolResultEvent(t *testing.T, events []providers.Event) providers.Event {
	t.Helper()
	for _, ev := range events {
		if ev.Type == providers.EventToolResult {
			return ev
		}
	}
	t.Fatal("no tool_result event")
	return providers.Event{}
}

// A vision model gets the tool's picture in the next request, inside the
// tool_result; the event carries it for the transcript, and the text names it
// the same way in both.
func TestToolImage_VisionModelGetsPictureInNextRequest(t *testing.T) {
	prov, events, pic := runPictureTurn(t, true)
	block := lastToolResultBlock(t, prov.second)
	want := base64.StdEncoding.EncodeToString(pic)
	if block.IsError || len(block.Images) != 1 || block.Images[0].MediaType != "image/png" || block.Images[0].Data != want {
		t.Fatalf("tool_result block = %+v", block)
	}
	if !strings.Contains(block.Text, "the chart") || !strings.Contains(block.Text, "[picture 1 of 1: image/png, 6×4 px") {
		t.Errorf("block text = %q", block.Text)
	}
	ev := toolResultEvent(t, events)
	if ev.Text != block.Text {
		t.Errorf("event text %q differs from the block's %q", ev.Text, block.Text)
	}
	if len(ev.ToolImages) != 1 || ev.ToolImages[0].Data != want {
		t.Errorf("event ToolImages = %+v", ev.ToolImages)
	}
}

// A model that cannot see images is refused the picture before the next call:
// the call fails saying so, no picture reaches the request, and the run goes
// on.
func TestToolImage_TextOnlyModelRefusedBeforeTheCall(t *testing.T) {
	prov, events, _ := runPictureTurn(t, false)
	block := lastToolResultBlock(t, prov.second)
	if !block.IsError || len(block.Images) != 0 || !strings.Contains(block.Text, "cannot see images") {
		t.Fatalf("tool_result block = %+v", block)
	}
	if providers.RequestHasImage(providers.Request{Messages: prov.second}) {
		t.Error("a picture reached a text-only model's request")
	}
	if ev := toolResultEvent(t, events); len(ev.ToolImages) != 0 || !ev.IsError {
		t.Errorf("event = %+v", ev)
	}
}

// A picture already in the history counts as an image for the run-start and
// fallback gates, so a resumed run or a fallback to a model that cannot see it
// is refused like a user's image is.
func TestToolImage_HistoryPictureCountsForTheImageGates(t *testing.T) {
	msgs := []providers.Message{{Role: "user", Content: []providers.ContentBlock{{
		Type: "tool_result", ToolUseID: "t", Text: "x",
		Images: []providers.ImagePart{{MediaType: "image/png", Data: "QUJD"}},
	}}}}
	if !messagesHaveImage(msgs) {
		t.Error("messagesHaveImage must count a tool result's picture")
	}
}
