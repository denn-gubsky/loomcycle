package builtin

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestNewSSRFGuardedClient_BlocksLoopback is the regression for the mem9 SSRF
// hole: the mem9 backend used a plain http.Client with no dial-time guard, so a
// model-authored base_url could reach a private/loopback/metadata address (and
// exfiltrate the allowlisted X-API-Key). The guarded client must refuse a
// private (loopback) target unless the operator allowlists the host.
func TestNewSSRFGuardedClient_BlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// No allowlist → the loopback (private) address is refused at dial time.
	blocked := newSSRFGuardedClient(5*time.Second, nil)
	if resp, err := blocked.Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("guarded client reached a loopback address %s — SSRF guard not applied", srv.URL)
	}

	// The operator's private-host allowlist exempts the host → the dial proceeds.
	host := mustHost(t, srv.URL)
	allowed := newSSRFGuardedClient(5*time.Second, []string{host})
	resp, err := allowed.Get(srv.URL)
	if err != nil {
		t.Fatalf("allowlisted host should dial through: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
