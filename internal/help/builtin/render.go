package builtin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrUnknownTopic is returned for a topic name the embedded corpus does not
// hold.
var ErrUnknownTopic = errors.New("no such help topic in the embedded corpus")

// ErrNoExamples is returned for a topic that exists but has no call example
// under a `## Examples` heading.
var ErrNoExamples = errors.New("help topic has no call examples")

// entry is one embedded article: its file, the tool it belongs to ("" for a
// topic that is not a tool or operation article) and its frontmatter aliases.
type entry struct {
	file    string
	tool    string
	aliases []string
}

// topicIndex resolves a name to an embedded article the way package help's
// Set.Get does for bundled topics: the canonical name exactly, then case-
// insensitively over canonical names and aliases (a canonical name wins over
// an alias), then with `.` or a space read as the `<Tool>/<op>` separator.
type topicIndex struct {
	byName map[string]entry  // canonical name → article
	folded map[string]string // lower-cased name or alias → canonical name
}

// bundled is computed once from the embedded files, which cannot change while
// the process runs; it is never written after initialisation.
var bundled = buildIndex()

// buildIndex walks the corpus with LoadSet's naming: `<topic>.md` →
// "<topic>", `tools/<Tool>.md` → "<Tool>", `tools/<Tool>/<op>.md` →
// "<Tool>/<op>". Tool articles are indexed after topics so a tool article wins
// a name clash, as it does in LoadSet. A file whose frontmatter does not parse
// is indexed without aliases: LoadSet refuses the bundled corpus over it, so
// the binary does not ship that way.
func buildIndex() topicIndex {
	idx := topicIndex{byName: map[string]entry{}, folded: map[string]string{}}
	add := func(name, file, tool string) {
		e := entry{file: file, tool: tool}
		if data, err := fs.ReadFile(corpus, file); err == nil {
			if fm, _, err := SplitFrontmatter(data); err == nil {
				var meta struct {
					Aliases []string `yaml:"aliases"`
				}
				if yaml.Unmarshal([]byte(fm), &meta) == nil {
					e.aliases = meta.Aliases
				}
			}
		}
		idx.byName[name] = e
	}
	top, _ := fs.ReadDir(corpus, ".")
	for _, d := range top {
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			add(strings.TrimSuffix(d.Name(), ".md"), d.Name(), "")
		}
	}
	tools, _ := fs.ReadDir(corpus, "tools")
	for _, d := range tools {
		switch {
		case d.IsDir():
			ops, _ := fs.ReadDir(corpus, path.Join("tools", d.Name()))
			for _, o := range ops {
				if !o.IsDir() && strings.HasSuffix(o.Name(), ".md") {
					add(d.Name()+"/"+strings.TrimSuffix(o.Name(), ".md"), path.Join("tools", d.Name(), o.Name()), d.Name())
				}
			}
		case strings.HasSuffix(d.Name(), ".md"):
			tool := strings.TrimSuffix(d.Name(), ".md")
			add(tool, path.Join("tools", d.Name()), tool)
		}
	}
	for name := range idx.byName {
		idx.folded[strings.ToLower(name)] = name
	}
	for name, e := range idx.byName {
		for _, a := range e.aliases {
			key := strings.ToLower(strings.TrimSpace(a))
			if _, taken := idx.folded[key]; key != "" && !taken {
				idx.folded[key] = name
			}
		}
	}
	return idx
}

var opSeparators = strings.NewReplacer(".", "/", " ", "/")

// Resolve returns the canonical name of the embedded article name resolves
// to, with Context op=help's tolerance (case, aliases, `.` or a space as the
// operation separator). Only embedded articles resolve: an operator's
// LOOMCYCLE_HELP_ROOT topic is not in this corpus.
func Resolve(name string) (string, bool) {
	if _, ok := bundled.byName[name]; ok {
		return name, true
	}
	key := strings.ToLower(strings.TrimSpace(name))
	if c, ok := bundled.folded[key]; ok {
		return c, true
	}
	if alt := opSeparators.Replace(key); alt != key {
		if c, ok := bundled.folded[alt]; ok {
			return c, true
		}
	}
	return "", false
}

// RenderExamples renders the call examples of the embedded help topic named
// topic, one per line, in the form agent prompts write a call card:
//
//	Document {"op":"search","query":"how do we roll back a failed migration"}
//
// The tool name, one space, then the argument object compacted onto one line
// with its keys in the order the article wrote them. Result fences (`json
// result`) are not calls and are not rendered; neither is the prose around
// the examples.
//
// The topic is resolved by Resolve. Only the EMBEDDED article is read, never
// an operator's LOOMCYCLE_HELP_ROOT override: the placeholder is validated at
// authoring and rendered at every run start, so it must read the same bytes
// both times, and reading a directory on the prompt-assembly path is a cost
// and a reach this family is defined not to have.
//
// Pure: it reads only the embedded files and returns the same bytes for the
// same topic, so an assembled prompt stays byte-stable for prompt caching.
func RenderExamples(topic string) (string, error) {
	canonical, ok := Resolve(topic)
	if !ok {
		return "", fmt.Errorf("%q: %w", topic, ErrUnknownTopic)
	}
	e := bundled.byName[canonical]
	data, err := fs.ReadFile(corpus, e.file)
	if err != nil {
		return "", fmt.Errorf("%s: %w", canonical, err)
	}
	_, body, err := SplitFrontmatter(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", canonical, err)
	}
	exs, err := ParseExamples(canonical, body, e.tool)
	if err != nil {
		return "", err
	}
	if len(exs) == 0 {
		return "", fmt.Errorf("%s: %w", canonical, ErrNoExamples)
	}
	lines := make([]string, 0, len(exs))
	for _, ex := range exs {
		var compact bytes.Buffer
		if err := json.Compact(&compact, ex.Input); err != nil {
			// ParseExamples already decoded it as an object; unreachable.
			return "", fmt.Errorf("%s: example at line %d: %w", canonical, ex.Line, err)
		}
		lines = append(lines, ex.Tool+" "+compact.String())
	}
	return strings.Join(lines, "\n"), nil
}
