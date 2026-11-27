package archtest

// Who may start a process (RFC GW §11 "Removal", P15). After the removal
// release no model-chosen command runs in the core: commands run on a
// workspace worker. process_test.go reads every non-test file of every
// package under internal/ and cmd/ of the in-tree modules and fails on a
// place that MAKES a process outside processStarters. The maker is the gate
// (D-P15-6): cmd.Start or cmd.Run on a command made elsewhere is not a site,
// and neither is exec.LookPath, so procgroup and hostenv, which act on a
// caller's command, are not listed.
//
// The list may only shrink: an entry whose package no longer makes a process
// fails too, so the change that removes the last site deletes the entry.
//
// Files are read with go/parser, not compiled, and every non-test .go file of
// a package directory counts whatever its build tags (a darwin-only file is
// checked on linux). A watched package is recognised by its import path and
// the name the file gives it, so a renamed import is still caught.

// processStarter is a package allowed to make a process, with the reason.
type processStarter struct {
	pkg    string
	reason string
	// testSupport marks a package that only tests may use: no non-test
	// package outside the test-support set may import it, directly or
	// through another package.
	testSupport bool
}

// processStarters is RFC GW §11's list, exactly. Paths are relative to the
// root module and match a package exactly, not its subpackages.
var processStarters = []processStarter{
	{pkg: "internal/conversion/local", reason: "the local worker supervisor: starts the core's own binary as conversion and workspace workers"},
	{pkg: "internal/tools/mcp/stdio", reason: "operator-configured stdio MCP servers (RFC GW D12)"},
	{pkg: "internal/transcribe/helper", reason: "operator-configured conversion and transcription helper programs (ffmpeg, pdftoppm, typst, diagram tools)"},
	{pkg: "internal/workspace/engine", reason: "the workspace engines, which run only in the worker role (see workerRole)"},
	{pkg: "internal/cli", reason: "interactive CLI commands, never the server: the OAuth browser open and the keychain tools"},
	{pkg: "internal/gitsource/gitsourcetest", reason: "test support (git)", testSupport: true},
	{pkg: "internal/workspace/workspacetest", reason: "test support (docker info)", testSupport: true},
}

// workerRole bounds where the workspace engines can run. The engines are
// linked into the one binary, so the bound is on who can reach them:
//
//   - every non-test package that imports an engine package, directly or
//     through others, is in packages (the worker side, plus test support);
//   - inside the binary's main package, only files may import any of those
//     packages, so no server boot file holds a reference to the engines;
//   - entry, the function that runs the worker role, is named in the main
//     package only by files and by entryCallers — the `worker` subcommand,
//     which exits when the worker does.
type workerRoleBound struct {
	engine       string
	mainPkg      string
	packages     []string
	files        []string
	entry        string
	entryCallers []string
}

var workerRole = workerRoleBound{
	engine:       "internal/workspace/engine",
	mainPkg:      "cmd/loomcycle",
	packages:     []string{"internal/workspace/worker", "internal/workspace/kernel", "cmd/loomcycle"},
	files:        []string{"worker_cmd.go", "worker_workspace_cmd.go"},
	entry:        "runWorker",
	entryCallers: []string{"boot_cli.go"},
}

// processMakers maps an import path to the names in it that make a process.
// exec.Cmd counts only as a composite literal: a *exec.Cmd parameter takes
// a command someone else made.
var processMakers = map[string]map[string]bool{
	"os/exec":               {"Command": true, "CommandContext": true},
	"os":                    {"StartProcess": true},
	"syscall":               {"ForkExec": true, "Exec": true, "StartProcess": true},
	"golang.org/x/sys/unix": {"Exec": true, "ForkExec": true},
}
