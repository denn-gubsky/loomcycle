package credential

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testKeyPEM generates a throwaway RSA key and returns it as a PKCS#1 PEM.
func testKeyPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	return k, string(pemBytes)
}

func TestBuildAppJWT_SignedRS256(t *testing.T) {
	key, keyPEM := testKeyPEM(t)
	now := time.Unix(1_800_000_000, 0)
	tok, err := buildAppJWT("12345", keyPEM, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt must have 3 parts, got %d", len(parts))
	}
	// Header.
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var hdr struct{ Alg, Typ string }
	if err := json.Unmarshal(hb, &hdr); err != nil {
		t.Fatal(err)
	}
	if hdr.Alg != "RS256" || hdr.Typ != "JWT" {
		t.Errorf("header = %+v, want RS256/JWT", hdr)
	}
	// Claims: iss numeric, iat backdated, exp within 10m.
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss int64 `json:"iss"`
		Iat int64 `json:"iat"`
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(cb, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != 12345 {
		t.Errorf("iss = %d, want 12345", claims.Iss)
	}
	if claims.Iat != now.Add(-60*time.Second).Unix() {
		t.Errorf("iat not backdated 60s: %d", claims.Iat)
	}
	if claims.Exp <= claims.Iat || claims.Exp-now.Unix() > 600 {
		t.Errorf("exp must be after iat and within 10m: iat=%d exp=%d", claims.Iat, claims.Exp)
	}
	// Signature verifies against the public key.
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Errorf("signature does not verify: %v", err)
	}
}

func TestMinter_MintCacheAndRefresh(t *testing.T) {
	_, keyPEM := testKeyPEM(t)
	var hits int
	// A fake GitHub installations endpoint.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/app/installations/999/access_tokens" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing bearer jwt")
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_minted_%d","expires_at":%q}`, hits, time.Unix(1_800_003_600, 0).UTC().Format(time.RFC3339))
	}))
	defer srv.Close()

	m := NewGitHubAppMinter(nil, srv.Client())
	clock := time.Unix(1_800_000_000, 0)
	m.now = func() time.Time { return clock }

	cfg := fmt.Sprintf(`{"app_id":12345,"installation_id":"999","private_key":%q,"base_url":%q}`, keyPEM, srv.URL)

	// First mint.
	tok1, err := m.token(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tok1 != "ghs_minted_1" || hits != 1 {
		t.Fatalf("first mint: tok=%q hits=%d", tok1, hits)
	}
	// Cached — no new HTTP call.
	tok2, err := m.token(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tok2 != "ghs_minted_1" || hits != 1 {
		t.Fatalf("cache miss: tok=%q hits=%d (want cached, 1 hit)", tok2, hits)
	}
	// Advance to within 5m of expiry → refresh.
	clock = time.Unix(1_800_003_600, 0).Add(-4 * time.Minute)
	tok3, err := m.token(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tok3 != "ghs_minted_2" || hits != 2 {
		t.Fatalf("expected refresh near expiry: tok=%q hits=%d", tok3, hits)
	}
}

func TestMinter_Substitute(t *testing.T) {
	_, keyPEM := testKeyPEM(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_abc","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"app_id":"1","installation_id":"2","private_key":%q,"base_url":%q}`, keyPEM, srv.URL)
	resolve := func(_ context.Context, _, _, _, name string) (string, bool, error) {
		if name == "myapp" {
			return cfg, true, nil
		}
		return "", false, nil
	}
	m := NewGitHubAppMinter(resolve, srv.Client())

	var registered []string
	out, unresolved, err := m.Substitute(context.Background(), "t", "a", "u", "$ghapp:myapp", func(s string) { registered = append(registered, s) })
	if err != nil {
		t.Fatal(err)
	}
	if out != "ghs_abc" {
		t.Errorf("substitute = %q, want ghs_abc", out)
	}
	if len(unresolved) != 0 {
		t.Errorf("unexpected unresolved: %v", unresolved)
	}
	if len(registered) != 1 || registered[0] != "ghs_abc" {
		t.Errorf("token should be registered with the redactor: %v", registered)
	}

	// Unresolved name → left literal, reported, no error.
	out, unresolved, err = m.Substitute(context.Background(), "t", "a", "u", "$ghapp:missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "$ghapp:missing" || len(unresolved) != 1 || unresolved[0] != "missing" {
		t.Errorf("unresolved handling wrong: out=%q unresolved=%v", out, unresolved)
	}
}

func TestGHAppRefNames(t *testing.T) {
	if !HasGHAppRef("$ghapp:foo") || HasGHAppRef("$cred:foo") {
		t.Errorf("HasGHAppRef mis-detects")
	}
	names := GHAppRefNames("a $ghapp:one b $ghapp:two-3 c")
	if len(names) != 2 || names[0] != "one" || names[1] != "two-3" {
		t.Errorf("GHAppRefNames = %v", names)
	}
}

func TestMinter_BadConfig(t *testing.T) {
	m := NewGitHubAppMinter(nil, nil)
	if _, err := m.token(context.Background(), "not json"); err == nil {
		t.Errorf("non-JSON config should error")
	}
	if _, err := m.token(context.Background(), `{"app_id":"1"}`); err == nil {
		t.Errorf("missing fields should error")
	}
	// Non-numeric installation_id is rejected before any network call.
	_, keyPEM := testKeyPEM(t)
	bad := fmt.Sprintf(`{"app_id":"1","installation_id":"abc","private_key":%q}`, keyPEM)
	if _, err := m.token(context.Background(), bad); err == nil {
		t.Errorf("non-numeric installation_id should error")
	}
}
