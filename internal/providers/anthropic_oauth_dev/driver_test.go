package anthropic_oauth_dev

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestOAuthTransport_AppliesAuthHeaders pins the per-request header
// set: x-api-key stripped, Authorization: Bearer set, anthropic-beta
// set to the pinned value, User-Agent set to the pinned Claude Code
// version.
func TestOAuthTransport_AppliesAuthHeaders(t *testing.T) {
	store := NewTokenStore(t.TempDir() + "/tokens.json")
	if err := store.Save(NewToken("sk-ant-oat-test", "sk-ant-ort-test", "user:inference", 3600)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	refresher := NewRefresher(store, ExchangeOptions{}, func(string, ...any) {})
	// Don't Start() — we don't want the background tick interfering
	// with the test; the cached token is loaded by NewRefresher's
	// constructor.

	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	transport := &oauthTransport{
		base:      http.DefaultTransport,
		refresher: refresher,
		version:   "2.1.99",
	}
	req, _ := http.NewRequest("POST", srv.URL, nil)
	req.Header.Set("x-api-key", "should-be-stripped")
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()

	if got := gotHeaders.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key should be stripped, got %q", got)
	}
	if got := gotHeaders.Get("Authorization"); got != "Bearer sk-ant-oat-test" {
		t.Errorf("Authorization = %q, want Bearer sk-ant-oat-test", got)
	}
	if got := gotHeaders.Get("anthropic-beta"); got != PinnedAnthropicBetas {
		t.Errorf("anthropic-beta = %q, want %q", got, PinnedAnthropicBetas)
	}
	if got := gotHeaders.Get("User-Agent"); got != "claude-cli/2.1.99" {
		t.Errorf("User-Agent = %q, want claude-cli/2.1.99", got)
	}
}

// TestOAuthTransport_401RetryDoesNotDoubleBetaHeader is the regression
// test for the v0.11.9 code-review finding #1: a 401 + in-line refresh
// + retry MUST NOT duplicate the anthropic-beta header. The pre-fix
// applyAuth read the existing header and appended the pinned betas
// onto whatever was there — on the second call (post-refresh), that
// produced `claude-code-...,oauth-...,claude-code-...,oauth-...`.
func TestOAuthTransport_401RetryDoesNotDoubleBetaHeader(t *testing.T) {
	store := NewTokenStore(t.TempDir() + "/tokens.json")
	if err := store.Save(NewToken("sk-ant-oat-stale", "sk-ant-ort-test", "user:inference", 3600)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	refresher := NewRefresher(store, ExchangeOptions{}, func(string, ...any) {})

	// Faux Anthropic: returns 401 on the first call, 200 on the
	// retry. Records the anthropic-beta header on every call.
	var attempts atomic.Int32
	var seenBetas []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenBetas = append(seenBetas, r.Header.Get("anthropic-beta"))
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"token expired"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Faux token endpoint: returns a fresh token on the refresh call.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "sk-ant-oat-fresh",
			"refresh_token": "sk-ant-ort-test",
			"expires_in": 3600,
			"scope": "user:inference"
		}`))
	}))
	defer tokenSrv.Close()
	refresher.httpClient = ExchangeOptions{Endpoint: tokenSrv.URL}

	transport := &oauthTransport{
		base:      http.DefaultTransport,
		refresher: refresher,
		version:   PinnedClaudeCodeVersion,
	}
	req, _ := http.NewRequest("POST", srv.URL, nil)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()

	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts (401 + retry), got %d", attempts.Load())
	}
	if len(seenBetas) != 2 {
		t.Fatalf("expected 2 beta headers captured, got %d", len(seenBetas))
	}
	// Both attempts MUST have the exact pinned string — no duplication.
	for i, beta := range seenBetas {
		if beta != PinnedAnthropicBetas {
			t.Errorf("attempt %d anthropic-beta = %q, want %q", i+1, beta, PinnedAnthropicBetas)
		}
	}
	// Specifically guard against the doubling regression: the second
	// header value must NOT contain the pinned string twice.
	if strings.Count(seenBetas[1], "claude-code-20250219") > 1 {
		t.Errorf("doubling regression: %q contains claude-code-20250219 more than once", seenBetas[1])
	}
}

// TestOAuthTransport_ApplyAuthIsIdempotent: calling applyAuth twice
// on the same request must produce the same header values. Direct
// regression guard for finding #1.
func TestOAuthTransport_ApplyAuthIsIdempotent(t *testing.T) {
	store := NewTokenStore(t.TempDir() + "/tokens.json")
	_ = store.Save(NewToken("x", "y", "s", 3600))
	refresher := NewRefresher(store, ExchangeOptions{}, func(string, ...any) {})

	transport := &oauthTransport{refresher: refresher, version: "test"}
	req, _ := http.NewRequest("POST", "http://example.com", nil)
	transport.applyAuth(req)
	first := req.Header.Get("anthropic-beta")
	transport.applyAuth(req)
	second := req.Header.Get("anthropic-beta")
	if first != second {
		t.Errorf("applyAuth not idempotent: first=%q second=%q", first, second)
	}
	if strings.Count(second, "claude-code-20250219") > 1 {
		t.Errorf("anthropic-beta duplicated after second apply: %q", second)
	}
}

// TestIsSubscriptionQuotaError pins the v0.11.10 A2 detection logic:
// 429 + "subscription" (case-insensitive in either token) matches;
// anything else passes through unwrapped.
func TestIsSubscriptionQuotaError(t *testing.T) {
	cases := []struct {
		name    string
		errText string
		want    bool
	}{
		{"happy path", `anthropic 429: {"type":"error","error":{"message":"subscription limit reached"}}`, true},
		{"case-insensitive Subscription", `anthropic 429: SUBSCRIPTION quota exhausted`, true},
		{"generic 429 rate-limit", `anthropic 429: {"error":{"message":"rate-limited"}}`, false},
		{"500", `anthropic 500: server error`, false},
		{"empty", ``, false},
		{"subscription word only without 429", `subscription update available`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isSubscriptionQuotaError(c.errText); got != c.want {
				t.Errorf("isSubscriptionQuotaError(%q) = %v, want %v", c.errText, got, c.want)
			}
		})
	}
}
