package config

import (
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"
)

// agent_telemetry.go — the per-agent `telemetry:` block (RFC GG P3).

// AgentContentCaptureNever is the one value of telemetry.content_capture: the
// agent's runs never capture content, whatever its tenant's policy says.
const AgentContentCaptureNever = "never"

// AgentTelemetry is an agent's `telemetry:` block. It can only take content
// capture away: an agent that handles data its tenant does not want leaving
// at all says `content_capture: never`, and its runs record no prompt, answer
// or tool call on any span, even when the tenant captures content for every
// other agent. Unset follows the tenant. Content-identifying (part of
// content_sha256). Not per-run overridable: no run-request field carries it,
// and a run may never widen it.
type AgentTelemetry struct {
	ContentCapture string `json:"content_capture,omitempty" yaml:"content_capture,omitempty"`
}

// IsZero reports whether nothing is set (collapses to nil so an agent without
// the block hashes as before).
func (a *AgentTelemetry) IsZero() bool {
	return a == nil || a.ContentCapture == ""
}

// Clone copies the block.
func (a *AgentTelemetry) Clone() *AgentTelemetry {
	if a == nil {
		return nil
	}
	out := *a
	return &out
}

// NeverCapturesContent reports whether the block opts the agent out of
// content capture.
func (a *AgentTelemetry) NeverCapturesContent() bool {
	return a != nil && a.ContentCapture == AgentContentCaptureNever
}

// MergeAgentTelemetry overlays over onto base per field: a set field in over
// wins, an unset one keeps base. nil when both are empty; never aliases.
// The only value is "never", so an overlay can opt an agent out and never
// back in.
func MergeAgentTelemetry(base, over *AgentTelemetry) *AgentTelemetry {
	if base.IsZero() && over.IsZero() {
		return nil
	}
	out := base.Clone()
	if out == nil {
		out = &AgentTelemetry{}
	}
	if !over.IsZero() {
		out.ContentCapture = over.ContentCapture
	}
	return out
}

// Validate checks the block's own values.
func (a *AgentTelemetry) Validate() error {
	if a == nil {
		return nil
	}
	switch a.ContentCapture {
	case "", AgentContentCaptureNever:
		return nil
	}
	return fmt.Errorf("telemetry.content_capture must be %q or unset, got %q", AgentContentCaptureNever, a.ContentCapture)
}

func (a *AgentTelemetry) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "telemetry", nil, reflect.TypeOf(*a)); err != nil {
		return err
	}
	type plain AgentTelemetry
	return n.Decode((*plain)(a))
}
