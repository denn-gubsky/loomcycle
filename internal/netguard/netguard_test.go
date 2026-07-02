package netguard

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsPrivateIP(t *testing.T) {
	private := []string{"127.0.0.1", "::1", "10.0.0.5", "192.168.1.1", "172.16.0.1", "169.254.169.254", "0.0.0.0"}
	for _, s := range private {
		if !IsPrivateIP(net.ParseIP(s)) {
			t.Errorf("IsPrivateIP(%s) = false, want true", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1"} {
		if IsPrivateIP(net.ParseIP(s)) {
			t.Errorf("IsPrivateIP(%s) = true, want false", s)
		}
	}
	if !IsPrivateIP(nil) {
		t.Error("IsPrivateIP(nil) = false, want true (fail-closed)")
	}
}

// TestNewGuardedClient_BlocksLoopback: the always-block client refuses a
// loopback (private) target unless the host is on the allowlist.
func TestNewGuardedClient_BlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	if resp, err := NewGuardedClient(5*time.Second, nil).Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("guarded client reached loopback %s — guard not applied", srv.URL)
	}
	host, _, _ := net.SplitHostPort(srv.Listener.Addr().String())
	resp, err := NewGuardedClient(5*time.Second, []string{host}).Get(srv.URL)
	if err != nil {
		t.Fatalf("allowlisted host should dial through: %v", err)
	}
	_ = resp.Body.Close()
}
