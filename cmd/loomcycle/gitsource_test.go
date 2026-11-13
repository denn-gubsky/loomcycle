package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The hosts are read and checked by config load (Env.GitSourceHosts); with
// none the feature is off.
func TestGitSourceConfig_OffUnlessHostsAreListed(t *testing.T) {
	if cfg := gitSourceConfig(nil, nil); cfg != nil {
		t.Errorf("cfg=%v, want the feature off", cfg)
	}
}

func TestGitSourceConfig_HostsAndFixedLimits(t *testing.T) {
	cfg := gitSourceConfig([]string{"github.com", "gitea.example:3000"}, nil)
	if cfg == nil {
		t.Fatal("no config for a host list")
	}
	if got := strings.Join(cfg.Hosts, "|"); got != "github.com|gitea.example:3000" {
		t.Errorf("hosts = %q", got)
	}
	if cfg.MaxBytes != 256<<20 || cfg.MaxObjectBytes != 64<<20 || cfg.MaxFileBytes != 5<<20 ||
		cfg.MaxFiles != 100000 || cfg.Timeout != 2*time.Minute || cap(cfg.Slots) != 1 || cfg.MaxHistory != 500 {
		t.Errorf("limits = %+v (slots %d)", cfg, cap(cfg.Slots))
	}
	if cfg.Transport == nil {
		t.Error("no transport: a clone would have nothing that enforces the network rules")
	}
}

// The transport a clone uses dials through the network guard: a host on a
// loopback address is refused before any connection, so the server sees no
// request, unless the operator vouched for it in the private host allowlist.
// The two cases differ in that list alone. Every other git source test hands
// in its own transport, so this is the one place the guard on the clone path
// is exercised.
func TestGitSourceConfig_TheTransportRefusesAPrivateAddress(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()
	hosts := []string{strings.TrimPrefix(srv.URL, "https://")}
	get := func(allowlist []string) error {
		t.Helper()
		cfg := gitSourceConfig(hosts, allowlist)
		if cfg == nil {
			t.Fatal("no config for a host list")
		}
		tr, ok := cfg.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("transport is %T", cfg.Transport)
		}
		// Trust the test server's certificate, and nothing else changes.
		tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/acme/app.git/info/refs", nil)
		resp, err := tr.RoundTrip(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}

	if err := get(nil); err == nil || hits.Load() != 0 {
		t.Errorf("a loopback host: err=%v, %d request(s) reached it; want refused before any", err, hits.Load())
	}
	if err := get([]string{"127.0.0.1"}); err != nil || hits.Load() != 1 {
		t.Errorf("a loopback host the operator vouched for: err=%v, %d request(s); want one", err, hits.Load())
	}
}
