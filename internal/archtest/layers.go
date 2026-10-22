// Package archtest guards the import layering of this module (RFC EX Phase 0).
//
// It holds no production code. This file declares where every package sits;
// layers_test.go reads the real import graph with `go list` and fails on an
// import that points the wrong way. The point is to stop the graph from getting
// worse while RFC EX Phase 2 cuts the existing cycles: knownViolations lists
// today's wrong-way imports, and it may only shrink.
//
// The rules, applied to every non-test import between two packages of this
// module (_test.go files are not checked: they are not compiled into the
// package, so they cannot make it depend on anything):
//
//   - Every package belongs to the subsystem whose directory is the longest
//     prefix of its path, and to that subsystem's layer. A package no
//     subsystem covers fails the test, so a new package is placed on purpose.
//   - An import inside one subsystem is always allowed.
//   - An import into a LOWER layer is allowed.
//   - An import into the SAME layer is allowed only when the subsystem pair is
//     in allowedSideways. That set must stay acyclic: it records the intended
//     order inside a layer, and a cycle in it would permit exactly the
//     subsystem cycles this package exists to remove.
//   - Anything else — an import into a higher layer, or an unlisted sideways
//     one — fails, unless the exact package edge is in knownViolations. An
//     entry there that no longer matches a wrong-way import also fails, so the
//     Phase 2 change that removes an import deletes its entry in the same PR.
//
// Placement follows a package's role, not just its imports. When a package's
// role is low (config is schema) but it imports something high, it stays low
// and the edge is a known violation: moving it up would hide the problem Phase
// 2 has to fix. A subpackage gets its own subsystem only when its role differs
// from its parent's: hooks/codehook runs hook bodies inside tool dispatch and
// needs the tools package, while hooks (the definitions and the webhook seam)
// does not.
//
// Test-support packages (storetest, credentialtest, mcptest, ...) follow the
// same rules as any other package. Every one of them is imported only by
// _test.go files today, but each ships in its subsystem's directory — and so in
// that subsystem's module once Phase 3 splits modules — so its imports form
// cycles exactly as production imports do (storetest's import of channels is
// the whole store↔channels cycle).
//
// The graph is read for the host GOOS/GOARCH. The in-module edges are identical
// on linux, darwin and windows today; should a build-tagged file ever add an
// edge on one platform only, CI's linux run is the authority.
package archtest

// layer is one rung of the layering. subsystems are directories relative to the
// module root; each covers itself and every package below it that a longer
// listed directory does not cover.
type layer struct {
	name       string
	subsystems []string
}

// layers is ordered from the bottom (index 0) up. An import may point down.
var layers = []layer{
	{
		// Leaf utilities: in-memory registries, codecs, guards, signers. They
		// import nothing in this module except another L0 package (see
		// allowedSideways). The root api package is the embedded OpenAPI
		// document, data only.
		name: "L0 utilities",
		subsystems: []string{
			"api",
			"internal/a2a/sign",
			"internal/audit",
			"internal/awsauth",
			"internal/breakpoints",
			"internal/cancel",
			"internal/concurrency",
			"internal/cryptoops",
			"internal/decisionq",
			"internal/dynvol",
			"internal/errkind",
			"internal/fence",
			"internal/gitsource",
			"internal/jsonpath",
			"internal/kms",
			"internal/logging",
			"internal/modelver",
			"internal/netguard",
			"internal/otel",
			"internal/procgroup",
			"internal/recipes",
			"internal/redact",
			"internal/search",
			"internal/sigv1",
			"internal/skillmatch",
			"internal/statepatch",
			"internal/steer",
			"internal/turncancel",
		},
	},
	{
		// The provider interface and its drivers, model resolution, and the
		// decision-model client (it asks a model typed questions through a
		// provider and imports nothing above L1; wiring it to lanes and
		// endpoints is decision/decisionbuild, in L5).
		name: "L1 providers",
		subsystems: []string{
			"internal/decision",
			"internal/providers",
			"internal/recall",
			"internal/resolve",
		},
	},
	{
		// Definitions: config, principals, credentials, and the content-
		// addressed Agent/Skill/MCPServer/hook definitions.
		name: "L2 schema",
		subsystems: []string{
			"internal/agents",
			"internal/auth",
			"internal/claudeimport",
			"internal/config",
			"internal/credential",
			"internal/hooks",
			"internal/mcp",
			"internal/skills",
		},
	},
	{
		// Persistence and the run state kept on it.
		name: "L3 storage",
		subsystems: []string{
			"internal/awaited",
			"internal/channels",
			"internal/coord",
			"internal/heartbeat",
			"internal/limits",
			"internal/metrics",
			"internal/pause",
			"internal/runstate",
			"internal/snapshot",
			"internal/sqlmem",
			"internal/store",
			"internal/usage",
		},
	},
	{
		// Memory, documents and the services that read or erase across them.
		// There is no separate document or path package: both live in
		// internal/sqlmem (L3) and the Document/Path tools (L5).
		name: "L4 memory",
		subsystems: []string{
			"internal/changesub",
			"internal/directory",
			"internal/docremote",
			"internal/erasure",
			"internal/memory",
			"internal/retention",
		},
	},
	{
		// The agent loop, tools, and everything that drives a run.
		name: "L5 runtime",
		subsystems: []string{
			"internal/a2a",
			"internal/capabilities",
			"internal/channelhooks",
			"internal/clienttools",
			"internal/connector",
			"internal/contextplugin",
			"internal/decision/decisionbuild",
			"internal/errclassify",
			"internal/help",
			"internal/hooks/codehook",
			"internal/lanegate",
			"internal/lookup",
			"internal/loop",
			"internal/providerbuild",
			"internal/runner",
			"internal/scheduler",
			"internal/teamgraph",
			"internal/teamrun",
			"internal/throughput",
			"internal/tools",
			"internal/tools/builtin",
			"internal/wiring",
		},
	},
	{
		// Transports and the CLI.
		name: "L6 surfaces",
		subsystems: []string{
			"internal/api",
			"internal/cli",
			"internal/webui",
		},
	},
	{
		name: "L7 entry",
		subsystems: []string{
			"cmd/loomcycle",
		},
	},
	{
		// Benchmarks, CI helpers, load tests and this package: nothing imports
		// them, and they may import anything.
		name: "L8 tooling",
		subsystems: []string{
			"bench",
			"internal/archtest",
			"scripts",
			"test",
		},
	},
}

// subsystemEdge is an import permission between two subsystems of one layer.
type subsystemEdge struct {
	from, to string
}

// allowedSideways is the intended order inside each layer. Adding a pair is a
// design decision, so it belongs in review; the test refuses a pair that would
// close a cycle.
var allowedSideways = []subsystemEdge{
	// L0
	{"internal/kms", "internal/awsauth"},
	{"internal/logging", "internal/redact"},

	// L1
	{"internal/decision", "internal/providers"},
	{"internal/recall", "internal/providers"},

	// L2
	{"internal/agents", "internal/hooks"},
	{"internal/config", "internal/agents"},
	{"internal/config", "internal/auth"},
	{"internal/config", "internal/credential"},
	{"internal/config", "internal/hooks"},
	{"internal/hooks", "internal/credential"},

	// L3: store is the base of the layer; coord sits on it, and the run-state
	// services sit on both.
	{"internal/awaited", "internal/store"},
	{"internal/channels", "internal/coord"},
	{"internal/channels", "internal/store"},
	{"internal/coord", "internal/store"},
	{"internal/heartbeat", "internal/store"},
	{"internal/limits", "internal/store"},
	{"internal/metrics", "internal/store"},
	{"internal/pause", "internal/coord"},
	{"internal/pause", "internal/store"},
	{"internal/runstate", "internal/coord"},
	{"internal/runstate", "internal/store"},
	{"internal/snapshot", "internal/limits"},
	{"internal/snapshot", "internal/sqlmem"},
	{"internal/snapshot", "internal/store"},
	{"internal/usage", "internal/store"},

	// L4
	{"internal/retention", "internal/memory"},

	// L5: tools defines the tool interface; loop runs it; runner and connector
	// drive the loop; tools/builtin and the run-level services sit on top.
	{"internal/a2a", "internal/connector"},
	{"internal/a2a", "internal/loop"},
	{"internal/a2a", "internal/runner"},
	{"internal/channelhooks", "internal/tools"},
	{"internal/clienttools", "internal/tools"},
	{"internal/connector", "internal/loop"},
	{"internal/connector", "internal/runner"},
	{"internal/connector", "internal/tools"},
	{"internal/decision/decisionbuild", "internal/lanegate"},
	{"internal/decision/decisionbuild", "internal/providerbuild"},
	{"internal/errclassify", "internal/runner"},
	{"internal/errclassify", "internal/tools"},
	{"internal/hooks/codehook", "internal/tools"},
	{"internal/lanegate", "internal/teamrun"},
	{"internal/loop", "internal/contextplugin"},
	{"internal/loop", "internal/tools"},
	{"internal/runner", "internal/loop"},
	{"internal/scheduler", "internal/errclassify"},
	{"internal/scheduler", "internal/lanegate"},
	{"internal/scheduler", "internal/loop"},
	{"internal/scheduler", "internal/runner"},
	{"internal/teamrun", "internal/teamgraph"},
	{"internal/tools", "internal/lookup"},
	{"internal/tools/builtin", "internal/capabilities"},
	{"internal/tools/builtin", "internal/connector"},
	{"internal/tools/builtin", "internal/errclassify"},
	{"internal/tools/builtin", "internal/help"},
	{"internal/tools/builtin", "internal/lanegate"},
	{"internal/tools/builtin", "internal/lookup"},
	{"internal/tools/builtin", "internal/loop"},
	{"internal/tools/builtin", "internal/teamgraph"},
	{"internal/tools/builtin", "internal/teamrun"},
	{"internal/tools/builtin", "internal/throughput"},
	{"internal/tools/builtin", "internal/tools"},
	{"internal/wiring", "internal/lanegate"},
	{"internal/wiring", "internal/providerbuild"},

	// L6
	{"internal/api", "internal/webui"},
}

// The RFC EX Phase 2 cuts that remove the known violations. Each names the
// change, so a failure message says which work an entry is waiting on.
const (
	cutEmbedded    = "move cmd/loomcycle/embedded under internal/ (cmd<->api<->cli cycle)"
	cutConfig      = "config's validators move down to config's layer (config hub)"
	cutStoreIfaces = "consumer-declared store interfaces (D4)"
	cutMemWiring   = "memory services' drivers built by the wiring layer and passed in (D3)"
	cutRunIdentity = "run identity moves below tools"
)

// knownViolation is one wrong-way import that exists today, as an exact
// package edge (paths relative to the module root).
type knownViolation struct {
	from, to string
	cut      string
}

// knownViolations only shrinks. A new wrong-way import is fixed, not added
// here: the test fails on an entry that no longer matches, so when a cut lands
// its entries go with it.
var knownViolations = []knownViolation{
	{"internal/api/http", "cmd/loomcycle/embedded", cutEmbedded},
	{"internal/cli", "cmd/loomcycle/embedded", cutEmbedded},

	// memory's grammar, store.ParseRetention and the team-channel names,
	// teamgraph's name/var validators.
	{"internal/config", "internal/memory", cutConfig},
	{"internal/config", "internal/store", cutConfig},
	{"internal/config", "internal/teamgraph", cutConfig},

	{"internal/credential", "internal/store", cutStoreIfaces},
	{"internal/credential/credentialtest", "internal/store", cutStoreIfaces},

	{"internal/memory/reranker", "internal/lanegate", cutMemWiring},
	{"internal/memory/reranker", "internal/providerbuild", cutMemWiring},
	{"internal/memory/unitgen", "internal/lanegate", cutMemWiring},
	{"internal/memory/unitgen", "internal/providerbuild", cutMemWiring},

	// tools.RunIdentity / tools.RunID.
	{"internal/docremote", "internal/tools", cutRunIdentity},
	{"internal/memory/backends/inprocess", "internal/tools", cutRunIdentity},
	{"internal/memory/backends/remote", "internal/tools", cutRunIdentity},
}
