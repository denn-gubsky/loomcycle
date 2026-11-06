package credential

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
)

func oauthID(name string) Identity { return Identity{TenantID: "t", Scope: "tenant", Name: name} }

func testOAuthConfig() OAuthConfig {
	return OAuthConfig{Resource: "https://docs.example.test/mcp",
		Client: OAuthClientConfig{Registration: OAuthRegistrationDynamic, TokenEndpointAuth: OAuthAuthNone}}
}

func TestIsCryptoDef_OAuthUsageIsNotACryptoKey(t *testing.T) {
	def := json.RawMessage(`{"usage":"oauth","config":{},"allowed_urls":[]}`)
	if isCryptoDef(def) || defKind(def) != kindOAuth {
		t.Errorf("an oauth definition: crypto=%t kind=%d, want the oauth kind", isCryptoDef(def), defKind(def))
	}
}

// Before OAuth, any usage marked a crypto key; that stays so for every
// usage but "oauth", and for an oauth usage with a crypto field: nothing
// half-one-kind reaches a secret path.
func TestDefKind_UnknownUsageStaysOffSecretPaths(t *testing.T) {
	for def, want := range map[string]defKindT{
		`{"usage":"crypto","key_type":"ed25519"}`:     kindCrypto,
		`{"usage":"mystery"}`:                         kindCrypto,
		`{"usage":"oauth","key_type":"ed25519"}`:      kindCrypto,
		`{"usage":"oauth","versions":[]}`:             kindCrypto, // any versions field is a crypto field
		`{"usage":"oauth","external":{}}`:             kindCrypto,
		`{"value":{"key_id":"k"}}`:                    kindSecret,
		`not json`:                                    kindSecret,
		`{"usage":"oauth","allowed_urls":["https:"]}`: kindOAuth,
	} {
		if got := defKind(json.RawMessage(def)); got != want {
			t.Errorf("defKind(%s) = %d, want %d", def, defKind(json.RawMessage(def)), want)
		}
	}
}

// An OAuth credential is never handed out as a $cred: secret, by any path,
// and the refusal does not fall through to a same-named credential in a
// less specific scope.
func TestEngineResolve_OAuthRowRefusedWithErrOAuthCredential(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	if _, err := e.PutInline(ctx, oauthID("docs"), "tenant-secret", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutOAuth(ctx, Identity{TenantID: "t", Scope: "user", ScopeID: "alice", Name: "docs"}, testOAuthConfig(), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Resolve(ctx, "t", "", "alice", "docs"); !errors.Is(err, ErrOAuthCredential) {
		t.Errorf("Resolve: %v, want ErrOAuthCredential", err)
	}
	pctx := netguard.WithPinnable(ctx)
	if _, _, err := e.ResolveFor(pctx, "t", "", "alice", "docs", "https://docs.example.test/mcp"); !errors.Is(err, ErrOAuthCredential) {
		t.Errorf("ResolveFor: %v, want ErrOAuthCredential", err)
	}
	out, _, err := e.Substitute(pctx, "t", "", "alice", "Bearer $cred:docs", "https://docs.example.test/mcp", nil)
	if !errors.Is(err, ErrOAuthCredential) || strings.Contains(out, "tenant-secret") {
		t.Errorf("Substitute: %q %v, want ErrOAuthCredential", out, err)
	}
	if _, err := e.PutOAuth(ctx, oauthID("hook"), testOAuthConfig(), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.ResolveToCompare(ctx, "t", "hook"); !errors.Is(err, ErrOAuthCredential) {
		t.Errorf("ResolveToCompare: %v", err)
	}
	if _, err := e.HasToCompare(ctx, "t", "hook"); !errors.Is(err, ErrOAuthCredential) {
		t.Errorf("HasToCompare: %v", err)
	}
	if _, _, err := e.ResolveKMSSecret(ctx, oauthID("hook")); !errors.Is(err, ErrOAuthCredential) {
		t.Errorf("ResolveKMSSecret: %v", err)
	}
	if _, _, err := e.ResolveKeyMaterial(ctx, "t", "", "", "hook", Scopes{Tenant: true}); !errors.Is(err, ErrOAuthCredential) {
		t.Errorf("ResolveKeyMaterial: %v", err)
	}
}

func TestHasKey_OAuthRowIsHeldButUnusable(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	if _, err := e.PutOAuth(ctx, oauthID("ANTHROPIC_API_KEY"), testOAuthConfig(), nil); err != nil {
		t.Fatal(err)
	}
	ok, err := e.HasKey(ctx, "t", "", "", "ANTHROPIC_API_KEY")
	if ok || !HeldButUnusable(err) || !errors.Is(err, ErrOAuthCredential) {
		t.Errorf("HasKey = %t, %v; want held but unusable", ok, err)
	}
	found, usable, err := e.HasKeyFor(ctx, "t", "", "", "ANTHROPIC_API_KEY", "https://docs.example.test/mcp")
	if !found || usable || !errors.Is(err, ErrOAuthCredential) {
		t.Errorf("HasKeyFor = %t %t %v", found, usable, err)
	}
}

// For the Crypto tool an OAuth credential is neither a key nor key
// material: it is skipped like the other kind, and named for what it is.
func TestFindOne_OAuthRowIsNeitherKeyNorKeyMaterial(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	if _, err := e.PutOAuth(ctx, oauthID("k"), testOAuthConfig(), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.ResolveCryptoKey(ctx, "t", "", "", "k", Scopes{Tenant: true}); !errors.Is(err, ErrNotCryptoKey) {
		t.Errorf("ResolveCryptoKey: %v, want ErrNotCryptoKey", err)
	}
	if _, _, err := e.ResolveKeyMaterial(ctx, "t", "", "", "k", Scopes{Tenant: true}); !errors.Is(err, ErrOAuthCredential) {
		t.Errorf("ResolveKeyMaterial: %v, want ErrOAuthCredential", err)
	}
	// A crypto key in another allowed scope is not made ambiguous by it.
	mustPutKey(t, e, Identity{TenantID: "t", Scope: "user", ScopeID: "u", Name: "k"}, "hmac-sha256", strings.Repeat("k", 32))
	if _, found, err := e.ResolveCryptoKey(ctx, "t", "", "u", "k", Scopes{Tenant: true, User: true}); err != nil || !found {
		t.Errorf("ResolveCryptoKey beside an OAuth credential: found=%t err=%v", found, err)
	}
}

func TestPutInline_RefusesToOverwriteOAuthCredential(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	if _, err := e.PutOAuth(ctx, oauthID("docs"), testOAuthConfig(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutInline(ctx, oauthID("docs"), "x", nil, nil, nil); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("PutInline over an OAuth credential: %v, want ErrKeyExists", err)
	}
	if m, _ := e.Get(ctx, oauthID("docs")); m.Usage != UsageOAuth {
		t.Errorf("the OAuth credential was replaced: usage %q", m.Usage)
	}
}

func TestPutOAuth_RefusesExistingName(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	if _, err := e.PutInline(ctx, oauthID("a"), "x", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutOAuth(ctx, oauthID("a"), testOAuthConfig(), nil); !errors.Is(err, ErrKeyExists) {
		t.Errorf("over a secret: %v", err)
	}
	if _, err := e.PutOAuth(ctx, oauthID("b"), testOAuthConfig(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutOAuth(ctx, oauthID("b"), testOAuthConfig(), nil); !errors.Is(err, ErrKeyExists) {
		t.Errorf("over an OAuth credential: %v", err)
	}
}

func TestPutOAuth_DefaultsAllowedURLsToResource(t *testing.T) {
	e := newEngine(t, testKey(1))
	m, err := e.PutOAuth(context.Background(), oauthID("docs"), testOAuthConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.AllowedURLs == nil || len(*m.AllowedURLs) != 1 || (*m.AllowedURLs)[0] != "https://docs.example.test/mcp" {
		t.Errorf("allowed_urls = %v, want [resource]", m.AllowedURLs)
	}
	if m.OAuth == nil || m.OAuth.Status.State != OAuthPendingConnect {
		t.Errorf("meta = %+v, want pending_connect", m.OAuth)
	}
}

func TestPutOAuth_RefusesAnInvalidConfig(t *testing.T) {
	e := newEngine(t, testKey(1))
	for name, edit := range map[string]func(*OAuthConfig){
		"http resource":     func(c *OAuthConfig) { c.Resource = "http://docs.example.test/mcp" },
		"no resource":       func(c *OAuthConfig) { c.Resource = "" },
		"http issuer":       func(c *OAuthConfig) { c.Issuer = "http://as.example.test" },
		"static":            func(c *OAuthConfig) { c.Client.Registration = OAuthRegistrationStatic },
		"metadata document": func(c *OAuthConfig) { c.Client.Registration = OAuthRegistrationMetadataDocument },
		"unknown auth":      func(c *OAuthConfig) { c.Client.TokenEndpointAuth = "private_key_jwt" },
		"bad scope":         func(c *OAuthConfig) { c.Scopes = []string{"a b"} },
	} {
		cfg := testOAuthConfig()
		edit(&cfg)
		if _, err := e.PutOAuth(context.Background(), oauthID("x"), cfg, nil); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
}

// A database writer who edits the config (say, the resource) or the URL
// restriction leaves the secret unopenable: both are what it is sealed with.
func TestOAuthState_EditedConfigOrURLsIsUnopenable(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	for name, edit := range map[string]func(m map[string]any){
		"resource": func(m map[string]any) {
			m["config"].(map[string]any)["resource"] = "https://elsewhere.example.test/mcp"
		},
		"allowed_urls": func(m map[string]any) { m["allowed_urls"] = []string{"https://elsewhere.example.test/"} },
	} {
		row, _ := h.st.CredentialDefGet(context.Background(), "t", "tenant", "", "docs")
		var m map[string]any
		_ = json.Unmarshal(row.Definition, &m)
		edit(m)
		raw, _ := json.Marshal(m)
		edited := row
		edited.Definition = raw
		if _, err := h.st.CredentialDefPut(context.Background(), edited); err != nil {
			t.Fatal(err)
		}
		r, err := h.e.getOAuth(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.e.openOAuthSecret(r); !errors.Is(err, ErrUnopenable) {
			t.Errorf("%s edited: %v, want ErrUnopenable", name, err)
		}
		if _, err := h.st.CredentialDefPut(context.Background(), row); err != nil { // put it back
			t.Fatal(err)
		}
	}
}

// An earlier ciphertext (an older refresh token) spliced under the current
// header does not open.
func TestOAuthState_SplicedOldCiphertextIsUnopenable(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	ctx := context.Background()
	old, _ := h.st.CredentialDefGet(ctx, "t", "tenant", "", "docs")
	var oldDef oauthDefinition
	_ = json.Unmarshal(old.Definition, &oldDef)
	// Move to a newer version.
	r, sec := h.secretOf(t, id)
	if _, ok, err := h.e.casOAuth(ctx, r, r.def, &sec); err != nil || !ok {
		t.Fatalf("cas: %v %v", ok, err)
	}
	cur, _ := h.st.CredentialDefGet(ctx, "t", "tenant", "", "docs")
	var m map[string]any
	_ = json.Unmarshal(cur.Definition, &m)
	m["secret"] = oldDef.Secret
	raw, _ := json.Marshal(m)
	cur.Definition = raw
	if _, err := h.st.CredentialDefPut(ctx, cur); err != nil {
		t.Fatal(err)
	}
	r, err := h.e.getOAuth(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.openOAuthSecret(r); !errors.Is(err, ErrUnopenable) {
		t.Errorf("a spliced old secret: %v, want ErrUnopenable", err)
	}
}

func TestOAuthMeta_NeverCarriesTokensOrDeviceCode(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	m, err := h.e.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m)
	list, _ := h.e.List(context.Background(), "t", "tenant", "")
	rawList, _ := json.Marshal(list)
	_, sec := h.secretOf(t, id)
	for _, s := range append(h.f.Secrets(), sec.ClientID) {
		if strings.Contains(string(raw), s) || strings.Contains(string(rawList), s) {
			t.Errorf("metadata carries a secret (%.6s…)", s)
		}
	}
	if m.Definition != nil {
		t.Error("metadata carries the definition")
	}
}
