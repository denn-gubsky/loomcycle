// Package engine runs a workspace's commands (RFC GW §4.3). An Engine is what
// a workspace worker hands an exec op to; P1 has one, Host. The container
// engine (P3) is the one that isolates.
package engine

import (
	"context"
	"errors"
	"io"
	"time"
)

// DefaultMaxOutput bounds the combined stdout and stderr an exec keeps.
const DefaultMaxOutput = 1 << 20

// ExecRequest is one command in one workspace.
type ExecRequest struct {
	// Dir is the directory the command starts in: the workspace's directory
	// or one inside it, already confined and symlink-free.
	Dir string
	// Command is a shell command line.
	Command string
	// Timeout ends the command, and everything it started in its process
	// group, when it passes. The caller always sets it.
	Timeout time.Duration
}

// ExecResult is how a command ended.
type ExecResult struct {
	// ExitCode is the command's exit status; -1 when it was killed.
	ExitCode int
	// Output is stdout and stderr interleaved as they were written, cut at
	// the engine's bound.
	Output []byte
	// Truncated is true when the output was cut.
	Truncated bool
	// TimedOut is true when the timeout killed the command.
	TimedOut bool
}

// Engine runs commands for a workspace worker.
type Engine interface {
	// Exec runs one command and waits for it. An error is a command that
	// could not be run (refused, failed to start), never one that ran and
	// failed: that is a result with its exit code.
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
	// Start starts a long-lived program in a workspace and returns at once:
	// a workspace kernel's driver (RFC GW §5.7), which the worker speaks to
	// on the program's stdin and stdout. The program outlives ctx; it ends
	// when its stdin closes, or with Process.Kill.
	Start(ctx context.Context, req StartRequest) (Process, error)
	// Close releases what the engine holds.
	Close() error
}

// StartRequest is one long-lived program in one workspace.
type StartRequest struct {
	// Dir is the directory it starts in, as ExecRequest.Dir.
	Dir string
	// Argv is the program and its arguments; no shell reads it.
	Argv []string
	// Env is what the engine adds to its own scrubbed environment.
	Env []string
}

// Process is a program Start started.
type Process interface {
	// Stdin is the program's standard input; closing it asks it to end.
	Stdin() io.WriteCloser
	// Stdout is the program's standard output. Its standard error is not
	// the caller's to read: the engine keeps a bounded tail of it for
	// StartError.
	Stdout() io.Reader
	// Interrupt asks the program to stop what it is doing (SIGINT), without
	// ending it.
	Interrupt() error
	// Kill ends the program and everything it started that the engine can
	// reach (its process group on the host).
	Kill() error
	// Done is closed once the program has exited and been reaped.
	Done() <-chan struct{}
	// Stderr is the last bytes the program wrote to its standard error, for
	// a program that died before it spoke.
	Stderr() string
}

// ErrRefused is a command the engine will not run, such as one whose program
// the operator did not list.
var ErrRefused = errors.New("command refused")
