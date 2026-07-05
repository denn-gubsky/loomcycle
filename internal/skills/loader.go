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
// agent's `tools` YAML field. A skill may NEVER widen the
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

	"github.com/denn-gubsky/loomcycle/internal/skillmatch"
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
	// Tools is the skill's declared tool requirement. The config
	// layer validates this is a subset of the bundling agent's
	// tools. Empty = the skill needs no tools (its body is
	// pure prompt guidance).
	Tools []string
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

// Add inserts or replaces a skill by its Name. Used to OVERLAY inline
// config-defined skills (the top-level `skills:` map) onto a file-loaded
// Set — an inline definition wins on a name collision with the
// LOOMCYCLE_SKILLS_ROOT directory (config is authoritative). No-op on a
// nil receiver or a nameless skill.
func (s *Set) Add(sk *Skill) {
	if s == nil || sk == nil || sk.Name == "" {
		return
	}
	if s.skills == nil {
		s.skills = map[string]*Skill{}
	}
	s.skills[sk.Name] = sk
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

// LoadSet walks root recursively, parses every SKILL.md it finds, and
// returns the populated registry keyed by the skill's `/`-grouped name.
// Empty root returns a non-nil empty Set so callers can always Get(); a
// missing root directory is an error (it almost certainly means the operator
// misconfigured LOOMCYCLE_SKILLS_ROOT).
//
// RFC BA `/`-grouping: a skill's name is the directory path of its SKILL.md
// RELATIVE to root (`root/doc/redactor/SKILL.md` → `doc/redactor`). Nested
// dirs let operators namespace skills (`doc/*`, `marketing/*`); the flat
// `root/foo/SKILL.md` → `foo` layout still works unchanged. Directories
// without a SKILL.md are traversed (their descendants may hold skills) or
// skipped as auxiliary content (e.g. a `references/` folder a skill links to).
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
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "SKILL.md" {
			return nil
		}
		// name = the SKILL.md's parent directory relative to root, `/`-joined.
		rel, rerr := filepath.Rel(root, filepath.Dir(path))
		if rerr != nil {
			return fmt.Errorf("skills root %s: relativize %s: %w", root, path, rerr)
		}
		if rel == "." {
			// A SKILL.md directly at root has no name — skip.
			return nil
		}
		name := filepath.ToSlash(rel)
		// Escape-prevention + `/`-grammar. rel from a real path under root
		// can't traverse out, but validate anyway so a hand-crafted symlink
		// or odd filename fails loud rather than minting a weird name.
		if verr := skillmatch.ValidateName(name); verr != nil {
			return fmt.Errorf("skills root %s: %w", root, verr)
		}
		raw, rderr := os.ReadFile(path)
		if rderr != nil {
			return fmt.Errorf("read %s: %w", path, rderr)
		}
		sk, perr := parseSkill(raw)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		sk.Path = path
		// The relative directory path is the canonical address. If frontmatter
		// declares a different name, that's drift the operator should notice
		// and fix; refusing to load is loud and unambiguous.
		if sk.Name != "" && sk.Name != name {
			return fmt.Errorf("skill %s: frontmatter name %q != directory path %q", path, sk.Name, name)
		}
		sk.Name = name
		set.skills[name] = sk
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return set, nil
}

// frontmatter is the strict subset of YAML keys we read out of a
// SKILL.md. The canonical tool-requirement key is `tools` (matching the
// agent frontmatter + the inline `skills:` yaml since the allowed_tools→
// tools rename). The hyphenated `allowed-tools` key is kept as the Claude
// Code skill-import alias so imported CC skills keep working; `tools`
// wins when both are present.
//
// Previously this read ONLY `allowed-tools`, so a SKILL.md declaring its
// requirement with `tools:` (as loomcycle's own bundle/example skills do)
// had that requirement silently dropped when loaded via a skills root —
// the gap this closes.
type frontmatter struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	Tools        []string `yaml:"tools"`
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
	// Canonical `tools` wins; fall back to the Claude Code `allowed-tools`
	// import alias when `tools` is absent.
	sk.Tools = fm.Tools
	if sk.Tools == nil {
		sk.Tools = fm.AllowedTools
	}
	sk.Body = body
	return sk, nil
}
