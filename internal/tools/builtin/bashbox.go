package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/ewhauser/gbash"
	"github.com/ewhauser/gbash/commands"
	awkcmd "github.com/ewhauser/gbash/contrib/awk"
	jqcmd "github.com/ewhauser/gbash/contrib/jq"
	"github.com/ewhauser/gbash/policy"
)

// Bashbox runs a shell command in a TRUE in-process sandbox, in contrast to
// the Bash tool's "restricted, not isolated" os/exec model (see bash.go).
//
// gbash (github.com/ewhauser/gbash, Apache-2.0, pure-Go) reimplements the
// common coreutils against a virtual filesystem and spawns NO operating-system
// process: there is no /bin/sh fork, no PATH lookup, no way to reach a host
// binary, and no network by default. That changes what we can honestly
// promise:
//
//   - **Read-only volumes are honored.** Bash REFUSES a ro volume because a
//     real shell defeats path-confinement (absolute paths, redirection — RFC
//     AH §6 / CLAUDE.md rule #7). Bashbox instead mounts a ro volume under
//     gbash's in-RAM write overlay: writes succeed *inside* the sandbox but
//     never mutate the host tree. The ro guarantee is real, so rule #7 is
//     lifted for THIS tool's code path (Bash keeps its refusal).
//
//   - **No host filesystem escape.** Every path is rooted at the mounted
//     volume; there is no absolute-path back door to the host.
//
//   - **No network.** v1 exposes no egress at all (gbash defaults to network
//     off and we add no network option) — curl and friends are refused.
//     Opt-in, operator-allowlisted egress is a deliberate follow-up so its
//     URL-prefix matching gets its own review.
//
//   - **Host-command fallback (RFC AJ §13, operator opt-in).** gbash can't run
//     commands it doesn't implement (git, gh, …). When the operator allowlists
//     such names (FallbackCommands), each gets a host-exec proxy that runs the
//     REAL host binary — but ONLY those names; every other command stays
//     sandboxed (so `git status; curl evil` runs git on the host and curl in
//     the sandbox — no smuggling escape). Fallback requires a rw volume (a host
//     process can't honor the ro overlay) and is off by default.
//
// Opt-in like Bash: Enabled=false refuses every call; enable per deployment
// with LOOMCYCLE_BASHBOX_ENABLED=1 and per agent via allowed_tools:[Bashbox].
//
// gbash is alpha (pinned in go.mod). The opt-in posture is the escape hatch:
// if a gbash bug surfaces, drop Bashbox from the agent's allowed_tools.
//
// Stateless per call (matches Bash's per-call os/exec): each Execute builds a
// fresh runtime and runs one script in a fresh session — no cross-call state,
// so there is nothing to snapshot across pause/resume.
type Bashbox struct {
	// Enabled gates the entire tool. False (default) refuses every call.
	Enabled bool
	// MaxOutputBytes caps stdout and stderr (each) inside gbash. Default 1 MiB.
	MaxOutputBytes int64
	// Timeout caps wall-clock per call. Default 30s. Hard ceiling 5min.
	Timeout time.Duration

	// FallbackCommands names host commands (e.g. git, gh) that gbash does NOT
	// implement and that the operator allows to ESCAPE the sandbox to the real
	// host shell (RFC AJ §13). Empty (default) = no fallback. Operator-only,
	// never model-supplied. Each listed name is registered as a host-exec
	// proxy; every other command stays sandboxed. Requires a rw volume (a host
	// process can't honor the in-RAM ro overlay) — a fallback command on a ro
	// volume refuses.
	FallbackCommands []string
	// FallbackAllowedEnv names env vars passed into fallback commands (e.g.
	// GH_TOKEN, HOME, SSH_AUTH_SOCK). Injected ONLY into the host child, never
	// into the sandbox env — the model can't read them via `env`. PATH always
	// passes. Empty (default) = only PATH.
	FallbackAllowedEnv []string

	// regOnce builds the base command registry exactly once per tool instance
	// (default builtins + contrib awk/jq), shared read-only across concurrent
	// calls (Lookup is RLock-guarded). Used only when no fallback is
	// configured; with fallback the per-call proxies force a fresh registry.
	regOnce sync.Once
	reg     commands.CommandRegistry
}

func (b *Bashbox) Name() string { return "Bashbox" }
func (b *Bashbox) Description() string {
	d := "Run a shell command in a TRUE in-process sandbox (no OS process, virtual filesystem rooted at your volume, no network). Honors read-only volumes — writes never touch the host. Returns combined stdout+stderr."
	if len(b.FallbackCommands) > 0 {
		// Tell the model which host commands the operator has allowlisted to
		// run on the real host (they require a read-write volume).
		d += " The operator has allowlisted these host commands to run on the real host (read-write volume required): " + strings.Join(b.FallbackCommands, ", ") + "."
	}
	return d
}

func (b *Bashbox) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command":         {"type": "string", "description": "Shell command to run inside the sandbox. Use paths RELATIVE to your volume root (e.g. \"ls .\", \"grep -rn foo src\"). Common coreutils are available (grep/sed/awk/find/sort/uniq/cut/tr/wc/jq/...). Host binaries and network are NOT reachable unless the operator allowlisted specific host commands — see this tool's description for which (if any). Call Context op=self to see your volumes."},
			"timeout_seconds": {"type": "integer", "description": "Per-call timeout. Capped at 300s."},
			"volume":          {"type": "string", "description": "Optional volume name to run in (sets the working directory). Omit to use your default volume. READ-ONLY volumes are allowed here — writes succeed inside the sandbox but never touch the host. Call Context op=self for the volumes you may access."}
		},
		"required": ["command"]
	}`)
}

// buildRegistry assembles the gbash command set for one call: the default
// builtins (grep/sed/find/sort/cut/tr/wc/...) plus the opt-in pure-Go contrib
// commands (awk, jq). When the operator configured host-command fallback it
// ALSO registers a host-exec proxy for each allowlisted name (RFC AJ §13),
// bound to this call's resolved host root + ro/rw mode.
//
// With no fallback the registry is identical across calls, so it is built once
// under a sync.Once and shared read-only. With fallback the proxies capture
// per-call state (the host root), so a fresh registry is built per call.
func (b *Bashbox) buildRegistry(hostRoot string, readOnly bool) commands.CommandRegistry {
	if len(b.FallbackCommands) == 0 {
		b.regOnce.Do(func() {
			reg := gbash.DefaultRegistry()
			_ = awkcmd.Register(reg) // contrib/awk — pure-Go awk
			_ = jqcmd.Register(reg)  // contrib/jq  — pure-Go jq
			b.reg = reg
		})
		return b.reg
	}
	reg := gbash.DefaultRegistry()
	_ = awkcmd.Register(reg)
	_ = jqcmd.Register(reg)
	for _, name := range b.FallbackCommands {
		// A proxy OVERRIDES a same-named gbash builtin (Register replaces by
		// name) — operators shouldn't allowlist commands gbash already has
		// unless they specifically want host behavior.
		_ = reg.Register(b.fallbackProxy(name, hostRoot, readOnly))
	}
	return reg
}

// fallbackProxy returns a gbash command that execs the REAL host binary `name`
// (RFC AJ §13) — the deliberate, operator-gated escape from the sandbox. Only
// the operator-allowlisted names get one, so every other command in a script
// still runs in gbash (a `git status; curl evil` script runs git on the host
// but curl in the sandbox, where it has no network). It composes with gbash:
// args + stdin/stdout/stderr come from the invocation, so pipes and redirection
// work, and the exit code is propagated.
//
// A host process cannot honor gbash's in-RAM read-only overlay, so on a ro
// volume the proxy REFUSES rather than write to the real host behind a false
// read-only guarantee.
//
// The binary that runs is always the operator's `name` resolved on the host
// PATH — never a model-supplied path. gbash resolves a path-form command
// (`/usr/bin/git`) by basename, so it maps to this proxy, but the proxy execs
// the captured `name`, so the model can't substitute a different binary via the
// path it types. A non-allowlisted basename never gets a proxy at all → it
// stays sandboxed.
func (b *Bashbox) fallbackProxy(name, hostRoot string, readOnly bool) commands.Command {
	return commands.DefineCommand(name, func(ctx context.Context, inv *commands.Invocation) error {
		if readOnly {
			fmt.Fprintf(inv.Stderr, "%s: requires a read-write volume — the host-command fallback cannot honor read-only\n", name)
			return &commands.ExitError{Code: 1}
		}
		// Translate the sandbox cwd to a real host path, re-checking containment
		// so a `cd ../..` can't run the host command outside the volume.
		hostCwd, err := fallbackHostCwd(hostRoot, inv.Cwd)
		if err != nil {
			fmt.Fprintf(inv.Stderr, "%s: %v\n", name, err)
			return &commands.ExitError{Code: 1}
		}
		// inv.Args excludes argv[0] (the command name) — see find.go:86.
		// ctx is the timeout-bound run context, so CommandContext SIGKILLs the
		// direct child on timeout/cancel. As with the Bash tool, descendants the
		// child spawns (a git credential helper, hook, pager) may survive — this
		// is not a container; run loomcycle in one for hard isolation.
		cmd := exec.CommandContext(ctx, name, inv.Args...)
		cmd.Dir = hostCwd
		cmd.Env = scrubbedHostEnv(b.FallbackAllowedEnv)
		cmd.Stdin = inv.Stdin
		cmd.Stdout = inv.Stdout
		cmd.Stderr = inv.Stderr
		runErr := cmd.Run()
		if runErr == nil {
			return nil
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			// The host command ran and exited non-zero — propagate its code.
			return &commands.ExitError{Code: exitErr.ExitCode()}
		}
		// Couldn't start it (binary not on the host PATH, etc.).
		fmt.Fprintf(inv.Stderr, "%s: %v\n", name, runErr)
		return &commands.ExitError{Code: 127}
	})
}

// fallbackHostCwd maps the sandbox working directory to a real host path. In rw
// mode (the only mode fallback runs in) gbash roots the sandbox at "/" == the
// host volume root, so a sandbox cwd of "/sub" is hostRoot/sub. resolveInsideRoot
// re-validates containment (EvalSymlinks + relInsideRoot), so a path that
// escapes the volume is rejected rather than run on the host.
func fallbackHostCwd(hostRoot, sandboxCwd string) (string, error) {
	rel := strings.TrimPrefix(filepath.ToSlash(sandboxCwd), "/")
	resolved, err := resolveInsideRoot(hostRoot, filepath.FromSlash(rel))
	if err != nil {
		return "", fmt.Errorf("working directory %q escapes the volume: %w", sandboxCwd, err)
	}
	return resolved, nil
}

func (b *Bashbox) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
		Volume         string `json:"volume"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.Result{Text: "invalid input: " + err.Error(), IsError: true}, nil
	}
	if !b.Enabled {
		return tools.Result{Text: "Bashbox tool is not enabled (set LOOMCYCLE_BASHBOX_ENABLED=1); refusing", IsError: true}, nil
	}
	if args.Command == "" {
		return tools.Result{Text: "command is required", IsError: true}, nil
	}

	// Bashbox HONORS read-only volumes (unlike Bash): a ro binding mounts
	// under gbash's in-RAM write overlay, so resolve WITHOUT needWrite. The
	// readOnly flag below selects the mount mode.
	root, readOnly, _, rootErr := resolveVolume(ctx, args.Volume)
	if rootErr != nil {
		return tools.Result{Text: rootErr.Error(), IsError: true}, nil
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return tools.Result{Text: "volume root: " + err.Error(), IsError: true}, nil
	}

	timeout := b.Timeout
	if timeout == 0 {
		timeout = bashTimeoutDefault
	}
	if args.TimeoutSeconds > 0 {
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
	}
	if timeout > bashTimeoutMax {
		timeout = bashTimeoutMax
	}
	maxOut := b.MaxOutputBytes
	if maxOut == 0 {
		maxOut = bashOutputDefault
	}

	// Mount mode is the load-bearing trust decision:
	//   ro → HostDirectoryFileSystem: host tree read-only + in-RAM write
	//        overlay; writes are visible in-sandbox but DISCARDED at end.
	//   rw → ReadWriteDirectoryFileSystem: writes persist to the host volume.
	var fsOpt gbash.Option
	if readOnly {
		fsOpt = gbash.WithFileSystem(gbash.HostDirectoryFileSystem(rootResolved, gbash.HostDirectoryOptions{}))
	} else {
		fsOpt = gbash.WithFileSystem(gbash.ReadWriteDirectoryFileSystem(rootResolved, gbash.ReadWriteDirectoryOptions{}))
	}

	rt, err := gbash.New(
		fsOpt,
		gbash.WithRegistry(b.buildRegistry(rootResolved, readOnly)),
		// Per-stream output cap inside gbash; combineOutput appends a marker.
		gbash.WithLimitOverrides(policy.Limits{MaxStdoutBytes: maxOut, MaxStderrBytes: maxOut}),
		// No network option → gbash itself has no egress. Operator-allowlisted
		// host commands (FallbackCommands) run as real processes and DO have
		// host network — that's inherent to the fallback (RFC AJ §13).
	)
	if err != nil {
		return tools.Result{Text: "bashbox init: " + err.Error(), IsError: true}, nil
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, runErr := rt.Run(runCtx, &gbash.ExecutionRequest{Script: args.Command, Timeout: timeout})
	if runErr != nil {
		// A genuine gbash-internal failure. A wall-clock TIMEOUT does NOT land
		// here: gbash returns (result, nil) with ExitCode 124 and an
		// "execution timed out after <d>" stderr line, surfaced via the
		// exit-code path below.
		return tools.Result{Text: "bashbox: " + runErr.Error(), IsError: true}, nil
	}

	out := combineBashboxOutput(res, maxOut)
	if res.ExitCode != 0 {
		// Non-zero exit is often legitimate (grep no-match → 1; a timeout is
		// exit 124, with gbash's "execution timed out" line already in `out`).
		// Surface as IsError so the model can self-correct, output preserved.
		out += fmt.Sprintf("\n[exit: %d]", res.ExitCode)
		return tools.Result{Text: out, IsError: true}, nil
	}
	return tools.Result{Text: out}, nil
}

// combineBashboxOutput renders stdout followed by stderr (gbash keeps them
// separate; the Bash tool interleaves them) and appends a truncation marker
// when gbash capped either stream.
func combineBashboxOutput(res *gbash.ExecutionResult, maxOut int64) string {
	var sb strings.Builder
	sb.WriteString(res.Stdout)
	if res.Stderr != "" {
		if sb.Len() > 0 && !strings.HasSuffix(res.Stdout, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString(res.Stderr)
	}
	if res.StdoutTruncated || res.StderrTruncated {
		// gbash caps each stream independently, so combined output can reach
		// 2*maxOut; the cap is per-stream.
		fmt.Fprintf(&sb, "\n[output truncated at %d bytes per stream]", maxOut)
	}
	return sb.String()
}
