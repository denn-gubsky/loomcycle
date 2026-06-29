package loop

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// visionProvider records the request it was handed, whether it was called, and
// advertises vision support per a flag — so a test can prove an image reaches
// the provider on a vision-capable model and is refused before the call on a
// text-only one.
type visionProvider struct {
	id     string
	vision bool
	mu     sync.Mutex
	called bool
	got    []providers.Message
}

func (p *visionProvider) ID() string                                   { return p.id }
func (p *visionProvider) Probe(context.Context) error                  { return nil }
func (p *visionProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *visionProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, SupportsVision: p.vision}
}
func (p *visionProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.called = true
	p.got = req.Messages
	p.mu.Unlock()
	ch := make(chan providers.Event, 1)
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

func (p *visionProvider) wasCalled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.called
}

func (p *visionProvider) imageBlocks() []providers.ContentBlock {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []providers.ContentBlock
	for _, m := range p.got {
		for _, c := range m.Content {
			if c.Type == "image" {
				out = append(out, c)
			}
		}
	}
	return out
}

const testPNGB64 = "iVBORw0KGgo=" // valid base64 (decodes); contents irrelevant to the wire validation

func imageSegs() []PromptSegment {
	return []PromptSegment{{Role: "user", Content: []PromptContentBlock{
		{Type: "trusted-text", Text: "describe this"},
		{Type: "image", MediaType: "image/png", Data: testPNGB64},
	}}}
}

// TestFlattenContent_ImagePassesThrough: an image block flattens to a provider
// image ContentBlock carrying media_type + data verbatim and is NOT tag-fenced.
func TestFlattenContent_ImagePassesThrough(t *testing.T) {
	got := flattenContent(PromptContentBlock{Type: "image", MediaType: "image/jpeg", Data: testPNGB64})
	if got.Type != "image" {
		t.Fatalf("Type = %q, want image", got.Type)
	}
	if got.MediaType != "image/jpeg" || got.Data != testPNGB64 {
		t.Fatalf("media/data not carried through: %+v", got)
	}
	if got.Text != "" {
		t.Errorf("image block should carry no text, got %q", got.Text)
	}
}

// TestRun_ImageReachesVisionProvider: on a vision-capable provider the image
// block flows through to the provider request intact.
func TestRun_ImageReachesVisionProvider(t *testing.T) {
	prov := &visionProvider{id: "vision-llm", vision: true}
	_, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "vision-model",
		Tools:      []tools.Tool{noopTool{}},
		Dispatcher: tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:   imageSegs(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	imgs := prov.imageBlocks()
	if len(imgs) != 1 {
		t.Fatalf("want 1 image block reaching provider, got %d", len(imgs))
	}
	if imgs[0].MediaType != "image/png" || imgs[0].Data != testPNGB64 {
		t.Errorf("image not carried to provider: %+v", imgs[0])
	}
}

// TestRun_ImageGatedOnTextOnlyProvider: an image sent to a SupportsVision=false
// provider is refused BEFORE the call — Run errors, an EventError is emitted,
// and Provider.Call is never invoked (so the image is never silently dropped).
func TestRun_ImageGatedOnTextOnlyProvider(t *testing.T) {
	prov := &visionProvider{id: "text-only", vision: false}
	var events []providers.Event
	_, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "text-model",
		Tools:      []tools.Tool{noopTool{}},
		Dispatcher: tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:   imageSegs(),
		OnEvent:    func(ev providers.Event) { events = append(events, ev) },
	})
	if err == nil {
		t.Fatal("want error gating image on a text-only provider, got nil")
	}
	if prov.wasCalled() {
		t.Error("Provider.Call must NOT be invoked when the image is gated")
	}
	var sawErr bool
	for _, ev := range events {
		if ev.Type == providers.EventError && strings.Contains(ev.Error, "does not support image input") {
			sawErr = true
		}
	}
	if !sawErr {
		t.Errorf("want an EventError naming the missing vision support, got events %+v", events)
	}
}

// TestValidateImageSegments rejects malformed images (bad role, media type,
// empty/invalid base64) and accepts a well-formed one.
func TestValidateImageSegments(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString([]byte("hello-bytes"))
	cases := []struct {
		name    string
		segs    []PromptSegment
		wantErr string // substring; "" means expect no error
	}{
		{
			name:    "valid user image",
			segs:    []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "image", MediaType: "image/webp", Data: valid}}}},
			wantErr: "",
		},
		{
			name:    "image in system role rejected",
			segs:    []PromptSegment{{Role: "system", Content: []PromptContentBlock{{Type: "image", MediaType: "image/png", Data: valid}}}},
			wantErr: "user-role",
		},
		{
			name:    "unsupported media type rejected",
			segs:    []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "image", MediaType: "image/tiff", Data: valid}}}},
			wantErr: "unsupported image media_type",
		},
		{
			name:    "empty data rejected",
			segs:    []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "image", MediaType: "image/png", Data: ""}}}},
			wantErr: "empty data",
		},
		{
			name:    "non-base64 data rejected",
			segs:    []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "image", MediaType: "image/png", Data: "not base64!!!"}}}},
			wantErr: "not valid base64",
		},
		{
			name:    "text-only segments pass",
			segs:    []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImageSegments(tc.segs)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
