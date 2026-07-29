package credential

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ghAppTokenRe matches a $ghapp:<name> reference — the sibling of $cred: for a
// GitHub App. The referenced credential holds an App config (JSON), and the
// resolver mints a short-lived installation ACCESS TOKEN from it: the App private
// key never leaves loomcycle, only the ~1h, repo-scoped token is substituted.
var ghAppTokenRe = regexp.MustCompile(`\$ghapp:([A-Za-z0-9_-]{1,128})`)

// HasGHAppRef reports whether s contains any $ghapp:<name> token.
func HasGHAppRef(s string) bool { return ghAppTokenRe.MatchString(s) }

// GHAppRefNames returns the <name> of every $ghapp:<name> token in s (nil when
// none) — so a caller with no resolver still drops those headers instead of
// sending a literal "$ghapp:foo" downstream (mirrors RefNames).
func GHAppRefNames(s string) []string {
	ms := ghAppTokenRe.FindAllStringSubmatch(s, -1)
	if len(ms) == 0 {
		return nil
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m[1])
	}
	return out
}

// ConfigResolver returns the stored App-config JSON for a credential name, using
// the same scope precedence as Engine.Resolve (agent > user > tenant). main.go
// wires it to Engine.Resolve; tests pass a fake.
type ConfigResolver func(ctx context.Context, tenantID, agentName, userID, name string) (config string, found bool, err error)

// GitHubAppMinter resolves $ghapp:<name> tokens by minting a GitHub App
// installation access token from the stored App config, cached until shortly
// before it expires. Safe for concurrent use.
type GitHubAppMinter struct {
	resolve ConfigResolver
	http    *http.Client
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]cachedGHToken
}

type cachedGHToken struct {
	token string
	exp   time.Time
}

// NewGitHubAppMinter builds a minter. A nil httpClient gets a sane default with a
// timeout (the mint is a server-side call to api.github.com).
func NewGitHubAppMinter(resolve ConfigResolver, httpClient *http.Client) *GitHubAppMinter {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &GitHubAppMinter{resolve: resolve, http: httpClient, now: time.Now, cache: map[string]cachedGHToken{}}
}

// Substitute replaces every $ghapp:<name> token in s with a freshly-minted (or
// cached) installation access token for the run identity. Mirrors
// Engine.Substitute: unresolved names are returned in `unresolved` (caller drops
// the header); a hard error (bad config / mint failure) aborts and is returned so
// the caller drops the header rather than sending a literal token.
func (m *GitHubAppMinter) Substitute(ctx context.Context, tenantID, agentName, userID, s string, register func(string)) (out string, unresolved []string, err error) {
	if !ghAppTokenRe.MatchString(s) {
		return s, nil, nil
	}
	var firstErr error
	out = ghAppTokenRe.ReplaceAllStringFunc(s, func(tok string) string {
		name := ghAppTokenRe.FindStringSubmatch(tok)[1]
		cfgJSON, found, rerr := m.resolve(ctx, tenantID, agentName, userID, name)
		if rerr != nil {
			if firstErr == nil {
				firstErr = rerr
			}
			return tok
		}
		if !found {
			unresolved = append(unresolved, name)
			return tok
		}
		access, terr := m.token(ctx, cfgJSON)
		if terr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("github app %q: %w", name, terr)
			}
			return tok
		}
		if register != nil {
			register(access)
		}
		return access
	})
	if firstErr != nil {
		return "", nil, firstErr
	}
	return out, unresolved, nil
}

// flexString unmarshals a JSON string OR number into a string, so app_id /
// installation_id may be written either way in the stored config.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	s = strings.Trim(s, `"`)
	*f = flexString(s)
	return nil
}

// ghAppConfig is the stored credential value for a $ghapp: reference.
type ghAppConfig struct {
	AppID          flexString        `json:"app_id"`
	InstallationID flexString        `json:"installation_id"`
	PrivateKey     string            `json:"private_key"`
	Repositories   []string          `json:"repositories,omitempty"` // optional down-scope to specific repos
	Permissions    map[string]string `json:"permissions,omitempty"`  // optional down-scope of permissions
	BaseURL        string            `json:"base_url,omitempty"`     // GitHub Enterprise; default api.github.com
}

// token returns a valid installation access token for the given App config,
// minting a new one (and caching it) when none is cached or the cached one is
// within 5 minutes of expiry.
func (m *GitHubAppMinter) token(ctx context.Context, cfgJSON string) (string, error) {
	var cfg ghAppConfig
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		return "", fmt.Errorf("credential is not a GitHub App config (expected JSON with app_id, private_key, installation_id): %w", err)
	}
	if cfg.AppID == "" || cfg.PrivateKey == "" || cfg.InstallationID == "" {
		return "", errors.New("github app config requires app_id, private_key, and installation_id")
	}
	key := ghCacheKey(cfg)
	m.mu.Lock()
	if c, ok := m.cache[key]; ok && m.now().Before(c.exp.Add(-5*time.Minute)) {
		tok := c.token
		m.mu.Unlock()
		return tok, nil
	}
	m.mu.Unlock()

	// Mint outside the lock so a slow GitHub call doesn't serialise unrelated
	// resolutions; a rare concurrent double-mint is harmless (both tokens valid).
	tok, exp, err := m.mint(ctx, cfg)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.cache[key] = cachedGHToken{token: tok, exp: exp}
	m.mu.Unlock()
	return tok, nil
}

// mint builds the App JWT and exchanges it for an installation access token.
func (m *GitHubAppMinter) mint(ctx context.Context, cfg ghAppConfig) (string, time.Time, error) {
	inst := string(cfg.InstallationID)
	if !isDigits(inst) {
		return "", time.Time{}, fmt.Errorf("installation_id must be numeric, got %q", inst)
	}
	jwt, err := buildAppJWT(string(cfg.AppID), cfg.PrivateKey, m.now())
	if err != nil {
		return "", time.Time{}, err
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	if !strings.HasPrefix(base, "https://") {
		return "", time.Time{}, fmt.Errorf("base_url must be https, got %q", base)
	}
	url := base + "/app/installations/" + inst + "/access_tokens"

	var body io.Reader
	if len(cfg.Repositories) > 0 || len(cfg.Permissions) > 0 {
		payload := map[string]any{}
		if len(cfg.Repositories) > 0 {
			payload["repositories"] = cfg.Repositories
		}
		if len(cfg.Permissions) > 0 {
			payload["permissions"] = cfg.Permissions
		}
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("installation token request: HTTP %d: %s", resp.StatusCode, truncateGH(string(rb), 200))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("parse installation token response: %w", err)
	}
	if out.Token == "" {
		return "", time.Time{}, errors.New("installation token response had no token")
	}
	if out.ExpiresAt.IsZero() {
		out.ExpiresAt = m.now().Add(time.Hour) // GitHub tokens last ~1h; be conservative if absent
	}
	return out.Token, out.ExpiresAt, nil
}

// buildAppJWT builds the RS256 App JWT (iss=app_id, iat backdated 60s for clock
// skew, exp +9m — under GitHub's 10m cap). Hand-rolled on stdlib (no JWT dep).
func buildAppJWT(appID, pemKey string, now time.Time) (string, error) {
	key, err := parseRSAPrivateKey(pemKey)
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
	}
	// iss is the App ID (numeric) or a Client ID (string). Emit numeric when it
	// looks numeric so classic App-ID auth keeps working.
	if isDigits(appID) {
		n, _ := strconv.ParseInt(appID, 10, 64)
		claims["iss"] = n
	} else {
		claims["iss"] = appID
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// parseRSAPrivateKey accepts a PKCS#1 ("RSA PRIVATE KEY") or PKCS#8 ("PRIVATE
// KEY") PEM — GitHub App keys are usually PKCS#1.
func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("private_key is not a PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private_key (not PKCS#1 or PKCS#8 RSA): %w", err)
	}
	rk, ok := keyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private_key is not an RSA key")
	}
	return rk, nil
}

// ghCacheKey keys the token cache by the token's SCOPE (app + installation +
// repos + permissions), not the private key — a key rotation reuses a still-valid
// token, while a different app/installation/scope re-mints.
func ghCacheKey(cfg ghAppConfig) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", cfg.AppID, cfg.InstallationID)
	repos := append([]string(nil), cfg.Repositories...)
	sort.Strings(repos)
	for _, r := range repos {
		io.WriteString(h, r)
		h.Write([]byte{0})
	}
	pkeys := make([]string, 0, len(cfg.Permissions))
	for k := range cfg.Permissions {
		pkeys = append(pkeys, k)
	}
	sort.Strings(pkeys)
	for _, k := range pkeys {
		fmt.Fprintf(h, "%s=%s\x00", k, cfg.Permissions[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func truncateGH(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
