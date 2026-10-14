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
	"io"
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
	tok1, err := m.token(context.Background(), "t", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tok1 != "ghs_minted_1" || hits != 1 {
		t.Fatalf("first mint: tok=%q hits=%d", tok1, hits)
	}
	// Cached — no new HTTP call.
	tok2, err := m.token(context.Background(), "t", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tok2 != "ghs_minted_1" || hits != 1 {
		t.Fatalf("cache miss: tok=%q hits=%d (want cached, 1 hit)", tok2, hits)
	}
	// Advance to within 5m of expiry → refresh.
	clock = time.Unix(1_800_003_600, 0).Add(-4 * time.Minute)
	tok3, err := m.token(context.Background(), "t", cfg)
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
	if _, err := m.token(context.Background(), "t", "not json"); err == nil {
		t.Errorf("non-JSON config should error")
	}
	if _, err := m.token(context.Background(), "t", `{"app_id":"1"}`); err == nil {
		t.Errorf("missing fields should error")
	}
	// Non-numeric installation_id is rejected before any network call.
	_, keyPEM := testKeyPEM(t)
	bad := fmt.Sprintf(`{"app_id":"1","installation_id":"abc","private_key":%q}`, keyPEM)
	if _, err := m.token(context.Background(), "t", bad); err == nil {
		t.Errorf("non-numeric installation_id should error")
	}
}

// ghTenantFixture is a fake GitHub that mints a numbered token per call, plus a
// resolver that hands each tenant its own stored App config.
func ghTenantFixture(t *testing.T, configs func(baseURL string) map[string]string) (*GitHubAppMinter, *int) {
	t.Helper()
	hits := new(int)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_minted_%d","expires_at":%q}`, *hits, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	t.Cleanup(srv.Close)
	byTenant := configs(srv.URL)
	resolve := func(_ context.Context, tenantID, _, _, _ string) (string, bool, error) {
		cfg, ok := byTenant[tenantID]
		return cfg, ok, nil
	}
	return NewGitHubAppMinter(resolve, srv.Client()), hits
}

// App and installation ids are not secret. A tenant that stores them with a
// key it made up must not be handed the token another tenant minted with the
// real key.
func TestMinter_ForeignTenantWithoutTheKeyGetsNoCachedToken(t *testing.T) {
	_, keyPEM := testKeyPEM(t)
	m, hits := ghTenantFixture(t, func(base string) map[string]string {
		return map[string]string{
			"victim":   fmt.Sprintf(`{"app_id":"1","installation_id":"2","private_key":%q,"base_url":%q}`, keyPEM, base),
			"attacker": fmt.Sprintf(`{"app_id":"1","installation_id":"2","private_key":"not a key","base_url":%q}`, base),
		}
	})

	out, _, err := m.Substitute(context.Background(), "victim", "a", "u", "$ghapp:app", nil)
	if err != nil || out != "ghs_minted_1" {
		t.Fatalf("victim mint: out=%q err=%v", out, err)
	}
	out, _, err = m.Substitute(context.Background(), "attacker", "a", "u", "$ghapp:app", nil)
	if err == nil {
		t.Fatalf("attacker got %q with a made-up key; want an error and no token", out)
	}
	if strings.Contains(out, "ghs_") {
		t.Errorf("attacker output carries a token: %q", out)
	}
	if *hits != 1 {
		t.Errorf("GitHub was called %d times, want 1 (the attacker's key cannot sign a request)", *hits)
	}
}

// A config with a different key is a different credential: it mints for
// itself, so GitHub is the one that accepts or refuses the key.
func TestMinter_DifferentKeySameIDsMintsItsOwnToken(t *testing.T) {
	_, keyA := testKeyPEM(t)
	_, keyB := testKeyPEM(t)
	m, hits := ghTenantFixture(t, func(base string) map[string]string {
		return map[string]string{
			"a": fmt.Sprintf(`{"app_id":"1","installation_id":"2","private_key":%q,"base_url":%q}`, keyA, base),
			"b": fmt.Sprintf(`{"app_id":"1","installation_id":"2","private_key":%q,"base_url":%q}`, keyB, base),
		}
	})
	for _, tenant := range []string{"a", "b"} {
		if _, _, err := m.Substitute(context.Background(), tenant, "a", "u", "$ghapp:app", nil); err != nil {
			t.Fatal(err)
		}
	}
	if *hits != 2 {
		t.Errorf("GitHub was called %d times, want 2: the second key must not reuse the first key's token", *hits)
	}
}

// The same config stored by two tenants is cached per tenant.
func TestMinter_SameConfigInTwoTenantsIsCachedPerTenant(t *testing.T) {
	_, keyPEM := testKeyPEM(t)
	m, hits := ghTenantFixture(t, func(base string) map[string]string {
		cfg := fmt.Sprintf(`{"app_id":"1","installation_id":"2","private_key":%q,"base_url":%q}`, keyPEM, base)
		return map[string]string{"a": cfg, "b": cfg}
	})
	for _, tenant := range []string{"a", "b", "a", "b"} {
		if _, _, err := m.Substitute(context.Background(), tenant, "a", "u", "$ghapp:app", nil); err != nil {
			t.Fatal(err)
		}
	}
	if *hits != 2 {
		t.Errorf("GitHub was called %d times, want 2 (one mint per tenant, then cached)", *hits)
	}
}

// Two GitHub servers can use the same numeric ids; a token from one must not
// be served for the other.
func TestGHCacheKey_SeparatesServersKeysAndTenants(t *testing.T) {
	base := ghAppConfig{AppID: "1", InstallationID: "2", PrivateKey: "k1", BaseURL: "https://one.example"}
	otherServer, otherKey := base, base
	otherServer.BaseURL = "https://two.example"
	otherKey.PrivateKey = "k2"
	k := ghCacheKey("t", base)
	if k == ghCacheKey("t", otherServer) {
		t.Error("two base URLs share a cache key")
	}
	if k == ghCacheKey("t", otherKey) {
		t.Error("two private keys share a cache key")
	}
	if k == ghCacheKey("u", base) {
		t.Error("two tenants share a cache key")
	}
	if k != ghCacheKey("t", base) {
		t.Error("the same tenant and config must share a cache key")
	}
}

// A token for a clone is narrowed to the one repository with read access to
// its contents, whatever the stored config allows; GitHub does the
// enforcing, so what matters is what is asked for.
func TestMinter_TokenForRepositoryAsksForOneRepositoryReadOnly(t *testing.T) {
	_, keyPEM := testKeyPEM(t)
	var bodies []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_minted_%d","expires_at":%q}`, len(bodies), time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer srv.Close()
	m := NewGitHubAppMinter(nil, srv.Client())
	// A config that allows every repository and write access.
	wide := fmt.Sprintf(`{"app_id":"1","installation_id":"2","private_key":%q,"base_url":%q,"permissions":{"contents":"write","issues":"write"}}`, keyPEM, srv.URL)

	tok, err := m.TokenForRepository(context.Background(), "t", wide, "app")
	if err != nil || tok != "ghs_minted_1" {
		t.Fatalf("tok=%q err=%v", tok, err)
	}
	var asked struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &asked); err != nil {
		t.Fatalf("request body %q: %v", bodies[0], err)
	}
	if len(asked.Repositories) != 1 || asked.Repositories[0] != "app" ||
		len(asked.Permissions) != 1 || asked.Permissions["contents"] != "read" {
		t.Errorf("asked GitHub for %s, want repository app with contents:read and nothing else", bodies[0])
	}

	// The narrowed token is not handed to a header that resolves the same
	// config with its own wider scope, nor the other way round.
	if tok, _ := m.token(context.Background(), "t", wide); tok == "ghs_minted_1" {
		t.Error("the config's own wide token was served from the narrowed one's cache entry")
	}
	if tok, _ := m.TokenForRepository(context.Background(), "t", wide, "app"); tok != "ghs_minted_1" {
		t.Errorf("a second clone of the same repository re-minted: %q", tok)
	}
	if tok, _ := m.TokenForRepository(context.Background(), "t", wide, "other"); tok == "ghs_minted_1" {
		t.Error("another repository got the first one's token")
	}

	// A config that lists its repositories must include this one: the
	// narrowing never widens. Refused without asking GitHub.
	listed := fmt.Sprintf(`{"app_id":"1","installation_id":"2","private_key":%q,"base_url":%q,"repositories":["web","api"]}`, keyPEM, srv.URL)
	before := len(bodies)
	if _, err := m.TokenForRepository(context.Background(), "t", listed, "app"); err == nil {
		t.Error("a repository the config does not list was given a token")
	}
	if len(bodies) != before {
		t.Error("GitHub was asked for a repository the config does not list")
	}
	if _, err := m.TokenForRepository(context.Background(), "t", listed, "API"); err != nil {
		t.Errorf("a listed repository, in another letter case: %v", err)
	}
	for _, bad := range []string{"", "a/b", "a b", "../x"} {
		if _, err := m.TokenForRepository(context.Background(), "t", wide, bad); err == nil {
			t.Errorf("repository name %q accepted", bad)
		}
	}

	// A config whose own permissions leave contents out was written to keep
	// the App's token away from code. A clone must not ask for more than it
	// allows: refused, without asking GitHub.
	for _, perms := range []string{`{"issues":"write"}`, `{"metadata":"read"}`, `{"contents":"none"}`, `{"contents":""}`} {
		noCode := fmt.Sprintf(`{"app_id":"1","installation_id":"2","private_key":%q,"base_url":%q,"permissions":%s}`, keyPEM, srv.URL, perms)
		before := len(bodies)
		if _, err := m.TokenForRepository(context.Background(), "t", noCode, "app"); err == nil {
			t.Errorf("permissions %s: a clone token was given", perms)
		}
		if len(bodies) != before {
			t.Errorf("permissions %s: GitHub was asked", perms)
		}
	}
	// One that allows reading contents is narrowed to read.
	readOnly := fmt.Sprintf(`{"app_id":"1","installation_id":"2","private_key":%q,"base_url":%q,"permissions":{"contents":"read","pull_requests":"write"}}`, keyPEM, srv.URL)
	if _, err := m.TokenForRepository(context.Background(), "t", readOnly, "web"); err != nil {
		t.Errorf("a config allowing contents: read: %v", err)
	} else if last := bodies[len(bodies)-1]; !strings.Contains(last, `"contents":"read"`) || strings.Contains(last, "pull_requests") {
		t.Errorf("asked GitHub for %s, want contents:read alone", last)
	}
}

// An answer other than 201 is an error, and the error goes to the operator's
// log. It carries GitHub's message, never the body: a body that holds a token
// (here a 200 from something that is not quite GitHub) must not reach the log.
func TestMinter_AnUnexpectedAnswerIsReportedWithoutItsBody(t *testing.T) {
	_, keyPEM := testKeyPEM(t)
	status, body := 0, ""
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	m := NewGitHubAppMinter(nil, srv.Client())
	cfg := fmt.Sprintf(`{"app_id":"1","installation_id":"2","private_key":%q,"base_url":%q}`, keyPEM, srv.URL)

	status, body = http.StatusOK, `{"token":"ghs_leakedThroughTheErrorPath0123456789","expires_at":"2030-01-01T00:00:00Z"}`
	_, err := m.TokenForRepository(context.Background(), "t", cfg, "app")
	if err == nil {
		t.Fatal("a 200 was taken as a minted token")
	}
	if strings.Contains(err.Error(), "ghs_leaked") {
		t.Errorf("the error carries the token: %v", err)
	}

	status, body = http.StatusUnauthorized, `{"message":"Bad credentials","documentation_url":"https://docs.github.com/rest"}`
	_, err = m.TokenForRepository(context.Background(), "t2", cfg, "app")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401: Bad credentials") || strings.Contains(err.Error(), "documentation_url") {
		t.Errorf("err = %v, want the status and GitHub's message alone", err)
	}
}
