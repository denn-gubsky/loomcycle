package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/remote"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"net/http/httptest"
)

// These tests pin the "$cred:<name>" form of a remote peer's api_key_env: a
// stored credential, resolved in the def's OWNING tenant at tenant scope —
// never in the calling run's tenant, and never from another tenant.

// storedCred is one credential to seed.
type storedCred struct {
	tenant, scope, scopeID, name, value string
	allowed                             []string // allowed_urls; nil = unrestricted
	expires                             *time.Time
}

func tenantCred(tenant, name, value string) storedCred {
	return storedCred{tenant: tenant, scope: "tenant", name: name, value: value}
}

// peerCredsOver builds PeerCredentials over a real credential engine on s, with
// the given credentials stored.
func peerCredsOver(t *testing.T, s store.Store, creds ...storedCred) *PeerCredentials {
	t.Helper()
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	eng := credential.NewEngine(s, sealer)
	for _, c := range creds {
		var list *[]string
		if c.allowed != nil {
			if list, err = credential.NormalizeAllowedURLs(&c.allowed); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := eng.PutInline(context.Background(), credential.Identity{TenantID: c.tenant, Scope: c.scope, ScopeID: c.scopeID, Name: c.name}, c.value, c.expires, list, nil); err != nil {
			t.Fatalf("seed credential %s/%s: %v", c.tenant, c.name, err)
		}
	}
	return NewPeerCredentials(eng, nil)
}

// ---- authoring ----

func credRefCases(remoteKind string) []credentialCase {
	create := func(apiKeyEnv string) string {
		return `{"op":"create","name":"peer","overlay":{` + remoteKind + `"config":{"base_url":"https://peer.example.com","api_key_env":"` + apiKeyEnv + `"}}}`
	}
	return []credentialCase{
		{"tenant names its own tenant credential", false, "acme", create(`$cred:peer_key`), true,
			"the credential exists in the author's tenant, where it resolves"},
		{"tenant names a credential it does not have", false, "acme", create(`$cred:absent`), false,
			"nothing to resolve"},
		{"tenant names a credential only another tenant has", false, "acme", create(`$cred:globex_key`), false,
			"a reference resolves only in the author's own tenant"},
		{"tenant-less author names a credential", false, "", create(`$cred:peer_key`), false,
			"a shared def: it would resolve the shared layer's credentials"},
		{"admin names a credential nobody has", true, "acme", create(`$cred:absent`), true,
			"an admin keeps free api_key_env"},
		{"tenant embeds a reference in text", false, "acme", create(`Bearer $cred:peer_key`), false,
			"api_key_env is a whole reference or an env-var name"},
		{"tenant names a credential restricted to its base_url", false, "acme", create(`$cred:scoped_key`), true,
			"allowed_urls covers base_url, so the dial will resolve it"},
		{"tenant names a credential restricted to other URLs", false, "acme", create(`$cred:elsewhere_key`), false,
			"allowed_urls does not cover base_url: the dial would refuse it"},
	}
}

// restrictedPeerCreds are the two restricted credentials the authoring cases
// name, beside the unrestricted ones.
func restrictedPeerCreds() []storedCred {
	return []storedCred{
		{tenant: "acme", scope: "tenant", name: "scoped_key", value: "v-scoped", allowed: []string{"https://peer.example.com"}},
		{tenant: "acme", scope: "tenant", name: "elsewhere_key", value: "v-elsewhere", allowed: []string{"https://elsewhere.example.com"}},
	}
}

func TestRemoteMemoryBackendAuthoring_CredRefMustExistInTheAuthorsTenant(t *testing.T) {
	runCredentialCases(t, func(t *testing.T) (tools.Tool, context.Context, func()) {
		tool, ctx, done := memoryBackendDefFixture(t)
		tool.PeerCredentials = peerCredsOver(t, tool.Store, append([]storedCred{tenantCred("acme", "peer_key", "v-acme"), tenantCred("globex", "globex_key", "v-globex"), tenantCred("", "peer_key", "v-shared")}, restrictedPeerCreds()...)...)
		return tool, ctx, done
	}, credRefCases(`"kind":"remote",`))
}

func TestRemoteDocumentSourceAuthoring_CredRefMustExistInTheAuthorsTenant(t *testing.T) {
	runCredentialCases(t, func(t *testing.T) (tools.Tool, context.Context, func()) {
		tool, ctx, done := documentSourceDefFixture(t)
		tool.PeerCredentials = peerCredsOver(t, tool.Store, append([]storedCred{tenantCred("acme", "peer_key", "v-acme"), tenantCred("globex", "globex_key", "v-globex"), tenantCred("", "peer_key", "v-shared")}, restrictedPeerCreds()...)...)
		return tool, ctx, done
	}, credRefCases(``))
}

// The author's OWN user-scope credential does not qualify, though the author
// is the user it belongs to: a def is the tenant's, read by every member's
// runs, and only a tenant-level credential resolves for it at dial.
func TestRemotePeerAuthoring_AuthorsUserScopeCredentialDoesNotQualify(t *testing.T) {
	tool, base, done := memoryBackendDefFixture(t)
	defer done()
	tool.PeerCredentials = peerCredsOver(t, tool.Store, storedCred{tenant: "acme", scope: "user", scopeID: "u1", name: "user_key", value: "v-user"})
	ctx := tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a_test", UserID: "u1", TenantID: "acme"})
	res := execDef(t, tool, ctx, `{"op":"create","name":"peer","overlay":{"kind":"remote","config":{"base_url":"https://peer.example.com","api_key_env":"$cred:user_key"}}}`)
	if !res.IsError || !strings.Contains(res.Text, "names no tenant-level credential") {
		t.Fatalf("a def was allowed to name its author's user-scope credential: %s", res.Text)
	}
}

// With no credential store wired, a non-admin's reference cannot be checked,
// so it is refused rather than stored to fail at every dial.
func TestRemotePeerAuthoring_CredRefRefusedWithNoCredentialStore(t *testing.T) {
	tool, base, done := memoryBackendDefFixture(t)
	defer done()
	res := execDef(t, tool, asTenant(base, "acme"), `{"op":"create","name":"peer","overlay":{"kind":"remote","config":{"base_url":"https://peer.example.com","api_key_env":"$cred:peer_key"}}}`)
	if !res.IsError || !strings.Contains(res.Text, "names no tenant-level credential") {
		t.Fatalf("a reference was accepted with no credential store to check it: %s", res.Text)
	}
}

// A reference to a tenant credential that has expired is refused at
// authoring, and the refusal says it expired rather than blaming its URLs.
func TestRemotePeerAuthoring_AnExpiredCredRefIsRefusedAsExpired(t *testing.T) {
	tool, base, done := memoryBackendDefFixture(t)
	defer done()
	past := time.Now().Add(-time.Minute)
	tool.PeerCredentials = peerCredsOver(t, tool.Store, storedCred{tenant: "acme", scope: "tenant", name: "peer_key", value: "v-acme", expires: &past})
	res := execDef(t, tool, asTenant(base, "acme"), `{"op":"create","name":"peer","overlay":{"kind":"remote","config":{"base_url":"https://peer.example.com","api_key_env":"$cred:peer_key"}}}`)
	if !res.IsError || !strings.Contains(res.Text, "expired") {
		t.Fatalf("a def naming an expired credential: %s; want it refused as expired", res.Text)
	}
	if strings.Contains(res.Text, "v-acme") {
		t.Fatal("the refusal carries the credential's value")
	}
}

// ---- dial ----

// A def whose credential expired after it was authored dials with nothing:
// the peer is not sent the expired value, nor a request without it.
func TestRemoteMemoryBackend_AnExpiredCredRefSendsNothing(t *testing.T) {
	peer := newHeaderPeer(t, nil)
	defTool, _, done := memoryBackendDefFixture(t)
	defer done()
	plantRemoteMemoryBackend(t, defTool.Store, "acme", fmt.Sprintf(`{"name":"peer","kind":"remote","config":{"base_url":%q,"api_key_env":"$cred:peer_key"}}`, localhostURL(t, peer.srv.URL)))
	past := time.Now().Add(-time.Minute)
	m := &Memory{Store: defTool.Store, Cfg: localPeerCfg(), PeerCredentials: peerCredsOver(t, defTool.Store,
		storedCred{tenant: "acme", scope: "tenant", name: "peer_key", value: "v-acme", expires: &past})}
	ctx := memoryRunCtx("acme")
	_, err := m.backend(ctx).Get(ctx, store.MemoryScopeUser, "u1", "k")
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("Get through a peer whose credential expired: err = %v, want it to say the credential expired", err)
	}
	if got := peer.sent(labelled); len(got) != 0 {
		t.Errorf("the peer received %v; want no request once the credential expired", got)
	}
}

// headerPeer is an httptest peer that records every Authorization header.
type headerPeer struct {
	*hitCounter
	mu    sync.Mutex
	authz []string
}

func newHeaderPeer(t *testing.T, h http.HandlerFunc) *headerPeer {
	t.Helper()
	p := &headerPeer{}
	p.hitCounter = newHitCounter(t, func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.authz = append(p.authz, r.Header.Get("Authorization"))
		p.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	return p
}

// sent reports which of the labelled values the peer received as a bearer,
// by label, so a failure never prints a credential.
func (p *headerPeer) sent(values map[string]string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, h := range p.authz {
		label := "an unknown value"
		if h == "" {
			label = "no header"
		}
		for l, v := range values {
			if h == "Bearer "+v {
				label = l
			}
		}
		out = append(out, label)
	}
	return out
}

func onlySent(t *testing.T, p *headerPeer, values map[string]string, want string) {
	t.Helper()
	got := p.sent(values)
	if len(got) == 0 {
		t.Fatalf("the peer received no request; want one carrying %s", want)
	}
	for _, g := range got {
		if g != want {
			t.Errorf("the peer received a bearer from %s; want only %s (all: %v)", g, want, got)
			return
		}
	}
}

// localPeerCfg lists localhost as a private host, so a runtime def may dial
// the httptest peer.
func localPeerCfg() *config.Config {
	return &config.Config{Env: config.Env{HTTPPrivateHostAllowlist: []string{"localhost"}}}
}

func memoryRunCtx(tenant string) context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a1", UserID: "u1", TenantID: tenant})
	return tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"user"}, Backend: "peer"})
}

var labelled = map[string]string{"acme's credential": "v-acme", "globex's credential": "v-globex", "the shared layer's credential": "v-shared"}

// Authored through the def tool by a tenant operator, then dialed by a run in
// that tenant: the bearer is the author tenant's credential, though another
// tenant holds one of the same name.
func TestRemoteMemoryBackend_CredRefSendsTheAuthorTenantsCredential(t *testing.T) {
	peer := newHeaderPeer(t, nil)
	defTool, base, done := memoryBackendDefFixture(t)
	defer done()
	defTool.Cfg.Env = localPeerCfg().Env
	creds := peerCredsOver(t, defTool.Store, tenantCred("acme", "peer_key", "v-acme"), tenantCred("globex", "peer_key", "v-globex"))
	defTool.PeerCredentials = creds
	if res := execDef(t, defTool, asTenant(base, "acme"), fmt.Sprintf(`{"op":"create","name":"peer","overlay":{"kind":"remote","config":{"base_url":%q,"api_key_env":"$cred:peer_key"}}}`, localhostURL(t, peer.srv.URL))); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}

	m := &Memory{Store: defTool.Store, Cfg: defTool.Cfg, PeerCredentials: creds}
	ctx := memoryRunCtx("acme")
	_, _ = m.backend(ctx).Get(ctx, store.MemoryScopeUser, "u1", "k")
	onlySent(t, peer, labelled, "acme's credential")
}

// The run's own user-scope credential of the same name is not a fallback: the
// def is the tenant's, so a member's personal token never reaches a host
// another member chose.
func TestRemoteMemoryBackend_CredRefIgnoresTheRunUsersOwnCredential(t *testing.T) {
	peer := newHeaderPeer(t, nil)
	defTool, _, done := memoryBackendDefFixture(t)
	defer done()
	plantRemoteMemoryBackend(t, defTool.Store, "acme", fmt.Sprintf(`{"name":"peer","kind":"remote","config":{"base_url":%q,"api_key_env":"$cred:peer_key"}}`, localhostURL(t, peer.srv.URL)))
	m := &Memory{Store: defTool.Store, Cfg: localPeerCfg(), PeerCredentials: peerCredsOver(t, defTool.Store,
		storedCred{tenant: "acme", scope: "user", scopeID: "u1", name: "peer_key", value: "v-acme"})}
	ctx := memoryRunCtx("acme") // user u1
	_, _ = m.backend(ctx).Get(ctx, store.MemoryScopeUser, "u1", "k")
	if got := peer.sent(labelled); len(got) != 0 {
		t.Errorf("the peer received %v; want no request when only the run user holds the credential", got)
	}
}

// A shared ("") def is resolved by every tenant. Its reference resolves in
// the shared layer, the def's owner, so a run in globex sends the shared
// credential — and, when only globex holds one of that name, sends nothing
// rather than globex's own.
func TestRemoteMemoryBackend_SharedDefCredRefNeverResolvesTheCallersTenant(t *testing.T) {
	for _, tc := range []struct {
		name  string
		creds []storedCred
		want  string // "" = no request may reach the peer
	}{
		{"the owner holds the credential", []storedCred{tenantCred("", "peer_key", "v-shared"), tenantCred("globex", "peer_key", "v-globex")}, "the shared layer's credential"},
		{"only the calling tenant holds it", []storedCred{tenantCred("globex", "peer_key", "v-globex")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := newHeaderPeer(t, nil)
			defTool, _, done := memoryBackendDefFixture(t)
			defer done()
			plantRemoteMemoryBackend(t, defTool.Store, "", fmt.Sprintf(`{"name":"peer","kind":"remote","config":{"base_url":%q,"api_key_env":"$cred:peer_key"}}`, localhostURL(t, peer.srv.URL)))
			m := &Memory{Store: defTool.Store, Cfg: localPeerCfg(), PeerCredentials: peerCredsOver(t, defTool.Store, tc.creds...)}
			ctx := memoryRunCtx("globex")
			_, err := m.backend(ctx).Get(ctx, store.MemoryScopeUser, "u1", "k")
			if tc.want != "" {
				onlySent(t, peer, labelled, tc.want)
				return
			}
			if got := peer.sent(labelled); len(got) != 0 {
				t.Errorf("the peer received %v; want no request when the owning tenant has no such credential", got)
			}
			if err == nil || !strings.Contains(err.Error(), "has no tenant-level credential") {
				t.Errorf("the call did not fail on the missing credential: %v", err)
			}
		})
	}
}

// plantDocumentSource stores an active document source def named "peer".
func plantDocumentSource(t *testing.T, s store.Store, tenant, body string) {
	t.Helper()
	row, err := s.DocumentSourceDefCreate(context.Background(), store.DocumentSourceDefRow{DefID: mintDefID(), Name: "peer", Definition: json.RawMessage(body), TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DocumentSourceDefSetActive(context.Background(), tenant, "peer", row.DefID, "planter"); err != nil {
		t.Fatal(err)
	}
}

// syncThrough binds a fresh local document to the "peer" source and syncs it,
// as a run in tenant.
func syncThrough(t *testing.T, d *Document, base context.Context, tenant string) tools.Result {
	t.Helper()
	ctx := tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: tenant})
	out, res := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Local"}`)
	if res.IsError {
		t.Fatalf("create_document: %s", res.Text)
	}
	docID := out["document_id"].(string)
	if _, bres := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","scope":"user","id":%q,"source":"peer","remote_ref":"/docs/remote"}`, docID)); bres.IsError {
		t.Fatalf("set_remote: %s", bres.Text)
	}
	_, sres := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID))
	return sres
}

func TestRemoteDocumentSource_CredRefSendsTheOwningTenantsCredential(t *testing.T) {
	peer := newHeaderPeer(t, newStubDocPeer().handler().ServeHTTP)
	d, base, s := documentFixture(t)
	d.Cfg = localPeerCfg()
	d.PeerCredentials = peerCredsOver(t, s, tenantCred("acme", "peer_key", "v-acme"), tenantCred("globex", "peer_key", "v-globex"))
	plantDocumentSource(t, s, "acme", fmt.Sprintf(`{"name":"peer","config":{"base_url":%q,"api_key_env":"$cred:peer_key"}}`, localhostURL(t, peer.srv.URL)))
	if res := syncThrough(t, d, base, "acme"); res.IsError {
		t.Fatalf("sync: %s", res.Text)
	}
	onlySent(t, peer, labelled, "acme's credential")
}

func TestRemoteDocumentSource_SharedDefCredRefNeverResolvesTheCallersTenant(t *testing.T) {
	for _, tc := range []struct {
		name  string
		creds []storedCred
		want  string
	}{
		{"the owner holds the credential", []storedCred{tenantCred("", "peer_key", "v-shared"), tenantCred("globex", "peer_key", "v-globex")}, "the shared layer's credential"},
		{"only the calling tenant holds it", []storedCred{tenantCred("globex", "peer_key", "v-globex")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := newHeaderPeer(t, newStubDocPeer().handler().ServeHTTP)
			d, base, s := documentFixture(t)
			d.Cfg = localPeerCfg()
			d.PeerCredentials = peerCredsOver(t, s, tc.creds...)
			plantDocumentSource(t, s, "", fmt.Sprintf(`{"name":"peer","config":{"base_url":%q,"api_key_env":"$cred:peer_key"}}`, localhostURL(t, peer.srv.URL)))
			res := syncThrough(t, d, base, "globex")
			if tc.want != "" {
				if res.IsError {
					t.Fatalf("sync: %s", res.Text)
				}
				onlySent(t, peer, labelled, tc.want)
				return
			}
			if got := peer.sent(labelled); len(got) != 0 {
				t.Errorf("the peer received %v; want no request when the owning tenant has no such credential", got)
			}
			if !res.IsError || !strings.Contains(res.Text, "has no tenant-level credential") {
				t.Errorf("sync did not fail on the missing credential: %s", res.Text)
			}
		})
	}
}

// At dial, a tenant credential restricted by allowed_urls is sent to a peer
// whose base_url its list covers, and to nowhere else: not to a peer it does
// not cover, and not through a covered peer's redirect to another host. The
// client is the pin-honouring policy every peer client has (NewGuardedClient
// in production; here wrapped around one that trusts the test certificate).
func TestRemoteMemoryBackend_ARestrictedCredReachesOnlyItsBaseURL(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]string{} // server -> Authorization values
	record := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[name] = append(seen[name], r.Header.Get("Authorization"))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}
	covered := httptest.NewTLSServer(record("covered"))
	defer covered.Close()
	sink := httptest.NewTLSServer(record("sink"))
	defer sink.Close()
	hop := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer hop.Close()

	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	creds := peerCredsOver(t, st, storedCred{tenant: "acme", scope: "tenant", name: "peer_key", value: "v-acme", allowed: []string{covered.URL, hop.URL}})
	dial := func(base string) {
		t.Helper()
		b, err := remote.New(remote.Options{
			BaseURL:          base,
			DefaultAPIKeyEnv: "$cred:peer_key",
			KeyResolver:      peerKeyResolver(creds, "acme", base),
			HTTPClient:       netguard.HonorPins(covered.Client()), // httptest's one certificate serves all three
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx := memoryRunCtx("acme")
		_, _ = b.Get(ctx, store.MemoryScopeUser, "u1", "k")
	}

	dial(covered.URL)
	dial(sink.URL)
	dial(hop.URL)
	mu.Lock()
	defer mu.Unlock()
	if got := seen["covered"]; len(got) != 1 || got[0] != "Bearer v-acme" {
		t.Errorf("the covered peer received %d request(s); want one carrying the credential", len(got))
	}
	if got := seen["sink"]; len(got) != 0 {
		t.Errorf("a host outside the credential's list received %d request(s)", len(got))
	}
}
