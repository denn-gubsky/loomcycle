package claudeimport

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// claudeAgentFrontmatter is the subset of .claude/agents/<name>.md
// frontmatter the importer recognises. Unmapped fields are surfaced
// in the report (see knownClaudeAgentFields below).
type claudeAgentFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Model       string `yaml:"model"`
	// Tools is Claude Code's shape: either a comma-string OR []string.
	// We accept any here and coerce in agentToFragment.
	Tools any `yaml:"tools"`
	// Some .claude/agents/<name>.md files use "allowed_tools" instead
	// (loomcycle-shape mirroring Claude Code's tools field). Accept
	// both; allowed_tools wins on conflict.
	AllowedTools []string `yaml:"allowed_tools"`
}

// knownClaudeAgentFields is the closed set of frontmatter keys the
// importer expects to see on .claude/agents/<name>.md. Anything else
// surfaces in the unmapped-fields list with a fixed hint.
var knownClaudeAgentFields = map[string]struct{}{
	"name":          {},
	"description":   {},
	"model":         {},
	"tools":         {},
	"allowed_tools": {},
}

// unmappedFieldHints maps frontmatter keys to an explanatory hint
// (per the RFC's "lossy import is loud" sharp edge). Unknown keys
// get a generic hint pointing to validate.
var unmappedFieldHints = map[string]string{
	"hooks":         "Claude Code-side hooks — loomcycle has no equivalent today.",
	"output_style":  "Claude Code-side UX (e.g. /learning). Not part of loomcycle's agent runtime.",
	"output-style":  "Claude Code-side UX (e.g. /learning). Not part of loomcycle's agent runtime.",
	"temperature":   "Provider-side sampling; loomcycle exposes via tier policy, not per-agent.",
	"top_p":         "Provider-side sampling; loomcycle exposes via tier policy, not per-agent.",
	"subagents":     "Claude Code subagent declarations are not loomcycle's Agent-tool spawn pattern. See rfcs/implemented/agent-tool-fan-out.md.",
	"color":         "Claude Code IDE-side UX. Not part of loomcycle's agent runtime.",
}

// walkAgents enumerates .claude/agents/<name>.md, parses each, and
// appends an AgentEntry to the report. Malformed individual files are
// recorded as warnings rather than aborting the entire walk.
func walkAgents(dir string, report *ImportReport) error {
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
		name := strings.TrimSuffix(e.Name(), ".md")
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("agent %s: read failed: %v", path, err))
			continue
		}
		entry, perFileErr := buildAgentEntry(name, path, raw)
		if perFileErr != nil {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("agent %s: parse failed: %v", path, perFileErr))
			continue
		}
		// Append any unmapped fields surfaced by buildAgentEntry into
		// the report directly so the renderer groups them.
		for _, u := range entry.unmappedFields {
			report.Unmapped = append(report.Unmapped, &UnmappedField{
				SourcePath: path, Field: u.Field, Hint: u.Hint,
			})
		}
		entry.unmappedFields = nil
		report.Agents = append(report.Agents, entry.AgentEntry)
	}
	return nil
}

// agentEntryBuilder bundles the per-agent unmapped fields with the
// AgentEntry so walkAgents can attach them once.
type agentEntryBuilder struct {
	*AgentEntry
	unmappedFields []*UnmappedField
}

// buildAgentEntry parses one .claude/agents/<name>.md file into an
// AgentEntry. Errors are reserved for unrecoverable problems (frontmatter
// completely unparseable); recognised-but-unmapped fields surface via
// the builder's unmappedFields slice.
func buildAgentEntry(name, path string, raw []byte) (*agentEntryBuilder, error) {
	ymlBytes, body, err := splitFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("split frontmatter: %w", err)
	}

	// Parse the frontmatter into a raw map first so we can detect
	// unmapped fields; then again into the typed struct for the
	// fields we care about. Two passes is cheap (frontmatters are
	// tiny) and keeps the struct-tags-driven happy path clean.
	rawFM := map[string]any{}
	if len(bytes.TrimSpace(ymlBytes)) > 0 {
		if err := yaml.Unmarshal(ymlBytes, &rawFM); err != nil {
			return nil, fmt.Errorf("parse frontmatter: %w", err)
		}
	}
	var fm claudeAgentFrontmatter
	if len(bytes.TrimSpace(ymlBytes)) > 0 {
		if err := yaml.Unmarshal(ymlBytes, &fm); err != nil {
			return nil, fmt.Errorf("parse frontmatter (typed): %w", err)
		}
	}

	// Resolve the tool list: allowed_tools wins; otherwise coerce
	// Claude Code's comma-string OR []string into []string.
	tools := fm.AllowedTools
	if len(tools) == 0 && fm.Tools != nil {
		tools = coerceToolsField(fm.Tools)
	}

	// Substrate-field heuristics (v0.12.7).
	heuristics := []string{}

	// 1. Credentials comment: any mcp__<server>__ tool implies the
	//    agent needs per-user credentials for that server. Extract
	//    the <server> token (the part after the first `__`).
	credServers := extractMCPCredentialServers(tools)
	if len(credServers) > 0 {
		heuristics = append(heuristics,
			fmt.Sprintf("credentials comment for %s (mcp__ tools present)",
				strings.Join(credServers, ", ")))
	}

	// 2. Scheduler scope stub: *-scheduler / *-orchestrator / *-scheduling.
	emitScheduleScopes := false
	if matchesScheduler(name) {
		emitScheduleScopes = true
		heuristics = append(heuristics,
			"schedule_def_scopes stub (name matches scheduler pattern)")
	}

	// 3. Evolver scope stub: *-evolver / *-meta-* / *-author.
	emitAgentDefScopes := false
	if matchesEvolver(name) {
		emitAgentDefScopes = true
		heuristics = append(heuristics,
			"agent_def_scopes stub (name matches evolver pattern)")
	}

	yamlFragment := buildAgentYAML(name, &fm, tools, credServers,
		emitScheduleScopes, emitAgentDefScopes, body)

	// Surface unmapped frontmatter fields. We compare against the
	// known set; everything else gets an entry. The frontmatter has
	// already been parsed, so we know the keys.
	var unmapped []*UnmappedField
	keys := make([]string, 0, len(rawFM))
	for k := range rawFM {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, known := knownClaudeAgentFields[k]; known {
			continue
		}
		hint := unmappedFieldHints[k]
		if hint == "" {
			hint = "no loomcycle equivalent; field dropped on import. " +
				"Run loomcycle validate after --write to confirm yaml validity."
		}
		unmapped = append(unmapped, &UnmappedField{
			SourcePath: path, Field: k, Hint: hint,
		})
	}

	return &agentEntryBuilder{
		AgentEntry: &AgentEntry{
			Name:               name,
			SourcePath:         path,
			YAMLFragment:       yamlFragment,
			V0_12_7_Heuristics: heuristics,
		},
		unmappedFields: unmapped,
	}, nil
}

// coerceToolsField accepts Claude Code's comma-string OR an explicit
// []string and returns []string. Whitespace around comma-separated
// entries is trimmed; empties are dropped.
func coerceToolsField(v any) []string {
	switch val := v.(type) {
	case string:
		parts := strings.Split(val, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(val))
		for _, p := range val {
			if s, ok := p.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(val))
		for _, p := range val {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

// extractMCPCredentialServers returns the sorted unique list of
// MCP server names referenced by mcp__<server>__<tool> entries in
// the tool list. The result is used to emit the `# credentials:`
// comment in the agent's yaml fragment.
func extractMCPCredentialServers(tools []string) []string {
	seen := map[string]struct{}{}
	for _, t := range tools {
		if !strings.HasPrefix(t, "mcp__") {
			continue
		}
		rest := t[len("mcp__"):]
		// Server name is the token before the next `__`.
		if i := strings.Index(rest, "__"); i > 0 {
			seen[rest[:i]] = struct{}{}
			continue
		}
		// `mcp__<server>` (no tool suffix) — accept as bare server.
		if rest != "" {
			seen[rest] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// matchesScheduler returns true when the agent name matches one of the
// scheduler-shaped patterns the importer treats as schedule_def_scopes
// candidates. Default-deny everywhere else (operators opt in manually).
func matchesScheduler(name string) bool {
	low := strings.ToLower(name)
	switch {
	case strings.HasSuffix(low, "-scheduler"),
		strings.HasSuffix(low, "-orchestrator"),
		strings.HasSuffix(low, "-scheduling"),
		strings.Contains(low, "scheduler-"),
		strings.Contains(low, "orchestrator-"):
		return true
	}
	return false
}

// matchesEvolver returns true when the agent name matches one of the
// evolver-shaped patterns the importer treats as agent_def_scopes
// candidates.
func matchesEvolver(name string) bool {
	low := strings.ToLower(name)
	switch {
	case strings.HasSuffix(low, "-evolver"),
		strings.HasSuffix(low, "-author"),
		strings.Contains(low, "-meta-"),
		strings.HasPrefix(low, "meta-"):
		return true
	}
	return false
}

// buildAgentYAML emits the `<name>:` block as a string. We hand-format
// instead of going through yaml.Marshal because the substrate-field
// heuristics need to appear as YAML comments (the `# credentials:`
// line in particular), and yaml.v3's encoder doesn't expose a way to
// attach comments to a mapping value cleanly. The output is parsed
// back through yaml at config-load time, so any indentation drift
// would surface immediately.
func buildAgentYAML(name string, fm *claudeAgentFrontmatter,
	tools, credServers []string,
	emitScheduleScopes, emitAgentDefScopes bool, body []byte) string {
	var b strings.Builder
	// description: loomcycle has no `description:` field on AgentDef;
	// surface the authoring intent as a yaml comment above the block
	// so it's preserved without lying about the schema.
	if fm.Description != "" {
		fmt.Fprintf(&b, "# description: %s\n", strings.ReplaceAll(fm.Description, "\n", " "))
	}
	if len(credServers) > 0 {
		fmt.Fprintf(&b, "# credentials: %s\n", strings.Join(credServers, ", "))
		fmt.Fprintln(&b, "# (each agent run should populate user_credentials with these keys; ")
		fmt.Fprintln(&b, "#  see rfcs/implemented/per-run-credentials.md for the substitution pattern)")
	}
	fmt.Fprintf(&b, "%s:\n", name)
	if fm.Model != "" {
		fmt.Fprintf(&b, "  model: %s\n", fm.Model)
	}
	if len(tools) > 0 {
		fmt.Fprintln(&b, "  allowed_tools:")
		for _, t := range tools {
			fmt.Fprintf(&b, "    - %s\n", t)
		}
	}
	if emitScheduleScopes {
		fmt.Fprintln(&b, "  # schedule_def_scopes: name matched scheduler pattern; see")
		fmt.Fprintln(&b, "  # rfcs/implemented/scheduled-agent-runs.md. Tighten to a")
		fmt.Fprintln(&b, "  # narrower scope (e.g. [\"named:foo\"]) if appropriate.")
		fmt.Fprintln(&b, "  schedule_def_scopes: [\"any\"]")
	}
	if emitAgentDefScopes {
		fmt.Fprintln(&b, "  # agent_def_scopes: name matched evolver pattern. \"self\" is the")
		fmt.Fprintln(&b, "  # safer floor; widen to \"descendants\" / \"any\" only if intended.")
		fmt.Fprintln(&b, "  agent_def_scopes: [\"self\"]")
	}
	if len(body) > 0 {
		fmt.Fprintln(&b, "  system_prompt: |")
		for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	return b.String()
}
