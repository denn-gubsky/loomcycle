package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"gopkg.in/yaml.v3"
)

// What RFC GW §9 moved out of the core and v1.172.0 removed: the Bash tool,
// Bashbox and its host fallback, the shell_credentials field, the variables
// that configured them, and the sandbox and dev-exec bundles. Workspaces
// replace them (RFC GW P1–P13).
//
// The table is the one place the words live: every surface reads it. An
// operator's config that still names a row stops the start with the line to
// change (env_retired.go for the variables, removedUseErrors for the rest);
// a stored definition that names one runs without it and is told so
// (InertToolGrants), and is counted by the P14 surfaces, which keep their
// "deprecat…" names (RFC GW P15 Q3). Its sections are headings of
// MigrationGuide.

// MigrationGuide is the guide every line links, from the repository root.
const MigrationGuide = "docs/MIGRATING-FROM-BASH.md"

// RemovedIn is the release that removed the table's rows.
const RemovedIn = "v1.172.0"

// The kinds of removed thing.
const (
	RemovedKindEnv    = "env"
	RemovedKindTool   = "tool"
	RemovedKindField  = "field"
	RemovedKindBundle = "bundle"
)

// RemovedGate is the capability_inert gate of a removed item a stored
// definition still names.
const RemovedGate = "removed"

// Removal is one thing v1.172.0 removed and what replaces it.
type Removal struct {
	Kind string
	// Name is the variable, tool, field or bundle.
	Name string
	// Replacement completes "… was removed in v1.172.0: ".
	Replacement string
	// Section is the guide's heading, as a link fragment.
	Section string
}

// removals has no mcp__sandbox__ row (P15 Q2): the builder sidecar is gone,
// but a server an operator calls `sandbox` is an ordinary MCP server.
var removals = []Removal{
	{RemovedKindEnv, "LOOMCYCLE_BASH_ENABLED", "turn on workspaces (workspace.mode) and give the agent the Workspace tool with a workspace grant", "environment-variables"},
	{RemovedKindEnv, "LOOMCYCLE_BASH_ALLOWED_CREDS", "give the workspace an egress route with a credential; the core adds it and no shell sees it", "environment-variables"},
	{RemovedKindEnv, "LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS", "run the commands in a workspace, on a worker that carries their toolchain", "environment-variables"},
	{RemovedKindEnv, "LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV", "run the commands in a workspace, on a worker that carries their toolchain", "environment-variables"},
	{RemovedKindEnv, "LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS", "give the workspace an egress route with a credential, or open a git workspace with the credential", "environment-variables"},
	{RemovedKindEnv, "LOOMCYCLE_BASHBOX_OVERLAY_MAX_BYTES", "nothing: a workspace has a disk quota on its worker instead", "environment-variables"},
	{RemovedKindTool, "Bash", "use the Workspace tool, which runs commands on a workspace worker", "the-bash-tool"},
	{RemovedKindTool, "Bashbox", "use the Workspace tool, which runs commands on a workspace worker", "bashbox"},
	{RemovedKindField, "shell_credentials", "give the workspace an egress route with a credential; no credential reaches a shell", "shell_credentials"},
	{RemovedKindBundle, "sandbox", "select the dev-workspace bundle, whose dev/exec runs the same envelope in a workspace on a workspace worker", "the-builder-sidecar"},
	{RemovedKindBundle, "dev-exec", "select the dev-workspace bundle, whose dev/exec runs the same envelope in a workspace on a workspace worker", "the-builder-sidecar"},
}

// Removals is the table, a copy.
func Removals() []Removal { return slices.Clone(removals) }

// RemovedTool is the row for a tool name an agent's tools list holds, if it
// was removed. A removal entry ("-Bash") is not a use.
func RemovedTool(name string) (Removal, bool) { return removedRow(RemovedKindTool, name) }

// RemovedField is the row for an agent field, if it was removed.
func RemovedField(name string) (Removal, bool) { return removedRow(RemovedKindField, name) }

func removedRow(kind, name string) (Removal, bool) {
	for _, r := range removals {
		if r.Kind == kind && r.Name == name {
			return r, true
		}
	}
	return Removal{}, false
}

// Line is the row's sentence about subject: what was removed, what replaces
// it, and the guide's section.
func (r Removal) Line(subject string) string {
	return fmt.Sprintf("%s, which was removed in %s: %s (see %s#%s)", subject, RemovedIn, r.Replacement, MigrationGuide, r.Section)
}

// Instead is the row's text for a removed variable's retired-env entry: it
// completes "NAME: " and "NAME is set but no longer read: ".
func (r Removal) Instead() string {
	return fmt.Sprintf("removed in %s: %s (see %s#%s)", RemovedIn, r.Replacement, MigrationGuide, r.Section)
}

// removedEnvInstead is Instead for a variable row of the table. Only the
// table's own names reach it (env_retired.go's rows).
func removedEnvInstead(name string) string {
	r, ok := removedRow(RemovedKindEnv, name)
	if !ok {
		panic("unreachable: " + name + " is not a removed variable")
	}
	return r.Instead()
}

// removedUseErrors lists every use of a removed tool or field in the
// operator's layers (every layer not compiled into the binary) and in the
// markdown agents, one line each, sorted. The field is gone from AgentDef, so
// only the raw layer shows it was set. Only names are written, never the
// credentials shell_credentials listed.
func removedUseErrors(layers []Layer, mdUses map[string][]string) []string {
	var out []string
	for _, l := range layers {
		if l.Embedded || l.Data == nil {
			continue
		}
		for name, items := range removedItemsOfLayer(l.Data) {
			for _, it := range items {
				out = append(out, removedUseLine("agent "+fmt.Sprintf("%q", name)+" in "+l.Name, it))
			}
		}
	}
	for name, items := range mdUses {
		for _, it := range items {
			out = append(out, removedUseLine(fmt.Sprintf("markdown agent %q", name), it))
		}
	}
	sort.Strings(out)
	return out
}

func removedUseLine(who, item string) string {
	if r, ok := RemovedTool(item); ok {
		return r.Line(who + ": tools names " + item)
	}
	r, _ := RemovedField(item)
	return r.Line(who + " sets " + item)
}

// removedItemsOfLayer reads a layer's agents and the removed tools and fields
// each names, in the order the agent lists them. A layer that does not parse
// names none: the merge has already read it, so it parses.
func removedItemsOfLayer(data []byte) map[string][]string {
	var doc struct {
		Agents map[string]struct {
			Tools            []string  `yaml:"tools"`
			ShellCredentials yaml.Node `yaml:"shell_credentials"`
		} `yaml:"agents"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	out := map[string][]string{}
	for name, a := range doc.Agents {
		var items []string
		for _, tool := range a.Tools {
			if _, ok := RemovedTool(tool); ok {
				items = append(items, tool)
			}
		}
		if !a.ShellCredentials.IsZero() {
			items = append(items, "shell_credentials")
		}
		if len(items) > 0 {
			out[name] = items
		}
	}
	return out
}

// isRemovedEnv reports whether name is a removed variable.
func isRemovedEnv(name string) bool {
	_, ok := removedRow(RemovedKindEnv, name)
	return ok
}

// joinRemoved is the load error for removed uses.
func joinRemoved(uses []string) error {
	return fmt.Errorf("the config names what %s removed; change each line, then start again: %s", RemovedIn, strings.Join(uses, " | "))
}

// removedItemsOfMarkdownAgent lists the removed tools and fields a markdown
// agent's front matter names, in the order it lists them.
func removedItemsOfMarkdownAgent(a *agents.Agent) []string {
	var out []string
	for _, tool := range a.Tools {
		if _, ok := RemovedTool(tool); ok {
			out = append(out, tool)
		}
	}
	if a.ShellCredentials != nil {
		out = append(out, "shell_credentials")
	}
	return out
}
