package tools

import (
	"context"
	"sync/atomic"
)

// AnswerShape is what Context op=self reports about the two settings that
// shape a run's requests rather than its reach: the forced tool choice and the
// answer schema. The loop publishes a fresh value whenever either moves — a
// retune adopted at an operator turn, a spent `until`, a fallback onto a target
// that enforces differently — so op=self answers for the NEXT model call.
//
// Published through an atomic pointer rather than stamped per iteration: the
// tool runs after this iteration's model call, which is exactly when a
// first_call choice becomes spent, and a value stamped before the call would
// report it as still in effect.
type AnswerShape struct {
	ToolChoice   *ToolChoiceReport
	OutputFormat *OutputFormatReport
}

// ToolChoiceReport is the run's tool_choice as op=self shows it.
type ToolChoiceReport struct {
	Mode  string `json:"mode"`
	Name  string `json:"name,omitempty"`
	Until string `json:"until"`
	// InEffect is whether the next model call still carries it; false once
	// its `until` is spent.
	InEffect bool `json:"in_effect"`
	// Enforced is whether the current provider and model hold the call to it.
	// False means the model is only asked, and may not comply.
	Enforced bool `json:"enforced"`
}

// OutputFormatReport is the run's output_format as op=self shows it.
type OutputFormatReport struct {
	Type   string         `json:"type"`
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
	// Enforcement says what holds the answer to the schema on the current
	// target: "native" (the provider's structured-output API), "grammar"
	// (constrained decoding, with the schema also shown in the prompt),
	// "prompt" (only asked for in the prompt — the answer is unchecked), or
	// "none" (a scripted agent that reads no prompt).
	Enforcement string `json:"enforcement"`
}

// Output-format enforcement values, as OutputFormatReport.Enforcement.
const (
	EnforcementNative  = "native"
	EnforcementGrammar = "grammar"
	EnforcementPrompt  = "prompt"
	EnforcementNone    = "none"
)

type ctxKeyAnswerShape struct{}

// WithAnswerShape attaches the loop's published answer shape. The loop stamps
// its own holder at the start of every run, so a sub-agent's op=self never
// reads its parent's. nil is a no-op.
func WithAnswerShape(ctx context.Context, p *atomic.Pointer[AnswerShape]) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyAnswerShape{}, p)
}

// AnswerShapeNow returns the latest published answer shape, or nil when the
// run publishes none (no loop, or a stateful run, which ignores both).
func AnswerShapeNow(ctx context.Context) *AnswerShape {
	p, _ := ctx.Value(ctxKeyAnswerShape{}).(*atomic.Pointer[AnswerShape])
	if p == nil {
		return nil
	}
	return p.Load()
}
