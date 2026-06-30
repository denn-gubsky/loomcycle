package ollama

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// Ollama maps the agent's effort hint to its top-level `think` flag, which
// turns a reasoning model's thinking trace on/off. These tests pin both the
// capability contract and the effort→think wire translation.

func TestCapabilities_SupportsEffortIsTrue(t *testing.T) {
	// SupportsEffort=true: the loop forwards the effort hint to the driver,
	// which translates it into `think` rather than dropping it.
	d := New("", "", "http://localhost:11434", streamhttp.Options{}, nil)
	caps := d.Capabilities()
	if !caps.SupportsEffort {
		t.Error("Ollama driver must report SupportsEffort=true (effort drives `think`)")
	}
	if !caps.SupportsThinking {
		t.Error("Ollama driver must report SupportsThinking=true")
	}
}

func TestBuildRequestBody_EffortMapsToThink(t *testing.T) {
	d := New("", "", "http://localhost:11434", streamhttp.Options{}, nil)
	req := func(effort string) providers.Request {
		return providers.Request{
			Model:  "qwen3",
			Effort: effort,
			Messages: []providers.Message{
				{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}},
			},
		}
	}

	cases := []struct {
		effort   string
		wantSet  bool // is `think` present on the wire?
		wantBool bool // its value when present
	}{
		{"high", true, true},
		{"medium", true, true},
		{"low", true, false},
		{"", false, false},
	}
	for _, c := range cases {
		body, err := d.buildRequestBody(req(c.effort))
		if err != nil {
			t.Fatalf("effort %q: buildRequestBody: %v", c.effort, err)
		}
		var w struct {
			Think *bool `json:"think"`
		}
		if err := json.Unmarshal(body, &w); err != nil {
			t.Fatalf("effort %q: unmarshal: %v", c.effort, err)
		}
		if c.wantSet {
			if w.Think == nil {
				t.Errorf("effort %q: want think=%v, got omitted", c.effort, c.wantBool)
			} else if *w.Think != c.wantBool {
				t.Errorf("effort %q: want think=%v, got %v", c.effort, c.wantBool, *w.Think)
			}
		} else if w.Think != nil {
			t.Errorf("effort %q: want think omitted, got %v", c.effort, *w.Think)
		}
	}
}
