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

	// Embedded default-providers layer (RFC BF) — the true base of the stack, so
	// CLI introspection resolves the built-in providers from the SAME source the
	// server assembles (cmd/loomcycle/main.go prepends it identically). Without
	// this, validate/doctor/agents fell back on the config package's built-in
	// floor and could disagree with the running server. LOOMCYCLE_NO_DEFAULT_PROVIDERS=1
	// drops it (matching the server).
	if os.Getenv("LOOMCYCLE_NO_DEFAULT_PROVIDERS") != "1" {
		layers = append(layers, config.Layer{Name: "providers.default", Data: embedded.DefaultProviders()})
	}
	// The default-providers layer is always-on infrastructure, NOT operator-supplied
	// config — the explicit-path check below must not treat it as "a config was
	// already provided", or a missing --config file would be silently ignored.
	baseLayers := len(layers)

	// Embedded presets — layered over the default providers.
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

	// LOOMCYCLE_CONFIG_FILES — colon-separated. Collected (not yet layered) so the
	// RFC CK section-sibling expansion below runs over the whole operator file set.
	var opFiles []string
	for _, f := range strings.Split(os.Getenv("LOOMCYCLE_CONFIG_FILES"), ":") {
		if f = strings.TrimSpace(f); f != "" {
			opFiles = append(opFiles, f)
		}
	}

	// The explicit --config path (highest precedence). When it's absent from
	// disk but the presets/env layers already provide a config, skip it (a
	// presets-only stack, RFC AQ); when there is no other operator config at all,
	// keep it so LoadLayers surfaces the same file-not-found error the CLI produced
	// before (baseLayers excludes the always-on default-providers layer).
	if explicitPath = strings.TrimSpace(explicitPath); explicitPath != "" {
		if _, err := os.Stat(explicitPath); err == nil || (len(layers) == baseLayers && len(opFiles) == 0) {
			opFiles = append(opFiles, explicitPath)
		}
	}

	// RFC CK section-per-file: a loomcycle.yaml among the operator files brings its
	// loomcycle.*.yaml section siblings, deep-merged after it — the SAME helper the
	// server boot path uses, so validate/doctor/agents resolve the identical split
	// config the running server does (lockstep). LOOMCYCLE_CONFIG_DIR is excluded:
	// it already globs every *.yaml in its directory.
	for _, f := range WithSectionSiblings(opFiles) {
		layers = append(layers, config.Layer{Name: f})
	}

	return config.LoadLayers(layers...)
}

// byTierProvider is what the introspection commands print for an agent whose
// provider is chosen at run time. Not a provider id, and deliberately shaped so
// it cannot be mistaken for one.
const byTierProvider = "(by tier)"

// agentModelForDisplay reports how an agent will get its provider and model, for
// the introspection commands (validate, agents list).
//
// THREE outcomes, not two, and collapsing them into two is the bug this exists
// for. config.ResolveAgentModel answers the PIN path: an agent naming a provider
// or model (or an alias for one) resolves here and now. An agent naming a `tier:`
// is resolved at RUN time by the resolver against the live availability matrix,
// which a CLI process has no access to — so the pin path legitimately finds
// nothing for it. Reporting that as a config error made `loomcycle validate` exit
// 2 on every bundled agent preset (chat, memory, document-agent, agent-teams)
// naming an agent the running server resolves fine. The exit code was the small
// half: validate returns at the FIRST agent, so MCP servers and everything after
// went unchecked, and `agents list --json` bailed mid-array leaving output no
// parser accepts.
//
// TIER IS CHECKED FIRST, mirroring the runtime — internal/api/http.resolveAgentDef
// takes the tier path before it ever looks at a pin or at `defaults:`. Ordering it
// the other way (pin path first, tier only when that errors) looks equivalent and
// is not: for a tier agent in a config that also sets `defaults:`, the pin path
// succeeds and returns the DEFAULTS, so the command would confidently print a
// provider the server will never use for it.
//
// An agent with neither a pin nor a tier still fails, because that one is a real
// config error — the resolver refuses it too ("has neither pin nor tier").
func agentModelForDisplay(cfg *config.Config, name string) (provider, model string, err error) {
	// Config validation rejects pin+tier on one agent, so a non-empty tier here
	// means the tier path is the ONLY path this agent has.
	if def, ok := cfg.Agents[name]; ok && def.Tier != "" {
		return byTierProvider, "tier:" + def.Tier, nil
	}
	provider, model, pattern, err := cfg.ResolveAgentModel(name)
	if err != nil {
		return "", "", err
	}
	// RFC BG: a model_pattern alias resolves against the live catalog at run
	// time, so show the glob rather than the empty string it resolves to here.
	if pattern != "" {
		model = pattern
	}
	return provider, model, nil
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
