// Package engine runs a workspace's commands (RFC GW §4.3). An Engine is what
// a workspace worker hands an exec op to; P1 has one, Host. The container
// engine (P3) is the one that isolates.
package engine

import (
	"context"
	"errors"
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
	// Close releases what the engine holds.
	Close() error
}

// ErrRefused is a command the engine will not run, such as one whose program
// the operator did not list.
var ErrRefused = errors.New("command refused")
