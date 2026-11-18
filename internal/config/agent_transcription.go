package config

import (
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"
)

// agent_transcription.go — the per-agent `transcription:` block. The operator
// block it is judged against is in transcription.go.

// AgentTranscription is an agent's `transcription:` block. It may opt in to OCR,
// pick the OCR model among the operator's `models:` aliases, and lower the
// operator's per-run caps; it can never raise them or choose an engine.
// Content-identifying (part of content_sha256), like compaction.
//
// Zero values are "unset": OCR "" is never (OCR spends tokens per page, so it
// is opt-in), OCRModel "" is the operator's transcription.ocr model or tier,
// STT "" is on when the operator configured an engine, and a zero cap is the
// operator's.
type AgentTranscription struct {
	OCR string `json:"ocr,omitempty" yaml:"ocr,omitempty"`
	// OCRModel is a `models:` alias this agent's OCR calls use instead of the
	// operator's transcription.ocr.model / tier (RFC GV D9: a library that may
	// only be read by local models pins a local vision alias on the agent that
	// reads it, not on the whole deployment). Only an alias, never a literal
	// model: the operator's models: map is the menu, as for transcription.ocr.
	OCRModel         string `json:"ocr_model,omitempty" yaml:"ocr_model,omitempty"`
	STT              string `json:"stt,omitempty" yaml:"stt,omitempty"`
	MaxPagesPerRun   int    `json:"max_pages_per_run,omitempty" yaml:"max_pages_per_run,omitempty"`
	MaxMinutesPerRun int    `json:"max_minutes_per_run,omitempty" yaml:"max_minutes_per_run,omitempty"`
}

// The per-agent modes.
const (
	AgentOCRAuto   = "auto"
	AgentOCRAlways = "always"
	AgentOCRNever  = "never"
	AgentSTTOn     = "on"
	AgentSTTOff    = "off"
)

// IsZero reports whether nothing is set (collapses to nil so an agent without
// the block hashes as before).
func (a *AgentTranscription) IsZero() bool {
	return a == nil || *a == AgentTranscription{}
}

// Clone copies the block (every field is a value, so a shallow copy is deep).
func (a *AgentTranscription) Clone() *AgentTranscription {
	if a == nil {
		return nil
	}
	out := *a
	return &out
}

// MergeAgentTranscription overlays over onto base per field: a set field in
// over wins, an unset one keeps base. nil when both are empty; never aliases.
func MergeAgentTranscription(base, over *AgentTranscription) *AgentTranscription {
	if base.IsZero() && over.IsZero() {
		return nil
	}
	out := base.Clone()
	if out == nil {
		out = &AgentTranscription{}
	}
	if over == nil {
		return out
	}
	if over.OCR != "" {
		out.OCR = over.OCR
	}
	if over.OCRModel != "" {
		out.OCRModel = over.OCRModel
	}
	if over.STT != "" {
		out.STT = over.STT
	}
	if over.MaxPagesPerRun != 0 {
		out.MaxPagesPerRun = over.MaxPagesPerRun
	}
	if over.MaxMinutesPerRun != 0 {
		out.MaxMinutesPerRun = over.MaxMinutesPerRun
	}
	return out
}

// Validate checks the block's own values.
func (a *AgentTranscription) Validate() error {
	if a == nil {
		return nil
	}
	switch a.OCR {
	case "", AgentOCRAuto, AgentOCRAlways, AgentOCRNever:
	default:
		return fmt.Errorf("transcription.ocr: %q is not one of auto, always, never", a.OCR)
	}
	switch a.STT {
	case "", AgentSTTOn, AgentSTTOff:
	default:
		return fmt.Errorf("transcription.stt: %q is not one of on, off", a.STT)
	}
	if a.MaxPagesPerRun < 0 {
		return fmt.Errorf("transcription.max_pages_per_run must be >= 0, got %d", a.MaxPagesPerRun)
	}
	if a.MaxMinutesPerRun < 0 {
		return fmt.Errorf("transcription.max_minutes_per_run must be >= 0, got %d", a.MaxMinutesPerRun)
	}
	return nil
}

// CheckAgentTranscription validates an agent's block and REFUSES a cap above
// the operator's: the lower-only rule is enforced at authoring (config load,
// AgentDef create/fork) so the author hears about it, and again at use by
// taking the minimum (transcribe.Effective), which covers a stored definition
// the operator undercut later. A cap for an engine the operator has not
// configured is accepted: there is nothing to exceed yet, and the minimum
// binds it the day one is configured.
func (c *Config) CheckAgentTranscription(a *AgentTranscription) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.IsZero() {
		return nil
	}
	// The same check as the operator's transcription.ocr.model (validateOCR):
	// an alias of this config's models:, of kind chat. Whether the model can
	// see is a property of its provider, answered at use (vision.Engine.Plan
	// refuses a model that cannot accept images), as for the operator's.
	if a.OCRModel != "" {
		const place = "transcription.ocr_model"
		if _, ok := c.Models[a.OCRModel]; !ok {
			return fmt.Errorf("%s: %q is not a models: alias (the OCR model has no provider of its own, so it must be one)", place, a.OCRModel)
		}
		// CheckModelKind, not requireModelKind: this also runs at AgentDef
		// create/fork on the live config, which must not be written to, and
		// requireModelKind's advisory is never raised for a chat model anyway.
		if err := c.CheckModelKind(a.OCRModel, ModelKindChat); err != nil {
			return fmt.Errorf("%s: %w", place, err)
		}
	}
	op := c.Transcription.Resolved()
	if op.OCREnabled() && a.MaxPagesPerRun > op.OCR.MaxPagesPerRun {
		return fmt.Errorf("transcription.max_pages_per_run (%d) is above the operator's transcription.ocr.max_pages_per_run (%d): an agent may only lower it",
			a.MaxPagesPerRun, op.OCR.MaxPagesPerRun)
	}
	if op.STTEnabled() && a.MaxMinutesPerRun > op.STT.MaxMinutesPerRun {
		return fmt.Errorf("transcription.max_minutes_per_run (%d) is above the operator's transcription.stt.max_minutes_per_run (%d): an agent may only lower it",
			a.MaxMinutesPerRun, op.STT.MaxMinutesPerRun)
	}
	return nil
}

func (a *AgentTranscription) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "transcription", nil, reflect.TypeOf(*a)); err != nil {
		return err
	}
	type plain AgentTranscription
	return n.Decode((*plain)(a))
}
