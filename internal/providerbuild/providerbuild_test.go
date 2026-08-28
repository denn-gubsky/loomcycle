package providerbuild

import (
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestDriverOptions_TimeoutOptionsFoldIntoStreamOpts is the RFC CK regression:
// `options.header_timeout_ms` / `idle_timeout_ms` on a `providers:` entry are
// consumed into StreamOpts (so a slow local backend's cold-load window is
// tunable in YAML instead of env-only) and are REMOVED from the driver's Options
// map (so the driver's WarnUnknownOptions doesn't flag them).
func TestDriverOptions_TimeoutOptionsFoldIntoStreamOpts(t *testing.T) {
	cfg := &config.Config{}
	pc := config.ProviderConfig{
		Driver:  "vllm",
		BaseURL: "http://box:8000/v1",
		Options: map[string]any{
			"header_timeout_ms": 120000,
			"idle_timeout_ms":   300000,
			"keep_me":           "yes", // an unrelated option must survive
		},
	}
	o := DriverOptions("vllm-local", pc, cfg)

	if o.StreamOpts.HeaderTimeout != 120*time.Second {
		t.Errorf("HeaderTimeout = %v, want 120s", o.StreamOpts.HeaderTimeout)
	}
	if o.StreamOpts.IdleTimeout != 300*time.Second {
		t.Errorf("IdleTimeout = %v, want 300s", o.StreamOpts.IdleTimeout)
	}
	if _, ok := o.Options["header_timeout_ms"]; ok {
		t.Error("header_timeout_ms must be consumed (removed) from the driver Options map")
	}
	if _, ok := o.Options["idle_timeout_ms"]; ok {
		t.Error("idle_timeout_ms must be consumed (removed) from the driver Options map")
	}
	if o.Options["keep_me"] != "yes" {
		t.Error("unrelated options must survive the timeout extraction")
	}
}

// TestDriverOptions_NoTimeoutOptionsKeepsEnvDefault confirms the fallback: with
// no timeout options the env/global StreamOpts stand (a zero/absent entry must
// never install a zero — i.e. immediate-timeout — client).
func TestDriverOptions_NoTimeoutOptionsKeepsEnvDefault(t *testing.T) {
	cfg := &config.Config{}
	cfg.Env.ProviderHeaderTimeout = 45 * time.Second
	cfg.Env.ProviderIdleTimeout = 90 * time.Second
	o := DriverOptions("vllm-local", config.ProviderConfig{Driver: "vllm", BaseURL: "http://box:8000/v1"}, cfg)
	if o.StreamOpts.HeaderTimeout != 45*time.Second || o.StreamOpts.IdleTimeout != 90*time.Second {
		t.Errorf("StreamOpts = %+v, want env defaults (45s/90s)", o.StreamOpts)
	}
}

func TestOptionMillis(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want time.Duration
		ok   bool
	}{
		{"int", 1500, 1500 * time.Millisecond, true},
		{"int64", int64(2000), 2000 * time.Millisecond, true},
		{"float64", float64(2500), 2500 * time.Millisecond, true},
		{"zero", 0, 0, false},
		{"negative", -5, 0, false},
		{"non-numeric", "600000", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := optionMillis(map[string]any{"k": tc.val}, "k")
			if ok != tc.ok || got != tc.want {
				t.Errorf("optionMillis(%v) = (%v, %v), want (%v, %v)", tc.val, got, ok, tc.want, tc.ok)
			}
		})
	}
	if _, ok := optionMillis(map[string]any{}, "missing"); ok {
		t.Error("optionMillis on a missing key must return ok=false")
	}
}
