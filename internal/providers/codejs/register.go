package codejs

import (
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// init registers the code-js driver with the RFC BF driver registry.
// Registration records only the factory — it neither reads the
// LOOMCYCLE_CODE_AGENTS_* env nor enables the provider (that stays behind the
// resolver's LOOMCYCLE_CODE_AGENTS_ENABLED gate in cmd/loomcycle). The canonical
// dialect is "code-js" (the synthetic goja-replay "wire").
func init() {
	providers.RegisterDriver("code-js", []string{"code-js"}, newFromOptions)
}

// newFromOptions builds a code-js Provider from the registry DriverOptions. P1
// equivalent of cmd/loomcycle/main.go's hardcoded codejs.New(codejs.Config{});
// NOT yet on the hot path (P2 wires the registry into the resolver). code-js has
// no HTTP shape — its Config comes from the options map (code_root /
// deterministic / run_timeout_seconds), a natural mapping for when P2 sources it
// from a `providers:` entry rather than the env.
func newFromOptions(o providers.DriverOptions) (providers.Provider, error) {
	cfg := Config{Logf: o.Logf}
	if root, ok := providers.StringOption(o.Options, "code_root"); ok {
		cfg.CodeRoot = root
	}
	if det, ok := providers.BoolOption(o.Options, "deterministic"); ok {
		cfg.Deterministic = det
	}
	if secs, ok := providers.IntOption(o.Options, "run_timeout_seconds"); ok {
		cfg.RunTimeout = time.Duration(secs) * time.Second
	}
	p := New(cfg)
	if o.ID != "" {
		p.id = o.ID
	}
	p.capsPatch = o.Capabilities
	providers.WarnUnknownOptions(o.Logf, "code-js", o.Options, "code_root", "deterministic", "run_timeout_seconds")
	return p, nil
}
