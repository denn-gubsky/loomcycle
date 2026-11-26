package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// What RFC GW §9 moves out of the core: the Bash tool, Bashbox and its host
// fallback, the builder sidecar's MCP tools, the shell_credentials field and
// the variables that configure them. Workspaces replace them (RFC GW P1–P13);
// a later release removes them (P15). Until then each use is warned about,
// here at load (so boot, `validate` and `doctor` print it) and, for what a
// stored agent definition holds, when a run is given it.
//
// The table is the one place the words live: every surface reads it. Its
// sections are headings of MigrationGuide. No release is named: when the
// removal lands is the owner's to rule (P14 plan Q3).

// MigrationGuide is the guide every warning links, from the repository root.
const MigrationGuide = "docs/MIGRATING-FROM-BASH.md"

// The kinds of deprecated thing.
const (
	DeprecatedKindEnv    = "env"
	DeprecatedKindTool   = "tool"
	DeprecatedKindField  = "field"
	DeprecatedKindBundle = "bundle"
)

// Deprecation is one thing a later release removes and what replaces it.
type Deprecation struct {
	Kind string
	// Name is the variable, tool, field or bundle. A tool ending in "__" is
	// a prefix: every tool name under it.
	Name string
	// Replacement completes "… will be removed in a future release: ".
	Replacement string
	// Section is the guide's heading, as a link fragment.
	Section string
}

var deprecations = []Deprecation{
	{DeprecatedKindEnv, "LOOMCYCLE_BASH_ENABLED", "turn on workspaces (workspace.mode) and give the agent the Workspace tool with a workspace grant", "environment-variables"},
	{DeprecatedKindEnv, "LOOMCYCLE_BASH_ALLOWED_CREDS", "give the workspace an egress route with a credential; the core adds it and no shell sees it", "environment-variables"},
	{DeprecatedKindEnv, "LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS", "run the commands in a workspace, on a worker that carries their toolchain", "environment-variables"},
	{DeprecatedKindEnv, "LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV", "run the commands in a workspace, on a worker that carries their toolchain", "environment-variables"},
	{DeprecatedKindEnv, "LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS", "give the workspace an egress route with a credential, or open a git workspace with the credential", "environment-variables"},
	{DeprecatedKindEnv, "LOOMCYCLE_BASHBOX_OVERLAY_MAX_BYTES", "nothing: Bashbox's in-process sandbox goes, and workspaces run commands on a worker", "environment-variables"},
	{DeprecatedKindTool, "Bash", "use the Workspace tool, which runs commands on a workspace worker", "the-bash-tool"},
	{DeprecatedKindTool, "Bashbox", "use the Workspace tool, which runs commands on a workspace worker", "bashbox"},
	{DeprecatedKindTool, "mcp__sandbox__", "use the Workspace tool; a workspace worker's container engine replaces the builder sidecar", "the-builder-sidecar"},
	{DeprecatedKindField, "shell_credentials", "give the workspace an egress route with a credential; no credential reaches a shell", "shell_credentials"},
	{DeprecatedKindBundle, "sandbox", "select the dev-workspace bundle, whose dev/exec runs the same envelope in a workspace on a workspace worker", "the-builder-sidecar"},
	{DeprecatedKindBundle, "dev-exec", "select the dev-workspace bundle, whose dev/exec runs the same envelope in a workspace on a workspace worker", "the-builder-sidecar"},
}

// Deprecations is the table, a copy.
func Deprecations() []Deprecation { return slices.Clone(deprecations) }

// DeprecatedTool is the row for a tool name an agent's tools list holds, if
// it is deprecated. A removal entry ("-Bash") is not a use.
func DeprecatedTool(name string) (Deprecation, bool) {
	for _, d := range deprecations {
		if d.Kind != DeprecatedKindTool {
			continue
		}
		if name == d.Name || (strings.HasSuffix(d.Name, "__") && strings.HasPrefix(name, d.Name)) {
			return d, true
		}
	}
	return Deprecation{}, false
}

func isDeprecatedEnv(name string) bool {
	_, ok := rowOf(DeprecatedKindEnv, name)
	return ok
}

func rowOf(kind, name string) (Deprecation, bool) {
	for _, d := range deprecations {
		if d.Kind == kind && d.Name == name {
			return d, true
		}
	}
	return Deprecation{}, false
}

// warning is one row's line: what uses it, the row, and the guide's section.
func (d Deprecation) warning(subject string) string {
	return fmt.Sprintf("%s, which will be removed in a future release: %s (see %s#%s)", subject, d.Replacement, MigrationGuide, d.Section)
}

// deprecatedEnvWarnings warns about each deprecated variable set in environ,
// by name only: a value can be a credential's name list or worse.
func deprecatedEnvWarnings(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || value == "" {
			continue
		}
		if d, ok := rowOf(DeprecatedKindEnv, name); ok {
			out = append(out, d.warning(name+" is set"))
		}
	}
	sort.Strings(out)
	return out
}

// deprecationWarnings warns about the deprecated tools and fields cfg's
// agents use, and the deprecated bundles layered. An embedded bundle's own
// agents are the product's, moved onto workspaces by RFC GW P14's bundle
// PRs: what such an agent already held in its bundle is not warned about;
// what an operator's layer adds to it is. Only names are written, never the
// credentials shell_credentials lists.
func deprecationWarnings(cfg *Config, layers []Layer) []string {
	var out []string
	bundled := map[string]map[string]bool{} // agent -> deprecated items its bundle holds
	for _, l := range layers {
		if !l.Embedded || l.Data == nil {
			continue
		}
		if d, ok := rowOf(DeprecatedKindBundle, l.Name); ok {
			out = append(out, d.warning("the "+l.Name+" bundle is selected"))
		}
		for name, items := range deprecatedItemsOfLayer(l.Data) {
			if bundled[name] == nil {
				bundled[name] = map[string]bool{}
			}
			for it := range items {
				bundled[name][it] = true
			}
		}
	}
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		a := cfg.Agents[name]
		for _, tool := range a.Tools {
			d, ok := DeprecatedTool(tool)
			if !ok || bundled[name][tool] {
				continue
			}
			out = append(out, d.warning(fmt.Sprintf("agent %q names %s", name, tool)))
		}
		if a.ShellCredentials != nil && !bundled[name]["shell_credentials"] {
			d, _ := rowOf(DeprecatedKindField, "shell_credentials")
			out = append(out, d.warning(fmt.Sprintf("agent %q sets shell_credentials", name)))
		}
	}
	return out
}

// deprecatedItemsOfLayer reads a layer's agents and the deprecated tools and
// fields each names. A layer that does not parse names none: the merge has
// already read it, so it parses.
func deprecatedItemsOfLayer(data []byte) map[string]map[string]bool {
	var doc struct {
		Agents map[string]struct {
			Tools            []string  `yaml:"tools"`
			ShellCredentials yaml.Node `yaml:"shell_credentials"`
		} `yaml:"agents"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	out := map[string]map[string]bool{}
	for name, a := range doc.Agents {
		items := map[string]bool{}
		for _, tool := range a.Tools {
			if _, ok := DeprecatedTool(tool); ok {
				items[tool] = true
			}
		}
		if !a.ShellCredentials.IsZero() {
			items["shell_credentials"] = true
		}
		if len(items) > 0 {
			out[name] = items
		}
	}
	return out
}
