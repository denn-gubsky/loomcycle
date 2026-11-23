// Package engine runs a workspace's commands (RFC GW §4.3). An Engine is what
// a workspace worker hands an exec op to: Host runs it as a child of the
// worker, with no isolation; Container runs it in a container of its own per
// workspace, the engine that isolates (P3).
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// DefaultMaxOutput bounds the combined stdout and stderr an exec keeps.
const DefaultMaxOutput = 1 << 20

// ExecRequest is one command in one workspace.
type ExecRequest struct {
	// Workspace is the workspace's id (workspace.ValidID).
	Workspace string
	// Root is the workspace's directory on the worker's disk, symlink-free.
	Root string
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
	// Forget ends what the engine keeps for a workspace that is being
	// removed: its session and everything still running in it.
	Forget(ctx context.Context, workspace string) error
	// Close releases what the engine holds.
	Close() error
}

// StartRequest is one long-lived program in one workspace.
type StartRequest struct {
	// Workspace and Root are as ExecRequest's.
	Workspace string
	Root      string
	// Dir is the directory it starts in, as ExecRequest.Dir.
	Dir string
	// Argv is the program and its arguments; no shell reads it.
	Argv []string
	// Command, instead of Argv, is a shell command line a model wrote: a
	// workspace's server (RFC GW §5.3). The engine checks it against the
	// operator's command list exactly as Exec does, so start cannot run
	// what exec refuses, and runs it with /bin/sh -c, its standard error
	// joined to its standard output.
	Command string
	// Env is what the engine adds to its own scrubbed environment.
	Env []string
}

// serverShell runs a StartRequest's Command ($1) with its standard error on
// its standard output, so a server's log is one stream in the order written.
const serverShell = `exec 2>&1; eval "$1"`

// startArgv is the program Start runs for req: Argv as it is, or Command
// through the shell once commands (the operator's list) allows it.
func startArgv(commands []string, req StartRequest) ([]string, error) {
	if req.Command == "" {
		if len(req.Argv) == 0 {
			return nil, fmt.Errorf("%w: an empty program", ErrRefused)
		}
		return req.Argv, nil
	}
	if len(req.Argv) != 0 {
		return nil, errors.New("start: both a program and a command")
	}
	if err := allowed(commands, req.Command); err != nil {
		return nil, err
	}
	return []string{"/bin/sh", "-c", serverShell, "sh", req.Command}, nil
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
