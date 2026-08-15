// Package docremote is a thin HTTP client that proxies Document tool ops to a
// PEER loomcycle instance's POST /v1/_document (RFC CE). It is the document
// analogue of the remote MEMORY backend (internal/memory/backends/remote): the
// peer owns both document storage planes; this client forwards op-JSON and
// returns the tool's raw JSON result.
//
// It never reimplements the KV+SQL split — it just addresses a remote document
// namespace. Connection policy (the SSRF-guarded client, the credential-env
// allowlist) is injected by the caller, so this package is testable against an
// httptest peer.
package docremote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Options configures a client. Filled by the caller from a config.DocumentSource.
type Options struct {
	BaseURL          string // peer origin, e.g. "https://peer.example:8787"
	APIVersion       string // reserved; the data API is /v1 today
	DefaultAPIKeyEnv string // env-var NAME of the peer bearer (tenancy "")
	TenancyKind      string // "" (one shared credential) or "key_per_tenant"
	EnvPattern       string // key_per_tenant env-name template containing {tenant_id}
	KeyResolver      func(envName string) (string, error)
	HTTPClient       *http.Client // SSRF-guarded; required
}

// Client proxies Document ops to a peer's /v1/_document.
type Client struct {
	base         string
	defAPIKeyEnv string
	tenancyKind  string
	envPattern   string
	keyResolver  func(string) (string, error)
	http         *http.Client
}

// New validates the options and builds the client.
func New(o Options) (*Client, error) {
	if o.BaseURL == "" {
		return nil, fmt.Errorf("remote document source: base_url is required")
	}
	if o.HTTPClient == nil {
		return nil, fmt.Errorf("remote document source: HTTPClient is required")
	}
	return &Client{
		base:         strings.TrimRight(o.BaseURL, "/"),
		defAPIKeyEnv: o.DefaultAPIKeyEnv,
		tenancyKind:  o.TenancyKind,
		envPattern:   o.EnvPattern,
		keyResolver:  o.KeyResolver,
		http:         o.HTTPClient,
	}, nil
}

// Do POSTs one Document op-payload to the peer's /v1/_document and returns the
// tool's raw JSON result. A tool refusal (422 tool_refused) and any non-2xx map
// to an error; the bearer rides the Authorization header, never the URL, so a
// logged error is secret-free.
func (c *Client) Do(ctx context.Context, payload map[string]any) (json.RawMessage, error) {
	status, raw, err := c.request(ctx, payload)
	if err != nil {
		return nil, err
	}
	if status == http.StatusOK {
		return raw, nil
	}
	code, msg := bodyErr(raw)
	if code != "" {
		op, _ := payload["op"].(string)
		return nil, fmt.Errorf("remote document source: op %q refused (%d %s): %s", op, status, code, msg)
	}
	return nil, fmt.Errorf("remote document source: request failed with status %d", status)
}

func (c *Client) authHeader(ctx context.Context) (string, error) {
	envName := c.defAPIKeyEnv
	if c.tenancyKind == "key_per_tenant" && c.envPattern != "" {
		envName = strings.ReplaceAll(c.envPattern, "{tenant_id}", tools.RunIdentity(ctx).TenantID)
	}
	if envName == "" || c.keyResolver == nil {
		return "", nil
	}
	tok, err := c.keyResolver(envName)
	if err != nil {
		return "", err // contains only the env-var NAME, never the value
	}
	return "Bearer " + tok, nil
}

func (c *Client) request(ctx context.Context, payload map[string]any) (int, []byte, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("remote document source: encode request: %w", err)
	}
	u := c.base + "/v1/_document"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	authz, err := c.authHeader(ctx)
	if err != nil {
		return 0, nil, err
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("remote document source: POST %s: %w", u, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, raw, nil
}

// bodyErr extracts a {code,error} envelope (the peer's tool_refused / internal
// shape) from an error body.
func bodyErr(raw []byte) (code, msg string) {
	var e struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		return e.Code, e.Error
	}
	return "", ""
}
