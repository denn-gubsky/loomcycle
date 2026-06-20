package builtin

import (
	"context"
	"encoding/json"
	"fmt"
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

	// regOnce builds the command registry exactly once per tool instance: the
	// default builtins plus the opt-in contrib commands. The registry is then
	// shared read-only across concurrent calls (Lookup is RLock-guarded).
	regOnce sync.Once
	reg     commands.CommandRegistry
}

func (b *Bashbox) Name() string { return "Bashbox" }
func (b *Bashbox) Description() string {
	return "Run a shell command in a TRUE in-process sandbox (no OS process, virtual filesystem rooted at your volume, no network). Honors read-only volumes — writes never touch the host. Returns combined stdout+stderr."
}

func (b *Bashbox) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command":         {"type": "string", "description": "Shell command to run inside the sandbox. Use paths RELATIVE to your volume root (e.g. \"ls .\", \"grep -rn foo src\"). Common coreutils are available (grep/sed/awk/find/sort/uniq/cut/tr/wc/jq/...); NO host binaries (git/curl) and NO network are reachable. Call Context op=self to see your volumes."},
			"timeout_seconds": {"type": "integer", "description": "Per-call timeout. Capped at 300s."},
			"volume":          {"type": "string", "description": "Optional volume name to run in (sets the working directory). Omit to use your default volume. READ-ONLY volumes are allowed here — writes succeed inside the sandbox but never touch the host. Call Context op=self for the volumes you may access."}
		},
		"required": ["command"]
	}`)
}

// registry lazily assembles the command set once per tool instance: gbash's
// default builtins (grep/sed/find/sort/cut/tr/wc/...) plus the opt-in,
// pure-Go contrib commands agents commonly reach for (awk, jq). Built under a
// sync.Once and shared read-only thereafter.
func (b *Bashbox) registry() commands.CommandRegistry {
	b.regOnce.Do(func() {
		reg := gbash.DefaultRegistry()
		_ = awkcmd.Register(reg) // contrib/awk — pure-Go awk
		_ = jqcmd.Register(reg)  // contrib/jq  — pure-Go jq
		b.reg = reg
	})
	return b.reg
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
		gbash.WithRegistry(b.registry()),
		// Per-stream output cap inside gbash; combineOutput appends a marker.
		gbash.WithLimitOverrides(policy.Limits{MaxStdoutBytes: maxOut, MaxStderrBytes: maxOut}),
		// No network option → no egress (curl refused). Egress is a follow-up.
	)
	if err != nil {
		return tools.Result{Text: "bashbox init: " + err.Error(), IsError: true}, nil
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, runErr := rt.Run(runCtx, &gbash.ExecutionRequest{Script: args.Command, Timeout: timeout})
	if runErr != nil {
		// Covers ctx-deadline (timeout) and gbash-internal failures.
		msg := runErr.Error()
		if ctxErr := runCtx.Err(); ctxErr != nil {
			msg = fmt.Sprintf("%s [killed: %v]", msg, ctxErr)
		}
		return tools.Result{Text: msg, IsError: true}, nil
	}

	out := combineBashboxOutput(res, maxOut)
	if res.ExitCode != 0 {
		// Non-zero exit is often legitimate (grep no-match → 1). Surface as
		// IsError so the model can self-correct, but include the output.
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
		fmt.Fprintf(&sb, "\n[output truncated at %d bytes]", maxOut)
	}
	return sb.String()
}
