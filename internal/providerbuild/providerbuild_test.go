package providerbuild

import (
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
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

// The stream timeouts come from the provider's options and nowhere else: the
// defaults are 60s/90s, 10 minutes each for the local Ollama registration, and
// an option that is not a positive number leaves the default (it must never
// install a zero, i.e. immediate-timeout, client).
func TestDriverOptions_StreamTimeoutsComeFromOptions(t *testing.T) {
	for _, tc := range []struct {
		name         string
		id           string
		options      map[string]any
		header, idle time.Duration
	}{
		{"default", "vllm-local", nil, 60 * time.Second, 90 * time.Second},
		{"set", "vllm-local", map[string]any{"header_timeout_ms": 5000, "idle_timeout_ms": 30000}, 5 * time.Second, 30 * time.Second},
		{"one set", "openai", map[string]any{"idle_timeout_ms": 240000}, 60 * time.Second, 240 * time.Second},
		{"invalid", "vllm-local", map[string]any{"header_timeout_ms": "5000", "idle_timeout_ms": -1}, 60 * time.Second, 90 * time.Second},
		{"local ollama default", "ollama-local", nil, 600 * time.Second, 600 * time.Second},
		{"local ollama set", "ollama-local", map[string]any{"header_timeout_ms": 900000, "idle_timeout_ms": 450000}, 900 * time.Second, 450 * time.Second},
		{"local ollama invalid", "ollama-local", map[string]any{"header_timeout_ms": 0}, 600 * time.Second, 600 * time.Second},
		{"hosted ollama", "ollama", nil, 60 * time.Second, 90 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := DriverOptions(tc.id, config.ProviderConfig{Driver: "x", Options: tc.options}, &config.Config{})
			if o.StreamOpts.HeaderTimeout != tc.header || o.StreamOpts.IdleTimeout != tc.idle {
				t.Errorf("StreamOpts = %+v, want %v / %v", o.StreamOpts, tc.header, tc.idle)
			}
		})
	}
	// The defaults config judges a retired variable against are the ones
	// applied here and in streamhttp.
	if config.ProviderHeaderTimeoutMS*time.Millisecond != streamhttp.DefaultHeaderTimeout ||
		config.ProviderIdleTimeoutMS*time.Millisecond != streamhttp.DefaultIdleTimeout {
		t.Error("config's provider timeout defaults differ from streamhttp's")
	}
}

// The Ollama options reach the driver from the yaml. The local context window
// is the one option the environment also sets, and there the environment wins.
func TestDriverOptions_OllamaOptionsComeFromTheYAML(t *testing.T) {
	cfg := &config.Config{}
	if o := DriverOptions("ollama", config.ProviderConfig{Driver: "ollama"}, cfg); len(o.Options) != 0 {
		t.Errorf("hosted ollama with no options carries %v", o.Options)
	}
	if o := DriverOptions("ollama-local", config.ProviderConfig{Driver: "ollama"}, cfg); len(o.Options) != 0 {
		t.Errorf("local ollama with no options carries %v", o.Options)
	}
	o := DriverOptions("ollama", config.ProviderConfig{Driver: "ollama", Options: map[string]any{"num_ctx": 8192}}, cfg)
	if o.Options["num_ctx"] != 8192 {
		t.Errorf("hosted ollama num_ctx = %v, want 8192", o.Options["num_ctx"])
	}
	cfg.Env.OllamaLocalNumCtx = 32768
	o = DriverOptions("ollama-local", config.ProviderConfig{Driver: "ollama", Options: map[string]any{"num_gpu": 99}}, cfg)
	if o.Options["num_ctx"] != 32768 || o.Options["num_gpu"] != 99 {
		t.Errorf("local ollama options = %v, want num_ctx 32768 (environment) and num_gpu 99 (yaml)", o.Options)
	}
	local := config.ProviderConfig{Driver: "ollama", Options: map[string]any{"num_ctx": 65536}}
	if o = DriverOptions("ollama-local", local, cfg); o.Options["num_ctx"] != 32768 {
		t.Errorf("local ollama num_ctx = %v, want the environment's 32768 over the yaml's", o.Options["num_ctx"])
	}
	cfg.Env.OllamaLocalNumCtx = 0
	if o = DriverOptions("ollama-local", local, cfg); o.Options["num_ctx"] != 65536 {
		t.Errorf("local ollama num_ctx = %v, want the yaml's 65536 with the variable unset", o.Options["num_ctx"])
	}
	// The variable sizes the local registration only.
	cfg.Env.OllamaLocalNumCtx = 32768
	if o = DriverOptions("ollama", local, cfg); o.Options["num_ctx"] != 65536 {
		t.Errorf("hosted ollama num_ctx = %v, want its own yaml value", o.Options["num_ctx"])
	}
}

// The code-js budgets are options only. Unset, none is passed, so the driver
// applies its own defaults (deterministic off, 120s, 24h).
func TestDriverOptions_CodeJSBudgetsComeFromOptions(t *testing.T) {
	cfg := &config.Config{}
	cfg.Env.CodeAgentsRoot = "/srv/agent_code"
	o := DriverOptions("code-js", config.ProviderConfig{Driver: "code-js"}, cfg)
	if len(o.Options) != 1 || o.Options["code_root"] != "/srv/agent_code" {
		t.Errorf("options = %v, want only code_root", o.Options)
	}
	o = DriverOptions("code-js", config.ProviderConfig{Driver: "code-js", Options: map[string]any{
		"deterministic": true, "run_timeout_seconds": 45, "max_wall_seconds": 3600,
	}}, cfg)
	if o.Options["deterministic"] != true || o.Options["run_timeout_seconds"] != 45 || o.Options["max_wall_seconds"] != 3600 {
		t.Errorf("options = %v, want the yaml's three", o.Options)
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
