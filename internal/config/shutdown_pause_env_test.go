package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The shutdown pause's bound defaults to DefaultShutdownPauseTimeoutMs, and an
// EXPLICIT 0 is a real value — it turns the pause off — not unset. A value that
// is not a non-negative integer keeps the default.
func TestShutdownPauseTimeout_DefaultsAndExplicitZero(t *testing.T) {
	tmp := t.TempDir()
	yamlPath := filepath.Join(tmp, "c.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  default: { model: claude-sonnet-4-6 }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		env  string
		set  bool
		want int64
	}{
		{set: false, want: DefaultShutdownPauseTimeoutMs},
		{env: "0", set: true, want: 0},
		{env: "25000", set: true, want: 25000},
		{env: "-1", set: true, want: DefaultShutdownPauseTimeoutMs},
		{env: "8s", set: true, want: DefaultShutdownPauseTimeoutMs},
	} {
		if tc.set {
			t.Setenv("LOOMCYCLE_SHUTDOWN_PAUSE_TIMEOUT_MS", tc.env)
		} else {
			t.Setenv("LOOMCYCLE_SHUTDOWN_PAUSE_TIMEOUT_MS", "")
			os.Unsetenv("LOOMCYCLE_SHUTDOWN_PAUSE_TIMEOUT_MS")
		}
		cfg, err := Load(yamlPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Env.ShutdownPauseTimeoutMs; got != tc.want {
			t.Errorf("LOOMCYCLE_SHUTDOWN_PAUSE_TIMEOUT_MS=%q (set=%v): got %d, want %d", tc.env, tc.set, got, tc.want)
		}
	}
}
