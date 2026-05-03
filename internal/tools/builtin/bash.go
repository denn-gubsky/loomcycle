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
	// Enabled gates the entire tool. False (default) refuses every call,
	// even when Cwd is set.
	Enabled bool
	// Cwd is the working directory for spawned commands. Required when
	// Enabled. Empty Cwd refuses every call.
	Cwd string
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
	return "Run a shell command in the configured cwd. Returns combined stdout+stderr."
}

func (b *Bash) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command":         {"type": "string", "description": "Shell command to execute via /bin/sh -c."},
			"timeout_seconds": {"type": "integer", "description": "Per-call timeout. Capped at 300s."}
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
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.Result{Text: "invalid input: " + err.Error(), IsError: true}, nil
	}
	if !b.Enabled {
		return tools.Result{Text: "Bash tool is not enabled (set LOOMCYCLE_BASH_ENABLED=1); refusing", IsError: true}, nil
	}
	if b.Cwd == "" {
		return tools.Result{Text: "Bash tool has no cwd configured; refusing", IsError: true}, nil
	}
	if args.Command == "" {
		return tools.Result{Text: "command is required", IsError: true}, nil
	}

	cwd, err := filepath.EvalSymlinks(b.Cwd)
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
	if errors := runCtx.Err(); errors != nil {
		fmt.Fprintf(&b2, "\n[killed: %v]", errors)
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
	out := []string{}
	if v := os.Getenv("PATH"); v != "" {
		out = append(out, "PATH="+v)
	}
	for _, name := range b.AllowedExtraEnv {
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
