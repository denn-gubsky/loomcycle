package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/cmd/loomcycle/embedded"
	"github.com/denn-gubsky/loomcycle/internal/cli"
	"github.com/denn-gubsky/loomcycle/internal/config"
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

// configDirLayers returns the *.yaml / *.yml files directly under dir, sorted
// lexically — the LOOMCYCLE_CONFIG_DIR layer group (RFC AQ §4). The directory
// must exist and be readable (a set-but-missing dir is the operator's typo →
// surfaced); an empty dir (no matching files) is fine, returning nil. Files in
// subdirectories are ignored — a flat drop-in dir.
func configDirLayers(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if name := e.Name(); strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	sort.Strings(files)
	return files, nil
}

// assembleConfigLayers builds the ordered config-layer list from the embedded
// preset selection + the operator's config files, exactly the way boot does.
// It is shared by boot AND the RFC CK runtime reload: because it RE-runs the
// preset resolution, CONFIG_DIR glob, and loomcycle.*.yaml section-sibling
// discovery every call, a reload picks up config or section files that were
// added or removed since boot without a restart (the "re-glob on reload"
// follow-up). Precedence, base → top, last wins:
//
//	providers.default → embedded presets → LOOMCYCLE_CONFIG_DIR/*.yaml
//	→ LOOMCYCLE_CONFIG_FILES → --config (+ each base's loomcycle.*.yaml siblings)
//
// It returns an error (never os.Exit) so a reload can reject a now-broken
// assembly and keep the running config; boot wraps the error as fatal. verbose
// gates the boot-time info logs — the reload path passes false so it does not
// re-log the whole layering on every reload (auth.env is set-if-unset, so its
// re-run at reload is a silent no-op regardless).
func assembleConfigLayers(presetFlags, cfgPaths []string, verbose bool) ([]config.Layer, error) {
	logf := func(format string, args ...any) {
		if verbose {
			log.Printf(format, args...)
		}
	}

	// Embedded presets (RFC AQ) — the base of the stack, opt-in.
	presetNames := selectPresetNames(presetFlags, os.Getenv("LOOMCYCLE_PRESETS"))
	var presetLayers []config.Layer
	if len(presetNames) > 0 {
		units, err := embedded.ResolveUnits(presetNames)
		if err != nil {
			return nil, err
		}
		for _, u := range units {
			presetLayers = append(presetLayers, config.Layer{Name: u.Name, Data: u.Data})
		}
		logf("config: layering %d embedded preset(s)/bundle(s) as base: %s", len(presetNames), strings.Join(presetNames, ", "))
	}

	// LOOMCYCLE_CONFIG_DIR (RFC AQ §4) — a directory of *.yaml/*.yml layered as a
	// group, lexically, between the presets and CONFIG_FILES. Unset → skipped.
	var configDirFiles []string
	if dir := strings.TrimSpace(os.Getenv("LOOMCYCLE_CONFIG_DIR")); dir != "" {
		files, err := configDirLayers(dir)
		if err != nil {
			return nil, fmt.Errorf("LOOMCYCLE_CONFIG_DIR: %w", err)
		}
		configDirFiles = files
		if len(files) > 0 {
			logf("config: layering %d file(s) from LOOMCYCLE_CONFIG_DIR=%s (lexical order)", len(files), dir)
		}
	}

	// LOOMCYCLE_CONFIG_FILES + --config.
	var cfgFiles []string
	for _, f := range strings.Split(os.Getenv("LOOMCYCLE_CONFIG_FILES"), ":") {
		if f = strings.TrimSpace(f); f != "" {
			cfgFiles = append(cfgFiles, f)
		}
	}
	cfgFiles = append(cfgFiles, cfgPaths...)

	if len(cfgFiles) == 0 && len(configDirFiles) == 0 {
		// No explicit operator config → XDG auto-discovery (lockstep with doctor).
		resolvedCfg, found := resolveConfigPath("loomcycle.yaml")
		switch {
		case found:
			cfgFiles = []string{resolvedCfg}
		case len(presetLayers) > 0:
			// Presets-only: the embedded base IS the config (RFC AQ §2.2).
			logf("config: no operator config file — running from embedded presets only")
		default:
			var b strings.Builder
			b.WriteString("no config found at any of:\n")
			for _, p := range configAutoDiscoveryPaths() {
				fmt.Fprintf(&b, "    %s\n", p)
			}
			b.WriteString("\nRun `loomcycle init` to create one, pass --config <path>, set LOOMCYCLE_CONFIG_DIR, or select an embedded base with LOOMCYCLE_PRESETS=base.")
			return nil, errors.New(b.String())
		}
	} else {
		// Explicit --config / CONFIG_FILES layers must each exist (no per-file XDG
		// fallback). CONFIG_DIR files were already validated by configDirLayers.
		for _, f := range cfgFiles {
			if _, err := os.Stat(f); err != nil {
				return nil, fmt.Errorf("config file not found: %s", f)
			}
		}
	}

	// RFC CK section-per-file: a loomcycle.yaml base brings its loomcycle.*.yaml
	// section siblings, deep-merged after it. Re-globbed every call so an
	// added/removed section file is seen on reload. Shared with loadLayeredConfig
	// (validate/doctor lockstep). Only files existing on disk are added.
	if expanded := cli.WithSectionSiblings(cfgFiles); len(expanded) > len(cfgFiles) {
		orig := make(map[string]bool, len(cfgFiles))
		for _, f := range cfgFiles {
			orig[f] = true
		}
		var added []string
		for _, f := range expanded {
			if !orig[f] {
				added = append(added, f)
			}
		}
		logf("config: + %d section file(s): %s", len(added), strings.Join(added, ", "))
		cfgFiles = expanded
	}

	// Ordered disk-file layers: CONFIG_DIR first, then CONFIG_FILES/--config.
	diskFiles := append(append([]string{}, configDirFiles...), cfgFiles...)
	if len(diskFiles) > 1 {
		logf("config: layering %d files (last wins): %s", len(diskFiles), strings.Join(diskFiles, " ◁ "))
	}
	// Auto-load <configdir>/auth.env BEFORE config.Load reads os.Getenv, keyed on
	// the LAST (authoritative) disk-file layer. Set-if-unset (real env wins), so a
	// reload re-run sets nothing (n=0) and stays silent. A presets-only stack has
	// no file to load from.
	if len(diskFiles) > 0 {
		if authEnvPath, n, err := cli.LoadAuthEnv(diskFiles[len(diskFiles)-1]); err != nil {
			logf("auth.env: %v (continuing without it)", err)
		} else if n > 0 {
			logf("auth.env: loaded %d var(s) from %s (a shell export overrides them)", n, authEnvPath)
		}
	}

	// Build the full ordered layer list. RFC BF P2a: the embedded default-providers
	// layer is the UNCONDITIONAL base (before opt-in presets); LOOMCYCLE_NO_DEFAULT_PROVIDERS=1
	// drops it. Kept OUT of presetLayers so the "no config found" error still fires
	// on a bare run. Order, base→top (last wins):
	//   providers.default → opt-in presets → CONFIG_DIR → CONFIG_FILES/--config
	layers := make([]config.Layer, 0, 1+len(presetLayers)+len(diskFiles))
	if os.Getenv("LOOMCYCLE_NO_DEFAULT_PROVIDERS") != "1" {
		layers = append(layers, config.Layer{Name: "providers.default", Data: embedded.DefaultProviders()})
	}
	layers = append(layers, presetLayers...)
	for _, f := range diskFiles {
		layers = append(layers, config.Layer{Name: f})
	}
	return layers, nil
}
