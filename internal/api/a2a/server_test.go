package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/denn-gubsky/loomcycle/internal/a2a/sign"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakeStore is the minimal CardAndRunStore: it serves the card only via
// the cfg path in lookup.A2AServerCard (so A2AServerCardDefGetActive is
// never reached in these tests) and reports no runs.
type fakeStore struct{}

func (fakeStore) A2AServerCardDefGetActive(ctx context.Context, name string) (store.A2AServerCardDefRow, error) {
	return store.A2AServerCardDefRow{}, &store.ErrNotFound{Kind: "a2a_server_card_def", ID: name}
}
func (fakeStore) GetRun(ctx context.Context, runID string) (store.Run, error) {
	return store.Run{}, &store.ErrNotFound{Kind: "run", ID: runID}
}
func (fakeStore) GetRunByAgentID(ctx context.Context, agentID string) (store.Run, error) {
	return store.Run{}, &store.ErrNotFound{Kind: "run", ID: agentID}
}
func (fakeStore) InterruptListByRun(ctx context.Context, runID, statusFilter string) ([]store.InterruptRow, error) {
	return nil, nil
}
func (fakeStore) InterruptResolve(ctx context.Context, interruptID, answer, resolvedBy string, answerMeta json.RawMessage) error {
	return nil
}

// noopRunner satisfies runner.Runner without doing anything; the routing
// + card tests never drive a run.
type noopRunner struct{}

func (noopRunner) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	return nil
}

// noopConnector embeds the interface; the card tests never cancel.
type noopConnector struct{ connector.Connector }

// newTestServer builds an enabled A2A server serving fixtureCard from
// cfg with the given tenancy mode and public base URL.
func newTestServer(t *testing.T, tenancy, baseURL string) *Server {
	t.Helper()
	cfg := &config.Config{
		A2AServerCards: map[string]config.A2AServerCard{
			"loomcycle-fleet": fixtureCard(),
		},
	}
	cfg.Env.A2AServerEnabled = true
	cfg.Env.A2AServerCardName = "loomcycle-fleet"
	cfg.Env.A2ATenancyRouting = tenancy
	cfg.Env.A2APublicBaseURL = baseURL

	srv, err := New(context.Background(), Deps{
		Cfg:   cfg,
		Store: fakeStore{},
		Conn:  noopConnector{},
		Run:   noopRunner{},
		Auth:  nil, // open mode
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv == nil {
		t.Fatal("New returned nil server for an enabled config")
	}
	return srv
}

// mountedHandler builds the full request handler the way main.go does:
// the mux with the A2A routes mounted, wrapped in PathTenantWrapper so
// path-mode tenant stripping runs. A "/ui/" subtree route is registered
// to guard against the ServeMux wildcard-conflict regression that an
// open "/{tenant}" first segment would have caused.
func mountedHandler(srv *Server) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /ui/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Mount(mux, nil)
	return srv.PathTenantWrapper(mux)
}

func TestNew_DisabledByDefault(t *testing.T) {
	cfg := &config.Config{}
	// A2AServerEnabled defaults to false.
	srv, err := New(context.Background(), Deps{Cfg: cfg, Store: fakeStore{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv != nil {
		t.Fatal("expected nil server when A2A surface is disabled")
	}
	// Nil server methods must be no-ops, not panics.
	srv.Mount(http.NewServeMux(), nil)
	srv.RegisterGRPC(nil)
}

func TestNew_MissingCardIsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Env.A2AServerEnabled = true
	cfg.Env.A2AServerCardName = "nope"
	_, err := New(context.Background(), Deps{Cfg: cfg, Store: fakeStore{}, Run: noopRunner{}, Conn: noopConnector{}})
	if err == nil {
		t.Fatal("expected error for a missing active card")
	}
}

func fetchCard(t *testing.T, h http.Handler, target, host string) (*httptest.ResponseRecorder, *a2asdk.AgentCard) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if host != "" {
		req.Host = host
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var card a2asdk.AgentCard
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("decode card: %v (body=%s)", err, rec.Body.String())
	}
	return rec, &card
}

func TestWellKnownCard_ServedWithCacheControl(t *testing.T) {
	h := mountedHandler(newTestServer(t, "none", "https://agents.example"))
	rec, card := fetchCard(t, h, "/.well-known/agent-card.json", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "max-age=300" {
		t.Errorf("Cache-Control = %q, want max-age=300", got)
	}
	if card.Name != "loomcycle-fleet" || len(card.Skills) != 2 {
		t.Errorf("served card = %+v", card)
	}
}

// TestWellKnownCard_ServedSignedWhenKeyAllowlisted asserts the full
// served-card path signs the card end-to-end when sign_with_key_env names
// an allowlisted var holding a usable key — the signature is present on
// the fetched card and verifies via the self-contained path.
func TestWellKnownCard_ServedSignedWhenKeyAllowlisted(t *testing.T) {
	const envName = "LOOMCYCLE_A2A_SIGNING_KEY"
	_, pemStr := ecKeyPEM(t)
	t.Setenv(envName, pemStr)

	cardCfg := fixtureCard()
	cardCfg.SignWithKeyEnv = envName
	cfg := &config.Config{
		A2AServerCards: map[string]config.A2AServerCard{"loomcycle-fleet": cardCfg},
	}
	cfg.Env.A2AServerEnabled = true
	cfg.Env.A2AServerCardName = "loomcycle-fleet"
	cfg.Env.A2APublicBaseURL = "https://agents.example"
	cfg.Env.SchedulerEnvAllowlist = []string{envName}

	srv, err := New(context.Background(), Deps{Cfg: cfg, Store: fakeStore{}, Conn: noopConnector{}, Run: noopRunner{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec, card := fetchCard(t, mountedHandler(srv), "/.well-known/agent-card.json", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(card.Signatures) != 1 {
		t.Fatalf("served card has %d signatures, want 1 (signed)", len(card.Signatures))
	}
	if err := sign.VerifyCardSelfContained(card); err != nil {
		t.Fatalf("served signature does not verify: %v", err)
	}
}

// TestWellKnownCard_ServedUnsignedWhenKeyNotAllowlisted asserts a card
// whose sign_with_key_env is NOT on the allowlist is served unsigned —
// serving never fails on the signing gate.
func TestWellKnownCard_ServedUnsignedWhenKeyNotAllowlisted(t *testing.T) {
	const envName = "LOOMCYCLE_A2A_SIGNING_KEY"
	_, pemStr := ecKeyPEM(t)
	t.Setenv(envName, pemStr)

	cardCfg := fixtureCard()
	cardCfg.SignWithKeyEnv = envName
	cfg := &config.Config{
		A2AServerCards: map[string]config.A2AServerCard{"loomcycle-fleet": cardCfg},
	}
	cfg.Env.A2AServerEnabled = true
	cfg.Env.A2AServerCardName = "loomcycle-fleet"
	cfg.Env.A2APublicBaseURL = "https://agents.example"
	// SchedulerEnvAllowlist intentionally omits envName.

	srv, err := New(context.Background(), Deps{Cfg: cfg, Store: fakeStore{}, Conn: noopConnector{}, Run: noopRunner{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, card := fetchCard(t, mountedHandler(srv), "/.well-known/agent-card.json", "")
	if card == nil {
		t.Fatal("card not served")
	}
	if len(card.Signatures) != 0 {
		t.Fatalf("served card was signed despite key not being allowlisted")
	}
}

func TestHostTenancy_DistinctTenantsAndRootFallback(t *testing.T) {
	h := mountedHandler(newTestServer(t, "host", "https://agents.example"))

	// Two tenant subdomains both resolve their card (the served card is
	// the same fixture; the distinction is the routed tenant, which a
	// follow-up run would attribute differently). We assert both serve
	// 200 and the bare root also serves the single-tenant card.
	for _, host := range []string{"tenant-acme.agents.example", "tenant-globex.agents.example", "agents.example"} {
		rec, card := fetchCard(t, h, "/.well-known/agent-card.json", host)
		if rec.Code != http.StatusOK {
			t.Fatalf("host %q: status = %d, want 200", host, rec.Code)
		}
		if card.Name != "loomcycle-fleet" {
			t.Errorf("host %q: card name = %q", host, card.Name)
		}
	}
}

func TestPathTenancy_DistinctTenantCardsAndRootFallback(t *testing.T) {
	h := mountedHandler(newTestServer(t, "path", "https://agents.example"))

	// Path-mode: /{tenant}/.well-known/... serves a card whose binding
	// URLs are prefixed with /{tenant}; the bare root serves the
	// un-prefixed single-tenant card.
	_, acme := fetchCard(t, h, "/acme/.well-known/agent-card.json", "")
	if acme == nil {
		t.Fatal("acme tenant card not served")
	}
	if !hasInterfaceURL(acme, "https://agents.example/acme/a2a/v1") {
		t.Errorf("acme card REST URL not tenant-prefixed: %+v", acme.SupportedInterfaces)
	}

	_, globex := fetchCard(t, h, "/globex/.well-known/agent-card.json", "")
	if globex == nil || !hasInterfaceURL(globex, "https://agents.example/globex/a2a/v1") {
		t.Errorf("globex card REST URL not tenant-prefixed: %+v", globex)
	}

	// Distinct tenants produce distinct binding URLs (cross-tenant
	// isolation at the discovery layer).
	if hasInterfaceURL(acme, "https://agents.example/globex/a2a/v1") {
		t.Error("acme card leaked globex's tenant-prefixed URL")
	}

	_, root := fetchCard(t, h, "/.well-known/agent-card.json", "")
	if root == nil || !hasInterfaceURL(root, "https://agents.example/a2a/v1") {
		t.Errorf("root card should serve un-prefixed binding URL: %+v", root)
	}
}

func hasInterfaceURL(card *a2asdk.AgentCard, url string) bool {
	for _, iface := range card.SupportedInterfaces {
		if iface.URL == url {
			return true
		}
	}
	return false
}

func TestExtendedCard_GatedByAdminAuth(t *testing.T) {
	srv := newTestServer(t, "none", "https://agents.example")
	mux := http.NewServeMux()
	// adminAuth that rejects everything (simulates unauth'd caller).
	denyAll := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	}
	srv.Mount(mux, denyAll)

	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent-card.json/extended", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("extended card status = %d, want 401 (admin auth must gate it)", rec.Code)
	}
}
