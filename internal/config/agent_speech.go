package config

import (
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"
)

// agent_speech.go — the per-agent `speech:` block. The operator block it is
// judged against is in speech.go.

// AgentSpeech is an agent's `speech:` block. It may switch speech off for the
// agent and lower the operator's per-run character cap; it can never raise
// the cap or choose an engine, a model or a voice. Content-identifying (part
// of content_sha256), like transcription.
//
// Zero values are "unset": a nil Enabled is on when the operator configured an
// engine (a pointer, so an explicit false is not mistaken for unset), and a
// zero cap is the operator's.
type AgentSpeech struct {
	Enabled        *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	MaxCharsPerRun int   `json:"max_chars_per_run,omitempty" yaml:"max_chars_per_run,omitempty"`
}

// IsZero reports whether nothing is set (collapses to nil so an agent without
// the block hashes as before).
func (a *AgentSpeech) IsZero() bool {
	return a == nil || (a.Enabled == nil && a.MaxCharsPerRun == 0)
}

// Clone deep-copies the block (Enabled is a pointer).
func (a *AgentSpeech) Clone() *AgentSpeech {
	if a == nil {
		return nil
	}
	out := *a
	if a.Enabled != nil {
		v := *a.Enabled
		out.Enabled = &v
	}
	return &out
}

// MergeAgentSpeech overlays over onto base per field: a set field in over
// wins, an unset one keeps base. nil when both are empty; never aliases.
func MergeAgentSpeech(base, over *AgentSpeech) *AgentSpeech {
	if base.IsZero() && over.IsZero() {
		return nil
	}
	out := base.Clone()
	if out == nil {
		out = &AgentSpeech{}
	}
	if over == nil {
		return out
	}
	if over.Enabled != nil {
		v := *over.Enabled
		out.Enabled = &v
	}
	if over.MaxCharsPerRun != 0 {
		out.MaxCharsPerRun = over.MaxCharsPerRun
	}
	return out
}

// Validate checks the block's own values.
func (a *AgentSpeech) Validate() error {
	if a == nil {
		return nil
	}
	if a.MaxCharsPerRun < 0 {
		return fmt.Errorf("speech.max_chars_per_run must be >= 0, got %d", a.MaxCharsPerRun)
	}
	return nil
}

// CheckAgentSpeech validates an agent's block and REFUSES a cap above the
// operator's: the lower-only rule is enforced at authoring (config load,
// AgentDef create/fork) so the author hears about it; the runtime takes the
// minimum as well, which covers a stored definition the operator undercut
// later. A cap with no engine configured is accepted: there is nothing to
// exceed yet, and the minimum binds it the day one is configured.
func (c *Config) CheckAgentSpeech(a *AgentSpeech) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.IsZero() {
		return nil
	}
	op := c.Speech.Resolved()
	if op.Enabled() && a.MaxCharsPerRun > op.MaxCharsPerRun {
		return fmt.Errorf("speech.max_chars_per_run (%d) is above the operator's speech.max_chars_per_run (%d): an agent may only lower it",
			a.MaxCharsPerRun, op.MaxCharsPerRun)
	}
	return nil
}

func (a *AgentSpeech) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "speech", nil, reflect.TypeOf(*a)); err != nil {
		return err
	}
	type plain AgentSpeech
	return n.Decode((*plain)(a))
}
