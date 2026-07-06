package clienttools

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
)

// Sentinel errors from Invoke, so the resolver can map each to a clear,
// actionable tool error the agent sees (the failures the Channel bridge could
// never surface — RFC BC §2/§5.3).
var (
	// ErrNoClient — no live connection for this principal provides the tool
	// (either no connection at all, or none registered this name).
	ErrNoClient = errors.New("no client connection provides this tool")
	// ErrClientDisconnected — the connection dropped while the invoke was
	// in flight; the run continues with a failed tool call, never hangs.
	ErrClientDisconnected = errors.New("client disconnected during the call")
	// ErrTooManyConnections — the per-principal connection cap is reached.
	ErrTooManyConnections = errors.New("too many client-tool connections for this principal")
)

// PrincipalKey identifies the run + connection meeting point: the authoritative
// (tenant, subject) the bearer resolves to (== RunIdentity.TenantID/UserID). A
// connection only ever serves runs of its own key (the RFC BC trust floor).
type PrincipalKey struct {
	Tenant  string
	Subject string
}

// Conn is one live client connection. The WebSocket read/write lives in the
// HTTP handler; Conn owns the registered schemas + the pending invoke↔result
// correlation. `send` is the handler-provided, mutex-guarded frame writer.
type Conn struct {
	id    string
	key   PrincipalKey
	tools map[string]ToolSchema // bare tool name → schema
	send  func(context.Context, any) error

	mu      sync.Mutex
	pending map[string]chan ResultFrame // call_id → waiter
	closed  bool
}

// ID is the server-assigned connection id (echoed in hello_ok). Diagnostic.
func (c *Conn) ID() string { return c.id }

// Provides reports whether this connection registered the named (bare) tool.
func (c *Conn) Provides(tool string) bool {
	_, ok := c.tools[tool]
	return ok
}

// DeliverResult routes an inbound result frame to its waiting invoke. Called by
// the handler read-loop. A result for an unknown/expired call_id is dropped
// (the invoke already timed out or the conn is closing) — never blocks.
func (c *Conn) DeliverResult(f ResultFrame) {
	c.mu.Lock()
	ch, ok := c.pending[f.CallID]
	if ok {
		delete(c.pending, f.CallID)
	}
	c.mu.Unlock()
	if ok {
		ch <- f // buffered (cap 1); the waiter is guaranteed to be selecting
	}
}

// failAllPending resolves every in-flight invoke with a disconnect error. Called
// once on teardown so no waiter hangs past the socket's death.
func (c *Conn) failAllPending() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pend := c.pending
	c.pending = map[string]chan ResultFrame{}
	c.mu.Unlock()
	for id, ch := range pend {
		ch <- ResultFrame{Type: FrameResult, CallID: id, OK: false, Error: ErrClientDisconnected.Error()}
	}
}

// Registry maps a principal to its live connections. Mirrors
// internal/steer/registry.go (RWMutex + Register→deregister-closure) with a
// per-key slice of connections (a user may hold several — e.g. two browser
// windows). Process-global, in-memory: it vanishes on restart, and a client
// re-connects + re-hello's; there is no durable record (a connection IS the
// live route to a machine).
type Registry struct {
	mu        sync.RWMutex
	byKey     map[PrincipalKey][]*Conn
	maxPerKey int
	counter   atomic.Uint64 // monotonic source for connection + call ids
}

// NewRegistry returns an empty registry. maxPerKey caps concurrent connections
// per principal (<=0 → default 8) — a DoS bound.
func NewRegistry(maxPerKey int) *Registry {
	if maxPerKey <= 0 {
		maxPerKey = 8
	}
	return &Registry{byKey: map[PrincipalKey][]*Conn{}, maxPerKey: maxPerKey}
}

func (r *Registry) newID(prefix string) string {
	return prefix + strconv.FormatUint(r.counter.Add(1), 10)
}

// Register installs a connection for a principal and returns it plus a
// deregister closure (defer it — panic-safe; it fails any pending invokes).
// send is the handler's mutex-guarded frame writer. Errors with
// ErrTooManyConnections when the per-principal cap is reached.
func (r *Registry) Register(key PrincipalKey, tools []ToolSchema, send func(context.Context, any) error) (*Conn, func(), error) {
	r.mu.Lock()
	if len(r.byKey[key]) >= r.maxPerKey {
		r.mu.Unlock()
		return nil, nil, ErrTooManyConnections
	}
	c := &Conn{
		id:      r.newID("ctc-"),
		key:     key,
		tools:   toolMap(tools),
		send:    send,
		pending: map[string]chan ResultFrame{},
	}
	// Newest last — Invoke picks the most-recently-registered provider (RFC §5.3).
	r.byKey[key] = append(r.byKey[key], c)
	r.mu.Unlock()

	return c, func() {
		r.mu.Lock()
		conns := r.byKey[key]
		for i, cc := range conns {
			if cc == c {
				r.byKey[key] = append(conns[:i], conns[i+1:]...)
				break
			}
		}
		if len(r.byKey[key]) == 0 {
			delete(r.byKey, key)
		}
		r.mu.Unlock()
		c.failAllPending()
	}, nil
}

// Provides returns the union of tool schemas the principal's live connections
// advertise (deduped by name, newest wins) — the enumerator's source for
// advertising client-tools to the model.
func (r *Registry) Provides(key PrincipalKey) []ToolSchema {
	r.mu.RLock()
	conns := append([]*Conn(nil), r.byKey[key]...)
	r.mu.RUnlock()
	seen := map[string]ToolSchema{}
	for _, c := range conns { // oldest→newest, so newest overwrites
		for name, sch := range c.tools {
			seen[name] = sch
		}
	}
	out := make([]ToolSchema, 0, len(seen))
	for _, sch := range seen {
		out = append(out, sch)
	}
	return out
}

// InvokeMeta carries the run context onto the invoke frame (observability on the
// client side; the client may render "run X is asking to read the page").
type InvokeMeta struct {
	RunID   string
	AgentID string
}

// Invoke sends a tool call to the most-recently-registered connection that
// provides the tool and blocks for the reply. The caller's ctx bounds the wait
// (a WithTimeout, and run-cancel unblocks it). Returns:
//   - the ResultFrame on a client reply (ok true or false),
//   - ErrNoClient when no live connection provides the tool,
//   - ErrClientDisconnected when the connection dropped mid-call,
//   - ctx.Err() on timeout / cancel.
func (r *Registry) Invoke(ctx context.Context, key PrincipalKey, tool string, input json.RawMessage, meta InvokeMeta) (ResultFrame, error) {
	c := r.pick(key, tool)
	if c == nil {
		return ResultFrame{}, ErrNoClient
	}
	callID := r.newID("call-")
	ch := make(chan ResultFrame, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ResultFrame{}, ErrClientDisconnected
	}
	c.pending[callID] = ch
	c.mu.Unlock()

	frame := InvokeFrame{
		Type: FrameInvoke, CallID: callID, RunID: meta.RunID,
		AgentID: meta.AgentID, Tool: tool, Input: input,
	}
	if err := c.send(ctx, frame); err != nil {
		// Send failed — clean up the pending entry so it can't leak.
		c.mu.Lock()
		delete(c.pending, callID)
		c.mu.Unlock()
		return ResultFrame{}, ErrClientDisconnected
	}

	select {
	case res := <-ch:
		if !res.OK && res.Error == ErrClientDisconnected.Error() {
			return res, ErrClientDisconnected
		}
		return res, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, callID)
		c.mu.Unlock()
		return ResultFrame{}, ctx.Err()
	}
}

// pick returns the most-recently-registered live connection for key that
// provides tool, or nil.
func (r *Registry) pick(key PrincipalKey, tool string) *Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	conns := r.byKey[key]
	for i := len(conns) - 1; i >= 0; i-- { // newest first
		if conns[i].Provides(tool) {
			return conns[i]
		}
	}
	return nil
}

// Count returns the total number of live connections. Test/diagnostic helper.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, conns := range r.byKey {
		n += len(conns)
	}
	return n
}

func toolMap(ts []ToolSchema) map[string]ToolSchema {
	m := make(map[string]ToolSchema, len(ts))
	for _, t := range ts {
		m[t.Name] = t
	}
	return m
}
