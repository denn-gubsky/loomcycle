// Package agents loads `<name>.md` files from a directory into a
// name→Agent registry that the config layer merges into the yaml
// `agents:` map at load time.
//
// Wire model:
//
//  1. Operator points LOOMCYCLE_AGENTS_ROOT at a directory of flat
//     `<name>.md` files. Each file's YAML frontmatter is the agent's
//     base config; the body (after the closing `---`) is the system
//     prompt.
//  2. The yaml `agents:` map remains an OPTIONAL override layer. When
//     a yaml entry exists with the same name, its non-zero fields
//     replace the discovered ones (per-field shallow merge).
//  3. The merged AgentDef goes through the existing
//     resolveSystemPromptFiles → resolveSkills → validate pipeline.
//     No special-case handling at the runtime layer.
//
// File format mirrors Claude Code's agent files so a single MD can
// drive both Claude Code and loomcycle. Standard Claude Code keys
// (name / description / tools / model) are honoured. Loomcycle-
// specific keys (tier / models / effort / max_tokens / skills /
// memory_scopes / memory_quota_bytes / providers /
// system_prompt_file / provider) sit alongside them at the top level.
// Claude Code ignores keys it doesn't know, so MDs stay forward-
// compatible across both consumers.
//
// Tool-list shape: the single `tools` key accepts BOTH Claude Code's
// comma-string (`tools: A, B, C`) and loomcycle's list (`tools: [A, B, C]`).
// One canonical key aligns natively with Claude Code frontmatter.
//
// SECURITY: this package only parses + exposes metadata. Validation
// (Pin XOR Tier, Effort domain, MemoryScopes domain, etc.) and
// skill-subset enforcement (skill.allowed-tools ⊆ agent.tools)
// happen at the config layer post-merge, against the existing rules.
package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Agent is one parsed `<name>.md`.
//
// Field set is structurally a superset of config.AgentDef: every
// AgentDef field has a counterpart here, plus a Description field
// (Claude Code metadata; loomcycle currently ignores it at runtime
// but keeps it for a future agents-listing surface). The loader
// returns these instead of config.AgentDef directly so the
// dependency arrow stays config → agents (config converts on merge).
type Agent struct {
	Name        string
	Description string
	Provider    string
	Model       string
	// Code is the inline code-js orchestrator source (RFC J). Mirrors
	// config.AgentDef.Code; when set and Provider is "code-js", the runtime
	// runs this body instead of reading agent_code/<name>/index.js. Carried
	// here so a .md-discovered code agent and the `hash agent` CLI compute
	// the SAME content_sha256 as the substrate (FromYAMLAgent → AgentContent).
	Code      string
	Tier      string
	Effort    string
	MaxTokens int
	// MaxIterations caps the agent loop at this many provider calls
	// before terminating with stop_reason="max_iterations". 0 means
	// use the loop default (16). Set higher for discovery-style
	// agents (job-searcher, employer-profiler) whose workflow is
	// intrinsically iterative: search → enumerate → fetch → score
	// across many tool calls. Default 16 is too low for those; a
	// 1.09M-input run was observed in production hitting the cap
	// before reaching the final write (2026-05-21).
	MaxIterations int
	// MaxConcurrentChildren caps how many sub-agents this agent may
	// spawn in parallel via Agent.parallel_spawn (v0.11.8+). 0 = use
	// runtime default (builtin.DefaultMaxConcurrentChildren = 4).
	// Sequential Agent.spawn calls are unaffected.
	MaxConcurrentChildren int
	Tools                 []string
	Skills                []string
	SystemPrompt          string
	SystemPromptFile      string
	Providers             []string
	SearchProviders       []string
	Models                map[string][]TierCandidate
	MemoryScopes          []string
	MemoryQuotaBytes      int
	MemoryBackend         string
	// CoreBlocks / InheritCoreBlocks / MemoryInjectMaxTokens / MemoryProtocol
	// mirror config.AgentDef (RFC BL P1). Carried here so an MD-declared agent
	// round-trips these to config at boot AND the `hash agent` CLI computes the
	// SAME content_sha256 as the substrate (which hashes them).
	CoreBlocks            []CoreBlock
	InheritCoreBlocks     bool
	MemoryInjectMaxTokens int
	MemoryProtocol        bool
	MemoryIndexMaxBytes   int
	MemoryRoots           string
	// Channels is the v0.8.4 Channel-tool ACL. Empty Publish /
	// Subscribe = no access on that side.
	Channels AgentChannelACL
	// AgentDefScopes is the v0.8.5 AgentDef-tool capability gate.
	// Closed set: "self" / "descendants" / "named:<name>" / "any".
	// Empty = default-deny.
	AgentDefScopes []string
	// (The SkillDef def-scope gate was removed in RFC BA — skill authoring is
	// governed by the unified `skills:` pattern allowlist, not a def-scope gate.)
	// VolumeDefScopes is the RFC AH Phase 2a VolumeDef-tool capability
	// gate. Closed set: "named:<volume-name>" / "any". Empty =
	// default-deny. No "self" (volumes have no agent identity).
	VolumeDefScopes []string
	// EvaluationScopes is the v0.8.5 Evaluation-tool capability gate.
	// Closed set: "submit_self" / "submit_siblings" /
	// "submit_descendants" / "submit_any" / "read_any". Empty =
	// default-deny.
	EvaluationScopes []string
	// Interruption is the v0.8.16 Interruption-tool ACL (enabled / kinds /
	// max_pending). Mirrors config.AgentDef.Interruption. Carried here (F14)
	// so an MD-declared `interruption:` block round-trips to config at boot
	// AND the `hash agent` CLI computes the SAME content_sha256 as the
	// substrate (which hashes interruption). Zero value = disabled.
	Interruption AgentInterruptionACL
	// Path is the absolute path of the source MD, kept for diagnostic
	// logging (skills/loader.go follows the same convention).
	Path string
}

// AgentChannelACL mirrors config.AgentChannelACL locally so this
// package doesn't import config. The merger in config converts.
//
// json: tags are LOAD-BEARING for the content_sha256 (F14): the
// AgentContent hash includes channels, and the substrate read path
// (FromOverlay) unmarshals the persisted snake_case JSON into this type.
// Without the tags the hash would key on capitalized field names and
// diverge from the substrate write path.
type AgentChannelACL struct {
	Publish   []string `json:"publish,omitempty"   yaml:"publish"`
	Subscribe []string `json:"subscribe,omitempty" yaml:"subscribe"`
}

// AgentInterruptionACL mirrors config.AgentInterruptionACL locally so this
// package doesn't import config. json: tags mirror the snake_case the
// substrate persists (F14 — see AgentChannelACL for why they're
// load-bearing for the content hash).
type AgentInterruptionACL struct {
	Enabled    bool     `json:"enabled,omitempty"     yaml:"enabled"`
	Kinds      []string `json:"kinds,omitempty"       yaml:"kinds"`
	MaxPending int      `json:"max_pending,omitempty" yaml:"max_pending"`
}

// Sampling mirrors config.Sampling locally so the agents package stays
// config-free (config → agents would otherwise cycle). json: tags mirror the
// snake_case the substrate persists and are LOAD-BEARING for content_sha256
// (see AgentChannelACL). Pointers so an unset field omits — and so a meaningful
// temperature:0.0 is distinct from "unset".
type Sampling struct {
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	TopK             *int     `json:"top_k,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	Seed             *int     `json:"seed,omitempty"`
	Stop             []string `json:"stop,omitempty"`
}

// Compaction mirrors config.Compaction locally (the agents package stays
// config-free). json: tags mirror the persisted snake_case and are LOAD-BEARING
// for content_sha256. Pointers so an unset field omits.
type Compaction struct {
	Enabled          *bool   `json:"enabled,omitempty"`
	TargetPercentage *int    `json:"target_percentage,omitempty"`
	KeepLastN        *int    `json:"keep_last_n,omitempty"`
	KeepFirst        *bool   `json:"keep_first,omitempty"`
	AutoCompactAtPct *int    `json:"autocompact_at_pct,omitempty"`
	Model            *string `json:"model,omitempty"`
}

// CoreBlock mirrors config.CoreBlock locally so the agents package stays
// config-free. json: tags mirror the persisted snake_case and are LOAD-BEARING
// for content_sha256 (RFC BL P1 — a fork that changes a core block's
// scope/limit mints a distinct hash). yaml tags parse the MD frontmatter.
type CoreBlock struct {
	Label      string `json:"label" yaml:"label"`
	Scope      string `json:"scope,omitempty" yaml:"scope"`
	LimitBytes int    `json:"limit_bytes,omitempty" yaml:"limit_bytes"`
	ReadOnly   bool   `json:"read_only,omitempty" yaml:"read_only"`
}

// TierCandidate mirrors config.TierCandidate's shape locally so this
// package doesn't import config (which would create a cycle:
// config → agents → config). The merger in config converts these to
// config.TierCandidate when populating AgentDef.Models.
//
// json: tags are LOAD-BEARING for the v0.9.x content_sha256: without
// them, encoding/json falls back to capitalized field names (`Provider`,
// `Model`) and downstream readers expecting lowercase break. Adding the
// tags later would silently invalidate every deployed agent's hash with
// a non-empty `models:` field — see sign_test.go's TierCandidate
// known-vector test for the pin.
type TierCandidate struct {
	Provider string `json:"provider" yaml:"provider"`
	Model    string `json:"model"    yaml:"model"`
}

// Set is a name→Agent registry.
type Set struct {
	agents map[string]*Agent
}

// Get returns the named agent, or (nil, false) if absent. Safe on a
// nil receiver so callers can do `set.Get(name)` without checking
// AgentsRoot first.
func (s *Set) Get(name string) (*Agent, bool) {
	if s == nil {
		return nil, false
	}
	a, ok := s.agents[name]
	return a, ok
}

// Names returns all loaded agent names sorted lexicographically.
// Used by the diagnostic startup log.
func (s *Set) Names() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.agents))
	for n := range s.agents {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// LoadSet walks root, parses every `<name>.md`, and returns the
// populated registry. Empty root returns a non-nil empty Set so
// callers can always Get; a missing root directory is an error
// (almost certainly an operator misconfiguration of LOOMCYCLE_AGENTS_ROOT).
//
// Subdirectories under root are skipped silently — they may be
// auxiliary content (per-agent fixtures, prompt fragments operators
// stage alongside the MDs). Files not ending in `.md` are also
// skipped silently for the same reason.
func LoadSet(root string) (*Set, error) {
	set := &Set{agents: map[string]*Agent{}}
	if root == "" {
		return set, nil
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("agents root %s: %w", root, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("agents root %s: not a directory", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read agents root %s: %w", root, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fileName := e.Name()
		if !strings.HasSuffix(fileName, ".md") {
			continue
		}
		name := strings.TrimSuffix(fileName, ".md")
		// Reject names that could escape the root if a future caller
		// constructs a path from them. Belt-and-braces — ReadDir doesn't
		// return entries with "/" in the name, but nothing stops a
		// creative filename, so we sanity-check.
		if strings.ContainsAny(name, "/\\") || name == "" || name == "." || name == ".." {
			return nil, fmt.Errorf("invalid agent file name %q under %s", fileName, root)
		}
		path := filepath.Join(root, fileName)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		a, err := parseAgent(raw)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		a.Path = path
		// The filename (sans .md) is the canonical address. If the
		// frontmatter declares a different name, that's drift the
		// operator should notice and fix; refusing to load is loud
		// and unambiguous (mirrors the skills loader's check).
		if a.Name != "" && a.Name != name {
			return nil, fmt.Errorf("agent %s: frontmatter name %q != filename %q", path, a.Name, name)
		}
		a.Name = name
		set.agents[name] = a
	}
	return set, nil
}

// frontmatter is the strict-ish set of YAML keys we read from an MD.
//
// Tools field accepts a string OR a list, modelled as `any` and
// post-processed in parseAgent via coerceToolsField. One canonical
// `tools` key serves both the Claude Code comma-string and the
// loomcycle list shape, so MDs stay portable across both consumers.
type frontmatter struct {
	Name                  string                     `yaml:"name"`
	Description           string                     `yaml:"description"`
	Tools                 any                        `yaml:"tools"` // string OR []string (CC comma-string or loomcycle list)
	Provider              string                     `yaml:"provider"`
	Model                 string                     `yaml:"model"`
	Code                  string                     `yaml:"code"` // inline code-js body (RFC J)
	Tier                  string                     `yaml:"tier"`
	Effort                string                     `yaml:"effort"`
	MaxTokens             int                        `yaml:"max_tokens"`
	MaxIterations         int                        `yaml:"max_iterations"`
	MaxConcurrentChildren int                        `yaml:"max_concurrent_children"`
	Skills                []string                   `yaml:"skills"`
	Providers             []string                   `yaml:"providers"`
	SearchProviders       []string                   `yaml:"search_providers"`
	Models                map[string][]TierCandidate `yaml:"models"`
	MemoryScopes          []string                   `yaml:"memory_scopes"`
	MemoryQuotaBytes      int                        `yaml:"memory_quota_bytes"`
	MemoryBackend         string                     `yaml:"memory_backend"`
	CoreBlocks            []CoreBlock                `yaml:"core_blocks"`              // RFC BL P1
	InheritCoreBlocks     bool                       `yaml:"inherit_core_blocks"`      // RFC BL P1
	MemoryInjectMaxTokens int                        `yaml:"memory_inject_max_tokens"` // RFC BL P1
	MemoryProtocol        bool                       `yaml:"memory_protocol"`          // RFC BL P1
	MemoryIndexMaxBytes   int                        `yaml:"memory_index_max_bytes"`   // RFC BL P1
	MemoryRoots           string                     `yaml:"memory_roots"`             // RFC BL P1
	Channels              AgentChannelACL            `yaml:"channels"`
	AgentDefScopes        []string                   `yaml:"agent_def_scopes"`
	VolumeDefScopes       []string                   `yaml:"volume_def_scopes"`
	EvaluationScopes      []string                   `yaml:"evaluation_scopes"`
	Interruption          AgentInterruptionACL       `yaml:"interruption"` // F14: round-trips like channels
	SystemPromptFile      string                     `yaml:"system_prompt_file"`
	// SystemPrompt as an inline frontmatter field is intentionally
	// NOT supported. The body of the MD is the prompt; if you want a
	// pointer to a different file, use system_prompt_file.
}

// parseAgent splits raw bytes into frontmatter + body. The frontmatter
// is delimited by leading "---\n" and a closing "---" line; everything
// after the closing line is the body and becomes Agent.SystemPrompt.
//
// An MD without a leading "---\n" is treated as body-only: name will
// fall back to the filename at the LoadSet layer, and Tools /
// model / etc. all stay zero. This tolerates ad-hoc MD files that
// haven't been written with frontmatter yet.
func parseAgent(raw []byte) (*Agent, error) {
	a := &Agent{}
	text := string(raw)
	// Normalise CRLF to LF for the line-based delimiter scan.
	text = strings.ReplaceAll(text, "\r\n", "\n")

	if !strings.HasPrefix(text, "---\n") {
		a.SystemPrompt = text
		return a, nil
	}
	rest := text[len("---\n"):]
	// Closing delimiter is a line that is exactly "---". We accept
	// either "\n---\n..." or a trailing "\n---" with no body.
	endIdx := strings.Index(rest, "\n---\n")
	bodyOffset := -1
	if endIdx >= 0 {
		bodyOffset = endIdx + len("\n---\n")
	} else if strings.HasSuffix(rest, "\n---") {
		endIdx = len(rest) - len("\n---")
		bodyOffset = len(rest)
	} else {
		return nil, fmt.Errorf("frontmatter has no closing ---")
	}
	fmYAML := rest[:endIdx]
	body := ""
	if bodyOffset < len(rest) {
		body = rest[bodyOffset:]
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(fmYAML), &fm); err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
	}

	a.Name = fm.Name
	a.Description = fm.Description
	a.Provider = fm.Provider
	a.Model = fm.Model
	a.Code = fm.Code
	a.Tier = fm.Tier
	a.Effort = fm.Effort
	a.MaxTokens = fm.MaxTokens
	a.MaxIterations = fm.MaxIterations
	a.MaxConcurrentChildren = fm.MaxConcurrentChildren
	a.Skills = fm.Skills
	a.Providers = fm.Providers
	a.SearchProviders = fm.SearchProviders
	a.Models = fm.Models
	a.MemoryScopes = fm.MemoryScopes
	a.MemoryQuotaBytes = fm.MemoryQuotaBytes
	a.MemoryBackend = fm.MemoryBackend
	a.CoreBlocks = fm.CoreBlocks
	a.InheritCoreBlocks = fm.InheritCoreBlocks
	a.MemoryInjectMaxTokens = fm.MemoryInjectMaxTokens
	a.MemoryProtocol = fm.MemoryProtocol
	a.MemoryIndexMaxBytes = fm.MemoryIndexMaxBytes
	a.MemoryRoots = fm.MemoryRoots
	a.Channels = fm.Channels
	a.AgentDefScopes = fm.AgentDefScopes
	a.VolumeDefScopes = fm.VolumeDefScopes
	a.EvaluationScopes = fm.EvaluationScopes
	a.Interruption = fm.Interruption
	a.SystemPromptFile = fm.SystemPromptFile
	a.SystemPrompt = body

	// The single `tools` key accepts a comma-string OR a list; nil →
	// Tools stays nil (default-deny — matches the existing semantics
	// that an agent without a tools list sees no tools).
	if fm.Tools != nil {
		toolsList, err := coerceToolsField(fm.Tools)
		if err != nil {
			return nil, fmt.Errorf("tools: %w", err)
		}
		a.Tools = toolsList
	}

	return a, nil
}

// coerceToolsField accepts Claude Code's comma-string OR an explicit
// YAML list and normalises to []string. Returns an error for unsupported
// shapes (numbers, maps, etc.) so a typo'd value gets caught at config-
// load rather than silently dropping to nil.
func coerceToolsField(v any) ([]string, error) {
	switch t := v.(type) {
	case string:
		return ParseToolList(t), nil
	case []any:
		out := make([]string, 0, len(t))
		for i, el := range t {
			s, ok := el.(string)
			if !ok {
				return nil, fmt.Errorf("element %d is %T, want string", i, el)
			}
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("expected string or list, got %T", v)
	}
}

// ParseToolList splits a comma-separated tool string ("A, B, C") into
// a trimmed []string. Empty input → empty slice (NOT nil; that
// distinction matters in the merge layer where a non-nil empty list
// signals "explicit empty, override discovered" vs nil "absent").
//
// Exported because the same comma-vs-list duality appears in MCP
// server tools fields and would benefit from one canonical
// implementation; future callers can reuse this without copy-pasting.
func ParseToolList(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
