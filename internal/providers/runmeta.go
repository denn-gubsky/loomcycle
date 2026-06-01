package providers

import "context"

// RunMeta carries the small slice of run identity a Provider may need that
// is NOT part of the LLM-shaped Request. The canonical LLM drivers ignore it
// entirely; the synthetic code-js provider (RFC J) reads it to (a) resolve
// which agent's JS file to run — Request has no agent name — and (b) populate
// the JS `run({metadata})` argument.
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
