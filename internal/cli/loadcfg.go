package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/cmd/loomcycle/embedded"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// loadLayeredConfig assembles the SAME layered configuration the server builds
// (RFC AN/AQ), so CLI introspection (validate / agents / doctor) reflects what
// the running server actually resolves — embedded presets (LOOMCYCLE_PRESETS) as
// the base, then LOOMCYCLE_CONFIG_DIR/*.yaml, then LOOMCYCLE_CONFIG_FILES, then
// the explicit --config path (last wins).
//
// Before this, the CLI called config.Load(path) — a single file — so an agent
// whose model is a preset-defined alias (e.g. `deepseek-pro` from the base
// preset) reported a false "no provider resolved" in these tools even though
// the running server resolved it fine. The layer set + precedence mirror
// cmd/loomcycle/main.go's server assembly; the CLI variant omits the
// server-only concerns (XDG auto-discovery beyond the explicit path, auth.env,
// os.Exit) it doesn't need. With neither LOOMCYCLE_PRESETS nor the CONFIG_*
// env vars set, behaviour is byte-identical to the old config.Load(path).
func loadLayeredConfig(explicitPath string) (*config.Config, error) {
	var layers []config.Layer

	// Embedded presets — the base of the stack.
	var presetNames []string
	for _, n := range strings.Split(os.Getenv("LOOMCYCLE_PRESETS"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			presetNames = append(presetNames, n)
		}
	}
	if len(presetNames) > 0 {
		units, err := embedded.ResolveUnits(presetNames)
		if err != nil {
			return nil, fmt.Errorf("presets: %w", err)
		}
		for _, u := range units {
			layers = append(layers, config.Layer{Name: u.Name, Data: u.Data})
		}
	}

	// LOOMCYCLE_CONFIG_DIR — *.yaml/*.yml, lexical order.
	if dir := strings.TrimSpace(os.Getenv("LOOMCYCLE_CONFIG_DIR")); dir != "" {
		files, err := configDirYAMLs(dir)
		if err != nil {
			return nil, fmt.Errorf("LOOMCYCLE_CONFIG_DIR: %w", err)
		}
		for _, f := range files {
			layers = append(layers, config.Layer{Name: f})
		}
	}

	// LOOMCYCLE_CONFIG_FILES — colon-separated.
	for _, f := range strings.Split(os.Getenv("LOOMCYCLE_CONFIG_FILES"), ":") {
		if f = strings.TrimSpace(f); f != "" {
			layers = append(layers, config.Layer{Name: f})
		}
	}

	// The explicit --config path (highest precedence). When it's absent from
	// disk but the presets/env layers already provide a config, skip it (a
	// presets-only stack, RFC AQ); when there are no other layers, keep it so
	// LoadLayers surfaces the same file-not-found error the CLI produced before.
	if explicitPath = strings.TrimSpace(explicitPath); explicitPath != "" {
		if _, err := os.Stat(explicitPath); err == nil || len(layers) == 0 {
			layers = append(layers, config.Layer{Name: explicitPath})
		}
	}

	return config.LoadLayers(layers...)
}

// configDirYAMLs lists *.yaml/*.yml in dir in lexical order — mirrors
// cmd/loomcycle.configDirLayers (the server's LOOMCYCLE_CONFIG_DIR reader).
func configDirYAMLs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if n := e.Name(); strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml") {
			files = append(files, filepath.Join(dir, n))
		}
	}
	sort.Strings(files)
	return files, nil
}
