package anthropic_oauth_dev

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// Driver implements providers.Provider for the OAuth-dev path. It
// wraps an inner internal/providers/anthropic.Driver — the existing
// production driver does the heavy lifting (request body building, SSE
// streaming, rate-limit retry, cache_control placement). This driver
// adds two layers:
//
//  1. An HTTP transport that swaps `x-api-key` for
//     `Authorization: Bearer <token>`, appends the OAuth beta marker
//     (`claude-code-20250219,oauth-2025-04-20`) to `anthropic-beta`,
//     and overrides `user-agent` with the pinned Claude Code version
//     string. See transport.go.
//
//  2. A bidirectional name mask: outbound, loomcycle-only built-in tool
//     names (Memory, Channel, etc.) get renamed to
//     `mcp__loomcycle__<name>` so Anthropic's request shape detector
//     sees them as MCP tools. Inbound, `tool_use` events get the names
//     reversed before the loop sees them — the in-process tool
//     dispatcher continues to receive the canonical names (Memory,
//     Channel) and dispatches via the existing path.
//
// The mask + transport are the ONLY differences between this driver
// and `internal/providers/anthropic.Driver`. Everything else (SSE
// parsing, retry logic, error classification, capabilities) is
// delegated to the inner driver. This means the OAuth-dev path
// inherits all of the existing driver's behaviour without
// duplication.
type Driver struct {
	inner     *anthropic.Driver
	refresher *Refresher
	version   string // pinned Claude Code version; operator-overridable
}

// New constructs an OAuth-dev driver. tokens must already be loaded
// (operator ran `loomcycle anthropic login`) — when refresher.Token()
// returns the zero Token, every Call() refuses with a clear error
// pointing at the CLI subcommand.
//
// streamOpts controls per-stream timeouts (passed through to the inner
// anthropic.Driver verbatim — same SSE semantics). httpClient is
// optional; when nil, a fresh streaming client honouring
// streamOpts.HeaderTimeout is built.
//
// version is the Claude Code version string sent in User-Agent. Empty
// = PinnedClaudeCodeVersion. Operators override via the
// LOOMCYCLE_CLAUDE_CODE_VERSION env var (read at construction time in
// cmd/loomcycle/main.go).
func New(refresher *Refresher, streamOpts streamhttp.Options, version string, httpClient *http.Client) *Driver {
	if version == "" {
		version = PinnedClaudeCodeVersion
	}
	streamOpts = streamOpts.Resolve()
	if httpClient == nil {
		httpClient = streamhttp.NewClient(streamOpts.HeaderTimeout)
	}
	// Wrap the underlying transport with our OAuth-aware transport.
	// Whatever transport streamhttp.NewClient gave us, we layer on top.
	// The inner anthropic.Driver still sets x-api-key (with our placeholder
	// value) but the transport strips it before the request goes on the
	// wire, replacing it with Authorization: Bearer <token>.
	httpClient.Transport = &oauthTransport{
		base:      httpClient.Transport,
		refresher: refresher,
		version:   version,
	}
	// Pass a placeholder api key to the inner driver — it'll set
	// x-api-key in every request, but the transport strips it before
	// the bytes hit the wire. Empty would also work, but using a
	// non-empty placeholder avoids any "empty header" edge cases in
	// the inner driver's request building.
	inner := anthropic.New("oauth-dev-placeholder", "", streamOpts, httpClient)
	return &Driver{
		inner:     inner,
		refresher: refresher,
		version:   version,
	}
}

// ID is the unique provider ID used by the resolver + tier config.
// Distinct from the production anthropic driver's "anthropic" ID so
// operator yaml that pins `provider: anthropic-oauth-dev` is
// unambiguous.
func (d *Driver) ID() string { return "anthropic-oauth-dev" }

// Capabilities delegates to the inner driver. OAuth-dev uses the same
// Messages API; the capability surface is identical.
func (d *Driver) Capabilities() providers.Capabilities {
	return d.inner.Capabilities()
}

// Call applies the mask + delegates to the inner driver. The mask
// rewrites the outbound Request.Tools[] and previous-turn `tool_use`
// blocks in Request.Messages to use `mcp__loomcycle__*` names; on the
// returned event channel, every `tool_use` event has its Name reversed
// before being forwarded to the caller.
//
// Refuses immediately when no token is loaded — the operator hasn't
// run `loomcycle anthropic login` yet, so there's no way to
// authenticate.
func (d *Driver) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	if d.refresher.Token().AccessToken == "" {
		return nil, fmt.Errorf("anthropic-oauth-dev: no OAuth token loaded — run `loomcycle anthropic login`")
	}
	maskedReq := req
	maskedReq.Tools = MaskOutbound(req.Tools)
	maskedReq.Messages = MaskMessages(req.Messages)
	// v0.11.10 — OAuth mode requires the Claude Code identity as the
	// first system block. Without it, Anthropic's subscription-billing
	// validator returns a misleading "messages: Input should be a
	// valid array" 400. Pi reference: providers/anthropic.ts §
	// `if (isOAuthToken)` branch.
	maskedReq.System = adaptSystemForOAuth(req.System)
	innerCh, err := d.inner.Call(ctx, maskedReq)
	if err != nil {
		// v0.11.10 A2: subscription-quota detection on the SYNCHRONOUS
		// error path. Anthropic's 429 from the Messages API is
		// consumed by the inner driver's ratelimit.Do retry loop and
		// then returned here as `err`, formatted as "anthropic 429:
		// {body}". We sniff the body for "subscription" to wrap with
		// the typed sentinel for tier-policy fallback consumers.
		//
		// Detection on the EVENT channel (initial v0.11.10 commit)
		// was wrong: 429s never reach the event channel — they exit
		// here. Caught in code review.
		if isSubscriptionQuotaError(err.Error()) {
			return nil, &subscriptionQuotaErr{inner: err}
		}
		return nil, err
	}
	// Wrap the inner channel: copy events through, reversing the mask
	// on any tool_use event AND sniffing for subscription-quota-
	// exhausted error events. The wrap goroutine exits when innerCh
	// closes (clean stream end or error), at which point it closes the
	// outer channel.
	outCh := make(chan providers.Event, cap(innerCh)+1)
	go func() {
		defer close(outCh)
		for ev := range innerCh {
			if ev.ToolUse != nil && IsMasked(ev.ToolUse.Name) {
				// Copy the ToolUse so we don't mutate any reference
				// the inner driver retains.
				tu := *ev.ToolUse
				tu.Name = UnmaskInbound(ev.ToolUse.Name)
				ev.ToolUse = &tu
			}
			select {
			case outCh <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return outCh, nil
}

// isSubscriptionQuotaError pattern-matches an error string for the
// "429 + subscription" combo that signals subscription billing
// exhaustion under OAuth. Detection is intentionally generous: the
// inner anthropic driver formats 429s as "anthropic 429: {json}"
// which embeds Anthropic's error body verbatim. We sniff the body
// text for "subscription" (case-insensitive); a status-code-only
// check would false-positive on the more-common "429 rate-limited"
// case which should still be retryable.
//
// False-positive risk: if Anthropic ever ships an unrelated 429 with
// "subscription" elsewhere in the body, we'd wrap it as a quota
// error. Acceptable trade-off — the wrapping is additive (original
// error text preserved); tier-fallback consumers that match on
// ErrSubscriptionQuotaExhausted get a slightly broader catch than
// strict-string-equality would give.
func isSubscriptionQuotaError(errText string) bool {
	if errText == "" {
		return false
	}
	lower := strings.ToLower(errText)
	return strings.Contains(lower, "429") && strings.Contains(lower, "subscription")
}

// Probe delegates to the inner driver but with our transport — the
// /v1/models call goes through the OAuth transport, so a fresh token
// gets exercised before any real Call.
func (d *Driver) Probe(ctx context.Context) error {
	if d.refresher.Token().AccessToken == "" {
		return fmt.Errorf("anthropic-oauth-dev: no OAuth token loaded — run `loomcycle anthropic login`")
	}
	return d.inner.Probe(ctx)
}

// ListModels delegates to the inner driver. Same /v1/models endpoint,
// same response shape — subscription tokens see the same model
// catalogue as API keys.
func (d *Driver) ListModels(ctx context.Context) ([]string, error) {
	if d.refresher.Token().AccessToken == "" {
		return nil, fmt.Errorf("anthropic-oauth-dev: no OAuth token loaded — run `loomcycle anthropic login`")
	}
	return d.inner.ListModels(ctx)
}

// oauthTransport is the http.RoundTripper layer that:
//
//   - Strips `x-api-key` (set by the inner anthropic driver with our
//     placeholder value)
//   - Sets `Authorization: Bearer <current access token>` from the
//     refresher
//   - Appends `claude-code-20250219,oauth-2025-04-20` to
//     `anthropic-beta` (or sets the header if absent)
//   - Sets `user-agent: claude-cli/<version>`
//
// On a 401, the transport attempts a single in-line refresh + retry
// — handles the case where the access token expired in the gap
// between the background-refresh tick and the request. After one
// retry the error surfaces to the caller (probably means the refresh
// token is also dead — operator must re-login).
type oauthTransport struct {
	base      http.RoundTripper
	refresher *Refresher
	version   string
}

func (t *oauthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.base == nil {
		t.base = http.DefaultTransport
	}
	t.applyAuth(req)
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	// 401 — try an in-line refresh + one retry. If refresh fails OR the
	// retry also 401s, surface the result without further retries.
	_ = resp.Body.Close()
	refreshCtx, cancel := context.WithTimeout(req.Context(), 20*time.Second)
	defer cancel()
	if refreshErr := t.refresher.RefreshNow(refreshCtx); refreshErr != nil {
		return nil, fmt.Errorf("anthropic-oauth-dev: 401 + refresh failed: %w", refreshErr)
	}
	// Build a fresh request clone — http.Request.Body has been read
	// once already; the inner driver passes a bytes.Reader (re-usable)
	// or nil for GET. For nil-body requests this is fine; for
	// bytes.Reader-bodied POSTs we need to rewind. The
	// inner anthropic.Driver builds requests with bytes.NewReader, so
	// req.GetBody returns a fresh reader on every call when set —
	// http.NewRequestWithContext + bytes.Reader does set GetBody.
	if req.Body != nil && req.GetBody != nil {
		body, bodyErr := req.GetBody()
		if bodyErr == nil {
			req.Body = body
		}
	}
	t.applyAuth(req)
	return t.base.RoundTrip(req)
}

func (t *oauthTransport) applyAuth(req *http.Request) {
	req.Header.Del("x-api-key")
	tok := t.refresher.Token()
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	// Always SET (not append) the pinned betas. The 401-retry path
	// calls applyAuth on the SAME *http.Request twice — an append
	// strategy would silently duplicate the betas to
	// `claude-code-...,oauth-...,claude-code-...,oauth-...` on the
	// retry, which Anthropic either rejects with 400 INVALID_BETA or
	// silently fails closed.
	//
	// The inner anthropic driver doesn't set anthropic-beta itself
	// today, so a straight SET is correct. If a future caller needs
	// to layer additional betas, that's an additive change to the
	// PinnedAnthropicBetas constant (or a per-Request beta-list
	// accessor on the inner driver) — never a header-append at this
	// layer.
	req.Header.Set("anthropic-beta", PinnedAnthropicBetas)
	req.Header.Set("User-Agent", "claude-cli/"+t.version)
}

// ErrSubscriptionQuotaExhausted is returned (wrapped via the
// internal subscriptionQuotaErr type) when Anthropic's subscription
// billing reports quota exhaustion. Non-retryable on the OAuth-dev
// provider itself; tier-policy fallback configured via
// `user_tiers.<tier>.fallback_on_error: true` lets downstream
// API-key providers handle the request instead.
//
// Detection (v0.11.10 A2): on the SYNCHRONOUS error return from
// Driver.Call(), if the inner anthropic driver's error text contains
// both "429" and "subscription" (case-insensitive), we wrap with a
// subscriptionQuotaErr whose .Error() preserves the original "anthropic
// 429: {body}" string verbatim (so internal/providers/errclass.go's
// statusRe regex still matches → ClassifyError still returns
// ErrorClassRetryable → existing tier-fallback path still fires).
// The wrap adds an Is() method so `errors.Is(err,
// ErrSubscriptionQuotaExhausted)` matches for callers that want to
// distinguish quota exhaustion from generic rate-limiting.
//
// Generic 429s (rate-limited but quota intact) are NOT wrapped — the
// inner driver's ratelimit.Do path retries them transparently per
// the standard rate-limit handling.
var ErrSubscriptionQuotaExhausted = errors.New("anthropic-oauth-dev: subscription quota exhausted")

// subscriptionQuotaErr is the wrapper that lets errors.Is match
// ErrSubscriptionQuotaExhausted while preserving the original error
// text verbatim (so downstream ClassifyError regex still works).
type subscriptionQuotaErr struct{ inner error }

func (e *subscriptionQuotaErr) Error() string { return e.inner.Error() }
func (e *subscriptionQuotaErr) Unwrap() error { return e.inner }
func (e *subscriptionQuotaErr) Is(target error) bool {
	return target == ErrSubscriptionQuotaExhausted
}

// ResolveClaudeCodeVersion reads the env-var override (if set) or
// returns PinnedClaudeCodeVersion. Exposed so callers in
// cmd/loomcycle/main.go thread the same logic.
func ResolveClaudeCodeVersion() string {
	if v := os.Getenv(EnvClaudeCodeVersion); v != "" {
		return v
	}
	return PinnedClaudeCodeVersion
}
