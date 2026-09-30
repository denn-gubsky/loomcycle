package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// These tests pin the dial-time trust of the two runtime-authorable remote
// peers — a kind:remote MemoryBackendDef and a DocumentSourceDef. Both send an
// operator credential (api_key_env) to their base_url, so a definition authored
// at runtime (by a tenant operator, or an agent granted the def tool) must not
// be able to point that at a private / loopback / metadata host on its own
// say-so. Only an operator-declared def (yaml) keeps its own host trusted; a
// dynamic one reaches a private host only when the operator lists that host in
// LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST.
//
// The peers are httptest servers on 127.0.0.1. A dynamic def names them as
// "localhost" — a HOSTNAME that resolves to loopback, the shape that passes the
// authoring-time literal-IP check and must be stopped by the dial guard.

// hitCounter is an httptest peer that counts every request it receives.
type hitCounter struct {
	hits atomic.Int32
	srv  *httptest.Server
}

func newHitCounter(t *testing.T, h http.HandlerFunc) *hitCounter {
	t.Helper()
	c := &hitCounter{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// localhostURL rewrites an httptest URL (http://127.0.0.1:PORT) to the
// equivalent http://localhost:PORT.
func localhostURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return "http://localhost:" + u.Port()
}

// ---- memory backends ----

// remoteMemoryFixture authors a kind:remote MemoryBackendDef named "peer" at
// baseURL through the def tool (the real runtime-authoring path) under tenant
// "tnt", and returns a Memory tool + a run ctx whose agent selects it.
func remoteMemoryFixture(t *testing.T, cfg *config.Config, baseURL string) (*Memory, context.Context) {
	t.Helper()
	defTool, defCtx, cleanup := memoryBackendDefFixture(t)
	t.Cleanup(cleanup)
	defCtx = tools.WithRunIdentity(defCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "tnt"})
	res, _ := defTool.Execute(defCtx, json.RawMessage(fmt.Sprintf(
		`{"op":"create","name":"peer","overlay":{"kind":"remote","config":{"base_url":%q}}}`, baseURL)))
	if res.IsError {
		t.Fatalf("author remote backend: %s", res.Text)
	}
	m := &Memory{Store: defTool.Store, Cfg: cfg}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a1", UserID: "u1", TenantID: "tnt"})
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"user"}, Backend: "peer"})
	return m, ctx
}

// publicListedLocalhost lists "localhost" as a PUBLIC host only: the name
// floor lets a runtime def aim at it, the address guard still refuses the
// loopback address it resolves to.
func publicListedLocalhost() *config.Config {
	return &config.Config{Env: config.Env{HTTPHostAllowlist: []string{"localhost"}}}
}

func TestRemoteMemoryBackend_DynamicDefIsRefusedAtAPrivateHost(t *testing.T) {
	peer := newHitCounter(t, nil)
	m, ctx := remoteMemoryFixture(t, publicListedLocalhost(), localhostURL(t, peer.srv.URL))

	_, err := m.backend(ctx).Get(ctx, store.MemoryScopeUser, "u1", "k")
	if err == nil {
		t.Fatalf("Get through a runtime-authored backend at a loopback host succeeded; want the dial refused")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("error = %v, want the SSRF guard's refusal", err)
	}
	if n := peer.hits.Load(); n != 0 {
		t.Errorf("the private peer received %d request(s); a runtime-authored def must not reach it", n)
	}
}

func TestRemoteMemoryBackend_DynamicDefReachesAnOperatorAllowlistedHost(t *testing.T) {
	peer := newHitCounter(t, nil)
	cfg := &config.Config{Env: config.Env{HTTPPrivateHostAllowlist: []string{"localhost"}}}
	m, ctx := remoteMemoryFixture(t, cfg, localhostURL(t, peer.srv.URL))

	_, _ = m.backend(ctx).Get(ctx, store.MemoryScopeUser, "u1", "k")
	if n := peer.hits.Load(); n != 1 {
		t.Errorf("the operator-allowlisted peer received %d request(s), want 1", n)
	}
}

func TestRemoteMemoryBackend_YAMLDeclaredDefStillReachesItsPrivateHost(t *testing.T) {
	peer := newHitCounter(t, nil)
	cfg := &config.Config{MemoryBackends: map[string]config.MemoryBackend{
		"peer": {Kind: "remote", Config: config.MemoryBackendConfig{BaseURL: peer.srv.URL}},
	}}
	m := &Memory{Cfg: cfg}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a1", UserID: "u1"})
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"user"}, Backend: "peer"})

	_, _ = m.backend(ctx).Get(ctx, store.MemoryScopeUser, "u1", "k")
	if n := peer.hits.Load(); n != 1 {
		t.Errorf("the yaml-declared private peer received %d request(s), want 1", n)
	}
}

// TestRemoteMemoryBackend_RedirectToAPrivateHostIsBlocked: the allowlisted
// peer answers with a redirect to a DIFFERENT private address; the hop must be
// refused for both kinds of def — the operator's vouch is per host, not a
// licence to be bounced anywhere on the private network.
func TestRemoteMemoryBackend_RedirectToAPrivateHostIsBlocked(t *testing.T) {
	target := newHitCounter(t, nil)
	// Non-vacuity: the redirect target is up and reachable by an unguarded client.
	if resp, err := http.Get(target.srv.URL); err != nil {
		t.Fatalf("probe the redirect target: %v", err)
	} else {
		_ = resp.Body.Close()
	}
	target.hits.Store(0)
	bouncer := newHitCounter(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})

	t.Run("dynamic", func(t *testing.T) {
		target.hits.Store(0)
		bouncer.hits.Store(0)
		cfg := &config.Config{Env: config.Env{HTTPPrivateHostAllowlist: []string{"localhost"}}}
		m, ctx := remoteMemoryFixture(t, cfg, localhostURL(t, bouncer.srv.URL))
		if _, err := m.backend(ctx).Get(ctx, store.MemoryScopeUser, "u1", "k"); err == nil {
			t.Errorf("Get followed a redirect to a private host; want it refused")
		}
		if n := target.hits.Load(); n != 0 {
			t.Errorf("the redirect target received %d request(s), want 0", n)
		}
		if bouncer.hits.Load() == 0 {
			t.Errorf("the allowlisted peer received no request — the redirect was never exercised")
		}
	})
	t.Run("yaml", func(t *testing.T) {
		target.hits.Store(0)
		bouncer.hits.Store(0)
		cfg := &config.Config{MemoryBackends: map[string]config.MemoryBackend{
			"peer": {Kind: "remote", Config: config.MemoryBackendConfig{BaseURL: localhostURL(t, bouncer.srv.URL)}},
		}}
		m := &Memory{Cfg: cfg}
		ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a1", UserID: "u1"})
		ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"user"}, Backend: "peer"})
		if _, err := m.backend(ctx).Get(ctx, store.MemoryScopeUser, "u1", "k"); err == nil {
			t.Errorf("Get followed a redirect to a private host; want it refused")
		}
		if n := target.hits.Load(); n != 0 {
			t.Errorf("the redirect target received %d request(s), want 0", n)
		}
		if bouncer.hits.Load() == 0 {
			t.Errorf("the allowlisted peer received no request — the redirect was never exercised")
		}
	})
}

// ---- document sources ----

// remoteDocumentFixture authors a DocumentSourceDef named "peer" at baseURL
// through the def tool under the Document fixture's tenant, binds a fresh local
// document to it, and returns what a sync call needs.
func remoteDocumentFixture(t *testing.T, cfg *config.Config, baseURL string) (*Document, context.Context, string) {
	t.Helper()
	d, ctx, s := documentFixture(t)
	d.Cfg = cfg
	defTool := &DocumentSourceDef{Store: s, Cfg: cfg}
	defCtx := tools.WithDocumentSourceDefPolicy(ctx, tools.DocumentSourceDefPolicyValue{Scopes: []string{"any"}})
	res, _ := defTool.Execute(defCtx, json.RawMessage(fmt.Sprintf(
		`{"op":"create","name":"peer","overlay":{"config":{"base_url":%q}}}`, baseURL)))
	if res.IsError {
		t.Fatalf("author document source: %s", res.Text)
	}
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Local"}`)
	docID := out["document_id"].(string)
	if _, bres := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","scope":"user","id":%q,"source":"peer","remote_ref":"/docs/remote"}`, docID)); bres.IsError {
		t.Fatalf("set_remote: %s", bres.Text)
	}
	return d, ctx, docID
}

func TestRemoteDocumentSource_DynamicDefIsRefusedAtAPrivateHost(t *testing.T) {
	peer := newHitCounter(t, newStubDocPeer().handler().ServeHTTP)
	d, ctx, docID := remoteDocumentFixture(t, publicListedLocalhost(), localhostURL(t, peer.srv.URL))

	_, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID))
	if !res.IsError {
		t.Fatalf("sync through a runtime-authored source at a loopback host succeeded; want the dial refused")
	}
	if !strings.Contains(res.Text, "blocked") {
		t.Errorf("sync error = %s, want the SSRF guard's refusal", res.Text)
	}
	if n := peer.hits.Load(); n != 0 {
		t.Errorf("the private peer received %d request(s); a runtime-authored def must not reach it", n)
	}
}

func TestRemoteDocumentSource_DynamicDefReachesAnOperatorAllowlistedHost(t *testing.T) {
	peer := newHitCounter(t, newStubDocPeer().handler().ServeHTTP)
	cfg := &config.Config{Env: config.Env{HTTPPrivateHostAllowlist: []string{"localhost"}}}
	d, ctx, docID := remoteDocumentFixture(t, cfg, localhostURL(t, peer.srv.URL))

	if _, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID)); res.IsError {
		t.Fatalf("sync to an operator-allowlisted peer: %s", res.Text)
	}
	if peer.hits.Load() == 0 {
		t.Errorf("the operator-allowlisted peer received no request")
	}
}

func TestRemoteDocumentSource_RedirectToAPrivateHostIsBlocked(t *testing.T) {
	target := newHitCounter(t, newStubDocPeer().handler().ServeHTTP)
	bouncer := newHitCounter(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	cfg := &config.Config{Env: config.Env{HTTPPrivateHostAllowlist: []string{"localhost"}}}
	d, ctx, docID := remoteDocumentFixture(t, cfg, localhostURL(t, bouncer.srv.URL))

	if _, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID)); !res.IsError {
		t.Errorf("sync followed a redirect to a private host; want it refused")
	}
	if n := target.hits.Load(); n != 0 {
		t.Errorf("the redirect target received %d request(s), want 0", n)
	}
	if bouncer.hits.Load() == 0 {
		t.Errorf("the allowlisted peer received no request — the redirect was never exercised")
	}
}
