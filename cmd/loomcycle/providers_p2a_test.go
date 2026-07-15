package main

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/cmd/loomcycle/embedded"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// loadDefaultProvidersOnly loads the embedded default-providers layer as the ONLY
// config layer — i.e. an operator config with NO `providers:` block, which is
// exactly what main() assembles (the layer is prepended unconditionally). This is
// the RFC BF P2a behavior-identity substrate: the built-ins come only from the
// embedded layer, so what newProviderResolver builds here must equal what the
// pre-P2a hardcoded resolver built.
func loadDefaultProvidersOnly(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadLayers(config.Layer{Name: "providers.default", Data: embedded.DefaultProviders()})
	if err != nil {
		t.Fatalf("LoadLayers(providers.default): %v", err)
	}
	return cfg
}

// TestNewProviderResolver_NoProvidersBlock_BackCompat is the RFC BF P2a
// behaviour-identity guarantee: with NO `providers:` block and only
// ANTHROPIC_API_KEY set, the config-driven resolver builds the anthropic provider
// (ID()=="anthropic") and returns the SAME "not configured" errors for the
// unkeyed built-ins that the pre-P2a hardcoded resolver did. The expected error
// strings are the pre-P2a literals (captured from the old Get() switch), so this
// asserts against the pre-flip behaviour, not just self-consistency.
func TestNewProviderResolver_NoProvidersBlock_BackCompat(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-not-a-real-key")
	for _, k := range []string{"OPENAI_API_KEY", "DEEPSEEK_API_KEY", "GEMINI_API_KEY", "OLLAMA_API_KEY"} {
		t.Setenv(k, "")
	}
	// Turn off the non-keyed built-ins so only anthropic is enabled — the exact
	// pre-P2a "only ANTHROPIC_API_KEY set" resolver shape.
	t.Setenv("OLLAMA_BASE_URL", "disabled")
	t.Setenv("LOOMCYCLE_MOCK_ENABLED", "")
	t.Setenv("LOOMCYCLE_CODE_AGENTS_ENABLED", "")

	cfg := loadDefaultProvidersOnly(t)
	pr, err := newProviderResolver(cfg)
	if err != nil {
		t.Fatalf("newProviderResolver: %v", err)
	}

	ap, err := pr.Get("anthropic")
	if err != nil {
		t.Fatalf("Get(anthropic): %v", err)
	}
	if ap.ID() != "anthropic" {
		t.Errorf("anthropic ID() = %q, want %q", ap.ID(), "anthropic")
	}

	// Byte-identical pre-P2a "not configured" errors for the unkeyed built-ins.
	wantErr := map[string]string{
		"openai":   "openai provider not configured (set OPENAI_API_KEY)",
		"deepseek": "deepseek provider not configured (set DEEPSEEK_API_KEY)",
		"gemini":   "gemini provider not configured (set GEMINI_API_KEY)",
		"ollama":   "ollama provider not configured (set OLLAMA_API_KEY for hosted ollama.com; use provider id \"ollama-local\" for a local-network Ollama)",
	}
	for id, want := range wantErr {
		_, err := pr.Get(id)
		if err == nil || err.Error() != want {
			t.Errorf("Get(%s) error = %v, want %q", id, err, want)
		}
	}

	// An id the config never declares is "unknown provider", not "not configured".
	if _, err := pr.Get("does-not-exist"); err == nil || err.Error() != `unknown provider "does-not-exist"` {
		t.Errorf("Get(unknown) error = %v, want unknown-provider", err)
	}
}

// TestResolveProbe_NoProvidersBlock_ExclusionReasons locks the probe's exclusion
// reasons byte-for-byte against the pre-P2a jobs slice, and proves code-js is
// still NOT probed (it never was pre-P2a). Fully hermetic: every provider is
// excluded (no keys, ollama-local disabled, mock off), so no ListModels network
// call runs.
func TestResolveProbe_NoProvidersBlock_ExclusionReasons(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "DEEPSEEK_API_KEY", "GEMINI_API_KEY", "OLLAMA_API_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("OLLAMA_BASE_URL", "disabled")
	t.Setenv("LOOMCYCLE_MOCK_ENABLED", "")
	t.Setenv("LOOMCYCLE_CODE_AGENTS_ENABLED", "")

	cfg := loadDefaultProvidersOnly(t)
	pr, err := newProviderResolver(cfg)
	if err != nil {
		t.Fatalf("newProviderResolver: %v", err)
	}
	r := buildResolver(cfg, pr) // synchronous probe; all excluded → no network
	snap := r.Snapshot()

	wantReason := map[string]string{
		"anthropic":    "ANTHROPIC_API_KEY not set",
		"openai":       "OPENAI_API_KEY not set",
		"deepseek":     "DEEPSEEK_API_KEY not set",
		"gemini":       "GEMINI_API_KEY not set",
		"ollama":       "OLLAMA_API_KEY not set",
		"ollama-local": "OLLAMA_BASE_URL not configured",
		"mock":         "LOOMCYCLE_MOCK_ENABLED not set",
		"mock-stable":  "LOOMCYCLE_MOCK_ENABLED not set",
	}
	for id, want := range wantReason {
		av, ok := snap[id]
		if !ok {
			t.Errorf("probe: no snapshot entry for %q", id)
			continue
		}
		if !av.Excluded || av.LastError != want {
			t.Errorf("probe %q: excluded=%v reason=%q, want excluded=true reason=%q", id, av.Excluded, av.LastError, want)
		}
	}
	// code-js is never probed (pre-P2a parity): it must not appear in the matrix.
	if _, ok := snap["code-js"]; ok {
		t.Error("probe: code-js should not be in the resolver matrix (pre-P2a never probed it)")
	}
}

// TestRegisteredDrivers_SevenCompiledIn proves the RFC BF P2a blank imports work:
// every driver's init() ran, so the registry holds exactly the 7 driver names the
// resolver depends on. A missing name means a dropped blank import → NewDriver
// would fail at boot for that provider.
func TestRegisteredDrivers_SevenCompiledIn(t *testing.T) {
	got := map[string]bool{}
	for _, d := range providers.RegisteredDrivers() {
		got[d] = true
	}
	for _, want := range []string{"anthropic", "openai", "gemini", "ollama", "deepseek", "mock", "code-js"} {
		if !got[want] {
			t.Errorf("driver %q not registered (blank import missing?); registered=%v", want, providers.RegisteredDrivers())
		}
	}
	// anthropic-oauth-dev is a residual hardcoded path, NOT a registry driver.
	if got["anthropic-oauth-dev"] {
		t.Error("anthropic-oauth-dev must not be a registered driver (it is the residual path)")
	}
}

// TestToDriverOptions_PortsEnvBaseURLsAndTimeouts locks the behaviour-identity
// bridge: with no `base_url` in the config (the default-providers layer sets
// none), toDriverOptions must supply the exact per-id env/driver defaults and
// per-provider timeouts the pre-P2a resolver used.
func TestToDriverOptions_PortsEnvBaseURLsAndTimeouts(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "http://gpu.local:11434")
	t.Setenv("OLLAMA_CLOUD_BASE_URL", "https://cloud.example")
	t.Setenv("DEEPSEEK_BASE_URL", "https://ds.mirror")
	t.Setenv("GEMINI_BASE_URL", "https://vertex.example")
	t.Setenv("LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX", "131072")
	t.Setenv("LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU", "99")
	t.Setenv("LOOMCYCLE_OLLAMA_NUM_CTX", "8192")

	cfg := loadDefaultProvidersOnly(t)

	local := toDriverOptions("ollama-local", cfg.Providers["ollama-local"], cfg)
	if local.BaseURL != "http://gpu.local:11434" {
		t.Errorf("ollama-local BaseURL = %q, want the OLLAMA_BASE_URL value", local.BaseURL)
	}
	if local.KeyEnvName != "" {
		t.Errorf("ollama-local KeyEnvName = %q, want empty (keyless)", local.KeyEnvName)
	}
	if local.StreamOpts.HeaderTimeout != cfg.Env.OllamaLocalHeaderTimeout {
		t.Errorf("ollama-local HeaderTimeout = %v, want the local (600s) timeout %v", local.StreamOpts.HeaderTimeout, cfg.Env.OllamaLocalHeaderTimeout)
	}
	if n, _ := providers.IntOption(local.Options, "num_ctx"); n != 131072 {
		t.Errorf("ollama-local num_ctx = %d, want 131072", n)
	}
	if n, _ := providers.IntOption(local.Options, "num_gpu"); n != 99 {
		t.Errorf("ollama-local num_gpu = %d, want 99", n)
	}

	hosted := toDriverOptions("ollama", cfg.Providers["ollama"], cfg)
	if hosted.BaseURL != "https://cloud.example" {
		t.Errorf("ollama BaseURL = %q, want the OLLAMA_CLOUD_BASE_URL value", hosted.BaseURL)
	}
	if hosted.KeyEnvName != "OLLAMA_API_KEY" {
		t.Errorf("ollama KeyEnvName = %q, want OLLAMA_API_KEY", hosted.KeyEnvName)
	}
	if n, _ := providers.IntOption(hosted.Options, "num_ctx"); n != 8192 {
		t.Errorf("ollama num_ctx = %d, want 8192 (LOOMCYCLE_OLLAMA_NUM_CTX)", n)
	}
	if _, ok := providers.IntOption(hosted.Options, "num_gpu"); ok {
		t.Error("hosted ollama must not carry num_gpu (that is ollama-local only)")
	}

	ds := toDriverOptions("deepseek", cfg.Providers["deepseek"], cfg)
	if ds.BaseURL != "https://ds.mirror" || ds.KeyEnvName != "DEEPSEEK_API_KEY" {
		t.Errorf("deepseek opts = {BaseURL:%q KeyEnvName:%q}, want the DEEPSEEK_BASE_URL + DEEPSEEK_API_KEY", ds.BaseURL, ds.KeyEnvName)
	}

	gm := toDriverOptions("gemini", cfg.Providers["gemini"], cfg)
	if gm.BaseURL != "https://vertex.example" {
		t.Errorf("gemini BaseURL = %q, want the GEMINI_BASE_URL value", gm.BaseURL)
	}
}

// TestToDriverOptions_CodeJSPortsEnvRoot is the regression for the P2a code-js
// boot break: the switch in toDriverOptions ported the ollama/deepseek/gemini env
// defaults but NOT code-js, so the driver factory (codejs.newFromOptions) got no
// code_root and built a compiler with an empty root — every static `provider:
// code-js` agent then failed to load ("no index.js at <name>/index.js", a
// relative path = the empty-root tell), crash-looping boot (the "Runtime suites
// (code-js)" CI job). This asserts the three code-js env knobs are ported into
// the driver options.
//
// Fail-before: drop the `case "code-js"` in toDriverOptions and code_root is
// absent (StringOption ok=false) → this fails.
func TestToDriverOptions_CodeJSPortsEnvRoot(t *testing.T) {
	t.Setenv("LOOMCYCLE_CODE_AGENTS_ROOT", "/srv/agent_code")
	t.Setenv("LOOMCYCLE_CODE_AGENTS_DETERMINISTIC", "1")
	t.Setenv("LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", "45")

	cfg := loadDefaultProvidersOnly(t)
	if cfg.Env.CodeAgentsRoot != "/srv/agent_code" {
		t.Fatalf("cfg.Env.CodeAgentsRoot = %q, want /srv/agent_code (env not parsed?)", cfg.Env.CodeAgentsRoot)
	}

	opts := toDriverOptions("code-js", cfg.Providers["code-js"], cfg)
	if root, ok := providers.StringOption(opts.Options, "code_root"); !ok || root != "/srv/agent_code" {
		t.Errorf("code-js code_root option = %q (ok=%v), want /srv/agent_code — without it the compiler root is empty and every static code agent fails to load", root, ok)
	}
	if det, _ := providers.BoolOption(opts.Options, "deterministic"); !det {
		t.Errorf("code-js deterministic option = false, want true (LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=1)")
	}
	if secs, ok := providers.IntOption(opts.Options, "run_timeout_seconds"); !ok || secs != 45 {
		t.Errorf("code-js run_timeout_seconds = %d (ok=%v), want 45", secs, ok)
	}
}

// TestGet_MockStable_IsStableVariant proves mock-stable, built through the driver
// registry with `options: {stable: true}` (from the default-providers layer),
// reports the right id — the config-driven replacement for the pre-P2a
// NewStableProvider() wiring.
func TestGet_MockStable_IsStableVariant(t *testing.T) {
	t.Setenv("LOOMCYCLE_MOCK_ENABLED", "1")
	cfg := loadDefaultProvidersOnly(t)
	pr, err := newProviderResolver(cfg)
	if err != nil {
		t.Fatalf("newProviderResolver: %v", err)
	}
	ms, err := pr.Get("mock-stable")
	if err != nil {
		t.Fatalf("Get(mock-stable): %v", err)
	}
	if ms.ID() != "mock-stable" {
		t.Errorf("mock-stable ID() = %q, want mock-stable", ms.ID())
	}
	m, err := pr.Get("mock")
	if err != nil {
		t.Fatalf("Get(mock): %v", err)
	}
	if m.ID() != "mock" {
		t.Errorf("mock ID() = %q, want mock", m.ID())
	}
}

// TestNewProviderResolver_NoDefaultProviders_Empty proves LOOMCYCLE_NO_DEFAULT_PROVIDERS
// semantics at the resolver level: with no declared providers, every Get fails —
// there is no built-in floor once the default layer is dropped.
func TestNewProviderResolver_NoDefaultProviders_Empty(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-not-a-real-key")
	t.Setenv("LOOMCYCLE_NO_DEFAULT_PROVIDERS", "1") // suppress the divergence WARN
	// An empty config (no providers:) stands in for the LOOMCYCLE_NO_DEFAULT_PROVIDERS
	// stack, where main() omits the default-providers layer entirely.
	cfg, err := config.LoadLayers(config.Layer{Name: "empty", Data: []byte("concurrency:\n  max_concurrent_runs: 1\n")})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	pr, err := newProviderResolver(cfg)
	if err != nil {
		t.Fatalf("newProviderResolver: %v", err)
	}
	if _, err := pr.Get("anthropic"); err == nil {
		t.Error("Get(anthropic) should fail when no providers are declared")
	}
}
