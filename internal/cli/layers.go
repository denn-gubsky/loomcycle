package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/embedded"
)

// layers.go — the one assembly of the config-layer stack.
//
// The server (boot and config reload), `tokens`, `audit` and the introspection
// commands (validate, doctor, agents list, the memory tools) all build their
// layer list here. There used to be two assemblies, one for the server and one
// for the CLI, and a release shipped with the two disagreeing: `validate`
// reported an agent as unresolvable that the running server resolved.

// layerEnv is the environment read before a config exists, because it decides
// which config is loaded. These variables cannot be fields of config.Env: the
// config they would be read into is what they locate. readLayerEnv is the only
// place they are read.
type layerEnv struct {
	// Presets is LOOMCYCLE_PRESETS: the embedded presets and bundles layered
	// as the base, comma-separated, in order.
	Presets string
	// ConfigDir is LOOMCYCLE_CONFIG_DIR: a directory whose *.yaml / *.yml
	// files are layered in lexical order.
	ConfigDir string
	// NoDefaultProviders is LOOMCYCLE_NO_DEFAULT_PROVIDERS=1: leave out the
	// embedded default-providers layer.
	NoDefaultProviders bool
	// XDGConfigHome is XDG_CONFIG_HOME, the root of the auto-discovery path.
	XDGConfigHome string
}

func readLayerEnv() layerEnv {
	return layerEnv{
		Presets:            os.Getenv("LOOMCYCLE_PRESETS"),
		ConfigDir:          strings.TrimSpace(os.Getenv("LOOMCYCLE_CONFIG_DIR")),
		NoDefaultProviders: os.Getenv("LOOMCYCLE_NO_DEFAULT_PROVIDERS") == "1",
		XDGConfigHome:      os.Getenv("XDG_CONFIG_HOME"),
	}
}

// ConfigStack is an assembled layer list and what it was built from.
type ConfigStack struct {
	// Layers is the stack, base first; config.LoadLayers merges it last-wins.
	Layers []config.Layer
	// Presets is the embedded presets and bundles among the layers, in layer
	// order, after every include is expanded. Empty when none is selected.
	Presets []string
	// Files is the operator's disk files among the layers, in layer order.
	// Empty for a stack made of embedded presets alone.
	Files []string
	// AuthEnvPath and AuthEnvVars report the auth.env beside the last file:
	// its path and how many variables it set that the environment lacked.
	AuthEnvPath string
	AuthEnvVars int
}

// SelectPresetNames resolves the embedded-preset selection: --preset flags,
// if any, override LOOMCYCLE_PRESETS (comma-separated, ordered). Empty or
// unset selects none. Order is preserved; it becomes the base layer order,
// with each preset's own includes layered under it.
func SelectPresetNames(flags []string, env string) []string {
	if len(flags) > 0 {
		return flags
	}
	var out []string
	for _, n := range strings.Split(env, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// ConfigSearchPaths returns, in order, the paths tried when no config file is
// named: ./loomcycle.yaml, then the XDG config home, then ~/.config.
func ConfigSearchPaths() []string {
	paths := []string{"./loomcycle.yaml"}
	if xdg := readLayerEnv().XDGConfigHome; xdg != "" {
		paths = append(paths, filepath.Join(xdg, "loomcycle", "loomcycle.yaml"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "loomcycle", "loomcycle.yaml"))
	}
	return paths
}

// discoverConfigPath returns the first of ConfigSearchPaths that exists. The
// working directory's file is returned as "loomcycle.yaml", the name the
// server has always logged and layered it under.
func discoverConfigPath() (string, bool) {
	if _, err := os.Stat("loomcycle.yaml"); err == nil {
		return "loomcycle.yaml", true
	}
	for _, p := range ConfigSearchPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// configDirLayers returns the *.yaml / *.yml files directly under dir, sorted
// lexically: the LOOMCYCLE_CONFIG_DIR layer group. The directory must exist
// and be readable (a set-but-missing dir is the operator's typo, surfaced); an
// empty dir is fine. Files in subdirectories are ignored.
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

// AssembleConfigStack builds the ordered config-layer list from the embedded
// preset selection and the operator's config files. Precedence, base to top,
// last wins:
//
//	providers.default → embedded presets → LOOMCYCLE_CONFIG_DIR/*.yaml
//	→ --config, in the order given (+ each loomcycle.yaml's loomcycle.*.yaml siblings)
//
// Which presets: --preset, else LOOMCYCLE_PRESETS, else the `presets:` key
// of the primary file (the last --config, else the discovered loomcycle.yaml,
// else the config dir's loomcycle.yaml). The key is read here, before the
// stack is built, because the presets go under the file that names them.
//
// presetFlags are the --preset values (empty: LOOMCYCLE_PRESETS decides);
// cfgPaths are the config files named explicitly, each of which must exist.
// With no file named and none in the config dir, ./loomcycle.yaml and the XDG
// paths are tried; with none of those either, the presets alone are the
// config, and with no presets it is an error.
//
// Every call re-runs the preset resolution, the directory listing and the
// section-sibling discovery, so a config reload sees files added or removed
// since boot. It returns an error and never exits, so a reload can reject a
// broken assembly and keep the running config. verbose gates the info logs:
// boot passes true, every other caller false.
//
// It also loads the auth.env beside the last file into the process
// environment, set-if-unset, so that config.LoadLayers reads the same
// variables whoever calls it; a repeat call sets nothing.
func AssembleConfigStack(presetFlags, cfgPaths []string, verbose bool) (ConfigStack, error) {
	info := func(msg string, args ...any) {
		if verbose {
			slog.Info(msg, args...)
		}
	}
	// The variable that used to name config files is retired. Refused here as
	// well as in the load: the files it names are not layered, and when they
	// were the only config the "no config found" below would hide why.
	if refused := config.RetiredLayerEnvRefusals(os.Environ()); len(refused) > 0 {
		return ConfigStack{}, fmt.Errorf("a retired environment variable names config files that are no longer layered; act on it, then remove its line: %s", strings.Join(refused, " | "))
	}
	env := readLayerEnv()

	// The selection the flags or the environment make. The primary file's
	// `presets:` is read further down, once the files are known; this one
	// wins over it.
	selected := SelectPresetNames(presetFlags, env.Presets)
	selectedBy := "LOOMCYCLE_PRESETS"
	if len(presetFlags) > 0 {
		selectedBy = "--preset"
	}

	// LOOMCYCLE_CONFIG_DIR: a directory of *.yaml/*.yml layered as a group,
	// lexically, between the presets and --config. Unset: skipped.
	var configDirFiles []string
	if env.ConfigDir != "" {
		files, err := configDirLayers(env.ConfigDir)
		if err != nil {
			return ConfigStack{}, fmt.Errorf("LOOMCYCLE_CONFIG_DIR: %w", err)
		}
		configDirFiles = files
		if len(files) > 0 {
			info("config: layering files from LOOMCYCLE_CONFIG_DIR (lexical order)", "files", len(files), "dir", env.ConfigDir)
		}
	}

	cfgFiles := append([]string{}, cfgPaths...)
	if len(cfgFiles) == 0 && len(configDirFiles) == 0 {
		// No explicit operator config: auto-discovery.
		resolvedCfg, found := discoverConfigPath()
		switch {
		case found:
			cfgFiles = []string{resolvedCfg}
		case len(selected) > 0:
			// Presets-only: the embedded base is the config.
			info("config: no operator config file — running from embedded presets only")
		default:
			var b strings.Builder
			b.WriteString("no config found at any of:\n")
			for _, p := range ConfigSearchPaths() {
				fmt.Fprintf(&b, "    %s\n", p)
			}
			b.WriteString("\nRun `loomcycle init` to create one, pass --config <path>, set LOOMCYCLE_CONFIG_DIR, or select an embedded base with LOOMCYCLE_PRESETS=base.")
			return ConfigStack{}, errors.New(b.String())
		}
	} else {
		// Explicit --config layers must each exist (no per-file fallback). The
		// config dir's files were already listed from disk.
		for _, f := range cfgFiles {
			// #nosec G703 -- f is the operator's own --config path.
			if _, err := os.Stat(f); err != nil {
				return ConfigStack{}, fmt.Errorf("config file not found: %s", f)
			}
		}
	}

	// The primary file, the one place a `presets:` key is read from: the last
	// file named, else the one discovered, else the config dir's
	// loomcycle.yaml. Decided before the section siblings are added, so a
	// sibling is never it.
	primary := ""
	switch {
	case len(cfgFiles) > 0:
		primary = cfgFiles[len(cfgFiles)-1]
	default:
		for _, f := range configDirFiles {
			if filepath.Base(f) == "loomcycle.yaml" {
				primary = f
			}
		}
	}
	presetLayers, keyRead, err := selectPresetLayers(selected, selectedBy, primary, info)
	if err != nil {
		return ConfigStack{}, err
	}

	// A loomcycle.yaml base brings its loomcycle.*.yaml section siblings,
	// deep-merged after it. Only files existing on disk are added.
	if expanded := WithSectionSiblings(cfgFiles); len(expanded) > len(cfgFiles) {
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
		info("config: + section files", "files", len(added), "paths", strings.Join(added, ", "))
		cfgFiles = expanded
	}

	// Ordered disk-file layers: the config dir first, then --config. A file
	// reached both ways (the config dir's loomcycle.yaml named again with
	// --config, or its section files) is one file: it is layered once, at
	// its --config position. Files are compared by resolved path, so another
	// spelling or a symlink of the same file is recognised.
	named := make(map[string]bool, len(cfgFiles))
	for _, f := range cfgFiles {
		named[canonicalPath(f)] = true
	}
	stack := ConfigStack{}
	for _, f := range configDirFiles {
		if !named[canonicalPath(f)] {
			stack.Files = append(stack.Files, f)
		}
	}
	stack.Files = append(stack.Files, cfgFiles...)
	if len(stack.Files) > 1 {
		info("config: layering files (last wins)", "files", len(stack.Files), "paths", strings.Join(stack.Files, " ◁ "))
	}
	// auth.env beside the last (authoritative) file, before config.LoadLayers
	// reads the environment. Set-if-unset: the real environment wins, and a
	// repeat call sets nothing. A presets-only stack has no file to load from.
	if len(stack.Files) > 0 {
		authEnvPath, n, err := LoadAuthEnv(stack.Files[len(stack.Files)-1])
		switch {
		case err != nil:
			if verbose {
				slog.Warn("auth.env: loading it failed (continuing without it)", "err", err)
			}
		case n > 0:
			stack.AuthEnvPath, stack.AuthEnvVars = authEnvPath, n
			info("auth.env: loaded vars (a shell export overrides them)", "vars", n, "path", authEnvPath)
		}
	}

	// The embedded default-providers layer is the unconditional base, before
	// the opt-in presets; LOOMCYCLE_NO_DEFAULT_PROVIDERS=1 drops it. It is not
	// counted as a preset above, so a bare run still gets "no config found".
	stack.Layers = make([]config.Layer, 0, 1+len(presetLayers)+len(stack.Files))
	if !env.NoDefaultProviders {
		stack.Layers = append(stack.Layers, config.Layer{Name: "providers.default", Data: embedded.DefaultProviders(), Embedded: true})
	}
	stack.Layers = append(stack.Layers, presetLayers...)
	for _, l := range presetLayers {
		stack.Presets = append(stack.Presets, l.Name)
	}
	for _, f := range stack.Files {
		l := config.Layer{Name: f}
		// Marked only when the key was read: a `presets:` the pre-parse did
		// not see (the file parses only after ${VAR} expansion) is then
		// refused by the merge, not dropped with nothing layered.
		if f == primary && keyRead {
			l.Primary = true
		}
		stack.Layers = append(stack.Layers, l)
	}
	return stack, nil
}

// selectPresetLayers resolves which embedded presets and bundles are the base
// of the stack. selected is what the flags or the environment name (by
// selectedBy); primary is the operator's primary config file, "" for none.
//
// The environment wins: with selected non-empty the file's `presets:` is not
// layered, and when the two would give different stacks a warning on the
// returned layers says which is in force. With selected empty the file's key selects. A
// preset named in the file is trusted as the file is: it adds only what a
// unit built into the binary carries, and no unit can set a security gate.
func selectPresetLayers(selected []string, selectedBy, primary string, info func(string, ...any)) (layers []config.Layer, inFile bool, err error) {
	var fromFile []string
	if primary != "" {
		if fromFile, inFile, err = config.ReadPresetsKey(primary); err != nil {
			return nil, false, err
		}
	}
	var fileUnits []embedded.Unit
	var fileNotes []string
	if inFile {
		// Resolved even when the environment wins: a name the binary does not
		// carry is a mistake in the file whichever selection is in force.
		if fileUnits, fileNotes, err = embedded.ExpandSelection(fromFile); err != nil {
			return nil, false, fmt.Errorf("presets: in %s: %w", primary, err)
		}
	}
	units, warnings, by := fileUnits, fileNotes, "presets: in "+primary
	if len(selected) > 0 {
		if units, warnings, err = embedded.ExpandSelection(selected); err != nil {
			return nil, false, err
		}
		by = selectedBy
		if inFile && unitNames(units) != unitNames(fileUnits) {
			warnings = append(warnings, fmt.Sprintf(
				"presets: in %s names [%s], but %s selects [%s]: %s wins and the file's list is not layered. Remove one of the two",
				primary, strings.Join(fromFile, ", "), selectedBy, strings.Join(selected, ", "), selectedByWins(selectedBy)))
		}
	}
	if len(units) == 0 {
		return nil, inFile, nil
	}
	// The names after every `include:` is expanded, so the line below says
	// what is layered, not only what was written.
	for _, u := range units {
		layers = append(layers, config.Layer{Name: u.Name, Data: u.Data, Embedded: true})
	}
	// What the selection had to be told goes out with the load's own
	// advisories. It rides on the last preset layer: a stack made of presets
	// alone has no file to carry it.
	layers[len(layers)-1].Warnings = warnings
	// The TrueNAS install guide quotes this line's text; keep it.
	presets := unitNames(units)
	info(fmt.Sprintf("config: layering %d embedded preset(s)/bundle(s) as base: %s", len(units), presets),
		"presets", presets, "selected_by", by)
	return layers, inFile, nil
}

// canonicalPath is a path as files are compared by: absolute, with symlinks
// resolved; the cleaned path when it does not resolve (the file is reported
// missing elsewhere).
func canonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// selectedByWins names what overrode the file's list: the flag is not the
// environment.
func selectedByWins(selectedBy string) string {
	if selectedBy == "--preset" {
		return "the flag"
	}
	return "the environment"
}

// unitNames joins unit names in layer order.
func unitNames(units []embedded.Unit) string {
	names := make([]string, len(units))
	for i, u := range units {
		names[i] = u.Name
	}
	return strings.Join(names, ", ")
}

// loadLayeredConfig loads the configuration the server would load: the same
// stack, assembled by the same function. explicitPath is a config file the
// caller was given ("" for none, which takes the server's auto-discovery).
func loadLayeredConfig(explicitPath string) (*config.Config, error) {
	stack, err := assembleForCLI(explicitPath)
	if err != nil {
		return nil, err
	}
	return config.LoadLayers(stack.Layers...)
}

// assembleForCLI is the CLI's call of AssembleConfigStack: no --preset flags,
// at most one named file, no boot logging.
func assembleForCLI(explicitPath string) (ConfigStack, error) {
	var paths []string
	if p := strings.TrimSpace(explicitPath); p != "" {
		paths = []string{p}
	}
	return AssembleConfigStack(nil, paths, false)
}
