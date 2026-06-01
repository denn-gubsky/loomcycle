// Package codejs implements the RFC J synthetic "code-js" Provider: a
// providers.Provider that runs operator-authored JavaScript (via goja)
// instead of calling an LLM API. From the rest of loomcycle's perspective a
// code-agent IS an agent — same loop, same OTEL spans, same scheduler /
// webhook / A2A reachability, same sub-agent composition — differing only in
// its cost / determinism profile.
//
// # Loop-driven dispatch (the load-bearing design)
//
// code-js is a Provider like any other: it streams providers.Event and, when
// the JS calls a tool, emits EventToolCall + StopReason "tool_use" exactly as
// an LLM provider does. The agent LOOP dispatches that tool (its ctx, hooks,
// OTEL spans, ${run.credentials} substitution) and re-invokes Provider.Call
// with the result. The provider NEVER imports internal/tools and never
// dispatches a tool itself — preserving loomcycle's one-way provider→loop→
// tools layering. Prior art: internal/providers/mock/mcp_caller.go.
//
// # Statefulness departure
//
// Unlike every other provider (which is stateless across calls — see
// providers.Provider doc), code-js MUST hold a continuation across Call
// invocations: a real JS run() with local variables/loops cannot be
// reconstructed by replay. The continuation is a parked goroutine running the
// goja Runtime (Appendix-A Mechanism 1), keyed by a run-scoped token the
// provider mints into the tool_use ID and reads back from the round-tripped
// tool_result. It is torn down on completion / cancel / timeout; the run
// timeout is the universal leak backstop. See continuation.go.
package codejs

// ABIVersion is the semver of the JS-side API contract (memory.* / channel.*
// / agent.* / mcp__*__* signatures, error shapes, returned types) — versioned
// SEPARATELY from loomcycle's release vector (RFC J Decision 14) so the
// implementation can evolve without binding to release cadence. Breaking a
// signature → major bump + deprecation window; additive methods → minor bump.
// Advertised on Provider.Info() and the code-agents-abi help topic.
const ABIVersion = "1.0.0"
