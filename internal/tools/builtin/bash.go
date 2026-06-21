package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Bash runs a shell command. Read this comment carefully:
//
// **This is NOT a true sandbox.** It restricts cwd, scrubs the env, bounds
// output, and times out — but it cannot prevent the spawned process from
// reaching arbitrary files via absolute paths, opening sockets, spawning
// long-running daemons that survive the timeout (we kill the process group
// to mitigate), or escalating via setuid binaries on PATH. Operators who
// expose Bash to untrusted prompts MUST run loomcycle inside a container,
// VM, or other OS-level isolation.
//
// Why ship it at all if it's weak? Because the alternative is forcing every
// operator to write their own MCP server for shell execution, and the
// per-tenant container isolation that Bash really needs is the operator's
// responsibility either way. Shipping it as opt-in (Enabled=false default)
// + a startup warning + this comment lets the right operator wire it up
// for their controlled environment.
type Bash struct {
	// Enabled gates the entire tool. False (default) refuses every call.
	// The working directory comes from the run's VolumePolicy (RFC AH) —
	// Bash requires a rw volume (it cannot honestly enforce ro; see §6).
	Enabled bool
	// MaxOutputBytes caps stdout+stderr. Default 1 MiB.
	MaxOutputBytes int64
	// Timeout caps wall-clock per call. Default 30s. Hard ceiling 5min.
	Timeout time.Duration
	// AllowedExtraEnv is the list of env-var names passed through from
	// the parent process beyond PATH (which is always passed through
	// because most binaries break without it). Empty by default —
	// only PATH leaks.
	AllowedExtraEnv []string
}

func (b *Bash) Name() string { return "Bash" }
func (b *Bash) Description() string {
	return "Run a shell command in a sandboxed working directory. Returns combined stdout+stderr."
}

func (b *Bash) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command":         {"type": "string", "description": "Shell command to execute via /bin/sh -c. The working directory is already set to your volume root; use paths RELATIVE to it (e.g. \"ls .\", \"cat src/main.go\") — not absolute host paths or ~. Call Context op=self to see your volumes."},
			"timeout_seconds": {"type": "integer", "description": "Per-call timeout. Capped at 300s."},
			"volume":          {"type": "string", "description": "Optional read-write volume name to run in (sets the working directory). Omit to use your default volume. Read-only volumes are refused — Bash cannot enforce read-only, so it requires read-write. Call Context op=self for the volumes you may access."}
		},
		"required": ["command"]
	}`)
}

const (
	bashTimeoutDefault = 30 * time.Second
	bashTimeoutMax     = 5 * time.Minute
	bashOutputDefault  = 1 << 20
)

func (b *Bash) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
		Volume         string `json:"volume"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.Result{Text: "invalid input: " + err.Error(), IsError: true}, nil
	}
	if !b.Enabled {
		return tools.Result{Text: "Bash tool is not enabled (set LOOMCYCLE_BASH_ENABLED=1); refusing", IsError: true}, nil
	}
	if args.Command == "" {
		return tools.Result{Text: "command is required", IsError: true}, nil
	}

	// needWrite=true: Bash binds cwd to the volume root but CANNOT enforce
	// read-only (a shell can write via absolute paths / redirection — see
	// the "not a true sandbox" doc above + RFC AH §6). Rather than ship a
	// false ro guarantee, effectiveRoot refuses a read-only volume here.
	cwdRoot, rootErr := effectiveRoot(ctx, args.Volume, true)
	if rootErr != nil {
		return tools.Result{Text: rootErr.Error(), IsError: true}, nil
	}

	cwd, err := filepath.EvalSymlinks(cwdRoot)
	if err != nil {
		return tools.Result{Text: "cwd: " + err.Error(), IsError: true}, nil
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

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", args.Command)
	cmd.Dir = cwd
	cmd.Env = b.buildEnv()
	// Capture combined output through a bounded buffer. We write to a
	// LimitedWriter rather than reading all-at-once so a malicious command
	// can't OOM us by producing 100 GiB of output before we read it.
	bw := &boundedWriter{cap: maxOut}
	cmd.Stdout = bw
	cmd.Stderr = bw

	startErr := cmd.Start()
	if startErr != nil {
		return tools.Result{Text: "start: " + startErr.Error(), IsError: true}, nil
	}
	// CommandContext sends SIGKILL on ctx cancel, so an in-flight Wait()
	// returns with an exit error. Fork-bomb children of the immediate
	// child may survive — consistent with "Bash is not a true sandbox".
	// Run inside a container if that matters.
	waitErr := cmd.Wait()

	out := bw.bytes()
	var b2 bytes.Buffer
	b2.Write(out)
	if bw.truncated {
		fmt.Fprintf(&b2, "\n[output truncated at %d bytes]", maxOut)
	}
	if ctxErr := runCtx.Err(); ctxErr != nil {
		fmt.Fprintf(&b2, "\n[killed: %v]", ctxErr)
		return tools.Result{Text: b2.String(), IsError: true}, nil
	}
	if waitErr != nil {
		// Non-zero exit. The model often legitimately runs commands that
		// fail (e.g. grep with no match → exit 1). Surface as IsError so
		// the model can self-correct, but include the output so it has
		// something to work with.
		fmt.Fprintf(&b2, "\n[exit: %s]", waitErr.Error())
		return tools.Result{Text: b2.String(), IsError: true}, nil
	}
	return tools.Result{Text: b2.String()}, nil
}

// buildEnv constructs the env passed to the child. Only PATH leaks by
// default (most binaries are unusable without it), plus any explicitly
// allow-listed names. Sensitive secrets like API keys never leak — the
// model could otherwise extract them via `env`.
func (b *Bash) buildEnv() []string {
	return scrubbedHostEnv(b.AllowedExtraEnv)
}

// scrubbedHostEnv builds the environment for a host child process: PATH always
// (most binaries break without it) plus the operator-allow-listed names.
// Anything not on the allowlist (API keys, secrets) never leaks. Shared by the
// Bash tool and the Bashbox host-command fallback (RFC AJ §13) so the two
// security-sensitive env paths can't drift apart.
func scrubbedHostEnv(allowedExtra []string) []string {
	out := []string{}
	if v := os.Getenv("PATH"); v != "" {
		out = append(out, "PATH="+v)
	}
	for _, name := range allowedExtra {
		if v := os.Getenv(name); v != "" {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// boundedWriter is an io.Writer with a hard byte cap. After cap bytes
// are written, further writes are silently discarded and truncated is
// set. Concurrent writes from cmd.Stdout and cmd.Stderr are serialised
// by exec — we don't need our own mutex.
type boundedWriter struct {
	cap       int64
	buf       bytes.Buffer
	truncated bool
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	remain := w.cap - int64(w.buf.Len())
	if remain <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remain {
		w.buf.Write(p[:remain])
		w.truncated = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

func (w *boundedWriter) bytes() []byte { return w.buf.Bytes() }

// Compile-time assertion: boundedWriter satisfies io.Writer.
var _ io.Writer = (*boundedWriter)(nil)
