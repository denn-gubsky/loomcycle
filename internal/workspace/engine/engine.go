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
	// Dial opens a byte stream to a port on the workspace's own loopback
	// (RFC GW §5.3, P9 plan §3.2): the host engine dials 127.0.0.1; the
	// container engine starts a relay inside the session, so the session
	// keeps the network it has and the worker never dials into it. It
	// reaches no other address. ErrUnreachable is a port nothing answers on.
	Dial(ctx context.Context, req DialRequest) (Conn, error)
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

// DialRequest is a port to reach in one workspace.
type DialRequest struct {
	// Workspace and Root are as ExecRequest's.
	Workspace string
	Root      string
	// Port is the port on the workspace's own loopback, 1 to 65535.
	Port int
	// Owners are the processes of the servers that declared Port. The host
	// engine reaches the port only when every listener on it is a process of
	// one of their groups: a declared port is not a bound one, and on the
	// host's shared loopback it could be the core's or any other service's.
	// The container engine needs none: its relay dials inside the session.
	Owners []Process
}

// Conn is a byte stream Dial opened.
type Conn interface {
	io.ReadWriteCloser
	// CloseWrite ends what is sent; what the port answers can still be
	// read.
	CloseWrite() error
}

// ErrUnreachable is a port nothing in the workspace answers on.
var ErrUnreachable = errors.New("nothing answers on the port")

// dialTimeout bounds reaching a port: a loopback connect answers at once,
// and the relay's own start is what takes time.
const dialTimeout = 30 * time.Second

func validPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("dial: port %d is not from 1 to 65535", port)
	}
	return nil
}
