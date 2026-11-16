package config

import (
	"runtime"
	"strings"
	"testing"
)

func loadConversion(t *testing.T, block string) (*Config, error) {
	t.Helper()
	return LoadLayers(Layer{Name: "op", Data: []byte(transcriptionBase + block)})
}

// Absent, conversion runs in the platform's default mode at the documented
// defaults.
func TestConversionConfig_AbsentIsThePlatformDefault(t *testing.T) {
	cfg, err := loadConversion(t, "")
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Conversion.Resolved()
	want := ConversionConfig{Mode: DefaultConversionMode(runtime.GOOS), QueueWaitS: 60, QuarantineTTLS: 3600,
		Local: ConversionLocalConfig{Workers: 2, MemoryLimitMB: 1024}}
	if got != want {
		t.Fatalf("resolved = %+v, want %+v", got, want)
	}
}

func TestConversionConfig_LocalKeepsWhatIsWritten(t *testing.T) {
	cfg, err := loadConversion(t, "conversion:\n  mode: local\n  queue_wait_s: 5\n  local:\n    workers: 4\n    memory_limit_mb: 512\n")
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Conversion.Resolved()
	if got.Mode != ConversionLocal || got.QueueWaitS != 5 || got.Local.Workers != 4 || got.Local.MemoryLimitMB != 512 || got.QuarantineTTLS != 3600 {
		t.Fatalf("resolved = %+v", got)
	}
}

// Modes a later phase adds, an unknown mode, values out of range and a
// mistyped key are refused at load rather than read as the default.
func TestConversionConfig_RefusesWhatItCannotDo(t *testing.T) {
	for _, c := range []struct{ block, want string }{
		{"conversion:\n  mode: remote\n", "needs the host:port"},
		{"conversion:\n  mode: remote\n  listen: nowhere\n", "not a host:port"},
		{"conversion:\n  mode: local\n  listen: 127.0.0.1:7788\n", "set only in mode"},
		{"conversion:\n  mode: local+remote\n  listen: 0.0.0.0:8787\n", "never a public port"},
		{"conversion:\n  mode: remote\n  listen: 0.0.0.0:7788\n", "set conversion.tls"},
		{"conversion:\n  mode: remote\n  listen: 10.0.0.5:7788\n  tls:\n    cert_file: /c/e.crt\n", "set together"},
		{"conversion:\n  mode: remote\n  listen: 127.0.0.1:7788\n  tls:\n    client_ca_file: /c/ca.crt\n", "needs cert_file"},
		{"conversion:\n  mode: local\n  insecure: true\n", "set only in mode"},
		{"conversion:\n  mode: remote\n  listen: 127.0.0.1:7788\n  tls:\n    cert: x\n", `unknown key "cert"`},
		{"conversion:\n  mode: sideways\n", "unknown mode"},
		{"conversion:\n  mode: local\n  local:\n    workers: 65\n", "outside 1..64"},
		{"conversion:\n  mode: local\n  local:\n    memory_limit_mb: 64\n", "under the 128 MiB"},
		{"conversion:\n  queue_wait_s: -1\n", "may not be negative"},
		{"conversion:\n  mod: local\n", `unknown key "mod"`},
		{"conversion:\n  local:\n    worker: 3\n", `unknown key "worker"`},
	} {
		if _, err := loadConversion(t, c.block); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want one saying %q", c.block, err, c.want)
		}
	}
}

// A remote mode keeps the worker endpoint it is given.
func TestConversionConfig_RemoteKeepsItsEndpoint(t *testing.T) {
	cfg, err := loadConversion(t, "conversion:\n  mode: local+remote\n  listen: 0.0.0.0:7788\n  tls:\n    cert_file: /c/e.crt\n    key_file: /c/e.key\n")
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Conversion.Resolved()
	if got.Listen != "0.0.0.0:7788" || !got.LocalWorkers() || !got.RemoteWorkers() || got.TLS.CertFile != "/c/e.crt" {
		t.Fatalf("resolved = %+v", got)
	}
}

// Plaintext is accepted on the loopback, and off it only when the operator
// says the network is private.
func TestConversionConfig_PlaintextNeedsLoopbackOrAnInsecureFlag(t *testing.T) {
	for _, block := range []string{
		"conversion:\n  mode: remote\n  listen: 127.0.0.1:7788\n",
		"conversion:\n  mode: remote\n  listen: 0.0.0.0:7788\n  insecure: true\n",
	} {
		if _, err := loadConversion(t, block); err != nil {
			t.Errorf("%q: %v", block, err)
		}
	}
}

// Linux starts children by default (RFC GO D2); every other platform keeps
// converting in process.
func TestDefaultConversionMode_IsLocalOnLinuxOnly(t *testing.T) {
	for goos, want := range map[string]string{"linux": ConversionLocal, "darwin": ConversionInProcess, "windows": ConversionInProcess, "freebsd": ConversionInProcess} {
		if got := DefaultConversionMode(goos); got != want {
			t.Errorf("%s: %s, want %s", goos, got, want)
		}
	}
}
