package claudeimport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/recipes"
)

// WalkOptions controls behaviour of Walk. Defaults are conservative:
// no recipe-match opt-out, no overlay emission, yaml emission on.
type WalkOptions struct {
	// Library is the C1 recipe library used for the MCP recipe-match
	// path. When nil, the recipe-match layer is disabled and every
	// .mcp.json entry literal-ports. Callers should construct via
	// recipes.LoadLibrary(os.Getenv("LOOMCYCLE_MCP_RECIPES_ROOT")).
	Library *recipes.Library

	// NoRecipeMatch disables the recipe-library rewrite layer even
	// when Library is non-nil. Mirrors the CLI's --no-recipe-match.
	NoRecipeMatch bool

	// EmitRecipes triggers the --emit-recipes overlay path. Writes
	// each .mcp.json entry as <name>.json under OverlayRoot in
	// addition to (or, with NoYAML, instead of) the yaml emission.
	EmitRecipes bool

	// NoYAML pairs with EmitRecipes to suppress the mcp_servers yaml
	// emission entirely. Has no effect when EmitRecipes is false.
	NoYAML bool

	// OverlayRoot is the resolved $LOOMCYCLE_MCP_RECIPES_ROOT used
	// for the EmitRecipes path. Required when EmitRecipes is true;
	// Walk returns an error if it's empty.
	OverlayRoot string

	// EnvAllowlist is the operator's currently-allowlisted env-var
	// names (typically the LOOMCYCLE_* set + a few documented third-
	// party names). When non-nil, the mcp walker checks each entry's
	// env vars against the set and reports rewrites for those not
	// covered. nil disables the check.
	EnvAllowlist map[string]bool

	// SkillsDest is where SKILL.md files would be copied under --write.
	// Walk records the destination in each SkillEntry but never writes
	// files itself (that's the CLI's job).
	SkillsDest string
}

// Walk consumes a .claude/ directory and returns a typed ImportReport
// describing what would be imported. No filesystem writes occur — the
// CLI applies the report's plan under --write.
//
// The walker is best-effort: malformed individual files surface as
// warnings + skipped entries rather than aborting the entire walk.
// One bad agent shouldn't lose the whole import.
func Walk(root string, opts WalkOptions) (*ImportReport, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", root, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", abs, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", abs)
	}
	if opts.EmitRecipes && opts.OverlayRoot == "" {
		return nil, fmt.Errorf("--emit-recipes requires LOOMCYCLE_MCP_RECIPES_ROOT to be set")
	}

	report := &ImportReport{Root: abs}

	// .claude/agents/<name>.md
	agentsDir := filepath.Join(abs, "agents")
	if err := walkAgents(agentsDir, report); err != nil {
		return nil, fmt.Errorf("walk agents: %w", err)
	}

	// .claude/skills/<name>/SKILL.md
	skillsDir := filepath.Join(abs, "skills")
	if err := walkSkills(skillsDir, opts.SkillsDest, report); err != nil {
		return nil, fmt.Errorf("walk skills: %w", err)
	}

	// .claude/mcp.json AND <root>/.mcp.json (the per-project pattern)
	if err := walkMCP(abs, opts, report); err != nil {
		return nil, fmt.Errorf("walk mcp: %w", err)
	}

	// .claude/commands/<name>.md → SKIPPED with pointer to RFC B.
	commandsDir := filepath.Join(abs, "commands")
	if err := walkCommands(commandsDir, report); err != nil {
		return nil, fmt.Errorf("walk commands: %w", err)
	}

	report.sortAll()
	return report, nil
}

// walkCommands enumerates .claude/commands/<name>.md and records each
// as a SkippedFile with the standard rationale. Claude Code slash
// commands are IDE-side UX — see RFC B for the plugin-side surfacing.
func walkCommands(dir string, report *ImportReport) error {
	st, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !st.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		report.Skipped = append(report.Skipped, &SkippedFile{
			Path: filepath.Join(dir, e.Name()),
			Reason: "Claude Code slash commands are IDE-side UX, not agent runtime. " +
				"See RFC B (Claude Code plugin) for plugin-side surfacing of /loomcycle-* commands.",
		})
	}
	return nil
}
