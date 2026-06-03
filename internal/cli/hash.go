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
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/config"
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
		fmt.Fprintln(stderr, "       loomcycle hash agent --config <path-to-loomcycle.yaml> <name>")
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
	// v0.11.12 — when --config is set, the positional argument is the
	// agent NAME and we look it up in the loomcycle.yaml `agents:` block.
	// This is the natural pre-deploy verify path for operators whose
	// agents live in yaml (not standalone .md files): compute the hash
	// locally, then run `AgentDef.verify` against the deployed loomcycle
	// substrate to check drift before promoting a new agent version.
	cfgPath := fs.String("config", "", "Path to loomcycle.yaml; when set, the positional argument is the agent NAME (not a .md path).")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage: loomcycle hash agent <path-to-md>")
		fmt.Fprintln(stderr, "       loomcycle hash agent --config <path-to-loomcycle.yaml> <name>")
		return 2
	}
	if *cfgPath != "" {
		return runHashAgentByName(fs.Arg(0), *cfgPath, stdout, stderr)
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

// runHashAgentByName loads `loomcycle.yaml` from cfgPath, walks the
// `agents:` block for the named agent, and prints its content_sha256.
//
// The hash matches what the deployed loomcycle's `AgentDef.verify`
// returns IFF the deployed agent was loaded from the same yaml file
// at boot. config.Load applies two boot-time mutations the substrate
// row also carries (resolveSkills bakes skill bodies into
// SystemPrompt; addContextToolDefaults appends "Context" to
// AllowedTools unless `disable_context: true`); we let those run here
// so both paths produce identical AgentContent.
//
// Caveats:
//   - Agents created via `AgentDef set/fork` (substrate-only) have no
//     yaml row — the operator should hash the overlay via
//     `AgentDef get` instead.
//   - If any target agent lists `skills:`, the CLI needs
//     LOOMCYCLE_SKILLS_ROOT pointing at the same skills root the
//     deployed loomcycle uses. config.Load surfaces this as a hard
//     error rather than producing a guaranteed-wrong hash.
//
// Operators run this in their CI before a deploy:
//
//	local=$(loomcycle hash agent --config loomcycle.yaml researcher)
//	remote=$(curl /v1/agentdef -d '{"op":"verify","name":"researcher"}' | jq -r .current_sha256)
//	[ "$local" = "$remote" ] || echo "drift detected"
//
// Conversion: config.AgentDef and agents.Agent carry the same field
// set but live in different packages (agents → config would create a
// circular import). We hand-copy the content-bearing fields. The drift
// test in internal/lookup/agent_test.go catches any future field that
// gets added to AgentContent without a matching addition here.
func runHashAgentByName(name, cfgPath string, stdout, stderr io.Writer) int {
	if name == "" {
		return fail(stderr, "missing agent name")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fail(stderr, "parse config %s: %v", cfgPath, err)
	}
	def, ok := cfg.Agents[name]
	if !ok {
		return fail(stderr, "no agent named %q in %s (have: %s)", name, cfgPath, strings.Join(agentNames(cfg.Agents), ", "))
	}

	// Hand-copy config.AgentDef → agents.Agent. agents.FromYAMLAgent
	// then projects onto agents.AgentContent for the actual signing.
	a := &agents.Agent{
		Name:                  name,
		Provider:              def.Provider,
		Model:                 def.Model,
		Code:                  def.Code,
		Tier:                  def.Tier,
		Effort:                def.Effort,
		MaxTokens:             def.MaxTokens,
		MaxIterations:         def.MaxIterations,
		MaxConcurrentChildren: def.MaxConcurrentChildren,
		AllowedTools:          def.AllowedTools,
		Skills:                def.Skills,
		SystemPrompt:          def.SystemPrompt,
		Providers:             def.Providers,
		Models:                convertConfigModels(def.Models),
		MemoryScopes:          def.MemoryScopes,
		MemoryQuotaBytes:      def.MemoryQuotaBytes,
		MemoryBackend:         def.MemoryBackend,
	}
	fmt.Fprintln(stdout, agents.Sign(agents.FromYAMLAgent(a)))
	return 0
}

// agentNames returns yaml `agents:` keys sorted for stable error
// output. Helps operators spot typos in the agent name they passed.
func agentNames(m map[string]config.AgentDef) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// convertConfigModels translates config.TierCandidate to
// agents.TierCandidate. The two types are structurally identical but
// live in different packages (config and agents can't share a type to
// avoid circular import); this is the conversion seam.
func convertConfigModels(m map[string][]config.TierCandidate) map[string][]agents.TierCandidate {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string][]agents.TierCandidate, len(m))
	for tier, cands := range m {
		dst := make([]agents.TierCandidate, len(cands))
		for i, c := range cands {
			dst[i] = agents.TierCandidate{Provider: c.Provider, Model: c.Model}
		}
		out[tier] = dst
	}
	return out
}
