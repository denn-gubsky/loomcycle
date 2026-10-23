// Package wiring builds the memory services' runtime objects from a loaded
// config: the embedder, the search reranker and the Document unit generator.
//
// WHY A PACKAGE OF ITS OWN (RFC EX, decision D3): building one of these means
// resolving its provider endpoint and credential (internal/providerbuild) and
// putting its driver in that provider's call lane (internal/lanegate), both
// runtime concerns. When the builders lived beside the services, every memory
// package imported the runtime layer above it. Here the services keep their
// types and constructors (reranker.New, reranker.NewDecision, unitgen.New) and
// take an already-built driver, and this package, in the runtime layer, does
// the resolution and the gating.
//
// WHY IT IS NOT IN cmd/loomcycle: the server is not the only caller. Operator
// CLI subcommands (doctor, memory-calibrate) need the SAME embedder the running
// server builds, and a second copy of the per-provider auth/base-URL switch
// would drift — the CLI already learned that lesson once, when it loaded a
// single config file while the server layered presets and reported a false
// "no provider resolved" (see internal/cli.loadLayeredConfig).
//
// NOTE: the driver registry is populated by the blank imports in
// cmd/loomcycle/main.go, so a builder only resolves a provider in a binary that
// links them. A test in another package must import the driver it needs (or
// register a fake) exactly as before.
package wiring
