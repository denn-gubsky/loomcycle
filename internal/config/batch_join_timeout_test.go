package config

import (
	"strings"
	"testing"
	"time"
)

// A batch join waits an hour for its children unless the operator says
// otherwise: a Go duration, 0 for no server bound. A value that is not one is
// refused at load rather than read as the default.
func TestBatchJoinTimeout_Env(t *testing.T) {
	for _, c := range []struct {
		env  string
		want time.Duration
	}{
		{"", time.Hour},
		{"30m", 30 * time.Minute},
		{"0", 0},
	} {
		t.Setenv("LOOMCYCLE_BATCH_JOIN_TIMEOUT", c.env)
		cfg, err := loadYAMLString(t, "channels: {}\n")
		if err != nil {
			t.Fatalf("%q: %v", c.env, err)
		}
		if got := cfg.Env.BatchJoinTimeout; got != c.want {
			t.Errorf("%q: BatchJoinTimeout = %v, want %v", c.env, got, c.want)
		}
	}
	for _, bad := range []string{"60", "an hour", "-5m"} {
		t.Setenv("LOOMCYCLE_BATCH_JOIN_TIMEOUT", bad)
		if _, err := loadYAMLString(t, "channels: {}\n"); err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_BATCH_JOIN_TIMEOUT") {
			t.Errorf("%q: err = %v, want a refusal naming the knob", bad, err)
		}
	}
}
