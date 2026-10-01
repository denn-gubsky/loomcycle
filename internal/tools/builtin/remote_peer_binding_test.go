package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/remote"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// These tests pin what a runtime-authored remote peer — a kind:remote
// MemoryBackendDef or a DocumentSourceDef — may do with a credential. Both
// send `Authorization: Bearer <value of api_key_env>` to their base_url, so:
//
//   - the base_url host must be one the operator lists (the host floor), at
//     authoring, on restore and again at dial;
//   - a non-admin author may not name the credential freely: api_key_env could
//     name another tenant's LOOMCYCLE_PEER_KEY_<tenant> or a GITHUB_TOKEN.
//
// A yaml-declared peer is the operator's and is untouched.

const floorRefusal = "not in LOOMCYCLE_HTTP_HOST_ALLOWLIST"

// listingOnly is a config whose operator lists one public host.
func listingOnly(host string) *config.Config {
	return &config.Config{Env: config.Env{HTTPHostAllowlist: []string{host}}}
}

// ---- host floor at authoring ----

// The floor binds every author, admin included: it is the operator's list,
// and a def the tool writes is dialed as a runtime def whoever wrote it.
func TestRemoteMemoryBackend_DynamicDefRefusedAtUnallowlistedPublicHost(t *testing.T) {
	for _, who := range []string{"tenant author", "admin"} {
		t.Run(who, func(t *testing.T) {
			tool, ctx, cleanup := memoryBackendDefFixture(t)
			defer cleanup()
			tool.Cfg.Env = config.Env{HTTPHostAllowlist: []string{"peer.example"}}
			ctx = asTenant(ctx, "acme")
			if who == "admin" {
				ctx = asAdmin(ctx)
			}
			res := execDef(t, tool, ctx, `{"op":"create","name":"exfil","overlay":{"kind":"remote","config":{"base_url":"https://attacker.example"}}}`)
			if !res.IsError || !strings.Contains(res.Text, floorRefusal) {
				t.Fatalf("a remote backend at an unlisted public host was not refused by the host floor: %s", res.Text)
			}
			if res := execDef(t, tool, ctx, `{"op":"create","name":"listed","overlay":{"kind":"remote","config":{"base_url":"https://api.peer.example"}}}`); res.IsError {
				t.Fatalf("a remote backend at a listed host was refused: %s", res.Text)
			}
			res = execDef(t, tool, ctx, `{"op":"fork","name":"listed","overlay":{"config":{"base_url":"https://attacker.example"}}}`)
			if !res.IsError || !strings.Contains(res.Text, floorRefusal) {
				t.Fatalf("a fork re-aiming a backend at an unlisted host was not refused: %s", res.Text)
			}
			// A backend that dials nothing is not held to it.
			if res := execDef(t, tool, ctx, `{"op":"create","name":"local","overlay":{"kind":"inprocess","config":{"base_url":"https://attacker.example"}}}`); res.IsError {
				t.Errorf("an inprocess backend was held to the host floor: %s", res.Text)
			}
		})
	}
}

// As for memory backends: the floor binds every author.
func TestRemoteDocumentSource_DynamicDefRefusedAtUnallowlistedPublicHost(t *testing.T) {
	for _, who := range []string{"tenant author", "admin"} {
		t.Run(who, func(t *testing.T) {
			tool, ctx, cleanup := documentSourceDefFixture(t)
			defer cleanup()
			tool.Cfg.Env = config.Env{HTTPHostAllowlist: []string{"docs.example"}}
			ctx = asTenant(ctx, "acme")
			if who == "admin" {
				ctx = asAdmin(ctx)
			}
			res := execDef(t, tool, ctx, `{"op":"create","name":"exfil","overlay":{"config":{"base_url":"https://attacker.example"}}}`)
			if !res.IsError || !strings.Contains(res.Text, floorRefusal) {
				t.Fatalf("a source at an unlisted public host was not refused by the host floor: %s", res.Text)
			}
			if res := execDef(t, tool, ctx, `{"op":"create","name":"listed","overlay":{"config":{"base_url":"https://docs.example"}}}`); res.IsError {
				t.Fatalf("a source at a listed host was refused: %s", res.Text)
			}
			res = execDef(t, tool, ctx, `{"op":"fork","name":"listed","overlay":{"config":{"base_url":"https://attacker.example"}}}`)
			if !res.IsError || !strings.Contains(res.Text, floorRefusal) {
				t.Fatalf("a fork re-aiming a source at an unlisted host was not refused: %s", res.Text)
			}
		})
	}
}

// ---- credential binding at authoring ----

// credentialCase is one authoring attempt and whether the rule lets it through.
type credentialCase struct {
	name    string
	admin   bool
	tenant  string
	input   string // the def tool call
	allowed bool
	why     string
}

// credentialCases is the rule's table for one def tool. remoteKind is the
// overlay prefix that makes a def dial ("\"kind\":\"remote\"," for a memory
// backend, "" for a document source, which always dials).
func credentialCases(remoteKind string) []credentialCase {
	// create builds a create call at a listed host; extra continues the
	// config object (and may close it to add tenancy_strategy).
	create := func(extra string) string {
		return `{"op":"create","name":"peer","overlay":{` + remoteKind + `"config":{"base_url":"https://peer.example.com"` + extra + `}}}`
	}
	perTenant := `},"tenancy_strategy":{"kind":"key_per_tenant","env_pattern":"LOOMCYCLE_PEER_KEY_{tenant_id}"`
	return []credentialCase{
		{"tenant names another tenant's key", false, "acme", create(`,"api_key_env":"LOOMCYCLE_PEER_KEY_GLOBEX"`), false,
			"api_key_env reaches any credential-safe env var, globex's included"},
		{"tenant names a host credential", false, "acme", create(`,"api_key_env":"GITHUB_TOKEN"`), false,
			"a credential the operator holds for something else"},
		{"admin names any credential-safe key", true, "acme", create(`,"api_key_env":"LOOMCYCLE_PEER_KEY_GLOBEX"`), true,
			"an admin keeps free api_key_env"},
		{"tenant names no credential", false, "acme", create(``), true, "nothing is sent"},
		{"tenant binds a per-tenant key", false, "acme", create(perTenant), true,
			"completed from the run's own tenant"},
		{"tenant-less author binds a per-tenant key", false, "", create(perTenant), false,
			"a shared def: every tenant's key would go to the author's host"},
		{"tenant-less admin binds a per-tenant key", true, "", create(perTenant), true, "an admin's to choose"},
		// Forks of the yaml "primary": its operator-paired base_url + api_key_env.
		{"tenant fork keeps the operator's pairing", false, "acme",
			`{"op":"fork","name":"primary","overlay":{"config":{"api_version":"v2"}}}`, true,
			"the operator's key still goes to the operator's host"},
		{"tenant fork re-aims the operator's key", false, "acme",
			`{"op":"fork","name":"primary","overlay":{` + remoteKind + `"config":{"base_url":"https://evil.example.com"}}}`, false,
			"keeping api_key_env while choosing the host is the exfiltration"},
	}
}

func runCredentialCases(t *testing.T, fixture func(t *testing.T) (tools.Tool, context.Context, func()), cases []credentialCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool, base, done := fixture(t)
			defer done()
			ctx := asTenant(base, c.tenant)
			if c.admin {
				ctx = asAdmin(ctx)
				ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test", TenantID: c.tenant})
			}
			res := execDef(t, tool, ctx, c.input)
			switch {
			case c.allowed && res.IsError:
				t.Errorf("refused (%s): %s", c.why, res.Text)
			case !c.allowed && !res.IsError:
				t.Errorf("accepted (%s): %s", c.why, res.Text)
			}
		})
	}
}

func TestRemoteDefAuthoring_TenantAuthorCannotNameForeignEnvCredential(t *testing.T) {
	runCredentialCases(t, func(t *testing.T) (tools.Tool, context.Context, func()) { return memoryBackendDefFixture(t) },
		credentialCases(`"kind":"remote",`))
}

func TestRemoteDocumentSourceAuthoring_TenantAuthorCannotNameForeignEnvCredential(t *testing.T) {
	runCredentialCases(t, func(t *testing.T) (tools.Tool, context.Context, func()) { return documentSourceDefFixture(t) },
		credentialCases(``))
}

// An open-mode or stdio operator authenticates nobody, so it carries no
// principal to hold substrate:admin — and is still the operator, so it keeps
// free api_key_env like an admin. The in-run shape (no principal, no operator
// marker) does not.
func TestRemoteDefAuthoring_UnauthenticatedOperatorKeepsFreeCredential(t *testing.T) {
	for kind, fixture := range map[string]func(t *testing.T) (tools.Tool, context.Context, func()){
		"memory backend":  func(t *testing.T) (tools.Tool, context.Context, func()) { return memoryBackendDefFixture(t) },
		"document source": func(t *testing.T) (tools.Tool, context.Context, func()) { return documentSourceDefFixture(t) },
	} {
		t.Run(kind, func(t *testing.T) {
			remoteKind := ``
			if kind == "memory backend" {
				remoteKind = `"kind":"remote",`
			}
			input := `{"op":"create","name":"peer","overlay":{` + remoteKind + `"config":{"base_url":"https://peer.example.com","api_key_env":"LOOMCYCLE_PEER_KEY_GLOBEX"}}}`
			tool, base, done := fixture(t)
			defer done()
			if res := execDef(t, tool, tools.WithUnauthenticatedOperator(asTenant(base, "")), input); res.IsError {
				t.Errorf("an unauthenticated operator was refused a free api_key_env: %s", res.Text)
			}
			tool, base, done2 := fixture(t)
			defer done2()
			if res := execDef(t, tool, asTenant(base, ""), input); !res.IsError || !strings.Contains(res.Text, "may be set only by an admin") {
				t.Errorf("an in-run author with no principal was not refused the free api_key_env: %s", res.Text)
			}
		})
	}
}

// ---- host floor at dial ----

// authPeer is an httptest peer that counts requests and records whether any
// carried an Authorization header.
type authPeer struct {
	*hitCounter
	mu       sync.Mutex
	sawAuthz bool
}

func newAuthPeer(t *testing.T, h http.HandlerFunc) *authPeer {
	t.Helper()
	p := &authPeer{}
	p.hitCounter = newHitCounter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			p.mu.Lock()
			p.sawAuthz = true
			p.mu.Unlock()
		}
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

func (p *authPeer) authzSeen() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sawAuthz
}

const dialKeyEnv = "LOOMCYCLE_B_DIAL_TEST_PEER_KEY"

// A def stored before the floor existed (or restored unchecked) is planted
// straight into the store: authoring would refuse it now. At dial it must be
// refused BEFORE a client exists — the backend falls back to in-process — so
// nothing, and no Authorization header, reaches the peer.
func TestRemoteMemoryBackend_StoredDynamicDefAtUnlistedHostSendsNothing(t *testing.T) {
	t.Setenv(dialKeyEnv, "dial-test-value")
	peer := newAuthPeer(t, nil)
	defTool, _, cleanup := memoryBackendDefFixture(t)
	defer cleanup()
	body := fmt.Sprintf(`{"name":"peer","kind":"remote","config":{"base_url":%q,"api_key_env":%q}}`, localhostURL(t, peer.srv.URL), dialKeyEnv)
	plantRemoteMemoryBackend(t, defTool.Store, "tnt", body)

	// The operator lists some other host; localhost is on neither list.
	m := &Memory{Store: defTool.Store, Cfg: listingOnly("peer.example")}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a1", UserID: "u1", TenantID: "tnt"})
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"user"}, Backend: "peer"})

	b := m.backend(ctx)
	if _, isRemote := b.(*remote.Backend); isRemote {
		t.Fatalf("a runtime def at an unlisted host was built into a remote backend; want it refused before dial")
	}
	_, _ = b.Get(ctx, store.MemoryScopeUser, "u1", "k")
	if n := peer.hits.Load(); n != 0 {
		t.Errorf("the unlisted peer received %d request(s), want 0", n)
	}
	if peer.authzSeen() {
		t.Errorf("an Authorization header reached the unlisted peer")
	}
}

func TestRemoteDocumentSource_StoredDynamicDefAtUnlistedHostSendsNothing(t *testing.T) {
	t.Setenv(dialKeyEnv, "dial-test-value")
	peer := newAuthPeer(t, newStubDocPeer().handler().ServeHTTP)
	d, ctx, s := documentFixture(t)
	d.Cfg = listingOnly("docs.example")
	body := fmt.Sprintf(`{"name":"peer","config":{"base_url":%q,"api_key_env":%q}}`, localhostURL(t, peer.srv.URL), dialKeyEnv)
	row, err := s.DocumentSourceDefCreate(context.Background(), store.DocumentSourceDefRow{DefID: mintDefID(), Name: "peer", Definition: json.RawMessage(body), TenantID: "tnt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DocumentSourceDefSetActive(context.Background(), "tnt", "peer", row.DefID, "planter"); err != nil {
		t.Fatal(err)
	}
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Local"}`)
	docID := out["document_id"].(string)
	if _, bres := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","scope":"user","id":%q,"source":"peer","remote_ref":"/docs/remote"}`, docID)); bres.IsError {
		t.Fatalf("set_remote: %s", bres.Text)
	}

	_, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID))
	if !res.IsError || !strings.Contains(res.Text, floorRefusal) {
		t.Fatalf("sync through a runtime source at an unlisted host was not refused by the host floor: %s", res.Text)
	}
	if n := peer.hits.Load(); n != 0 {
		t.Errorf("the unlisted peer received %d request(s), want 0", n)
	}
	if peer.authzSeen() {
		t.Errorf("an Authorization header reached the unlisted peer")
	}
}

func plantRemoteMemoryBackend(t *testing.T, s store.Store, tenant, body string) {
	t.Helper()
	row, err := s.MemoryBackendDefCreate(context.Background(), store.MemoryBackendDefRow{DefID: mintDefID(), Name: "peer", Definition: json.RawMessage(body), TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MemoryBackendDefSetActive(context.Background(), tenant, "peer", row.DefID, "planter"); err != nil {
		t.Fatal(err)
	}
}

// ---- operator-declared peers are unchanged ----

// A yaml peer on neither list still dials its own host and still sends its
// credential: the operator chose both.
func TestRemoteMemoryBackend_YAMLDeclaredDefAtUnlistedHostStillSendsItsKey(t *testing.T) {
	t.Setenv(dialKeyEnv, "dial-test-value")
	peer := newAuthPeer(t, nil)
	cfg := &config.Config{MemoryBackends: map[string]config.MemoryBackend{
		"peer": {Kind: "remote", Config: config.MemoryBackendConfig{BaseURL: localhostURL(t, peer.srv.URL), APIKeyEnv: dialKeyEnv}},
	}}
	m := &Memory{Cfg: cfg}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a1", UserID: "u1"})
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"user"}, Backend: "peer"})

	_, _ = m.backend(ctx).Get(ctx, store.MemoryScopeUser, "u1", "k")
	if n := peer.hits.Load(); n != 1 {
		t.Errorf("the yaml peer received %d request(s), want 1", n)
	}
	if !peer.authzSeen() {
		t.Errorf("the yaml peer received no Authorization header")
	}
}

func TestRemoteDocumentSource_YAMLDeclaredDefAtUnlistedHostStillSendsItsKey(t *testing.T) {
	t.Setenv(dialKeyEnv, "dial-test-value")
	peer := newAuthPeer(t, newStubDocPeer().handler().ServeHTTP)
	d, ctx, _ := documentFixture(t)
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{
		"peer": {Config: config.DocumentSourceConfig{BaseURL: localhostURL(t, peer.srv.URL), APIKeyEnv: dialKeyEnv}},
	}}
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Local"}`)
	docID := out["document_id"].(string)
	if _, bres := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","scope":"user","id":%q,"source":"peer","remote_ref":"/docs/remote"}`, docID)); bres.IsError {
		t.Fatalf("set_remote: %s", bres.Text)
	}
	if _, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID)); res.IsError {
		t.Fatalf("sync to the yaml peer: %s", res.Text)
	}
	if peer.hits.Load() == 0 {
		t.Errorf("the yaml peer received no request")
	}
	if !peer.authzSeen() {
		t.Errorf("the yaml peer received no Authorization header")
	}
}
