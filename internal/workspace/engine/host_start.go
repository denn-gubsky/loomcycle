package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/hostenv"
	"github.com/denn-gubsky/loomcycle/internal/procgroup"
	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// stderrTail bounds the standard error a started program's Process keeps.
const stderrTail = 4 << 10

// Start starts req.Argv as a child of the worker in its own process group,
// with the scrubbed environment plus req.Env. The operator's command list
// does not apply to Argv: the worker starts only the kernel drivers it
// carries, and a kernel runs code as exec runs commands, so it is the same
// grant. It applies to req.Command, a server's command line, as to Exec.
func (h *Host) Start(ctx context.Context, req StartRequest) (Process, error) {
	argv, err := startArgv(h.Commands, req)
	if err != nil {
		return nil, err
	}
	extra, err := h.egressEnvFor(req.Workspace)
	if err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	// Not CommandContext: the program outlives the call that started it.
	// #nosec G204 -- the worker names the program (a kernel driver it carries and the workspace's interpreter, or /bin/sh for a server's command, checked against the operator's list as exec's is)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = req.Dir
	cmd.Env = append(append(hostenv.Scrubbed(nil), extra...), req.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	tail := &tailBuffer{max: stderrTail}
	cmd.Stderr = tail
	if err := procgroup.StartInOwnGroup(cmd); err != nil {
		return nil, fmt.Errorf("starting %s: %w", argv[0], err)
	}
	p := &hostProcess{cmd: cmd, stdin: stdin, stdout: stdout, tail: tail, done: make(chan struct{})}
	safego.Go(context.WithoutCancel(ctx), "workspace.engine.host-wait", func() {
		defer close(p.done)
		_ = cmd.Wait()
		// What the program started and left in its group dies with it.
		_ = procgroup.KillGroup(cmd)
	})
	return p, nil
}

type hostProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	tail   *tailBuffer
	done   chan struct{}
}

func (p *hostProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *hostProcess) Stdout() io.Reader     { return p.stdout }
func (p *hostProcess) Done() <-chan struct{} { return p.done }
func (p *hostProcess) Stderr() string        { return p.tail.String() }

// Interrupt signals the driver alone, not its group: a server a cell
// started in the background is not the cell, and keeps running.
func (p *hostProcess) Interrupt() error {
	err := p.cmd.Process.Signal(os.Interrupt)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (p *hostProcess) Kill() error { return procgroup.KillGroup(p.cmd) }

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(b) >= t.max {
		t.buf = append(t.buf[:0], b[len(b)-t.max:]...)
		return len(b), nil
	}
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
