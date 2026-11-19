package config

import (
	"strings"
	"testing"
)

// The worker endpoint is read from worker_endpoint, from the older
// conversion keys, or from both when they agree.
func TestWorkerEndpoint_ResolvesFromEitherBlock(t *testing.T) {
	tlsNew := "worker_endpoint:\n  listen: 10.0.0.5:7788\n  tls:\n    cert_file: /c/e.crt\n    key_file: /c/e.key\n    client_ca_file: /c/ca.crt\n"
	tlsOld := "conversion:\n  mode: remote\n  listen: 10.0.0.5:7788\n  tls:\n    cert_file: /c/e.crt\n    key_file: /c/e.key\n    client_ca_file: /c/ca.crt\n"
	want := WorkerEndpointConfig{Listen: "10.0.0.5:7788", TLS: WorkerEndpointTLSConfig{CertFile: "/c/e.crt", KeyFile: "/c/e.key", ClientCAFile: "/c/ca.crt"}}
	for name, block := range map[string]string{
		"new only":   "conversion:\n  mode: remote\n" + tlsNew,
		"old only":   tlsOld,
		"both equal": tlsOld + tlsNew,
		// Each value from whichever block writes it.
		"split": "conversion:\n  mode: remote\n  tls:\n    client_ca_file: /c/ca.crt\n" +
			"worker_endpoint:\n  listen: 10.0.0.5:7788\n  tls:\n    cert_file: /c/e.crt\n    key_file: /c/e.key\n",
	} {
		cfg, err := loadConversion(t, block)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := cfg.ResolvedWorkerEndpoint(); got != want {
			t.Errorf("%s: endpoint = %+v, want %+v", name, got, want)
		}
	}
}

// A value both blocks set differently stops the load, and the message names
// both keys.
func TestWorkerEndpoint_BothBlocksDifferingIsRefused(t *testing.T) {
	for _, c := range []struct{ block, keys string }{
		{"conversion:\n  mode: remote\n  listen: 127.0.0.1:7788\nworker_endpoint:\n  listen: 127.0.0.1:7789\n", "worker_endpoint.listen (\"127.0.0.1:7789\") and conversion.listen"},
		{"conversion:\n  mode: remote\n  listen: 10.0.0.5:7788\n  tls:\n    cert_file: /a.crt\n    key_file: /e.key\n" +
			"worker_endpoint:\n  tls:\n    cert_file: /b.crt\n", "worker_endpoint.tls.cert_file (\"/b.crt\") and conversion.tls.cert_file"},
		{"conversion:\n  mode: remote\n  listen: 10.0.0.5:7788\n  tls:\n    cert_file: /e.crt\n    key_file: /e.key\n    client_ca_file: /a.crt\n" +
			"worker_endpoint:\n  tls:\n    client_ca_file: /b.crt\n", "worker_endpoint.tls.client_ca_file (\"/b.crt\") and conversion.tls.client_ca_file"},
	} {
		_, err := loadConversion(t, c.block)
		if err == nil || !strings.Contains(err.Error(), c.keys) || !strings.Contains(err.Error(), "differ") {
			t.Errorf("%q: err = %v, want one naming %s", c.block, err, c.keys)
		}
	}
}

// The endpoint's checks apply to the resolved endpoint, whichever block it
// came from, and the message names the block the operator wrote.
func TestWorkerEndpoint_RefusesWhatItCannotServe(t *testing.T) {
	for _, c := range []struct{ block, want string }{
		{"conversion:\n  mode: remote\nworker_endpoint:\n  listen: 0.0.0.0:7788\n", "set worker_endpoint.tls"},
		{"conversion:\n  mode: remote\n  listen: 0.0.0.0:7788\n", "set conversion.tls"},
		{"conversion:\n  mode: remote\nworker_endpoint:\n  listen: nowhere\n", "worker_endpoint.listen: \"nowhere\" is not a host:port"},
		{"conversion:\n  mode: remote\nworker_endpoint:\n  listen: 0.0.0.0:8787\n", "never a public port"},
		{"conversion:\n  mode: remote\nworker_endpoint:\n  listen: 10.0.0.5:7788\n  tls:\n    key_file: /e.key\n", "worker_endpoint.tls: cert_file and key_file"},
		{"conversion:\n  mode: remote\nworker_endpoint:\n  listen: 127.0.0.1:7788\n  tls:\n    client_ca_file: /ca.crt\n", "worker_endpoint.tls.client_ca_file needs"},
		{"conversion:\n  mode: remote\nworker_endpoint:\n  tls:\n    cert_file: /e.crt\n    key_file: /e.key\n", "needs the host:port"},
		{"worker_endpoint:\n  listen: 127.0.0.1:7788\n  tsl: {}\n", `unknown key "tsl"`},
		{"worker_endpoint:\n  listen: 127.0.0.1:7788\n  tls:\n    cert: /e.crt\n", `unknown key "cert"`},
		// Unused in this mode, but still checked.
		{"conversion:\n  mode: inprocess\nworker_endpoint:\n  listen: 0.0.0.0:7788\n", "set worker_endpoint.tls"},
		{"conversion:\n  mode: inprocess\nworker_endpoint:\n  insecure: true\n", "host:port workers attach to is required"},
	} {
		if _, err := loadConversion(t, c.block); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want one saying %q", c.block, err, c.want)
		}
	}
}

// Off the loopback, worker_endpoint.insecure (or the older
// conversion.insecure) lets the endpoint serve in the clear.
func TestWorkerEndpoint_InsecureFromEitherBlockAllowsPlaintextOffTheLoopback(t *testing.T) {
	for _, block := range []string{
		"conversion:\n  mode: remote\nworker_endpoint:\n  listen: 0.0.0.0:7788\n  insecure: true\n",
		"conversion:\n  mode: remote\n  insecure: true\nworker_endpoint:\n  listen: 0.0.0.0:7788\n",
	} {
		cfg, err := loadConversion(t, block)
		if err != nil {
			t.Errorf("%q: %v", block, err)
			continue
		}
		if !cfg.ResolvedWorkerEndpoint().Insecure {
			t.Errorf("%q: insecure not resolved", block)
		}
	}
}

// worker_endpoint in a mode that attaches no workers of its own loads, with
// a warning that nothing listens on it.
func TestWorkerEndpoint_UnusedEndpointLoadsWithAWarning(t *testing.T) {
	cfg, err := loadConversion(t, "conversion:\n  mode: inprocess\nworker_endpoint:\n  listen: 127.0.0.1:7788\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "worker_endpoint is set") && strings.Contains(w, "127.0.0.1:7788") {
			return
		}
	}
	t.Fatalf("no warning about the unused endpoint in %q", cfg.Warnings)
}
