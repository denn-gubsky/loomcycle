// Package claudeimport is the v1.x RFC C2 Claude Code repo ingestion
// walker. It reads a `.claude/` directory (Anthropic's IDE convention)
// and emits loomcycle yaml — agents into `agents:`, MCP servers into
// `mcp_servers:`, skills into `skills/<name>/SKILL.md`. Slash commands
// (`.claude/commands/<name>.md`) are surfaced in the report as skipped
// with a pointer to RFC B (Claude Code plugin) for IDE-side UX.
//
// The walker is the read side of a one-way contract: `.claude/` is
// IDE-as-truth for authoring, loomcycle consumes. There is no reverse
// export. See `~/work/loomcycle-internal/doc-internal/rfcs/claude-code-import.md`
// for the locked design + sharp edges.
//
// Wire layering: this package depends on `internal/recipes` (RFC C1)
// for the MCP-recipe match path and the `--emit-recipes` overlay
// write. Everything else is stdlib + yaml.v3.
package claudeimport

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ImportReport is the typed result of a Walk. The CLI renders it in
// one of three shapes (text dry-run, --report-only summary, --dry-run
// --diff yaml). All slices are sorted lexicographically at Walk-time
// so the rendering is deterministic.
type ImportReport struct {
	// Root is the absolute path of the .claude/ directory the walker
	// consumed. Echoed in the report header so the operator sees what
	// was read.
	Root string `json:"root"`

	// Agents holds one entry per .claude/agents/<name>.md file.
	Agents []*AgentEntry `json:"agents,omitempty"`

	// Skills holds one entry per .claude/skills/<name>/SKILL.md file.
	Skills []*SkillEntry `json:"skills,omitempty"`

	// MCPServers holds one entry per top-level server in
	// .claude/mcp.json AND each per-project <root>/.mcp.json.
	MCPServers []*MCPEntry `json:"mcp_servers,omitempty"`

	// Skipped is the list of files the walker deliberately skipped
	// (currently: .claude/commands/<name>.md). Each entry carries the
	// reason + a pointer to the operator-facing alternative.
	Skipped []*SkippedFile `json:"skipped,omitempty"`

	// Unmapped is the long tail of frontmatter / JSON fields the
	// walker recognised but couldn't translate. One entry per (file,
	// field) pair. Per RFC: lossy import is loud.
	Unmapped []*UnmappedField `json:"unmapped,omitempty"`

	// Warnings is a free-form list of operator-actionable findings
	// (e.g. multi-file skills, env-var allowlist gaps, recipe-match
	// rewrites). One line each.
	Warnings []string `json:"warnings,omitempty"`
}

// AgentEntry is one .claude/agents/<name>.md mapping result.
type AgentEntry struct {
	Name         string `json:"name"`
	SourcePath   string `json:"source_path"`
	YAMLFragment string `json:"yaml_fragment"`

	// V0_12_7_Heuristics records which post-v0.12.7 substrate-field
	// heuristics fired for this agent (informational; the comments /
	// stubs already appear inside YAMLFragment).
	V0_12_7_Heuristics []string `json:"v0_12_7_heuristics,omitempty"`
}

// SkillEntry is one .claude/skills/<name>/SKILL.md mapping result.
type SkillEntry struct {
	Name             string   `json:"name"`
	SourcePath       string   `json:"source_path"`
	DestinationPath  string   `json:"destination_path"`
	MultiFile        bool     `json:"multi_file,omitempty"`
	SupplementaryAny []string `json:"supplementary_files,omitempty"`
}

// MCPEntry is one server mapped from .claude/mcp.json or per-project
// <root>/.mcp.json. RecipeMatch is set when the package matched an
// entry in the C1 recipe library.
type MCPEntry struct {
	Name         string `json:"name"`
	SourcePath   string `json:"source_path"`
	Transport    string `json:"transport"`
	RecipeMatch  string `json:"recipe_match,omitempty"`
	RecipeSource string `json:"recipe_source,omitempty"` // "bundled" / "overlay"

	// YAMLFragment is the yaml shape the importer would emit for this
	// entry's `mcp_servers.<name>:` block. Populated for both literal
	// ports and recipe rewrites. Empty when --emit-recipes --no-yaml
	// suppresses yaml emission entirely.
	YAMLFragment string `json:"yaml_fragment,omitempty"`

	// EmitRecipePath is the destination under
	// $LOOMCYCLE_MCP_RECIPES_ROOT when --emit-recipes is active.
	EmitRecipePath string `json:"emit_recipe_path,omitempty"`
	EmitRecipeJSON string `json:"emit_recipe_json,omitempty"`

	// EnvVarRewrites lists the per-env-var rewrites the walker did
	// (e.g. GITHUB_TOKEN → LOOMCYCLE_GITHUB_TOKEN).
	EnvVarRewrites []string `json:"env_var_rewrites,omitempty"`

	// recipe is the typed *recipes.Recipe the walker built (literal-
	// port OR matched-recipe-with-operator-name). Unexported so json
	// marshal drops it; consumed by WriteMCPToConfig under --write.
	// Stored as `any` so the report.go file doesn't need to import
	// the recipes package (which would create a tighter coupling than
	// necessary — mcp.go is the only consumer that needs the type).
	recipe any `json:"-"`
}

// SkippedFile is one deliberately-skipped path.
type SkippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// UnmappedField is one field the walker recognised but couldn't map.
type UnmappedField struct {
	SourcePath string `json:"source_path"`
	Field      string `json:"field"`
	Hint       string `json:"hint,omitempty"`
}

// Render writes a human-readable summary of the report to a strings.Builder.
// The "text" format mirrors the CLI's default dry-run output.
func (r *ImportReport) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "loomcycle import claude-code — dry-run report\n")
	fmt.Fprintf(&b, "  root: %s\n\n", r.Root)

	fmt.Fprintf(&b, "AGENTS (%d)\n", len(r.Agents))
	for _, a := range r.Agents {
		fmt.Fprintf(&b, "  %s\n    source: %s\n", a.Name, a.SourcePath)
		for _, h := range a.V0_12_7_Heuristics {
			fmt.Fprintf(&b, "    heuristic: %s\n", h)
		}
	}
	if len(r.Agents) == 0 {
		fmt.Fprintln(&b, "  (none)")
	}

	fmt.Fprintf(&b, "\nSKILLS (%d)\n", len(r.Skills))
	for _, s := range r.Skills {
		fmt.Fprintf(&b, "  %s\n    source: %s\n    dest:   %s\n", s.Name, s.SourcePath, s.DestinationPath)
		if s.MultiFile {
			fmt.Fprintf(&b, "    multi-file: %d supplementary files (not auto-copied)\n", len(s.SupplementaryAny))
		}
	}
	if len(r.Skills) == 0 {
		fmt.Fprintln(&b, "  (none)")
	}

	fmt.Fprintf(&b, "\nMCP SERVERS (%d)\n", len(r.MCPServers))
	for _, m := range r.MCPServers {
		fmt.Fprintf(&b, "  %s [%s]\n    source: %s\n", m.Name, m.Transport, m.SourcePath)
		if m.RecipeMatch != "" {
			fmt.Fprintf(&b, "    REWRITE: matched C1 recipe %q (%s)\n", m.RecipeMatch, m.RecipeSource)
		}
		for _, rw := range m.EnvVarRewrites {
			fmt.Fprintf(&b, "    env-rewrite: %s\n", rw)
		}
		if m.EmitRecipePath != "" {
			fmt.Fprintf(&b, "    emit-recipe: %s\n", m.EmitRecipePath)
		}
	}
	if len(r.MCPServers) == 0 {
		fmt.Fprintln(&b, "  (none)")
	}

	if len(r.Skipped) > 0 {
		fmt.Fprintf(&b, "\nSKIPPED (%d)\n", len(r.Skipped))
		for _, s := range r.Skipped {
			fmt.Fprintf(&b, "  %s\n    reason: %s\n", s.Path, s.Reason)
		}
	}

	if len(r.Unmapped) > 0 {
		fmt.Fprintf(&b, "\nUNMAPPED FIELDS (%d)\n", len(r.Unmapped))
		for _, u := range r.Unmapped {
			fmt.Fprintf(&b, "  %s :: %s\n", u.SourcePath, u.Field)
			if u.Hint != "" {
				fmt.Fprintf(&b, "    hint: %s\n", u.Hint)
			}
		}
	}

	if len(r.Warnings) > 0 {
		fmt.Fprintf(&b, "\nWARNINGS (%d)\n", len(r.Warnings))
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "  %s\n", w)
		}
	}

	return b.String()
}

// Summary produces a one-line per-category count, suitable for
// --report-only output.
func (r *ImportReport) Summary() string {
	return fmt.Sprintf(
		"would import %d agents, %d skills, %d mcp servers; %d files skipped, %d unmapped fields, %d warnings",
		len(r.Agents), len(r.Skills), len(r.MCPServers),
		len(r.Skipped), len(r.Unmapped), len(r.Warnings),
	)
}

// RenderJSON serialises the report as indented JSON.
func (r *ImportReport) RenderJSON() (string, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

// sortAll sorts the report's slices lexicographically for deterministic
// output. Called by Walk before returning.
func (r *ImportReport) sortAll() {
	sort.Slice(r.Agents, func(i, j int) bool { return r.Agents[i].Name < r.Agents[j].Name })
	sort.Slice(r.Skills, func(i, j int) bool { return r.Skills[i].Name < r.Skills[j].Name })
	sort.Slice(r.MCPServers, func(i, j int) bool { return r.MCPServers[i].Name < r.MCPServers[j].Name })
	sort.Slice(r.Skipped, func(i, j int) bool { return r.Skipped[i].Path < r.Skipped[j].Path })
	sort.Slice(r.Unmapped, func(i, j int) bool {
		if r.Unmapped[i].SourcePath != r.Unmapped[j].SourcePath {
			return r.Unmapped[i].SourcePath < r.Unmapped[j].SourcePath
		}
		return r.Unmapped[i].Field < r.Unmapped[j].Field
	})
	sort.Strings(r.Warnings)
}
