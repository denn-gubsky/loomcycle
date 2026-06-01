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
// # Stateless replay execution (RFC J Appendix B)
//
// code-js honors the providers.Provider "stateless across calls" contract:
// it holds NO continuation, registry, or parked goroutine. Each Call builds a
// fresh goja Runtime, fast-forwards the tool results already recorded in the
// transcript (req.Messages — the durable memoization log), and stops at the
// first un-recorded call (the "frontier") via rt.Interrupt; the loop then
// dispatches that call and re-invokes Call with the result appended. The
// runtime is discarded within each Call — nothing is held across the loop's
// dispatch gap. Consequences: resumable across restart/replica for free; no
// goroutine leak class; cancel is just the Call's ctx. See replay.go.
//
// Replay is deterministic by construction: ambient non-determinism
// (Math.random / Date.now / new Date()) is hooked and regenerated from a
// per-run seed + clock anchor (sandbox.go), so re-execution reproduces every
// value the JS reads. The superseded parked-goroutine design is RFC J
// Appendix A (Mechanism 1).
package codejs

// ABIVersion is the semver of the JS-side API contract (Memory.* / Channel.*
// / Agent.* / mcp__*__* signatures, error shapes, returned types) — versioned
// SEPARATELY from loomcycle's release vector (RFC J Decision 14) so the
// implementation can evolve without binding to release cadence. Breaking a
// signature → major bump + deprecation window; additive methods → minor bump.
// Advertised on Provider.Info() and the code-agents-abi help topic.
const ABIVersion = "1.0.0"
