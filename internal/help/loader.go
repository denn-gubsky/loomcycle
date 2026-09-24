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
	// Aliases are extra lookup names that resolve to this topic
	// (frontmatter `aliases:`). Lets a topic be found under the
	// tool's own name when they differ — e.g. the `skills` topic
	// aliased as `skill` so an agent probing `topic=Skill` (the
	// Skill tool's name) resolves it. Canonical names always win
	// over an alias.
	Aliases []string
	// Tool is set on a TOOL ARTICLE (tools/<Tool>.md, Op == "") and on an
	// OPERATION ARTICLE (tools/<Tool>/<op>.md). It is the tool's exact
	// model-facing name, which is what makes the pointer in that tool's
	// description a lookup rather than a table someone keeps in sync.
	// Derived from the file's location, never from frontmatter, so a
	// misfiled article cannot claim a tool it is not stored under.
	Tool string
	// Op is the operation an operation article documents ("" otherwise).
	Op string
}

// IsOpArticle reports whether t documents ONE operation of a tool. Operation
// articles are reachable by name and by search but are kept out of the topic
// index, which would otherwise be hundreds of entries long; their tool
// article lists them instead (see Set.OpsOf).
func (t *Topic) IsOpArticle() bool { return t != nil && t.Op != "" }

// Set is a name→Topic registry.
type Set struct {
	// topics is the canonical name→Topic map (case as authored),
	// the source of truth for Names()/All() and override semantics.
	topics map[string]*Topic
	// index is a case-insensitive lookup over canonical names AND
	// aliases (both lowercased), built once after load. Used only
	// by Get for forgiving resolution; never iterated for listing.
	index map[string]*Topic
}

// Get returns the named topic, or (nil, false) if absent. Safe on
// nil receiver. Resolution: exact canonical match first (fast path,
// preserves historical behaviour), then a case-insensitive lookup
// over canonical names and aliases — so `Skill`, `skill`, `Skills`
// all resolve the `skills` topic.
func (s *Set) Get(name string) (*Topic, bool) {
	if s == nil {
		return nil, false
	}
	if t, ok := s.topics[name]; ok {
		return t, true
	}
	key := strings.ToLower(strings.TrimSpace(name))
	if t, ok := s.index[key]; ok {
		return t, true
	}
	// An operation article is <Tool>/<op>, and a model reaching for one writes
	// it the way it writes the call it is about to make: Memory.recall, "Memory
	// recall". Accept both rather than fail the first attempt on punctuation. No
	// topic name contains "." or a space, so this only ever turns a miss into a
	// hit; it cannot redirect a name that already resolved.
	if alt := opSeparators.Replace(key); alt != key {
		if t, ok := s.index[alt]; ok {
			return t, true
		}
	}
	return nil, false
}

var opSeparators = strings.NewReplacer(".", "/", " ", "/")

// reindex rebuilds the case-insensitive alias index from topics.
// Called at the end of LoadSet, after any filesystem overlay, so
// the index reflects the final topic set. Canonical names take
// precedence over aliases (an alias never shadows a real topic).
func (s *Set) reindex() {
	idx := make(map[string]*Topic, len(s.topics)*2)
	// Canonical names first so they win on any collision.
	for name, t := range s.topics {
		idx[strings.ToLower(name)] = t
	}
	for _, t := range s.topics {
		for _, a := range t.Aliases {
			key := strings.ToLower(strings.TrimSpace(a))
			if key == "" {
				continue
			}
			if _, taken := idx[key]; taken {
				continue // don't let an alias shadow a canonical name
			}
			idx[key] = t
		}
	}
	s.index = idx
}

// Names returns all topic names sorted lexicographically, operation articles
// included. Used by the diagnostic startup log; the model-facing index uses
// IndexNames.
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

// IndexNames returns the names the topic INDEX shows: every topic except
// operation articles, sorted. Those are reached through their tool article.
func (s *Set) IndexNames() []string {
	var out []string
	for _, n := range s.Names() {
		if !s.topics[n].IsOpArticle() {
			out = append(out, n)
		}
	}
	return out
}

// OpsOf returns the operation articles of tool, sorted by op. Derived from what
// is loaded, so a tool article never has to list its own children.
func (s *Set) OpsOf(tool string) []*Topic {
	if s == nil {
		return nil
	}
	var out []*Topic
	for _, t := range s.topics {
		if t.IsOpArticle() && t.Tool == tool {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Op < out[j].Op })
	return out
}

// ToolArticle returns the article for the tool named exactly `tool`. Unlike Get
// it neither folds case nor consults aliases: a tool name is a precise
// identifier, and a feature topic that happens to share a word with a tool must
// not be taken for that tool's article.
func (s *Set) ToolArticle(tool string) (*Topic, bool) {
	if s == nil {
		return nil, false
	}
	t, ok := s.topics[tool]
	if !ok || t.Tool != tool || t.IsOpArticle() {
		return nil, false
	}
	return t, true
}

// toolsDir is the subdirectory, under both the bundled corpus and an operator's
// LOOMCYCLE_HELP_ROOT, that holds tool and operation articles:
//
//	tools/<Tool>.md       → topic "<Tool>"       (tool article)
//	tools/<Tool>/<op>.md  → topic "<Tool>/<op>"  (operation article)
const toolsDir = "tools"

//go:embed builtin/*.md builtin/tools
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
	if err := loadToolArticles(bundledDirFS{bundledFS, "builtin/" + toolsDir}, "", func(t *Topic) {
		t.Source = "bundled"
		set.topics[t.Name] = t
	}, func(path string, err error) error {
		return fmt.Errorf("bundled %s/%s: %w", toolsDir, path, err)
	}); err != nil {
		return nil, err
	}

	// Optionally overlay filesystem topics.
	if root == "" {
		set.reindex()
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
	// Operator tool articles — including ones for MCP tools, named after the
	// tool exactly as the model sees it (mcp__<server>__<tool>). A bad file is
	// skipped with a log line, like every operator topic, and an article that
	// breaks the authoring rules loads with a warning: loomcycle does not own an
	// MCP tool's schema, so it cannot hold that article to a test.
	toolsRoot := filepath.Join(root, toolsDir)
	if fi, err := os.Lstat(toolsRoot); err == nil {
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			log.Printf("help: skipping %s: symlink (operator-supplied help topics must be regular files)", toolsRoot)
		case fi.IsDir():
			var loaded []*Topic
			_ = loadToolArticles(osDirFS{toolsRoot}, toolsRoot, func(t *Topic) {
				t.Source = "filesystem"
				set.topics[t.Name] = t
				loaded = append(loaded, t)
			}, func(path string, err error) error {
				log.Printf("help: skipping %s: %v", filepath.Join(toolsRoot, path), err)
				return nil
			})
			// Linted after the walk: whether a tool article needs its own
			// example depends on whether its operation articles loaded.
			for _, t := range loaded {
				for _, w := range set.Lint(t) {
					log.Printf("help: %s: %s", t.Path, w)
				}
			}
		}
	}
	set.reindex()
	return set, nil
}

// dirFS is the two reads loadToolArticles needs, over either the embedded
// corpus or an operator directory. The operator side refuses symlinks at every
// level: the directory is a trust boundary, and a link inside tools/ reaches
// just as far as one at the top.
type dirFS interface {
	ReadDir(rel string) ([]os.DirEntry, error)
	ReadFile(rel string) ([]byte, error)
}

type bundledDirFS struct {
	fs   embed.FS
	base string
}

func (b bundledDirFS) ReadDir(rel string) ([]os.DirEntry, error) {
	return b.fs.ReadDir(joinRel(b.base, rel))
}

func (b bundledDirFS) ReadFile(rel string) ([]byte, error) {
	return b.fs.ReadFile(joinRel(b.base, rel))
}

type osDirFS struct{ base string }

func (o osDirFS) ReadDir(rel string) ([]os.DirEntry, error) {
	return os.ReadDir(filepath.Join(o.base, rel))
}

func (o osDirFS) ReadFile(rel string) ([]byte, error) {
	p := filepath.Join(o.base, rel)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("symlink (operator-supplied help topics must be regular files)")
	}
	return os.ReadFile(p)
}

func joinRel(base, rel string) string {
	if rel == "" {
		return base
	}
	return base + "/" + rel
}

// loadToolArticles reads tools/<Tool>.md and tools/<Tool>/<op>.md. The name,
// Tool and Op all come from the file's location, and the frontmatter name must
// agree, as for every topic. onErr decides whether a bad file is fatal (the
// bundled corpus) or skipped (an operator directory).
func loadToolArticles(fsys dirFS, pathPrefix string, add func(*Topic), onErr func(string, error) error) error {
	entries, err := fsys.ReadDir("")
	if err != nil {
		return onErr("", err)
	}
	load := func(rel, name, tool, op string) error {
		data, err := fsys.ReadFile(rel)
		if err != nil {
			return onErr(rel, err)
		}
		path := ""
		if pathPrefix != "" {
			path = filepath.Join(pathPrefix, rel)
		}
		t, err := parseTopic(data, name, path)
		if err != nil {
			return onErr(rel, err)
		}
		t.Tool, t.Op = tool, op
		add(t)
		return nil
	}
	for _, e := range entries {
		switch {
		case e.Type()&os.ModeSymlink != 0:
			if err := onErr(e.Name(), fmt.Errorf("symlink (operator-supplied help topics must be regular files)")); err != nil {
				return err
			}
		case e.IsDir():
			tool := e.Name()
			ops, err := fsys.ReadDir(tool)
			if err != nil {
				if err := onErr(tool, err); err != nil {
					return err
				}
				continue
			}
			for _, o := range ops {
				if o.IsDir() || !strings.HasSuffix(o.Name(), ".md") {
					continue
				}
				if o.Type()&os.ModeSymlink != 0 {
					if err := onErr(tool+"/"+o.Name(), fmt.Errorf("symlink (operator-supplied help topics must be regular files)")); err != nil {
						return err
					}
					continue
				}
				op := strings.TrimSuffix(o.Name(), ".md")
				if err := load(tool+"/"+o.Name(), tool+"/"+op, tool, op); err != nil {
					return err
				}
			}
		case strings.HasSuffix(e.Name(), ".md"):
			tool := strings.TrimSuffix(e.Name(), ".md")
			if err := load(e.Name(), tool, tool, ""); err != nil {
				return err
			}
		}
	}
	return nil
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
		Name        string   `yaml:"name"`
		Description string   `yaml:"description"`
		Aliases     []string `yaml:"aliases"`
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
		Aliases:     fm.Aliases,
	}, nil
}
