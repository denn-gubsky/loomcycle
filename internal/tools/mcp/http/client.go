// Package http implements an MCP transport over HTTP. v0.3 supports the
// "single POST → JSON response" path of the MCP streamable-HTTP spec —
// enough for tool dispatch but not for server-pushed notifications. SSE
// upgrade can be added later without changing the Caller surface.
//
// Per-session state is held in the Mcp-Session-Id header the server
// hands out on the initialize response. The Client stores it after the
// first successful Call and attaches it to subsequent requests
// (including notifications). If the server invalidates a session it
// returns 404 with a JSON-RPC error; the Client surfaces that and the
// pool's Healthy check evicts the entry.
package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

const (
	defaultTimeout = 5 * time.Minute
	// sessionHeader is the MCP streamable-HTTP session correlator.
	sessionHeader = "Mcp-Session-Id"
)

// Config configures an HTTP MCP client.
type Config struct {
	// URL is the absolute endpoint, e.g. "https://mcp.example.com/v1".
	URL string
	// Headers are added to every request. Useful for Authorization tokens
	// per-tenant when the operator deploys a multi-tenant MCP server.
	Headers map[string]string
	// HTTPClient overrides the default. Tests pass an httptest-driven
	// client; production passes nil to use the package default.
	HTTPClient *http.Client
}

// Client speaks MCP over HTTP. Implements mcp.Caller.
type Client struct {
	url     string
	headers map[string]string
	http    *http.Client

	ids  mcp.IDGenerator
	dead atomic.Bool // set to true on session invalidation; Healthy returns false

	sessMu    sync.Mutex
	sessionID string // populated after first response that carries one
}

// New constructs a Client. URL must be absolute.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("mcp http: URL is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		url:     cfg.URL,
		headers: cfg.Headers,
		http:    hc,
	}, nil
}

// Call sends a JSON-RPC request and waits for the JSON response.
//
//   - 200 OK with JSON body → decode and return.
//   - 404 with JSON-RPC error → marks the client dead, surfaces the error.
//   - Other 4xx/5xx → transport error.
//   - Network failure → transport error.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.dead.Load() {
		return nil, errors.New("mcp http: session invalidated; create a new client")
	}
	id := c.ids.Next()
	req, err := mcp.NewRequest(id, method, params)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if sid := resp.Header.Get(sessionHeader); sid != "" {
		c.sessMu.Lock()
		c.sessionID = sid
		c.sessMu.Unlock()
	}

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted:
		// Fall through to parse.
	case resp.StatusCode == http.StatusNotFound:
		// Session was invalidated server-side. Mark dead so the pool
		// evicts and respawns on next Get.
		c.dead.Store(true)
		return nil, fmt.Errorf("mcp http: session invalidated (404): %s", strings.TrimSpace(string(respBody)))
	default:
		return nil, fmt.Errorf("mcp http: %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	if len(bytes.TrimSpace(respBody)) == 0 {
		// Some servers return 202 + empty body for notifications-as-requests.
		// For a real Call we expect a JSON-RPC response.
		return nil, errors.New("mcp http: empty response body for request")
	}

	var rpcResp mcp.Response
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("mcp http: decode response: %w (body: %s)", err, truncate(string(respBody), 200))
	}
	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}
	return rpcResp.Result, nil
}

// Notify sends a JSON-RPC notification (no response expected).
// MCP servers respond 202 Accepted to notifications.
func (c *Client) Notify(method string, params any) error {
	if c.dead.Load() {
		return errors.New("mcp http: session invalidated")
	}
	n, err := mcp.NewNotification(method, params)
	if err != nil {
		return err
	}
	body, err := json.Marshal(n)
	if err != nil {
		return err
	}
	// Notifications shouldn't block on a long-running ctx. Use a short
	// fire-and-forget timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := c.do(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("mcp http notify: %d", resp.StatusCode)
	}
	return nil
}

// Healthy reports whether the client can still reach the server. Returns
// false once we've observed a session-invalidated 404; the pool will then
// evict + respawn on the next Get.
func (c *Client) Healthy() bool { return !c.dead.Load() }

// Close is a no-op for HTTP — there's no persistent connection to tear
// down. Kept so the pool can call Close uniformly across transports.
func (c *Client) Close() error { return nil }

// do performs one POST with the JSON-RPC body and returns the raw http
// response (caller closes Body). Adds session header + operator headers.
func (c *Client) do(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	c.sessMu.Lock()
	sid := c.sessionID
	c.sessMu.Unlock()
	if sid != "" {
		req.Header.Set(sessionHeader, sid)
	}
	return c.http.Do(req)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
