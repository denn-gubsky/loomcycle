// Package skills loads .claude/skills/<name>/SKILL.md files into a
// name→Skill registry that the config layer bundles into agent system
// prompts at load time.
//
// Wire model (Approach A in doc-internal/skills-design.md):
//
//  1. Operator points LOOMCYCLE_SKILLS_ROOT at a directory containing
//     one subdirectory per skill, each with a SKILL.md file.
//  2. Each agent's YAML lists `skills: [name1, name2]`. At config-load,
//     the named skill bodies are concatenated onto the agent's
//     system_prompt.
//  3. The bundled body lands inside the cacheable system block at the
//     provider layer, so subsequent runs replay the skill at
//     cache-read rates.
//
// Approach A trades cache effectiveness for static behaviour: the
// bundled set is fixed at config-load. Dynamic discovery (Approach B —
// a built-in Skill tool the model invokes on demand) is the v0.4.0
// follow-on for self-developing agents that need to pick skills at
// runtime. See PLAN.md and skills-design.md.
//
// SECURITY (intersection enforcement, applied by the config layer, not
// here): a skill's `allowed-tools` frontmatter must be a subset of the
// agent's `allowed_tools` YAML field. A skill may NEVER widen the
// agent's tool set. The config layer (resolveSkills) refuses to load if
// any skill demands a tool the agent doesn't grant. This package's job
// is only to parse and expose the metadata.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skill is one parsed SKILL.md.
type Skill struct {
	// Name is the skill's identifier — what an agent's `skills:` YAML
	// field references. Sourced from the directory name; the
	// frontmatter `name:` field, if present, must agree.
	Name string
	// Description is informational; surfaced for the dynamic Skill tool
	// (Approach B, v0.4.0) so the model can decide which skill to load.
	// In Approach A it is unused at runtime but kept for parity.
	Description string
	// AllowedTools is the skill's declared tool requirement. The config
	// layer validates this is a subset of the bundling agent's
	// allowed_tools. Empty = the skill needs no tools (its body is
	// pure prompt guidance).
	AllowedTools []string
	// Body is the markdown after the closing frontmatter `---`. The
	// config layer concatenates this onto the agent's system_prompt.
	// Trailing whitespace is preserved (some skills use a final newline
	// as their handoff signal).
	Body string
	// Path is the absolute path of the source SKILL.md, kept for
	// diagnostic logging.
	Path string
}

// Set is a name→Skill registry.
type Set struct {
	skills map[string]*Skill
}

// Get returns the named skill, or (nil, false) if absent. Safe on nil
// receiver so callers can do `set.Get(name)` without checking SkillsRoot
// first.
func (s *Set) Get(name string) (*Skill, bool) {
	if s == nil {
		return nil, false
	}
	sk, ok := s.skills[name]
	return sk, ok
}

// Names returns all loaded skill names sorted lexicographically. Used
// by the diagnostic startup log.
func (s *Set) Names() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.skills))
	for n := range s.skills {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// LoadSet walks root, parses every <name>/SKILL.md, and returns the
// populated registry. Empty root returns a non-nil empty Set so callers
// can always Get(); a missing root directory is an error (it almost
// certainly means the operator misconfigured LOOMCYCLE_SKILLS_ROOT).
//
// Subdirectories without a SKILL.md are skipped silently — they may be
// auxiliary content (e.g. a `references/` folder a skill body links to)
// that operators stage alongside the skill itself.
func LoadSet(root string) (*Set, error) {
	set := &Set{skills: map[string]*Skill{}}
	if root == "" {
		return set, nil
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("skills root %s: %w", root, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("skills root %s: not a directory", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read skills root %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Reject names that could escape the root if a future caller
		// constructs a path from them. Belt-and-braces — ReadDir
		// doesn't return entries with "/" in the name, but nothing
		// stops a creative filename so we sanity-check.
		if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
			return nil, fmt.Errorf("invalid skill name %q under %s", name, root)
		}
		path := filepath.Join(root, name, "SKILL.md")
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		sk, err := parseSkill(raw)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		sk.Path = path
		// The directory name is the canonical address. If frontmatter
		// declares a different name, that's drift the operator should
		// notice and fix; refusing to load is loud and unambiguous.
		if sk.Name != "" && sk.Name != name {
			return nil, fmt.Errorf("skill %s: frontmatter name %q != directory name %q", path, sk.Name, name)
		}
		sk.Name = name
		set.skills[name] = sk
	}
	return set, nil
}

// frontmatter is the strict subset of YAML keys we read out of a
// SKILL.md. Hyphenated keys match the Claude Code on-disk convention
// (allowed-tools, not allowed_tools).
type frontmatter struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	AllowedTools []string `yaml:"allowed-tools"`
}

// parseSkill splits raw bytes into frontmatter + body. The frontmatter
// is delimited by leading "---\n" and a closing "---" line; everything
// after the closing line is the body.
//
// A SKILL.md without a leading "---\n" is treated as body-only — the
// skill name will fall back to its directory at the LoadSet layer.
// This tolerates ad-hoc skill files that haven't been written with
// frontmatter yet.
func parseSkill(raw []byte) (*Skill, error) {
	sk := &Skill{}
	text := string(raw)
	// Normalise CRLF to LF for parsing; preserves byte semantics enough
	// for our line-based delimiter scan.
	text = strings.ReplaceAll(text, "\r\n", "\n")

	if !strings.HasPrefix(text, "---\n") {
		sk.Body = text
		return sk, nil
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
	sk.Name = fm.Name
	sk.Description = fm.Description
	sk.AllowedTools = fm.AllowedTools
	sk.Body = body
	return sk, nil
}
