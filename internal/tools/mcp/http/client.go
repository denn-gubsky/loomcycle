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
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/tools"
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
	// BlockPrivateIPs, when true, makes the default client refuse to dial
	// private / loopback / metadata IPs at connect time (DNS-rebinding
	// protection). DEFAULT false: MCP servers are commonly operator-run on
	// localhost / a private network — including loomcycle's own jobs-search-agent
	// /api/mcp consumer — so blocking by default would break them. Operators opt
	// in via LOOMCYCLE_MCP_ALLOW_PRIVATE_IPS=0. Ignored when HTTPClient is set.
	BlockPrivateIPs bool
	// PrivateHostAllowlist exempts specific hosts from BlockPrivateIPs (the
	// operator's vouch list, reusing LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST).
	PrivateHostAllowlist []string
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
		if cfg.BlockPrivateIPs {
			// Opt-in DNS-rebinding guard: an allowlisted MCP host that rebinds to
			// a private/metadata IP is refused at dial time. Default path stays a
			// plain client (unchanged) so internal/localhost MCP servers work.
			hc = netguard.NewGuardedClient(defaultTimeout, cfg.PrivateHostAllowlist)
		} else {
			hc = &http.Client{Timeout: defaultTimeout}
		}
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

	// MCP Streamable HTTP servers may reply with either application/json
	// (single-shot) or text/event-stream (a single SSE frame for the
	// response when the server side is running on an SSE-capable
	// transport). Both are spec-compliant — we accept whichever the
	// server picked. For SSE, extract the JSON from the `data:` line(s)
	// of the first complete frame.
	jsonBody := respBody
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		extracted, ok := extractSSEData(respBody)
		if !ok {
			return nil, fmt.Errorf("mcp http: SSE response has no data line (body: %s)", truncate(string(respBody), 200))
		}
		jsonBody = extracted
	}

	var rpcResp mcp.Response
	if err := json.Unmarshal(jsonBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("mcp http: decode response: %w (body: %s)", err, truncate(string(respBody), 200))
	}
	// JSON-RPC 2.0 requires the response id to match the request id. The
	// streamable-HTTP single-POST shape makes correlation implicit, but a
	// misbehaving server returning a stale id is a protocol violation we
	// surface rather than silently accept (and feed back to the model).
	if rpcResp.ID != id {
		return nil, fmt.Errorf("mcp http: response id %d does not match request id %d (server protocol violation)", rpcResp.ID, id)
	}
	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}
	return rpcResp.Result, nil
}

// Notify sends a JSON-RPC notification (no response expected). MCP servers
// respond 202 Accepted to notifications. ctx caps the request — callers
// pass the same ctx they used for surrounding Call invocations so a
// run-level cancellation tears down the whole chain consistently.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
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
	// MCP Streamable HTTP spec (2024-11-05) requires the client to advertise
	// both content-types. Servers that conform strictly (e.g., the official
	// @modelcontextprotocol/sdk's WebStandardStreamableHTTPServerTransport)
	// reject anything else with HTTP 406 Not Acceptable. The server picks
	// per-request whether to reply JSON or SSE based on what the response
	// shape is; we must accept both.
	req.Header.Set("Accept", "application/json, text/event-stream")
	// v0.8.x per-run MCP bearer: substitute ${run.user_bearer} (and
	// the ${run.user_bearer:-FALLBACK} POSIX form) at request-build
	// time. The Client is shared across runs (see pool.go's contract),
	// so substitution MUST be per-request — never against c.headers
	// in-place. drop=true means a bare ${run.user_bearer} survived
	// without a fallback because ctx carried no bearer; we drop the
	// header rather than send a literal placeholder downstream.
	// The MCP server's own auth check then returns a clean 401 that
	// the loop surfaces as a typed tool error — more debuggable than
	// a loomcycle-side dispatch failure.
	runIdent := tools.RunIdentity(ctx)
	for k, v := range c.headers {
		// Two-pass substitution: legacy ${run.user_bearer} first
		// (v0.8.x single-bearer form), then RFC F ${run.credentials.<n>}.
		// Both are no-ops when their token isn't present; ordering
		// preserves v0.8.x behaviour bit-for-bit when only the legacy
		// form is in play.
		subV, drop := substituteRunVars(v, runIdent.UserBearer)
		if drop {
			log.Printf("mcp http: ${run.user_bearer} unresolved for header %q on %q (agent_id=%s, bearer=%s); dropping header",
				k, c.url, runIdent.AgentID, tokenPrefix(runIdent.UserBearer))
			continue
		}
		subV, credDrop, missingCreds := substituteCredentialRefs(subV, runIdent.UserCredentials)
		if credDrop {
			log.Printf("mcp http: ${run.credentials.<name>} unresolved for header %q on %q (agent_id=%s, missing=%v); dropping header",
				k, c.url, runIdent.AgentID, missingCreds)
			continue
		}
		req.Header.Set(k, subV)
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

// extractSSEData parses a single SSE response frame and returns the
// JSON payload joined from `data:` lines. SSE format per HTML Living
// Standard: lines starting with `data: ` carry the payload; multiple
// data lines in one frame are joined with `\n`. Returns the joined
// payload + true if at least one data line was found.
//
// We process only the FIRST complete frame (terminated by a blank
// line). For MCP Streamable HTTP, a JSON-RPC response is always a
// single frame; servers that emit multiple frames per response are
// using the streaming sub-shape we don't yet consume here.
func extractSSEData(body []byte) ([]byte, bool) {
	var out []byte
	found := false
	for _, line := range bytes.Split(body, []byte("\n")) {
		// Trim trailing CR (SSE permits CRLF).
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			// Blank line ends the current frame.
			if found {
				return out, true
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimPrefix(line, []byte("data:"))
			// SSE permits a single optional space after the colon.
			payload = bytes.TrimPrefix(payload, []byte(" "))
			if found {
				out = append(out, '\n')
			}
			out = append(out, payload...)
			found = true
		}
		// `event:`, `id:`, `retry:` and unknown fields are ignored.
	}
	return out, found
}
