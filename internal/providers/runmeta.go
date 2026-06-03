package providers

import (
	"context"
	"time"
)

// RunMeta carries the small slice of run identity a Provider may need that
// is NOT part of the LLM-shaped Request. The canonical LLM drivers ignore it
// entirely; the synthetic code-js provider (RFC J) reads it to (a) resolve
// which agent's JS file to run — Request has no agent name — (b) populate the
// JS `run({metadata})` argument, and (c) derive the per-run seed + clock
// anchor that make its replay execution deterministic by construction (RFC J
// Appendix B): seed = hash(RunID), Date.now() anchor = StartedAt.
//
// It lives on the leaf `providers` package (not `internal/tools`, whose
// RunIdentity ctx key is unexported and which providers must not import —
// the one-way provider→loop→tools layering boundary). The agent LOOP, which
// imports both, stamps it once per run before driving Provider.Call.
//
// Credentials are deliberately ABSENT: a code-agent's tool calls are
// dispatched by the loop, where the existing ${run.credentials.<name>}
// substitution applies at the MCP transport boundary. The JS never sees
// bearer values (RFC F posture preserved).
type RunMeta struct {
	AgentName string
	UserID    string
	// RunID is a stable per-run identifier (the agent-instance id). It seeds
	// the code-js deterministic RNG so every replay re-execution of a run
	// regenerates the identical Math.random() sequence. Stable across a run's
	// turns; may change on cross-process resume (an accepted v1 limitation —
	// full restart-stable seeding would thread the persisted run id).
	RunID string
	// StartedAt is the run's wall-clock start, the anchor for code-js's
	// deterministic Date.now(): reads return StartedAt + a monotonic per-call
	// offset, so time advances within a run yet every replay reproduces it.
	// Zero when unstamped (tests / non-loop callers) — the provider then
	// falls back to a fixed epoch.
	StartedAt time.Time
	// CodeBody is the inline code-js orchestrator source resolved from the
	// agent's AgentDef (substrate/yaml `code_body`, RFC J). Non-empty ⇒ the
	// code-js provider compiles+runs this body; empty ⇒ it falls back to
	// agent_code/<AgentName>/index.js. This is the symmetry seam that lets a
	// code agent run with no host filesystem bind. The loop populates it from
	// the resolved AgentDef; every LLM driver ignores it.
	CodeBody string

	// Metadata / PayloadMetadata carry the run's NON-SECRET structured
	// metadata to the code-js provider (it surfaces them as input.metadata /
	// input.payload_metadata in run(input)). Metadata is TRUSTED (def/wire);
	// PayloadMetadata is UNTRUSTED (external-trigger-body projection). LLM
	// drivers ignore both — for those agents the loop serialises the metadata
	// into prompt segments instead. Credentials remain deliberately ABSENT.
	Metadata        map[string]any
	PayloadMetadata map[string]any
}

type ctxKeyRunMeta struct{}

// WithRunMeta returns ctx carrying meta. The loop calls this once per run.
func WithRunMeta(ctx context.Context, meta RunMeta) context.Context {
	return context.WithValue(ctx, ctxKeyRunMeta{}, meta)
}

// RunMetaFromContext returns the stamped RunMeta, or the zero value when none
// was set (every non-code-js path). The bool reports presence so a provider
// can distinguish "no meta" from "empty agent name".
func RunMetaFromContext(ctx context.Context) (RunMeta, bool) {
	m, ok := ctx.Value(ctxKeyRunMeta{}).(RunMeta)
	return m, ok
}
