package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// The triage routes are gated by the HTTP server, not by this package, so the
// tests reach them through the real server: its mux, its authMiddleware and
// its route-scope table, with the receiver mounted by the same SetWebhookMux
// hook main.go uses. A pass-through wrapper would let a test assert behaviour
// a real caller can never reach.

const (
	triagePepper = "pep-triage"
	// Bearers the servers accept. Test fixtures, not secrets.
	triageLegacyBearer = "triage-legacy-bearer" // triageLegacy only
	triageAdminBearer  = "triage-admin-bearer"  // triageTokens: substrate:admin, tenant ops
	triageTenantBearer = "triage-tenant-bearer" // triageTokens: substrate:tenant, tenant acme
)

// triageAuthMode picks how the test server authenticates.
type triageAuthMode int

const (
	triageOpen   triageAuthMode = iota // no bearer configured: no principal is stamped
	triageLegacy                       // only the shared LOOMCYCLE_AUTH_TOKEN bearer
	// Minted tokens: triageAdminBearer and triageTenantBearer. Kept apart
	// from triageLegacy because minting an admin token retires the legacy
	// bearer.
	triageTokens
)

// triageServer returns the real server mux with rec mounted on it.
func triageServer(t *testing.T, rec *Receiver, mode triageAuthMode) http.Handler {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{}
	cfg.Env.OperatorTokenPepper = triagePepper
	switch mode {
	case triageLegacy:
		cfg.Env.AuthToken = triageLegacyBearer
	case triageTokens:
		seedTriageToken(t, st, triageAdminBearer, "ops", "root", auth.ScopeAdmin)
		seedTriageToken(t, st, triageTenantBearer, "acme", "acme-op", auth.ScopeTenant)
	}
	srv := lchttp.New(cfg, nil, nil, concurrency.New(4, 4, time.Second), st)
	srv.SetWebhookMux(func(reg lchttp.MuxRegistrar, adminAuth func(http.Handler) http.Handler) {
		rec.Mount(reg)
		rec.MountAdmin(reg, adminAuth)
	})
	return srv.Mux()
}

func seedTriageToken(t *testing.T, st store.Store, bearer, tenant, subject, scope string) {
	t.Helper()
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID:         "def_" + subject,
		Name:          subject,
		TenantID:      tenant,
		Subject:       subject,
		TokenHash:     auth.HashToken(triagePepper, bearer),
		AllowedScopes: []string{scope},
	}); err != nil {
		t.Fatalf("seed token %s: %v", subject, err)
	}
}

// triageDo sends one request through h. bearer "" sends no Authorization
// header; hdr (may be nil) is copied onto the request.
func triageDo(h http.Handler, method, path, bearer string, body []byte, hdr http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// Webhook triage is operator-only: a tenant operator's token is refused by the
// route gate on both endpoints, for its own tenant's webhook too, before any
// handler runs.
func TestTriage_TenantTokenIsRefused(t *testing.T) {
	st := openScopeStore(t)
	putWebhookDef(t, st, "acme", "gh", signedSpawnDef("acme"))
	rec := newScopeReceiver(st, &storeRunner{st: st}, nil)
	h := triageServer(t, rec, triageTokens)

	body := []byte(`{"goal":"g"}`)
	sig := http.Header{}
	sig.Set("X-Hub-Signature-256", githubSig(scopeSecret, body))
	for _, c := range []struct {
		method, path string
		body         []byte
		hdr          http.Header
	}{
		{http.MethodGet, "/v1/_webhooks/gh/recent-deliveries", nil, nil},
		{http.MethodGet, "/v1/_webhooks/gh/recent-deliveries?tenant=acme", nil, nil},
		{http.MethodPost, "/v1/_webhooks/gh/test", body, sig},
		{http.MethodPost, "/v1/_webhooks/gh/test?tenant=acme", body, sig},
	} {
		if w := triageDo(h, c.method, c.path, triageTenantBearer, c.body, c.hdr); w.Code != http.StatusForbidden {
			t.Errorf("%s %s with a tenant token: status = %d, want 403; body=%s", c.method, c.path, w.Code, w.Body.String())
		}
	}

	// The same dry-run with the admin token gets through the gate, so the
	// 403s above are the scope's doing.
	if w := triageDo(h, http.MethodPost, "/v1/_webhooks/gh/test?tenant=acme", triageAdminBearer, body, sig); w.Code != http.StatusOK {
		t.Errorf("admin control: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// An admin dry-runs a tenant-owned webhook the way it reads its deliveries:
// ?tenant= names the tenant whose def is tested.
func TestReceiver_Test_AdminCanFocusAnotherTenantsWebhook(t *testing.T) {
	st := openScopeStore(t)
	putWebhookDef(t, st, "acme", "w", signedSpawnDef("acme"))
	fr := &storeRunner{st: st}
	rec := newScopeReceiver(st, fr, nil)
	h := triageServer(t, rec, triageTokens)

	body := []byte(`{"goal":"g"}`)
	sig := http.Header{}
	sig.Set("X-Hub-Signature-256", githubSig(scopeSecret, body))

	w := triageDo(h, http.MethodPost, "/v1/_webhooks/w/test?tenant=acme", triageAdminBearer, body, sig)
	if w.Code != http.StatusOK {
		t.Fatalf("admin focusing acme: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp testResult
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.WouldAccept || resp.Verdict != verdictAccepted {
		t.Errorf("would_accept=%v verdict=%q, want true/%s", resp.WouldAccept, resp.Verdict, verdictAccepted)
	}
	if n := fr.callCount(); n != 0 {
		t.Errorf("dry-run invoked the runner %d times, want 0", n)
	}

	// Without the focus the admin's own tenant (ops) has no such webhook, and
	// neither do the static or shared tiers, so the 200 above is the focus's
	// doing.
	if w := triageDo(h, http.MethodPost, "/v1/_webhooks/w/test", triageAdminBearer, body, sig); w.Code != http.StatusNotFound {
		t.Errorf("admin without ?tenant: status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// A def a snapshot restored without its credentials refuses every delivery,
// so the dry-run must not report that one would be accepted.
func TestReceiver_Test_CaptureDisabledDefIs404(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"go"}`)
	wh := config.Webhook{
		Enabled: true, Delivery: "spawn", Agent: "x",
		Auth:            config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		CaptureDisabled: &config.WebhookCaptureDisabled{StrippedCredentials: []string{"jobs"}},
	}
	sig := http.Header{}
	sig.Set("X-Hub-Signature-256", githubSig(secret, body))
	newRec := func(wh config.Webhook) *Receiver {
		return newTestReceiver(t, map[string]config.Webhook{"gh": wh}, &fakeRunner{runID: "r", agentID: "a"}, nil,
			map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)
	}

	h := triageServer(t, newRec(wh), triageTokens)
	if w := triageDo(h, http.MethodPost, "/v1/_webhooks/gh/test", triageAdminBearer, body, sig); w.Code != http.StatusNotFound {
		t.Fatalf("capture-disabled def: status = %d, want 404; body=%s", w.Code, w.Body.String())
	}

	// The same def without the marker dry-runs fine, so the 404 above is the
	// marker's doing.
	wh.CaptureDisabled = nil
	h = triageServer(t, newRec(wh), triageTokens)
	if w := triageDo(h, http.MethodPost, "/v1/_webhooks/gh/test", triageAdminBearer, body, sig); w.Code != http.StatusOK {
		t.Fatalf("unmarked control: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}
