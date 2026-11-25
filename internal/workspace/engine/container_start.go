package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// startWrapper records the program's pid in /tmp under the call's token ($1)
// and becomes the program (the rest of the arguments), so the pid recorded is
// the program's own and leads its process group, as execWrapper's shell does.
// The record outlives the program; Kill removes it.
const startWrapper = `f=/tmp/.loom-exec-$1; echo $$ > "$f" || exit 125; shift; exec "$@"`

// interruptScript signals the recorded program alone, not its group: a
// server a cell started is not the cell.
const interruptScript = `p=$(cat /tmp/.loom-exec-$1) && kill -INT $p`

// stopScript kills the recorded program's group and drops the record.
const stopScript = `p=$(cat /tmp/.loom-exec-$1) && kill -9 -$p; rm -f /tmp/.loom-exec-$1`

// Start starts req.Argv (or req.Command, a server's command line checked
// against the operator's list) in its workspace's session with stdin and stdout
// attached (`exec -i`), under the session's environment plus req.Env. It
// runs inside the same confinement as every command: the session's user,
// limits, read-only root and no network. Interrupt and Kill go through the
// engine (an exec into the session), since ending the CLI client would leave
// the program running in the session.
func (c *Container) Start(ctx context.Context, req StartRequest) (Process, error) {
	argv, err := startArgv(c.opt.Commands, req)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(req.Root, req.Dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, errors.New("start: the directory is not in the workspace")
	}
	for _, kv := range req.Env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !plainName(name) {
			return nil, fmt.Errorf("start: %q is not NAME=value", kv)
		}
	}
	if err := c.ensure(ctx, req.Workspace, req.Root); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	name := c.session(req.Workspace)
	args := []string{"exec", "-i", "-w", path.Join(WorkspaceMount, filepath.ToSlash(rel))}
	for _, kv := range req.Env {
		args = append(args, "--env", kv)
	}
	args = append(args, name, "/bin/sh", "-c", startWrapper, "sh", token)
	args = append(args, argv...)
	// Not CommandContext: the program outlives the call that started it.
	// #nosec G204 -- the CLI is the operator's setting, run without a shell; the program is one the worker carries, started inside the session
	cmd := exec.Command(c.opt.CLI, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// An os.Pipe, not cmd.StdoutPipe, for the reason Host.Start gives: the
	// reap below would close the pipe under a reader of a client that
	// exited quickly, losing the end of its output (a relay's last bytes).
	stdout, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = w
	tail := &tailBuffer{max: stderrTail}
	cmd.Stderr = tail
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("start: %w", err)
	}
	p := &containerProcess{c: c, name: name, token: token, cmd: cmd, stdin: stdin, stdout: &closeAtEnd{f: stdout}, tail: tail, done: make(chan struct{})}
	safego.Go(context.WithoutCancel(ctx), "workspace.engine.container.start-wait", func() {
		defer close(p.done)
		_ = cmd.Wait()
	})
	return p, nil
}

type containerProcess struct {
	c      *Container
	name   string
	token  string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	tail   *tailBuffer
	done   chan struct{}
	once   sync.Once
}

func (p *containerProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *containerProcess) Stdout() io.Reader     { return p.stdout }
func (p *containerProcess) Done() <-chan struct{} { return p.done }
func (p *containerProcess) Stderr() string        { return p.tail.String() }

// exited reports whether the client returned: the program ended, so its
// recorded pid may already name another process of the session.
func (p *containerProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *containerProcess) Interrupt() error {
	if p.exited() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), killGrace)
	defer cancel()
	_, err := p.c.cli(ctx, "exec", p.name, "/bin/sh", "-c", interruptScript, "sh", p.token)
	return err
}

// Kill ends the program's group in the session, then the CLI client, so the
// caller's reads end. A session that is gone took the program with it.
func (p *containerProcess) Kill() error {
	var err error
	p.once.Do(func() {
		if !p.exited() {
			ctx, cancel := context.WithTimeout(context.Background(), killGrace)
			defer cancel()
			_, err = p.c.cli(ctx, "exec", p.name, "/bin/sh", "-c", stopScript, "sh", p.token)
		}
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
	return err
}
