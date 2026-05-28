// Package cli's import.go owns the `loomcycle import claude-code`
// subcommand family. The walker logic lives in
// internal/claudeimport/; this file is the CLI thin-shell —
// argument parsing, output rendering, and the --write apply path.
//
// Subverb shape mirrors RunMigrate: RunImport dispatches on
// args[0]. Only `claude-code` is implemented today; the shape
// leaves room for future `import <other-source>` variants
// (e.g. plain `.mcp.json` directories, OpenAPI servers) without
// reshaping the CLI surface.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/claudeimport"
	"github.com/denn-gubsky/loomcycle/internal/recipes"
)

// RunImport dispatches `loomcycle import <subverb> [args]`. Returns
// 0 on success, 1 on operational failure (filesystem I/O, walker
// errors), 2 on user-error (missing flags, unknown verb, bad input).
func RunImport(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: loomcycle import <source> [flags]")
		fmt.Fprintln(stderr, "Sources: claude-code")
		return 2
	}
	switch args[0] {
	case "claude-code":
		return runImportClaudeCode(args[1:], stdout, stderr)
	default:
		return fail(stderr, "unknown import source %q (want: claude-code)", args[0])
	}
}

// runImportClaudeCode owns the `import claude-code` shape: parses
// flags, builds WalkOptions, dispatches to claudeimport.Walk, then
// renders the report or applies the diff per the mode.
func runImportClaudeCode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import claude-code", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		from          = fs.String("from", "", "path to the .claude/ directory to import")
		reportOnly    = fs.Bool("report-only", false, "print inventory summary only (no yaml output)")
		dryRun        = fs.Bool("dry-run", false, "print the yaml diff against --diff target")
		diffTarget    = fs.String("diff", "", "loomcycle.yaml path for --dry-run diff target")
		write         = fs.Bool("write", false, "apply the import to --diff target (or auto-detected loomcycle.yaml)")
		force         = fs.Bool("force", false, "allow clobbering existing entries under --write")
		noRecipeMatch = fs.Bool("no-recipe-match", false, "disable C1 recipe-library rewrite layer")
		emitRecipes   = fs.Bool("emit-recipes", false, "write .mcp.json entries to $LOOMCYCLE_MCP_RECIPES_ROOT")
		noYAML        = fs.Bool("no-yaml", false, "skip mcp_servers yaml emission (use with --emit-recipes)")
		jsonOut       = fs.Bool("json", false, "render report as JSON")
		skillsDest    = fs.String("skills-dest", "", "destination root for SKILL.md copies (default: <cwd>/skills)")
	)
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return 2
	}
	if *from == "" {
		return fail(stderr, "--from=<path-to-.claude/> is required")
	}

	// Resolve skills destination: explicit > <cwd>/skills.
	dest := *skillsDest
	if dest == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return failOp(stderr, "resolve cwd: %v", err)
		}
		dest = cwd + "/skills"
	}

	// Resolve diff target: explicit > "loomcycle.yaml" in cwd.
	target := *diffTarget
	if target == "" {
		target = "loomcycle.yaml"
	}

	overlayRoot := os.Getenv("LOOMCYCLE_MCP_RECIPES_ROOT")
	if *emitRecipes && overlayRoot == "" {
		return fail(stderr, "--emit-recipes requires LOOMCYCLE_MCP_RECIPES_ROOT to be set")
	}
	if *noYAML && !*emitRecipes {
		return fail(stderr, "--no-yaml is only valid with --emit-recipes")
	}

	// Load the C1 recipe library (bundled + overlay if set). The library
	// is read-only here; the importer just consults it for recipe match.
	lib, err := recipes.LoadLibrary(overlayRoot)
	if err != nil {
		// Library-load failure is non-fatal for the walker — fall back
		// to no-recipe-match. Surface a warning to stderr so the
		// operator notices.
		fmt.Fprintf(stderr, "warning: recipe library load failed (%v); proceeding without recipe match\n", err)
		lib = nil
	}

	opts := claudeimport.WalkOptions{
		Library:       lib,
		NoRecipeMatch: *noRecipeMatch,
		EmitRecipes:   *emitRecipes,
		NoYAML:        *noYAML,
		OverlayRoot:   overlayRoot,
		EnvAllowlist:  defaultEnvAllowlist(),
		SkillsDest:    dest,
	}

	report, err := claudeimport.Walk(*from, opts)
	if err != nil {
		return failOp(stderr, "walk %s: %v", *from, err)
	}

	// Output mode dispatch. Order matters: --write supersedes the
	// dry-run rendering; --report-only suppresses the yaml; --dry-run
	// + --diff implies the diff-shape rendering.
	switch {
	case *write:
		return applyImport(report, target, *force, *emitRecipes, stdout, stderr)
	case *reportOnly:
		fmt.Fprintln(stdout, report.Summary())
		return 0
	case *jsonOut:
		out, err := report.RenderJSON()
		if err != nil {
			return failOp(stderr, "render json: %v", err)
		}
		fmt.Fprint(stdout, out)
		return 0
	case *dryRun && *diffTarget != "":
		return renderDryRunDiff(report, *diffTarget, stdout)
	default:
		// Default dry-run report.
		fmt.Fprint(stdout, report.Render())
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, report.Summary())
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "  Re-run with --write to apply, --dry-run --diff=<file> to see the yaml diff,")
		fmt.Fprintln(stdout, "  or --report-only for just the inventory summary.")
		return 0
	}
}

// applyImport executes the --write path: appends agents + mcp servers
// to the target yaml, copies skills to disk, optionally writes the
// emitted recipes overlay. Order-independent — if one apply step
// fails, earlier steps are already committed (mirrors how
// recipes.AppendToConfig works in the C1 CLI).
func applyImport(report *claudeimport.ImportReport, target string,
	force, emitRecipes bool, stdout, stderr io.Writer) int {

	fmt.Fprintf(stdout, "applying to %s\n", target)

	// 1. Skill file copies.
	if written, err := claudeimport.WriteSkillCopies(report, force); err != nil {
		return failOp(stderr, "skill copy: %v", err)
	} else {
		for _, line := range written {
			fmt.Fprintln(stdout, "  "+line)
		}
	}

	// 2. Emit-recipes overlay writes (if --emit-recipes).
	if emitRecipes {
		if written, err := claudeimport.WriteEmittedRecipes(report, force); err != nil {
			return failOp(stderr, "emit-recipes: %v", err)
		} else {
			for _, line := range written {
				fmt.Fprintln(stdout, "  "+line)
			}
		}
	}

	// 3. MCP servers → target yaml's mcp_servers: block (delegated
	//    to claudeimport.WriteMCPToConfig, which calls
	//    recipes.AppendToConfig under the hood).
	if written, err := claudeimport.WriteMCPToConfig(report, target, force); err != nil {
		return failOp(stderr, "%v", err)
	} else {
		for _, line := range written {
			fmt.Fprintln(stdout, "  "+line)
		}
	}

	// 4. Agents → target yaml's agents: block.
	if written, err := claudeimport.WriteAgentsToConfig(report, target, force); err != nil {
		return failOp(stderr, "%v", err)
	} else {
		for _, line := range written {
			fmt.Fprintln(stdout, "  "+line)
		}
	}

	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, report.Summary())
	fmt.Fprintln(stdout, "Done. Run `loomcycle validate "+target+"` to confirm.")
	return 0
}

// renderDryRunDiff prints a yaml-shape preview of what would change
// in target. For each agent + mcp server, we show the fragment
// indented + a header. This is intentionally simple — operators
// run `git diff` after `--write` for the rigorous view.
func renderDryRunDiff(report *claudeimport.ImportReport, target string, stdout io.Writer) int {
	fmt.Fprintf(stdout, "# Dry-run diff against %s\n\n", target)
	if len(report.MCPServers) > 0 {
		fmt.Fprintln(stdout, "# mcp_servers: additions")
		for _, m := range report.MCPServers {
			if m.YAMLFragment == "" {
				continue
			}
			indented := indentLines(m.YAMLFragment, "  ")
			fmt.Fprintln(stdout, indented)
		}
	}
	if len(report.Agents) > 0 {
		fmt.Fprintln(stdout, "# agents: additions")
		for _, a := range report.Agents {
			indented := indentLines(a.YAMLFragment, "  ")
			fmt.Fprintln(stdout, indented)
		}
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "# "+report.Summary())
	return 0
}

func indentLines(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(prefix)
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// defaultEnvAllowlist provides a conservative starting allowlist for
// the env-var rewrite check. Real operators have their own list in
// loomcycle.yaml's env_allowlist; threading that in here would
// require loading the config (which the importer deliberately avoids
// — operators may not have a config yet). The walker's flag is
// advisory; final validation happens when the operator runs
// loomcycle validate after --write.
func defaultEnvAllowlist() map[string]bool {
	return map[string]bool{
		"LOOMCYCLE_ANTHROPIC_API_KEY":  true,
		"LOOMCYCLE_OPENAI_API_KEY":     true,
		"LOOMCYCLE_GEMINI_API_KEY":     true,
		"LOOMCYCLE_DEEPSEEK_API_KEY":   true,
		"LOOMCYCLE_GITHUB_TOKEN":       true,
		"LOOMCYCLE_GITLAB_TOKEN":       true,
		"LOOMCYCLE_SLACK_BOT_TOKEN":    true,
		"LOOMCYCLE_TELEGRAM_BOT_TOKEN": true,
		"LOOMCYCLE_DISCORD_BOT_TOKEN":  true,
		"LOOMCYCLE_NOTION_API_KEY":     true,
		"LOOMCYCLE_TAVILY_API_KEY":     true,
		"LOOMCYCLE_BRAVE_API_KEY":      true,
	}
}
