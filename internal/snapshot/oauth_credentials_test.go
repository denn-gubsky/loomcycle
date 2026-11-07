package snapshot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/credential/oauthtest"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// connectedOAuth stores a connected OAuth credential "docs" (user alice of
// tenant acme, registered with a client secret), an unconnected one, and an
// ordinary secret, against a fake authorization server.
func connectedOAuth(t *testing.T, s store.Store) (*oauthtest.Server, *credential.Engine) {
	t.Helper()
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	e := credential.NewEngine(s, sealer)
	f := oauthtest.New(t)
	// The grant carries an id_token, so the no-leak checks over f.Secrets()
	// cover it too.
	f.Set(func(f *oauthtest.Server) {
		f.Interval = 1
		f.IssueClientSecret = true
		f.IDToken = oauthtest.IDTokenSigned
	})
	m := credential.NewOAuthManager(e, f.Client(), "replica-src", nil)
	ctx := context.Background()
	cfg := credential.OAuthConfig{Resource: f.Resource(), Scopes: []string{"mcp"},
		Client: credential.OAuthClientConfig{Registration: credential.OAuthRegistrationDynamic, TokenEndpointAuth: credential.OAuthAuthClientSecretPost}}
	id := credential.Identity{TenantID: "acme", Scope: "user", ScopeID: "alice", Name: "docs"}
	if _, err := e.PutOAuth(ctx, id, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutOAuth(ctx, credential.Identity{TenantID: "acme", Scope: "tenant", Name: "later"}, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutInline(ctx, credential.Identity{TenantID: "acme", Scope: "tenant", Name: "plain-secret-name"}, "plain-secret-value", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ConnectStart(ctx, id); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if meta, _ := m.ConnectStatus(ctx, id); meta.OAuth.Status.State == credential.OAuthConnected {
			return f, e
		}
		if time.Now().After(deadline) {
			t.Fatal("never connected")
		}
	}
}

func TestSnapshotOAuth_CarriesConfigNeverSecrets(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	f, _ := connectedOAuth(t, src)
	env := mustCapture(t, src)
	// Non-vacuous: the grant issued every kind of secret the grep looks for.
	f.Set(func(f *oauthtest.Server) {
		if len(f.IssuedAccess) == 0 || len(f.IssuedRefresh) == 0 || len(f.IssuedDevice) == 0 || len(f.IssuedClientSecrets) == 0 {
			t.Fatalf("the grant issued access %d, refresh %d, device %d, client secret %d",
				len(f.IssuedAccess), len(f.IssuedRefresh), len(f.IssuedDevice), len(f.IssuedClientSecrets))
		}
	})
	for _, s := range f.Secrets() {
		if strings.Contains(string(env), s) {
			t.Errorf("the envelope carries a token, device code or client secret (%.8s…)", s)
		}
	}
	rows, _ := src.CredentialDefListAll(context.Background())
	for _, r := range rows {
		var d struct {
			Secret *credential.Sealed `json:"secret"`
			Value  *credential.Sealed `json:"value"`
		}
		_ = json.Unmarshal(r.Definition, &d)
		for _, sealed := range []*credential.Sealed{d.Secret, d.Value} {
			if sealed != nil && strings.Contains(string(env), sealed.Ciphertext) {
				t.Errorf("the envelope carries %s's ciphertext", r.Name)
			}
		}
	}
	for _, no := range []string{"plain-secret-name", "plain-secret-value", "connected_at", "access_expires_at", "replica-src"} {
		if strings.Contains(string(env), no) {
			t.Errorf("the envelope carries %q", no)
		}
	}
	var e Envelope
	if err := json.Unmarshal(env, &e); err != nil {
		t.Fatal(err)
	}
	sec := e.Sections.OAuthCredentials
	if sec == nil || len(sec.Entries) != 2 {
		t.Fatalf("oauth_credentials = %+v, want the two OAuth credentials", sec)
	}
	docs := sec.Entries[1] // ordered: acme/tenant/later, acme/user/alice/docs
	if docs.Name != "docs" || docs.Scope != "user" || docs.ScopeID != "alice" || docs.Config.Resource != f.Resource() ||
		len(docs.AllowedURLs) != 1 || !docs.HadGrant || sec.Entries[0].HadGrant {
		t.Errorf("entries = %+v", sec.Entries)
	}
}

func TestSnapshotOAuth_RestoreNeedsReconnect(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	f, _ := connectedOAuth(t, src)
	env := mustCapture(t, src)
	sealer, _ := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	e := credential.NewEngine(dst, sealer)
	res := mustRestore(t, dst, env, RestoreOptions{Validators: passValidators()})
	if res.OAuthCredentialsRestored != 2 || !hasWarning(res, "restored without their grant", "oauth_connect_start") {
		t.Errorf("restored %d; warnings:\n%s", res.OAuthCredentialsRestored, strings.Join(res.Warnings, "\n"))
	}
	ctx := context.Background()
	docs, err := e.Get(ctx, credential.Identity{TenantID: "acme", Scope: "user", ScopeID: "alice", Name: "docs"})
	if err != nil {
		t.Fatal(err)
	}
	if docs.OAuth == nil || docs.OAuth.Status.State != credential.OAuthNeedsReauth || docs.OAuth.Config.Resource != f.Resource() {
		t.Errorf("docs restored as %+v", docs.OAuth)
	}
	later, _ := e.Get(ctx, credential.Identity{TenantID: "acme", Scope: "tenant", Name: "later"})
	if later.OAuth == nil || later.OAuth.Status.State != credential.OAuthPendingConnect {
		t.Errorf("later restored as %+v", later.OAuth)
	}
	if _, err := e.Get(ctx, credential.Identity{TenantID: "acme", Scope: "tenant", Name: "plain-secret-name"}); err == nil {
		t.Error("an ordinary credential was restored")
	}
	// It connects again on the target, against the same server.
	m := credential.NewOAuthManager(e, f.Client(), "replica-dst", nil)
	id := credential.Identity{TenantID: "acme", Scope: "user", ScopeID: "alice", Name: "docs"}
	if _, err := m.ConnectStart(ctx, id); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	// A second restore leaves what is there and says so.
	res = mustRestore(t, dst, env, RestoreOptions{Validators: passValidators()})
	if res.OAuthCredentialsRestored != 0 || !hasWarning(res, "oauth credential acme/user/alice/docs", "already on this host") {
		t.Errorf("second restore: %d; warnings:\n%s", res.OAuthCredentialsRestored, strings.Join(res.Warnings, "\n"))
	}
}

// A snapshot taken before the section existed restores as it always did,
// and one without OAuth credentials has no section at all.
func TestSnapshotOAuth_OldSnapshotWithoutSectionRestores(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	env := mustCapture(t, src)
	if strings.Contains(string(env), "oauth_credentials") {
		t.Error("a capture with no OAuth credential has the section")
	}
	res := mustRestore(t, dst, env, RestoreOptions{Validators: passValidators()})
	if res.OAuthCredentialsRestored != 0 {
		t.Errorf("restored %d", res.OAuthCredentialsRestored)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "oauth") {
			t.Errorf("warned: %s", w)
		}
	}
}

// An entry whose config a create here would refuse is skipped.
func TestSnapshotOAuth_RestoreRefusesAnInvalidEntry(t *testing.T) {
	dst, dstClose := newTestStore(t)
	defer dstClose()
	res := RestoreResult{}
	restoreOAuthCredentials(context.Background(), dst, &OAuthCredentialsSection{Version: SectionVersion, Entries: []OAuthCredentialEntry{{
		TenantID: "acme", Scope: "tenant", Name: "bad",
		Config:      OAuthConfigEntry{Resource: "http://plain.example.test/mcp", Client: OAuthClientConfigEntry{Registration: "dynamic", TokenEndpointAuth: "none"}},
		AllowedURLs: []string{"http://plain.example.test/mcp"},
	}}}, &res)
	if res.OAuthCredentialsRestored != 0 || !hasWarning(res, "oauth credential acme/tenant//bad", "skipped") {
		t.Errorf("warnings:\n%s", strings.Join(res.Warnings, "\n"))
	}
}

// The envelope's mirror of an OAuth config round-trips every field of the
// credential's own: a field added there fails here, to be reviewed and
// mirrored (or deliberately left out), never dropped unnoticed.
func TestSnapshotOAuth_ConfigMirrorMatchesCredentialConfig(t *testing.T) {
	in := credential.OAuthConfig{Resource: "https://r.example.test/mcp", Issuer: "https://as.example.test", Scopes: []string{"a", "b"},
		Client: credential.OAuthClientConfig{Registration: "dynamic", TokenEndpointAuth: "client_secret_post"}}
	if got := oauthConfigEntry(in).config(); !reflect.DeepEqual(got, in) {
		t.Errorf("round trip = %+v, want %+v", got, in)
	}
	for _, pair := range [][2]reflect.Type{
		{reflect.TypeOf(credential.OAuthConfig{}), reflect.TypeOf(OAuthConfigEntry{})},
		{reflect.TypeOf(credential.OAuthClientConfig{}), reflect.TypeOf(OAuthClientConfigEntry{})},
	} {
		if pair[0].NumField() != pair[1].NumField() {
			t.Errorf("%s has %d fields, its envelope mirror %s %d", pair[0], pair[0].NumField(), pair[1], pair[1].NumField())
		}
		for i := 0; i < pair[0].NumField(); i++ {
			if f, ok := pair[1].FieldByName(pair[0].Field(i).Name); !ok || f.Tag.Get("json") != pair[0].Field(i).Tag.Get("json") {
				t.Errorf("%s.%s is not mirrored with the same JSON key", pair[0], pair[0].Field(i).Name)
			}
		}
	}
}

// A credential already on the target under the same key, of any kind, is left
// as it is: the restore never overwrites a row, so a live grant or an
// ordinary secret there survives.
func TestSnapshotOAuth_RestoreSkipsAnExistingRow(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	connectedOAuth(t, src)
	env := mustCapture(t, src)
	sealer, _ := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	e := credential.NewEngine(dst, sealer)
	ctx := context.Background()
	held := credential.Identity{TenantID: "acme", Scope: "user", ScopeID: "alice", Name: "docs"}
	if _, err := e.PutInline(ctx, held, "target-value", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := dst.CredentialDefGet(ctx, "acme", "user", "alice", "docs")
	res := mustRestore(t, dst, env, RestoreOptions{Validators: passValidators()})
	if res.OAuthCredentialsRestored != 1 || !hasWarning(res, "oauth credential acme/user/alice/docs", "already on this host", "left as it is") {
		t.Errorf("restored %d; warnings:\n%s", res.OAuthCredentialsRestored, strings.Join(res.Warnings, "\n"))
	}
	after, _ := dst.CredentialDefGet(ctx, "acme", "user", "alice", "docs")
	if string(after.Definition) != string(before.Definition) {
		t.Errorf("the held row changed:\n%s\n->\n%s", before.Definition, after.Definition)
	}
	if m, err := e.Get(ctx, held); err != nil || m.OAuth != nil {
		t.Errorf("the held credential reads as %+v, %v; want the ordinary one", m, err)
	}
}

// Each entry lands under its own tenant and nowhere else: a credential of
// the same name in another tenant is neither a reason to skip nor touched.
// (Snapshots are operator-wide — capture and restore are substrate:admin —
// so there is no tenant-scoped capture; isolation is the key carried
// verbatim and the full-key skip check.)
func TestSnapshotOAuth_RestoreKeepsEachTenantsRows(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	sealer, _ := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	ctx := context.Background()
	cfg := func(host string) credential.OAuthConfig {
		return credential.OAuthConfig{Resource: "https://" + host + "/mcp",
			Client: credential.OAuthClientConfig{Registration: credential.OAuthRegistrationDynamic, TokenEndpointAuth: credential.OAuthAuthNone}}
	}
	se := credential.NewEngine(src, sealer)
	for _, p := range []struct{ tenant, host string }{{"acme", "acme.example.test"}, {"globex", "globex.example.test"}} {
		if _, err := se.PutOAuth(ctx, credential.Identity{TenantID: p.tenant, Scope: "tenant", Name: "docs"}, cfg(p.host), nil); err != nil {
			t.Fatal(err)
		}
	}
	env := mustCapture(t, src)
	// The target already holds globex's name (another kind): only that row
	// is skipped, acme's lands.
	de := credential.NewEngine(dst, sealer)
	if _, err := de.PutInline(ctx, credential.Identity{TenantID: "globex", Scope: "tenant", Name: "docs"}, "globex-value", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	globexBefore, _ := dst.CredentialDefGet(ctx, "globex", "tenant", "", "docs")
	res := mustRestore(t, dst, env, RestoreOptions{Validators: passValidators()})
	if res.OAuthCredentialsRestored != 1 || !hasWarning(res, "oauth credential globex/tenant//docs", "already on this host") {
		t.Errorf("restored %d; warnings:\n%s", res.OAuthCredentialsRestored, strings.Join(res.Warnings, "\n"))
	}
	acme, err := de.Get(ctx, credential.Identity{TenantID: "acme", Scope: "tenant", Name: "docs"})
	if err != nil || acme.OAuth == nil || acme.OAuth.Config.Resource != "https://acme.example.test/mcp" {
		t.Fatalf("acme restored as %+v, %v", acme.OAuth, err)
	}
	if after, _ := dst.CredentialDefGet(ctx, "globex", "tenant", "", "docs"); string(after.Definition) != string(globexBefore.Definition) {
		t.Error("globex's own row changed")
	}
	rows, _ := dst.CredentialDefListAll(ctx)
	for _, r := range rows {
		if strings.Contains(string(r.Definition), "acme.example.test") && r.TenantID != "acme" {
			t.Errorf("acme's config landed under tenant %q", r.TenantID)
		}
	}
	if len(rows) != 2 {
		t.Errorf("target holds %d rows, want acme's restored one and globex's own", len(rows))
	}
}

// The restored credential is what the missing-credential scan then finds: a
// definition naming it is reported present but not connected, with the
// reconnect hint, never missing.
func TestSnapshotOAuth_ScanFindsTheRestoredCredentialNotConnected(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	sealer, _ := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	ctx := context.Background()
	if _, err := credential.NewEngine(src, sealer).PutOAuth(ctx, credential.Identity{TenantID: "acme", Scope: "tenant", Name: "docs-server"},
		credential.OAuthConfig{Resource: "https://docs.example.test/mcp",
			Client: credential.OAuthClientConfig{Registration: credential.OAuthRegistrationDynamic, TokenEndpointAuth: credential.OAuthAuthNone}}, nil); err != nil {
		t.Fatal(err)
	}
	plantOAuthWebhook(t, src)
	de := credential.NewEngine(dst, sealer)
	res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{
		Validators: passValidators(),
		EnvSet:     func(string) bool { return true },
		OAuthCredentialState: func(ctx context.Context, tenant, agent, user, name string) (string, bool) {
			state, found, err := de.HasOAuth(ctx, tenant, agent, user, name)
			return state, found && err == nil
		},
	})
	if !hasWarning(res, "credential not connected", "$oauth:docs-server", "pending_connect", "oauth_connect_start") {
		t.Errorf("no not-connected finding; warnings:\n%s", strings.Join(res.Warnings, "\n"))
	}
	if hasWarning(res, "missing credential:", "$oauth:docs-server") {
		t.Errorf("the restored credential was reported missing; warnings:\n%s", strings.Join(res.Warnings, "\n"))
	}
}
