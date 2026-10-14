package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestGitSourceConfigFromEnv_OffUnlessHostsAreListed(t *testing.T) {
	for _, env := range []map[string]string{
		{},
		{"LOOMCYCLE_GIT_SOURCE_HOSTS": "  , "},
		// The other settings alone do not turn it on.
		{"LOOMCYCLE_GIT_SOURCE_MAX_BYTES": "1000"},
	} {
		cfg, err := gitSourceConfigFromEnv(envOf(env), nil)
		if cfg != nil || err != nil {
			t.Errorf("%v: cfg=%v err=%v, want the feature off", env, cfg, err)
		}
	}
}

func TestGitSourceConfigFromEnv_DefaultsAndOverrides(t *testing.T) {
	cfg, err := gitSourceConfigFromEnv(envOf(map[string]string{"LOOMCYCLE_GIT_SOURCE_HOSTS": "GitHub.com, gitea.example:3000"}), nil)
	if err != nil || cfg == nil {
		t.Fatalf("cfg=%v err=%v", cfg, err)
	}
	if got := strings.Join(cfg.Hosts, "|"); got != "github.com|gitea.example:3000" {
		t.Errorf("hosts = %q", got)
	}
	if cfg.MaxBytes != 256<<20 || cfg.MaxObjectBytes != 64<<20 || cfg.MaxFileBytes != 5<<20 ||
		cfg.MaxFiles != 100000 || cfg.Timeout != 2*time.Minute || cap(cfg.Slots) != 1 || cfg.MaxHistory != 500 {
		t.Errorf("defaults = %+v (slots %d)", cfg, cap(cfg.Slots))
	}
	if cfg.Transport == nil {
		t.Error("no transport: a clone would have nothing that enforces the network rules")
	}

	cfg, err = gitSourceConfigFromEnv(envOf(map[string]string{
		"LOOMCYCLE_GIT_SOURCE_HOSTS":            "github.com",
		"LOOMCYCLE_GIT_SOURCE_MAX_BYTES":        "1000",
		"LOOMCYCLE_GIT_SOURCE_MAX_OBJECT_BYTES": "2000",
		"LOOMCYCLE_GIT_SOURCE_MAX_FILE_BYTES":   "300",
		"LOOMCYCLE_GIT_SOURCE_MAX_FILES":        "40",
		"LOOMCYCLE_GIT_SOURCE_TIMEOUT_SECONDS":  "5",
		"LOOMCYCLE_GIT_SOURCE_MAX_CONCURRENT":   "3",
		"LOOMCYCLE_GIT_SOURCE_MAX_HISTORY":      "50",
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxBytes != 1000 || cfg.MaxObjectBytes != 2000 || cfg.MaxFileBytes != 300 || cfg.MaxFiles != 40 ||
		cfg.Timeout != 5*time.Second || cap(cfg.Slots) != 3 || cfg.MaxHistory != 50 {
		t.Errorf("overrides = %+v (slots %d)", cfg, cap(cfg.Slots))
	}
}

// A setting that is not a positive number, or a host written as a URL, stops
// the start: an operator who wrote a limit meant one.
func TestGitSourceConfigFromEnv_RefusesWhatItCannotRead(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"a limit that is not a number": {"LOOMCYCLE_GIT_SOURCE_HOSTS": "github.com", "LOOMCYCLE_GIT_SOURCE_MAX_BYTES": "256MiB"},
		"a zero limit":                 {"LOOMCYCLE_GIT_SOURCE_HOSTS": "github.com", "LOOMCYCLE_GIT_SOURCE_MAX_OBJECT_BYTES": "0"},
		"a negative count":             {"LOOMCYCLE_GIT_SOURCE_HOSTS": "github.com", "LOOMCYCLE_GIT_SOURCE_MAX_CONCURRENT": "-1"},
		"a URL for a host":             {"LOOMCYCLE_GIT_SOURCE_HOSTS": "https://github.com"},
		"a path for a host":            {"LOOMCYCLE_GIT_SOURCE_HOSTS": "github.com/acme"},
	} {
		if cfg, err := gitSourceConfigFromEnv(envOf(env), nil); err == nil {
			t.Errorf("%s: accepted as %+v", name, cfg)
		}
	}
}

// The transport a clone uses dials through the network guard: a host on a
// loopback address is refused before any connection, so the server sees no
// request, unless the operator vouched for it in the private host allowlist.
// The two cases differ in that list alone. Every other git source test hands
// in its own transport, so this is the one place the guard on the clone path
// is exercised.
func TestGitSourceConfigFromEnv_TheTransportRefusesAPrivateAddress(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()
	env := envOf(map[string]string{"LOOMCYCLE_GIT_SOURCE_HOSTS": strings.TrimPrefix(srv.URL, "https://")})
	get := func(allowlist []string) error {
		t.Helper()
		cfg, err := gitSourceConfigFromEnv(env, allowlist)
		if err != nil || cfg == nil {
			t.Fatalf("cfg=%v err=%v", cfg, err)
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
