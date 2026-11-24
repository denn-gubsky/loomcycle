package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/hostenv"
	"github.com/denn-gubsky/loomcycle/internal/procgroup"
)

// Host runs a command with /bin/sh -c as a child of the worker, as the
// worker's own user, in its own process group. It is NOT a sandbox: the
// command can read and write anything the worker's user can, including the
// worker's token, and reach the network. It exists for a trusted machine and
// to build the rest on; the container engine (RFC GW P3) isolates.
//
// What it does bound: the environment (PATH and nothing else, hostenv), the
// time (the timeout kills the whole process group, background children
// included), and the output kept (MaxOutput).
type Host struct {
	// MaxOutput bounds the combined output kept; zero is DefaultMaxOutput.
	MaxOutput int
	// Commands, when not empty, are the programs a command may start with
	// (LOOMCYCLE_WORKSPACE_COMMANDS). The command must then be one simple
	// command: shell control characters are refused, since `allowed; other`
	// would otherwise run other. It names programs, not what they do: an
	// allowed interpreter runs any code it is given.
	Commands []string
	// Egress, when set, serves each workspace's routes on a loopback port
	// of its own, opened at its first command, closed by Forget, and named
	// in its commands' environment (host_egress.go). Not a boundary.
	Egress func(workspace string) http.Handler

	egressMu sync.Mutex
	egress   map[string]*hostEgress
}

func (h *Host) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	if strings.TrimSpace(req.Command) == "" {
		return ExecResult{}, fmt.Errorf("%w: an empty command", ErrRefused)
	}
	if err := allowed(h.Commands, req.Command); err != nil {
		return ExecResult{}, err
	}
	if req.Timeout <= 0 {
		return ExecResult{}, errors.New("exec: no timeout")
	}
	maxOut := h.MaxOutput
	if maxOut <= 0 {
		maxOut = DefaultMaxOutput
	}
	extra, err := h.egressEnvFor(req.Workspace)
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec: %w", err)
	}
	runCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	// #nosec G204 -- running the agent's command is this engine's purpose; the worker is opt-in, the command starts in the confined workspace directory with a scrubbed env, bounded output and a timeout that kills its process group
	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", req.Command)
	cmd.Dir = req.Dir
	procgroup.Bind(cmd)
	cmd.Env = append(hostenv.Scrubbed(nil), extra...)
	out := &capped{max: maxOut}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return ExecResult{}, fmt.Errorf("exec: starting the command: %w", err)
	}
	waitErr := cmd.Wait()
	res := ExecResult{ExitCode: -1}
	res.Output, res.Truncated = out.result()
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	// A command that exited before the deadline was not killed, even if the
	// deadline passed while its output drained (exec stops watching the
	// context at exit).
	if runCtx.Err() != nil && (cmd.ProcessState == nil || !cmd.ProcessState.Exited()) {
		res.TimedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		res.ExitCode = -1
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
	}
	if res.Truncated {
		res.Output = append(res.Output, fmt.Sprintf("\n[output truncated at %d bytes]", maxOut)...)
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) && !errors.Is(waitErr, exec.ErrWaitDelay) && !res.TimedOut {
		return res, fmt.Errorf("exec: %w", waitErr)
	}
	return res, nil
}

// Forget closes the workspace's egress port; nothing else is left: a host
// command does not outlive its exec.
func (h *Host) Forget(_ context.Context, workspace string) error {
	h.forgetEgress(workspace)
	return nil
}

func (h *Host) Close() error {
	h.closeEgress()
	return nil
}

// capped keeps the first max bytes written to it and counts the rest. Both
// stdout and stderr write to it, from exec's copying goroutines.
type capped struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.max - c.buf.Len()
	if room < len(p) {
		c.truncated = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		// Report the whole write as taken, so the command is not killed by
		// a broken pipe for writing more than is kept.
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *capped) result() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...), c.truncated
}
