package main

import (
	"flag"
	"os"
	"path/filepath"
)

// autodiscover.go — v0.11.1 config auto-discovery.
//
// When the operator runs `loomcycle` without --config, walk a small
// set of standard paths and pick the first one that exists. The
// goal is "brew install loomcycle && loomcycle init && loomcycle"
// Just Works — the default no-flags invocation should find the
// init-generated config in ~/.config/loomcycle/loomcycle.yaml
// without any extra plumbing.
//
// Auto-discovery only kicks in when the user didn't override
// --config. An explicit `--config /any/path` keeps today's semantics
// exactly — even pointing at a missing path, the config.Load call
// surfaces the operator's typo unchanged.

// resolveConfigPath returns the path to use for config.Load. When
// path is the unmodified flag default ("loomcycle.yaml") AND that
// file is absent in cwd, search the XDG paths instead. Otherwise
// return path as-is.
//
// found=false means: caller passed nothing AND no auto-discoverable
// file exists. Caller prints the first-run hint and exits.
func resolveConfigPath(path string) (resolved string, found bool) {
	if userOverrodeConfigFlag() {
		// Operator passed --config explicitly. Trust it; let the
		// downstream config.Load surface a missing-file error if
		// the path is wrong.
		return path, true
	}
	// Default value path. Check cwd first; if missing, walk XDG.
	if _, err := os.Stat(path); err == nil {
		return path, true
	}
	for _, p := range configAutoDiscoveryPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// configAutoDiscoveryPaths returns the in-order paths walked by
// resolveConfigPath when --config is left at its default.
func configAutoDiscoveryPaths() []string {
	paths := []string{"./loomcycle.yaml"}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		paths = append(paths, filepath.Join(xdg, "loomcycle", "loomcycle.yaml"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "loomcycle", "loomcycle.yaml"))
	}
	return paths
}

// userOverrodeConfigFlag reports whether --config was explicitly set
// on the command line, regardless of value. We use flag.Visit (which
// only walks set flags) rather than comparing f.Value against
// f.DefValue — the value-comparison approach silently treats
// `--config loomcycle.yaml` (the literal default) as "not set", which
// breaks the operator's explicit choice when it happens to match the
// default string.
func userOverrodeConfigFlag() bool {
	var overrode bool
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			overrode = true
		}
	})
	return overrode
}
