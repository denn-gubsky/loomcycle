// Package help loads loomcycle's documentation topics — narrative
// markdown content that agents pull via Context.help. Topics are
// cross-cutting guidance (how scopes compose across Memory and
// Channel, how AgentDef + Evaluation make experimentation work,
// when sub-agents beat channels) that no single tool's `doc` op
// can surface alone.
//
// Wire model:
//
//  1. Loomcycle ships a small bundled set of default topics
//     embedded in the binary (loomcycle, scopes, subagents,
//     experimentation, system-channels). These are operator-
//     curated baseline guidance applicable to every deployment.
//  2. Operators MAY point LOOMCYCLE_HELP_ROOT at a directory of
//     `<topic>.md` files. Filesystem topics override bundled
//     defaults of the same name, so an operator can replace the
//     default "scopes" topic with their own deployment-specific
//     conventions.
//  3. Agents call Context.help (no topic) → topic index; or
//     Context.help(topic=<name>) → full markdown body.
//
// File format: standard YAML frontmatter + markdown body. The
// frontmatter carries `name` (required; must match filename
// minus `.md`) and `description` (a one-liner shown in the index).
// Body is everything after the closing frontmatter `---` divider.
package help

import (
	"embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Topic is one parsed help topic — either a bundled default or
// an operator-supplied override.
type Topic struct {
	// Name is the topic identifier — the filename minus `.md`,
	// must match the frontmatter `name:` field.
	Name string
	// Description is the one-liner shown in the no-topic index.
	// Keep short; the body is for detail.
	Description string
	// Content is the markdown body — the heart of the topic.
	// Whatever the operator wants the agent to read.
	Content string
	// Source is "bundled" or "filesystem", surfaced in
	// diagnostic logs and in the Context.help response so
	// operators know whether an override took effect.
	Source string
	// Path is the absolute path of the source .md for filesystem
	// topics; empty for bundled.
	Path string
}

// Set is a name→Topic registry.
type Set struct {
	topics map[string]*Topic
}

// Get returns the named topic, or (nil, false) if absent. Safe on
// nil receiver.
func (s *Set) Get(name string) (*Topic, bool) {
	if s == nil {
		return nil, false
	}
	t, ok := s.topics[name]
	return t, ok
}

// Names returns all topic names sorted lexicographically. Used by
// the Context.help index op and the diagnostic startup log.
func (s *Set) Names() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.topics))
	for n := range s.topics {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// All returns all topics, sorted by name. Used by the index op so
// the topic list is deterministic.
func (s *Set) All() []*Topic {
	if s == nil {
		return nil
	}
	names := s.Names()
	out := make([]*Topic, 0, len(names))
	for _, n := range names {
		out = append(out, s.topics[n])
	}
	return out
}

//go:embed builtin/*.md
var bundledFS embed.FS

// LoadSet builds the topic registry. First loads bundled defaults
// from the embedded FS; then, if root != "", walks the operator's
// directory and overrides matching names. An empty root is the
// "bundled only" deployment shape.
//
// Errors fall into three buckets:
//
//   - Bundled parse errors are fatal (operator can't fix them
//     without rebuild; failing loudly catches build-time mistakes).
//   - Filesystem dir-resolution errors (missing root, root is a
//     file, ReadDir EIO) are fatal — these signal a clear operator
//     misconfiguration that they want to see at boot.
//   - Per-file filesystem errors (parse, read, symlink) are SKIPPED
//     with a log line; one malformed operator-supplied topic must
//     not take down the agent runtime. Bundled topics are already
//     loaded before this point, so the agent surface degrades to
//     "bundled only" for that name rather than failing the boot.
//
// Symlinks under root are refused — the operator-supplied directory
// is a trust boundary; a stray symlink at an innocuous-looking name
// would let the operator (or whoever wrote to that dir) exfiltrate
// arbitrary files into a topic body the model reads.
func LoadSet(root string) (*Set, error) {
	set := &Set{topics: map[string]*Topic{}}

	// Load bundled defaults first.
	entries, err := bundledFS.ReadDir("builtin")
	if err != nil {
		return nil, fmt.Errorf("read embedded help/builtin: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := bundledFS.ReadFile("builtin/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read bundled %s: %w", e.Name(), err)
		}
		nameFromFile := strings.TrimSuffix(e.Name(), ".md")
		t, err := parseTopic(data, nameFromFile, "")
		if err != nil {
			return nil, fmt.Errorf("parse bundled %s: %w", e.Name(), err)
		}
		t.Source = "bundled"
		set.topics[t.Name] = t
	}

	// Optionally overlay filesystem topics.
	if root == "" {
		return set, nil
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("help root %s: %w", root, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("help root %s: not a directory", root)
	}
	fsEntries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read help root %s: %w", root, err)
	}
	for _, e := range fsEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(root, e.Name())
		// Refuse to follow symlinks. The operator-supplied directory
		// is a trust boundary; a stray symlink (intentional or from
		// an automated tool laying out the dir) would let an "innocent"
		// path like `escape.md` exfiltrate any file the loomcycle
		// process can read into the topic body the model sees.
		fi, err := os.Lstat(path)
		if err != nil {
			log.Printf("help: skipping %s: lstat: %v", path, err)
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			log.Printf("help: skipping %s: symlink (operator-supplied help topics must be regular files)", path)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("help: skipping %s: %v", path, err)
			continue
		}
		nameFromFile := strings.TrimSuffix(e.Name(), ".md")
		t, err := parseTopic(data, nameFromFile, path)
		if err != nil {
			// Soft-skip per the doc-comment contract: one malformed
			// operator topic must not kill the runtime. Bundled
			// defaults (loaded above) remain intact, so the agent
			// surface degrades gracefully — the bad topic just
			// doesn't appear in the index.
			log.Printf("help: skipping %s: %v", path, err)
			continue
		}
		t.Source = "filesystem"
		set.topics[t.Name] = t // override bundled if name matches
	}
	return set, nil
}

// parseTopic parses one help .md file. nameFromFile is the
// filename stem; the frontmatter's `name:` must match. Returns
// errors for missing/malformed frontmatter, name mismatch, or
// empty body.
func parseTopic(data []byte, nameFromFile, path string) (*Topic, error) {
	src := string(data)
	if !strings.HasPrefix(src, "---\n") && !strings.HasPrefix(src, "---\r\n") {
		return nil, fmt.Errorf("missing opening frontmatter `---`")
	}
	// Strip leading delimiter.
	src = strings.TrimPrefix(src, "---\n")
	src = strings.TrimPrefix(src, "---\r\n")
	// Find closing delimiter at start of a line.
	closeIdx := strings.Index(src, "\n---\n")
	if closeIdx < 0 {
		closeIdx = strings.Index(src, "\n---\r\n")
	}
	if closeIdx < 0 {
		return nil, fmt.Errorf("missing closing frontmatter `---`")
	}
	fmText := src[:closeIdx]
	body := src[closeIdx:]
	// Skip past the closing delimiter line.
	body = strings.TrimPrefix(body, "\n---\n")
	body = strings.TrimPrefix(body, "\n---\r\n")
	// Body may be empty — operators can ship description-only
	// placeholder topics, but we reject it as a likely typo.
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, fmt.Errorf("empty topic body (frontmatter parsed; nothing after closing `---`)")
	}

	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		return nil, fmt.Errorf("frontmatter yaml: %w", err)
	}
	if fm.Name == "" {
		return nil, fmt.Errorf("frontmatter missing `name:`")
	}
	if fm.Name != nameFromFile {
		return nil, fmt.Errorf("frontmatter name %q doesn't match filename %q", fm.Name, nameFromFile)
	}
	if fm.Description == "" {
		return nil, fmt.Errorf("frontmatter missing `description:`")
	}
	return &Topic{
		Name:        fm.Name,
		Description: fm.Description,
		Content:     body,
		Path:        path,
	}, nil
}
