package config

import (
	"bytes"
	"encoding/json"
	"errors"
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

// RemovedItemsOf lists the removed tools and fields an agent definition
// names, in table order: each tool it lists and each removed field a reader
// of stored JSON found on it (AgentDef.RemovedFields). It is what a run of
// that definition is not given.
func RemovedItemsOf(def AgentDef) []Removal {
	var out []Removal
	for _, r := range removals {
		switch r.Kind {
		case RemovedKindTool:
			if slices.Contains(def.Tools, r.Name) {
				out = append(out, r)
			}
		case RemovedKindField:
			if slices.Contains(def.RemovedFields, r.Name) {
				out = append(out, r)
			}
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
	out = append(out, a.RemovedFields...)
	return out
}

// RemovedFieldsInJSON names the removed fields a stored definition's JSON
// still holds, in table order. A stored row keeps its bytes (and its content
// hash) after the field left every Go type, so its readers ask the raw JSON:
// each sets AgentDef.RemovedFields from this. The key is matched as
// encoding/json matches a field, without case, and in either spelling a row
// was written with: the substrate's snake_case (shell_credentials) and a
// registered agent's Go field name (ShellCredentials). A null value is
// absent: a registered agent's JSON wrote every unset pointer as null.
func RemovedFieldsInJSON(raw []byte) []string {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil
	}
	held := map[string]bool{}
	for k, v := range top {
		if string(bytes.TrimSpace(v)) == "null" {
			continue
		}
		held[strings.ToLower(strings.ReplaceAll(k, "_", ""))] = true
	}
	var out []string
	for _, r := range removals {
		if r.Kind == RemovedKindField && held[strings.ToLower(strings.ReplaceAll(r.Name, "_", ""))] {
			out = append(out, r.Name)
		}
	}
	return out
}

// removedInertMessage is the run-start notice for a removed item a stored
// definition names (InertToolGrants, gate RemovedGate).
func removedInertMessage(r Removal) string {
	what := fmt.Sprintf("`%s` is in this agent's tools but was removed in %s, so it is not given to this run", r.Name, RemovedIn)
	if r.Kind == RemovedKindField {
		what = fmt.Sprintf("`%s` is set on this agent but was removed in %s, so it does nothing", r.Name, RemovedIn)
	}
	return fmt.Sprintf("%s: %s (see %s#%s). Fork the definition without it.", what, r.Replacement, MigrationGuide, r.Section)
}

// RefuseRemovedAuthoring refuses a definition being authored (AgentDef create,
// a fork's overlay, register_agent) that names a removed item: a tool in
// tools, or a removed field among raw's keys (RFC GW P15 Q1). An author can
// fix the request; a stored row cannot, which is why stored rows are only
// told (InertToolGrants). One error names every item.
func RefuseRemovedAuthoring(tools []string, raw []byte) error {
	var lines []string
	for _, r := range removals {
		switch {
		case r.Kind == RemovedKindTool && slices.Contains(tools, r.Name):
			lines = append(lines, r.Line("tools names "+r.Name))
		case r.Kind == RemovedKindField && slices.Contains(RemovedFieldsInJSON(raw), r.Name):
			lines = append(lines, r.Line("the definition sets "+r.Name))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return errors.New(strings.Join(lines, "; "))
}

// WithoutRemovedTools is tools less every removed tool, and the names taken
// out. A fork that only inherits one from the version it forks has it
// stripped (RFC GW P15 Q1).
func WithoutRemovedTools(tools []string) (kept, dropped []string) {
	for _, t := range tools {
		if _, ok := RemovedTool(t); ok {
			dropped = append(dropped, t)
			continue
		}
		kept = append(kept, t)
	}
	return kept, dropped
}
