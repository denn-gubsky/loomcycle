package openai

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// OpenAI effort translation is much simpler than Anthropic's: pass
// the operator's hint through verbatim as `reasoning_effort`. These
// tests pin the wire-field name and the "no effort = field omitted"
// invariant.

func TestEffortTranslation_PassesThroughVerbatim(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high"} {
		t.Run(effort, func(t *testing.T) {
			body, err := buildRequestBody(providers.Request{
				Model:    "gpt-5.5",
				Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
				Effort:   effort,
			})
			if err != nil {
				t.Fatalf("buildRequestBody: %v", err)
			}
			var w map[string]any
			_ = json.Unmarshal(body, &w)
			got, ok := w["reasoning_effort"].(string)
			if !ok {
				t.Fatalf("reasoning_effort missing for effort=%q, body: %s", effort, body)
			}
			if got != effort {
				t.Errorf("reasoning_effort = %q, want %q", got, effort)
			}
		})
	}
}

func TestEffortTranslation_NoEffortMeansFieldOmitted(t *testing.T) {
	// Default behaviour for agents without effort: no
	// reasoning_effort field on the wire. omitempty does the work;
	// the test pins the contract so a future struct refactor
	// can't quietly start sending the field.
	body, err := buildRequestBody(providers.Request{
		Model:    "gpt-5.4",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
		// Effort intentionally empty.
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w map[string]any
	_ = json.Unmarshal(body, &w)
	if _, has := w["reasoning_effort"]; has {
		t.Errorf("missing effort should omit reasoning_effort, got: %v", w["reasoning_effort"])
	}
}

func TestEffortTranslation_DriverDoesNotGateOnModel(t *testing.T) {
	// Per-driver design decision: OpenAI passes the effort hint
	// through to the API regardless of model. The API decides
	// whether to honour or 400 — that surfaces as a clear error
	// in the loop's stall feedback rather than a silent drop.
	// This test pins the contract: even with a non-reasoning
	// model name, the field is still sent.
	body, err := buildRequestBody(providers.Request{
		Model:    "gpt-5.4-mini",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
		Effort:   "high",
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w map[string]any
	_ = json.Unmarshal(body, &w)
	got, _ := w["reasoning_effort"].(string)
	if got != "high" {
		t.Errorf("driver should pass effort even on non-reasoning models, got: %v", got)
	}
}

func TestCapabilities_SupportsEffortIsTrue(t *testing.T) {
	// Pin the SupportsEffort=true contract so a future Capabilities
	// edit can't quietly flip it to false (which would make the
	// loop log "effort dropped" on every OpenAI run, misleading
	// operators about what the driver actually does).
	d := New("test-key", "", streamhttp.Options{}, nil)
	if !d.Capabilities().SupportsEffort {
		t.Error("OpenAI driver must report SupportsEffort=true")
	}
}
