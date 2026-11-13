package config

import (
	"strings"
	"testing"
)

func loadConversion(t *testing.T, block string) (*Config, error) {
	t.Helper()
	return LoadLayers(Layer{Name: "op", Data: []byte(transcriptionBase + block)})
}

// Absent, conversion runs in process at the documented defaults: the state
// every existing deployment is in.
func TestConversionConfig_AbsentIsInProcessWithDefaults(t *testing.T) {
	cfg, err := loadConversion(t, "")
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Conversion.Resolved()
	want := ConversionConfig{Mode: ConversionInProcess, QueueWaitS: 60, QuarantineTTLS: 3600,
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
	cfg, err := loadConversion(t, "conversion:\n  mode: local+remote\n  listen: 0.0.0.0:7788\n")
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Conversion.Resolved()
	if got.Listen != "0.0.0.0:7788" || !got.LocalWorkers() || !got.RemoteWorkers() {
		t.Fatalf("resolved = %+v", got)
	}
}
