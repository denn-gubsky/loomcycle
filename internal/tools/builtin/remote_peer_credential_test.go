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

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// These tests pin the "$cred:<name>" form of a remote peer's api_key_env: a
// stored credential, resolved in the def's OWNING tenant at tenant scope —
// never in the calling run's tenant, and never from another tenant.

// storedCred is one credential to seed.
type storedCred struct {
	tenant, scope, scopeID, name, value string
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
		if _, err := eng.PutInline(context.Background(), credential.Identity{TenantID: c.tenant, Scope: c.scope, ScopeID: c.scopeID, Name: c.name}, c.value, nil); err != nil {
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
	}
}

func TestRemoteMemoryBackendAuthoring_CredRefMustExistInTheAuthorsTenant(t *testing.T) {
	runCredentialCases(t, func(t *testing.T) (tools.Tool, context.Context, func()) {
		tool, ctx, done := memoryBackendDefFixture(t)
		tool.PeerCredentials = peerCredsOver(t, tool.Store, tenantCred("acme", "peer_key", "v-acme"), tenantCred("globex", "globex_key", "v-globex"), tenantCred("", "peer_key", "v-shared"))
		return tool, ctx, done
	}, credRefCases(`"kind":"remote",`))
}

func TestRemoteDocumentSourceAuthoring_CredRefMustExistInTheAuthorsTenant(t *testing.T) {
	runCredentialCases(t, func(t *testing.T) (tools.Tool, context.Context, func()) {
		tool, ctx, done := documentSourceDefFixture(t)
		tool.PeerCredentials = peerCredsOver(t, tool.Store, tenantCred("acme", "peer_key", "v-acme"), tenantCred("globex", "globex_key", "v-globex"), tenantCred("", "peer_key", "v-shared"))
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

// ---- dial ----

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
