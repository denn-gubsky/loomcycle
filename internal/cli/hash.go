// hash.go — v0.9.x `loomcycle hash agent|skill <path>` subcommand.
//
// Computes the SHA-256 content signature of a local YAML agent or
// SKILL.md the operator has bundled in their Docker image. Prints the
// hash on stdout (e.g. `sha256:abc...`). The CI step in the operator's
// container build calls this, embeds the resulting hash in the bundle
// metadata, and the runtime container's start-up compares it against
// the deployed loomcycle's `AgentDef verify` op.
//
// Zero runtime, zero store, zero network — just file parsing + the
// same internal/agents.Sign / internal/skills.Sign functions that the
// running loomcycle uses on the server side. The hash on both sides is
// guaranteed identical for matching content.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/skills"
)

// RunHash dispatches `loomcycle hash agent <path>` and `loomcycle hash
// skill <path-or-dir>`. Both verbs print `sha256:<hex>\n` on success.
//
// Exit codes:
//
//	0  — hash printed.
//	2  — usage / parse error (no file, bad path, malformed frontmatter).
//	1  — IO error (path unreadable).
func RunHash(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: loomcycle hash agent <path-to-md>")
		fmt.Fprintln(stderr, "       loomcycle hash skill <path-to-dir-or-SKILL.md>")
		return 2
	}
	switch args[0] {
	case "agent":
		return runHashAgent(args[1:], stdout, stderr)
	case "skill":
		return runHashSkill(args[1:], stdout, stderr)
	default:
		return fail(stderr, "unknown hash verb %q (want \"agent\" or \"skill\")", args[0])
	}
}

func runHashAgent(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hash agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage: loomcycle hash agent <path-to-md>")
		return 2
	}
	path := fs.Arg(0)
	abs, err := filepath.Abs(path)
	if err != nil {
		return fail(stderr, "resolve path: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return fail(stderr, "stat %s: %v", abs, err)
	}

	// agents.Load takes a DIRECTORY. To hash one file we point Load at
	// the parent dir and pick out the agent whose Path matches. This
	// keeps the file-parsing logic in one place (no parallel MD parser).
	dir := filepath.Dir(abs)
	set, err := agents.LoadSet(dir)
	if err != nil {
		return fail(stderr, "parse agent dir %s: %v", dir, err)
	}
	wantName := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	agent, ok := set.Get(wantName)
	if !ok {
		return fail(stderr, "loaded %d agent(s) from %s but none named %q", len(set.Names()), dir, wantName)
	}
	if filepath.Clean(agent.Path) != filepath.Clean(abs) {
		// Two MDs declaring the same `name:` frontmatter would land
		// here. Loader picks one deterministically; flag the surprise.
		fmt.Fprintf(stderr, "warning: target path %s; loader picked %s\n", abs, agent.Path)
	}

	fmt.Fprintln(stdout, agents.Sign(agents.FromYAMLAgent(agent)))
	return 0
}

func runHashSkill(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hash skill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage: loomcycle hash skill <path-to-skill-dir-or-SKILL.md>")
		return 2
	}
	path := fs.Arg(0)
	abs, err := filepath.Abs(path)
	if err != nil {
		return fail(stderr, "resolve path: %v", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fail(stderr, "stat %s: %v", abs, err)
	}

	// Accept either:
	//   - A SKILL.md file path → use its parent as the skill dir.
	//   - A directory containing SKILL.md → use that dir directly.
	//   - A skills-root containing one or more <skill>/SKILL.md → load
	//     all + pick the one whose dir-name matches the trailing path.
	skillRoot := abs
	wantName := ""
	if !info.IsDir() {
		base := filepath.Base(abs)
		if base != "SKILL.md" {
			return fail(stderr, "%s: skill file must be named SKILL.md (got %s)", abs, base)
		}
		skillDir := filepath.Dir(abs)
		wantName = filepath.Base(skillDir)
		skillRoot = filepath.Dir(skillDir)
	} else if _, statErr := os.Stat(filepath.Join(abs, "SKILL.md")); statErr == nil {
		// Dir IS a single skill dir.
		wantName = filepath.Base(abs)
		skillRoot = filepath.Dir(abs)
	}

	set, err := skills.LoadSet(skillRoot)
	if err != nil {
		return fail(stderr, "parse skills root %s: %v", skillRoot, err)
	}
	if wantName == "" {
		return fail(stderr, "loaded %d skill(s) from %s but couldn't infer the target skill name from %s", len(set.Names()), skillRoot, abs)
	}
	skill, ok := set.Get(wantName)
	if !ok {
		return fail(stderr, "loaded %d skill(s) from %s but none named %q", len(set.Names()), skillRoot, wantName)
	}

	fmt.Fprintln(stdout, skills.Sign(skills.FromSkill(skill)))
	return 0
}
