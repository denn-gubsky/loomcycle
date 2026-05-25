package anthropic_oauth_dev

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestNewPKCEPair_Shape pins the PKCE verifier + challenge encoding so
// a future encoder swap doesn't silently break the auth flow. RFC 7636
// requires the verifier to be 43-128 chars (we use 32-byte random ↦ ~43
// chars base64url-unpadded) and the challenge to be exactly
// base64url(sha256(verifier)) without padding (~43 chars).
func TestNewPKCEPair_Shape(t *testing.T) {
	pair, err := NewPKCEPair()
	if err != nil {
		t.Fatalf("NewPKCEPair: %v", err)
	}
	if len(pair.Verifier) < 43 || len(pair.Verifier) > 128 {
		t.Errorf("Verifier length = %d, want 43-128", len(pair.Verifier))
	}
	if strings.ContainsAny(pair.Verifier, "=") {
		t.Errorf("Verifier should not carry padding: %q", pair.Verifier)
	}
	if len(pair.Challenge) != 43 {
		t.Errorf("Challenge length = %d, want 43 (sha256 base64url unpadded)", len(pair.Challenge))
	}
	if strings.ContainsAny(pair.Challenge, "=") {
		t.Errorf("Challenge should not carry padding: %q", pair.Challenge)
	}
}

// TestNewPKCEPair_FreshPerCall: two consecutive calls must produce
// different verifiers — entropy regression would otherwise allow a
// replay attack against the OAuth state.
func TestNewPKCEPair_FreshPerCall(t *testing.T) {
	a, _ := NewPKCEPair()
	b, _ := NewPKCEPair()
	if a.Verifier == b.Verifier {
		t.Error("PKCE verifier collision — entropy is broken")
	}
}

// TestNewPKCEPair_DecodableVerifier: the verifier must be valid
// base64url without padding. The exchange endpoint validates this
// strictly.
func TestNewPKCEPair_DecodableVerifier(t *testing.T) {
	pair, _ := NewPKCEPair()
	if _, err := base64.RawURLEncoding.DecodeString(pair.Verifier); err != nil {
		t.Errorf("Verifier not valid base64url: %v", err)
	}
	if _, err := base64.RawURLEncoding.DecodeString(pair.Challenge); err != nil {
		t.Errorf("Challenge not valid base64url: %v", err)
	}
}

// TestBuildAuthorizeURL pins the URL shape end-to-end. Anthropic's
// authorize endpoint is strict about parameter set + casing; a regression
// here would surface as a confusing 400 in the browser.
func TestBuildAuthorizeURL(t *testing.T) {
	pkce, _ := NewPKCEPair()
	u := BuildAuthorizeURL(pkce, 53692)
	// Required query params per Pi's reference.
	for _, want := range []string{
		"response_type=code",
		"client_id=" + ClaudeCodeClientID,
		"code_challenge=" + pkce.Challenge,
		"code_challenge_method=S256",
		"redirect_uri=http%3A%2F%2Flocalhost%3A53692%2Fcallback",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("authorize URL missing %q\n  got: %s", want, u)
		}
	}
	// Scope: every Pi scope is present (url-encoded). Decode the URL
	// to compare against the canonical scope strings instead of
	// hand-encoding them here.
	decoded, _ := url.QueryUnescape(u)
	for _, s := range Scopes {
		if !strings.Contains(decoded, s) {
			t.Errorf("authorize URL missing scope %q (decoded URL: %s)", s, decoded)
		}
	}
}

// TestExchangeCodeForToken_HappyPath verifies the request body shape
// + the 5-min slack on the returned ExpiresAt.
func TestExchangeCodeForToken_HappyPath(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "sk-ant-oat-abc",
			"refresh_token": "sk-ant-ort-def",
			"expires_in": 3600,
			"scope": "user:inference",
			"token_type": "Bearer"
		}`))
	}))
	defer srv.Close()

	got, err := ExchangeCodeForToken(t.Context(), "auth-code-xyz", "state-abc", "verifier-pqr", 53692,
		ExchangeOptions{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("ExchangeCodeForToken: %v", err)
	}
	if got.AccessToken != "sk-ant-oat-abc" {
		t.Errorf("AccessToken = %q", got.AccessToken)
	}
	if got.RefreshToken != "sk-ant-ort-def" {
		t.Errorf("RefreshToken = %q", got.RefreshToken)
	}
	// 5-min slack applied: ExpiresAt = obtainedAt + 3600s - 300s = +55min.
	delta := time.Until(got.ExpiresAt)
	if delta < 54*time.Minute || delta > 56*time.Minute {
		t.Errorf("ExpiresAt slack wrong; got %v, want ~55min", delta)
	}
	// Request body shape.
	for k, v := range map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     ClaudeCodeClientID,
		"code":          "auth-code-xyz",
		"state":         "state-abc",
		"redirect_uri":  "http://localhost:53692/callback",
		"code_verifier": "verifier-pqr",
	} {
		if gotBody[k] != v {
			t.Errorf("request body[%s] = %q, want %q", k, gotBody[k], v)
		}
	}
}

// TestExchangeCodeForToken_4xx surfaces server errors verbatim so
// operators see Anthropic's actual rejection reason.
func TestExchangeCodeForToken_4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad code"}`))
	}))
	defer srv.Close()

	_, err := ExchangeCodeForToken(t.Context(), "bad", "state", "v", 53692,
		ExchangeOptions{Endpoint: srv.URL})
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error should surface the 401 + server message: %v", err)
	}
}

// TestRefreshAccessToken_RotatesRefreshToken pins that the new refresh
// token (when Anthropic rotates it) flows through to the returned
// Token. Rotation is opt-in on Anthropic's side; we treat it as
// non-mandatory by populating from whatever comes back.
func TestRefreshAccessToken_RotatesRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "sk-ant-oat-new",
			"refresh_token": "sk-ant-ort-rotated",
			"expires_in": 3600,
			"scope": "user:inference"
		}`))
	}))
	defer srv.Close()

	got, err := RefreshAccessToken(t.Context(), "old-refresh", ExchangeOptions{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}
	if got.RefreshToken != "sk-ant-ort-rotated" {
		t.Errorf("rotated refresh token not captured: %q", got.RefreshToken)
	}
}

// TestCallbackServer_HappyPath verifies the full callback round-trip:
// start server, simulate browser callback, receive code in WaitFor.
func TestCallbackServer_HappyPath(t *testing.T) {
	cs, err := StartCallbackServer(0) // OS-picked port
	if err != nil {
		t.Fatalf("StartCallbackServer: %v", err)
	}
	defer cs.Close()

	// Simulate the browser hitting the callback.
	go func() {
		_, _ = http.Get("http://127.0.0.1:" + intToStr(cs.Port()) + CallbackPath + "?code=auth-code-xyz&state=verifier-abc")
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	res, err := cs.WaitFor(ctx)
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if res.Err != nil {
		t.Errorf("unexpected callback error: %v", res.Err)
	}
	if res.Code != "auth-code-xyz" || res.State != "verifier-abc" {
		t.Errorf("callback parse: code=%q state=%q", res.Code, res.State)
	}
}

// TestCallbackServer_ErrorParam surfaces operator-denied authorizations
// + auth-server errors as a callback Err.
func TestCallbackServer_ErrorParam(t *testing.T) {
	cs, _ := StartCallbackServer(0)
	defer cs.Close()
	go func() {
		_, _ = http.Get("http://127.0.0.1:" + intToStr(cs.Port()) + CallbackPath + "?error=access_denied&error_description=user+rejected")
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	res, _ := cs.WaitFor(ctx)
	if res.Err == nil {
		t.Error("expected error from access_denied callback")
	}
}

func intToStr(n int) string {
	// Tiny helper to keep the test file free of strconv import noise.
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%10]
		n /= 10
	}
	return string(b[i:])
}
