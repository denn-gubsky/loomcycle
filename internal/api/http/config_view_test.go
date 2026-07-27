package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
)

// configTestCfg is a config with auth CONFIGURED (so the 401 path is reachable —
// an empty AuthToken is open mode, which passes everything through) plus planted
// secrets and addresses in every field that sits near something /v1/config
// reports.
func configTestCfg() *config.Config {
	cfg := makeBaseConfig()
	cfg.Env.AuthToken = "config-test-token"
	cfg.Env.PublicURL = "https://loomcycle.cloud"
	cfg.Env.MaxRequestBytes = 16 << 20
	cfg.Env.BraveAPIKey = "brave-secret-key-value"
	cfg.Env.BashEnabled = true
	cfg.Storage.Backend = "postgres"
	cfg.Storage.PgDSN = "postgres://u:hunter2@db.internal:5432/loom"
	cfg.SearchProviders = map[string]config.SearchProviderConfig{
		// The BaseURL here is the whole reason the search block reports only map
		// KEYS: marshalling the struct would publish a private address.
		"searxng": {BaseURL: "http://192.168.0.77:8080"},
	}
	// An env-var NAME is itself a hint about key management, so it must not
	// survive either — planted where a field for one actually exists.
	cfg.Memory.Embedder = config.EmbedderConfig{
		Provider: "ollama-local", Model: "embeddinggemma",
		BaseURL: "http://192.168.0.77:11434", APIKeyEnv: "LOOMCYCLE_EMBEDDER_API_KEY",
	}
	cfg.UserTiers = map[string]config.UserTier{"enterprise-plan": {}, "free-plan": {}}
	return cfg
}

func getConfig(t *testing.T, srv *Server, bearer string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rec := httptest.NewRecorder()
	rec.Code = resp.StatusCode
	_, _ = rec.Body.ReadFrom(resp.Body)
	var out map[string]any
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
	}
	return rec, out
}

// TestConfig_RequiresAuthByDefault is the upgrade-safety guarantee: without the
// opt-in, this endpoint discloses nothing to an unauthenticated caller. Every
// deployment that upgrades keeps its provider and model inventory private, and
// nothing becomes world-readable because a new endpoint appeared.
func TestConfig_RequiresAuthByDefault(t *testing.T) {
	cfg := configTestCfg() // PublicConfig deliberately NOT set
	srv, _ := makeServer(t, completingProvider(), cfg)

	rec, _ := getConfig(t, srv, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	// And an authenticated caller does get it, so the 401 is about the credential
	// rather than the route being broken.
	rec, out := getConfig(t, srv, "config-test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("authed status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if out["view"] == configViewPublic {
		t.Error(`authed caller got the "public" view`)
	}
}

// TestConfig_InvalidBearerNeverDowngrades: with the opt-in ON, a presented but
// invalid credential must still fail. Falling back to the public view would mean
// a typo'd or revoked token silently reads as anonymous — a bad credential is an
// error, not a request for less detail.
func TestConfig_InvalidBearerNeverDowngrades(t *testing.T) {
	cfg := configTestCfg()
	cfg.Env.PublicConfig = true
	srv, _ := makeServer(t, completingProvider(), cfg)

	rec, _ := getConfig(t, srv, "wrong-token")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a bad bearer under PublicConfig (body=%s)", rec.Code, rec.Body.String())
	}
	// No bearer at all IS the public view, so the gate itself is on.
	rec, out := getConfig(t, srv, "")
	if rec.Code != http.StatusOK || out["view"] != configViewPublic {
		t.Errorf("anonymous under PublicConfig: status=%d view=%v, want 200/public", rec.Code, out["view"])
	}
}

// TestConfig_PublicViewLeaksNoSecretsTopologyOrPlans is the security gate on the
// world-readable view. Every planted value below sits one field away from
// something this endpoint legitimately reports.
func TestConfig_PublicViewLeaksNoSecretsTopologyOrPlans(t *testing.T) {
	cfg := configTestCfg()
	cfg.Env.PublicConfig = true
	srv, _ := makeServer(t, completingProvider(), cfg)
	srv.SetBuildInfo("v1.37.0", "deadbeefcafe", "2026-07-27T10:00:00Z")

	rec, out := getConfig(t, srv, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, banned := range []string{
		"brave-secret-key-value",     // a raw API key
		"LOOMCYCLE_EMBEDDER_API_KEY", // the env-var NAME is a hint too
		"hunter2", "postgres://",     // a DSN and its password
		"api_key", "apikey", "secret", // secret-shaped keys
		"password", "dsn", "bearer",
		"auth_token", "access_token",
	} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(banned)) {
			t.Errorf("public config leaked %q:\n%s", banned, body)
		}
	}
	for _, banned := range []string{
		"http://", "https://", // any URL, including PublicURL
		"192.168.0.77", "db.internal",
		":8080", ":5432", ":11434",
		"/var/", "/etc/",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("public config leaked topology %q:\n%s", banned, body)
		}
	}
	// Commercial structure: plan names are not a capability.
	for _, banned := range []string{"enterprise-plan", "free-plan", "user_tiers"} {
		if strings.Contains(body, banned) {
			t.Errorf("public config leaked the plan roster %q:\n%s", banned, body)
		}
	}
	// Build provenance and operational ceilings stop at an authenticated caller.
	for _, banned := range []string{"deadbeefcafe", "build_time", "limits", "storage", "last_error"} {
		if strings.Contains(body, banned) {
			t.Errorf("public config leaked %q:\n%s", banned, body)
		}
	}

	// ...while still reporting what a landing page is for.
	inst, _ := out["instance"].(map[string]any)
	if inst["version"] != "v1.37.0" {
		t.Errorf("instance.version = %v, want v1.37.0 reported publicly", inst["version"])
	}
	// Features are reduced to plain booleans (see publicFeatures).
	feats, ok := out["features"].(map[string]any)
	if !ok || len(feats) == 0 {
		t.Fatalf("features missing from the public view: %v", out["features"])
	}
	if b, isBool := feats["bash"].(bool); !isBool || !b {
		t.Errorf("features.bash = %v (%T), want the bool true", feats["bash"], feats["bash"])
	}
	if _, isMap := feats["search"].(map[string]any); isMap {
		t.Error("features.search is still a nested object in the public view; it must be reduced to a bool")
	}
}

// TestConfig_PublicFeaturesDropsAnyFieldItWasNotAskedFor pins the whitelist-by-
// SHAPE property directly. publicFeatures copies only `available`, so a field
// added to the capability probe LATER cannot reach a public reader — no lockstep
// update to a denylist required, and no silent failure when that update is
// forgotten. This is the test that makes the public view safe to leave alone.
func TestConfig_PublicFeaturesDropsAnyFieldItWasNotAskedFor(t *testing.T) {
	got := publicFeatures(map[string]any{
		"vector_memory": map[string]any{
			"available": true,
			// Every one of these is a field a future capability might add.
			"embedder":  map[string]any{"base_url": "http://192.168.0.77:11434"},
			"api_key":   "sk-live-xxxx",
			"row_count": 41_000,
		},
		"consolidation": map[string]any{"available": false, "merge_threshold": 0.95},
		// No `available` key at all ⇒ dropped entirely rather than passed through.
		"storage": map[string]any{"backend": "postgres"},
	})
	if got["vector_memory"] != true || got["consolidation"] != false {
		t.Errorf("availability not preserved: %+v", got)
	}
	if _, present := got["storage"]; present {
		t.Errorf("an entry with no `available` key survived: %+v", got)
	}
	blob, _ := json.Marshal(got)
	for _, banned := range []string{"192.168.0.77", "sk-live", "row_count", "41000", "merge_threshold", "postgres"} {
		if strings.Contains(string(blob), banned) {
			t.Errorf("publicFeatures copied %q it was not asked for: %s", banned, blob)
		}
	}
}

// TestConfig_PublicIsSubsetOfAuthedIsSubsetOfAdmin makes the layering a claim
// with content rather than an assertion in a comment.
func TestConfig_PublicIsSubsetOfAuthedIsSubsetOfAdmin(t *testing.T) {
	cfg := configTestCfg()
	cfg.Env.PublicConfig = true
	srv, _ := makeServer(t, completingProvider(), cfg)
	srv.SetBuildInfo("v1.37.0", "deadbeefcafe", "2026-07-27T10:00:00Z")

	_, pub := getConfig(t, srv, "")

	// A non-admin authenticated principal, and an admin, via the handler directly
	// (the legacy token resolves to admin, so scopes are set explicitly here).
	call := func(scopes []string) map[string]any {
		ctx := auth.WithPrincipal(context.Background(),
			auth.Principal{TenantID: "acme", Subject: "u1", Scopes: scopes})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/config", nil).WithContext(ctx)
		srv.handleConfig(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}
	authed := call([]string{auth.ScopeTenant})
	adm := call([]string{auth.ScopeAdmin})

	if authed["view"] != configViewAuthed {
		t.Errorf("tenant view = %v, want %q", authed["view"], configViewAuthed)
	}
	if adm["view"] != configViewAdmin {
		t.Errorf("admin view = %v, want %q", adm["view"], configViewAdmin)
	}
	// Public keys ⊆ authed keys ⊆ admin keys.
	for k := range pub {
		if _, ok := authed[k]; !ok {
			t.Errorf("public key %q absent from the authenticated view", k)
		}
	}
	for k := range authed {
		if _, ok := adm[k]; !ok {
			t.Errorf("authenticated key %q absent from the admin view", k)
		}
	}
	// The layering has content: each level adds something the one below lacks.
	if _, ok := pub["limits"]; ok {
		t.Error("public view carries limits")
	}
	if _, ok := authed["limits"]; !ok {
		t.Error("authenticated view is missing limits, so public ⊆ authed is vacuous")
	}
	af, _ := authed["features"].(map[string]any)
	if _, ok := af["storage"]; ok {
		t.Error("tenant sees the storage backend")
	}
	adf, _ := adm["features"].(map[string]any)
	if _, ok := adf["storage"]; !ok {
		t.Error("admin view is missing the storage backend, so authed ⊆ admin is vacuous")
	}
}

// TestConfig_ModelsAreDedupedAndCarryLiveStatus: the same model appears in every
// plan that includes it, and a landing page wants it once — tiers merged, active
// true if ANY tier would route to it now, selected true if it is what actually
// runs. deepseek is seeded UP and anthropic DOWN, so "active" has to discriminate
// rather than being constant.
//
// A real resolver is required: the default test fixture leaves s.resolver nil, in
// which case the handler reports an empty model list and this test would pass
// while asserting nothing.
func TestConfig_ModelsAreDedupedAndCarryLiveStatus(t *testing.T) {
	cfg := configTestCfg()
	cfg.Env.PublicConfig = true
	// Two plans that BOTH include the same two candidates — the dedup case.
	cfg.UserTiers = map[string]config.UserTier{
		"enterprise-plan": {ProviderPriority: []string{"deepseek", "anthropic"}},
		"free-plan":       {ProviderPriority: []string{"deepseek", "anthropic"}},
	}
	cfg.ProviderPriority = []string{"deepseek", "anthropic"}
	cfg.Tiers = map[string][]config.TierCandidate{
		"middle": {
			{Provider: "deepseek", Model: "deepseek-v4-pro"},
			{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		},
	}
	srv, _ := makeServer(t, completingProvider(), cfg)
	res := resolve.NewResolver([]string{"deepseek", "anthropic"}, map[string][]resolve.Candidate{
		"middle": {
			{Provider: "deepseek", Model: "deepseek-v4-pro"},
			{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		},
	})
	res.SetReachable("deepseek", true, []string{"deepseek-v4-pro"}, "")
	res.SetReachable("anthropic", false, nil, "probe failed on db.internal:5432")
	srv.SetResolver(res)

	rec, out := getConfig(t, srv, "")
	models, _ := out["models"].([]any)
	if len(models) == 0 {
		t.Fatalf("no models reported; the assertions below would be vacuous (body=%s)", rec.Body.String())
	}
	seen := map[string]map[string]any{}
	for _, m := range models {
		mm, _ := m.(map[string]any)
		key := mm["provider"].(string) + "/" + mm["model"].(string)
		if _, dup := seen[key]; dup {
			t.Errorf("model %s appears more than once; it must be deduplicated across plans", key)
		}
		seen[key] = mm
		if _, ok := mm["tiers"].([]any); !ok {
			t.Errorf("model %v has no tiers list", mm)
		}
	}
	up, ok := seen["deepseek/deepseek-v4-pro"]
	if !ok {
		t.Fatalf("deepseek candidate missing: %v", seen)
	}
	if up["active"] != true || up["selected"] != true {
		t.Errorf("reachable primary: active=%v selected=%v, want true/true", up["active"], up["selected"])
	}
	down, ok := seen["anthropic/claude-sonnet-4-6"]
	if !ok {
		t.Fatalf("anthropic candidate missing: %v", seen)
	}
	if down["active"] != false {
		t.Errorf("unreachable candidate reported active=%v, want false", down["active"])
	}
	// The provider header must discriminate the same way.
	provs, _ := out["providers"].([]any)
	if len(provs) == 0 {
		t.Fatal("no providers reported")
	}
	for _, p := range provs {
		pm, _ := p.(map[string]any)
		switch pm["provider"] {
		case "deepseek":
			if pm["active"] != true {
				t.Errorf("deepseek active=%v, want true", pm["active"])
			}
		case "anthropic":
			if pm["active"] != false {
				t.Errorf("anthropic active=%v, want false", pm["active"])
			}
		}
	}
	// And the probe error never reaches a public reader, even with a provider down.
	if strings.Contains(rec.Body.String(), "probe failed") || strings.Contains(rec.Body.String(), "db.internal") {
		t.Errorf("public view leaked a provider probe error:\n%s", rec.Body.String())
	}
}
