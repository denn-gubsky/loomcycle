package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
)

// webhookClient is the per-Dispatcher HTTP client used to call hook
// callback URLs. Clamps payload size, honours the per-hook timeout
// via context, and surfaces transport / 5xx errors so the
// Dispatcher's fail-mode logic can decide what to do.
//
// Default *http.Client.Timeout is intentionally NOT set — the
// caller passes a ctx with the per-hook deadline (so each Hook's
// configured TimeoutMs is honoured per-call rather than per-client).
//
// Two clients, chosen by who registered the hook. An operator-global hook
// (Tenant "": admin, legacy token, open mode) is the operator's own code and
// commonly a localhost sidecar, so it dials anywhere. A TENANT operator's hook
// is not: its callback receives every matching tool input, so an unguarded
// dial let a tenant aim that stream at the metadata endpoint or an internal
// service. Those dial through netguard, which refuses private addresses at
// connect time (re-checked on each redirect hop and after DNS resolution),
// except hosts the operator vouched for in hooks.private_host_allowlist.
type webhookClient struct {
	http    *http.Client
	guarded *http.Client
}

func newWebhookClient(httpClient *http.Client, privateHostAllowlist []string) *webhookClient {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &webhookClient{
		http:    httpClient,
		guarded: netguard.NewGuardedClient(0, privateHostAllowlist),
	}
}

// clientFor picks the dial policy for a hook's callback.
func (c *webhookClient) clientFor(h *Hook) *http.Client {
	if h.Tenant == "" {
		return c.http
	}
	return c.guarded
}

// post issues a POST to the webhook URL with `body` JSON-encoded,
// and unmarshals the response body into `out`. A 204 / empty body
// leaves *out as the caller's zero value (the typical "no rewrite"
// response shape).
//
// Returns an error on transport failures, non-2xx status codes,
// non-JSON response bodies, and ctx-deadline expiry. All of these
// flow into the Dispatcher's fail-mode branch.
func (c *webhookClient) post(ctx context.Context, hc *http.Client, url string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal hook payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build hook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("hook transport: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read up to 1 KiB of the body for diagnostics; webhook errors
		// are operator-side bugs and the response usually carries a
		// useful explanation.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("hook %s: %d %s", url, resp.StatusCode, bytes.TrimSpace(errBody))
	}

	// 204 → no rewrite. Empty body with 200 → also no rewrite.
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return fmt.Errorf("read hook response: %w", err)
	}
	if len(bytes.TrimSpace(respBody)) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode hook response: %w", err)
	}
	return nil
}
