// Package stdio implements an MCP transport over a child process's stdin/stdout.
// One Client manages one child process. Concurrent Call() invocations multiplex
// over the same stdio pair — JSON-RPC IDs correlate responses to callers.
package stdio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"

	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// Config configures a stdio MCP client.
type Config struct {
	// Command is the executable to run (e.g. "npx").
	Command string
	// Args is passed verbatim to the child.
	Args []string
	// Env is appended to the child's environment. Each entry is "KEY=VALUE".
	Env []string
	// OnNotification, when non-nil, is invoked for every incoming
	// notification (e.g. progress updates). Optional.
	OnNotification func(mcp.Notification)
	// OnStderr, when non-nil, is called for every line the child writes to
	// stderr. Useful for surfacing server-side logs in test output.
	// The default behaviour is to discard stderr.
	OnStderr func(string)
}

// Client is one connection to a stdio MCP server. Spawn() spawns the child
// and starts the reader goroutine; Call sends a request and waits for the
// matching response. Close kills the child and fails any in-flight Calls.
type Client struct {
	cfg Config

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	ids     mcp.IDGenerator
	writeMu sync.Mutex // serialises writes to stdin

	pendMu  sync.Mutex
	pending map[int64]chan mcp.Response // request ID → response sink

	closed  atomic.Bool
	doneCh  chan struct{} // closed when the reader goroutine exits
	exitErr error         // set before doneCh closes; read after
}

// Spawn starts a child process and returns a connected Client. The caller
// must Close the client to terminate the child.
func Spawn(cfg Config) (*Client, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...) //nolint:gosec // command is operator-supplied via YAML
	if len(cfg.Env) > 0 {
		cmd.Env = append(cmd.Environ(), cfg.Env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	c := &Client{
		cfg:     cfg,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		pending: make(map[int64]chan mcp.Response),
		doneCh:  make(chan struct{}),
	}
	go c.readLoop()
	go c.stderrLoop()
	return c, nil
}

// Call sends a JSON-RPC request and waits for the matching response. Returns
// ctx.Err() if ctx fires; *mcp.Error if the server returned a JSON-RPC error
// object; or a transport error if the child died mid-call.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.closed.Load() {
		return nil, errors.New("mcp: client closed")
	}
	id := c.ids.Next()
	req, err := mcp.NewRequest(id, method, params)
	if err != nil {
		return nil, err
	}

	respCh := make(chan mcp.Response, 1)
	c.pendMu.Lock()
	c.pending[id] = respCh
	c.pendMu.Unlock()
	defer func() {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
	}()

	if err := c.write(req); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.doneCh:
		// Reader exited (child died). Surface a useful error.
		if c.exitErr != nil {
			return nil, fmt.Errorf("mcp: server exited: %w", c.exitErr)
		}
		return nil, errors.New("mcp: server exited")
	}
}

// Notify sends a JSON-RPC notification (no response expected).
func (c *Client) Notify(method string, params any) error {
	if c.closed.Load() {
		return errors.New("mcp: client closed")
	}
	n, err := mcp.NewNotification(method, params)
	if err != nil {
		return err
	}
	return c.write(n)
}

// Close terminates the child process. Any in-flight Calls receive a
// transport error via doneCh. Idempotent.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	// Closing stdin signals EOF to a well-behaved server, which should
	// exit cleanly. If it doesn't, Wait() below will hang until we kill.
	_ = c.stdin.Close()
	// Best-effort kill if it hasn't exited; ignore errors.
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	<-c.doneCh
	return nil
}

// Wait waits for the reader goroutine to exit (child closed stdout). Useful
// when the caller wants to drain a finite-script test server.
func (c *Client) Wait() error {
	<-c.doneCh
	return c.exitErr
}

// write serialises a JSON value as one newline-terminated line on stdin.
// Concurrent callers are serialised by writeMu so a write doesn't interleave
// with another's (JSON would still parse, but mixing is brittle).
func (c *Client) write(v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(buf); err != nil {
		return err
	}
	if _, err := c.stdin.Write([]byte("\n")); err != nil {
		return err
	}
	return nil
}

// readLoop is the demuxer: it pulls lines off stdout, decodes each as either
// a Response (routed to pending[id]) or a Notification (handed to OnNotification).
func (c *Client) readLoop() {
	defer close(c.doneCh)
	defer c.failPending()

	// 16 MiB per-line cap. Real-world MCP servers (brave-search, fetch,
	// filesystem) routinely return single JSON-RPC frames > 4 MiB when a
	// tool result includes raw HTML or a large file. Hitting the cap
	// produces bufio.ErrTooLong, which kills the connection — we'd rather
	// allocate up to 16 MiB once than mark the server permanently dead.
	scanner := bufio.NewScanner(c.stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Copy because scanner reuses its buffer.
		buf := make([]byte, len(line))
		copy(buf, line)

		frame := mcp.DecodeFrame(buf)
		switch {
		case frame.ParseErr != nil:
			// Malformed line — log via stderr handler if any, otherwise drop.
			if c.cfg.OnStderr != nil {
				c.cfg.OnStderr("mcp: parse error: " + frame.ParseErr.Error())
			}
		case frame.Response != nil:
			c.deliver(*frame.Response)
		case frame.Notification != nil:
			if c.cfg.OnNotification != nil {
				c.cfg.OnNotification(*frame.Notification)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		c.exitErr = err
	}
	// Reap the child so it doesn't linger as a zombie.
	if waitErr := c.cmd.Wait(); waitErr != nil && c.exitErr == nil {
		c.exitErr = waitErr
	}
}

// stderrLoop forwards every stderr line to the configured handler (or
// discards if none). Exits when the child closes stderr.
func (c *Client) stderrLoop() {
	scanner := bufio.NewScanner(c.stderr)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		if c.cfg.OnStderr != nil {
			c.cfg.OnStderr(scanner.Text())
		}
	}
}

func (c *Client) deliver(resp mcp.Response) {
	c.pendMu.Lock()
	ch, ok := c.pending[resp.ID]
	c.pendMu.Unlock()
	if !ok {
		// Late or stray response — no caller waiting. Drop silently.
		return
	}
	select {
	case ch <- resp:
	default:
		// Caller already gave up (ctx); buffer was 1; drop.
	}
}

// failPending cancels all in-flight Calls when the reader goroutine exits.
// They observe the closed doneCh in their select.
func (c *Client) failPending() {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	c.pending = map[int64]chan mcp.Response{}
}
