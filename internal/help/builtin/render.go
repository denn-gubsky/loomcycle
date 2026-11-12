package builtin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
)

// ErrUnknownTopic is returned for a topic name the embedded corpus does not
// hold.
var ErrUnknownTopic = errors.New("no such help topic in the embedded corpus")

// ErrNoExamples is returned for a topic that exists but has no call example
// under a `## Examples` heading.
var ErrNoExamples = errors.New("help topic has no call examples")

// topicName is the shape of a help topic name: `<topic>`, `<Tool>` or
// `<Tool>/<op>`. Checked before any file is opened, so a name can only ever
// address a file this package embeds, never walk out of the corpus.
var topicName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*(?:/[A-Za-z][A-Za-z0-9_-]*)?$`)

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
// The topic is resolved the way package help names it: `<Tool>/<op>` is an
// operation article, and a bare name is a tool article (tools/<name>.md) before
// a topic (<name>.md), matching LoadSet, where a tool article loaded later wins
// a name clash. Only the EMBEDDED article is read, never an operator's
// LOOMCYCLE_HELP_ROOT override: the placeholder is validated at config load and
// rendered at every run start, so it must read the same bytes both times, and
// reading a directory on the prompt-assembly path is a cost and a reach this
// family is defined not to have.
//
// Pure: it reads only the embedded files and returns the same bytes for the
// same topic, so an assembled prompt stays byte-stable for prompt caching.
func RenderExamples(topic string) (string, error) {
	body, tool, err := article(topic)
	if err != nil {
		return "", err
	}
	exs, err := ParseExamples(topic, body, tool)
	if err != nil {
		return "", err
	}
	if len(exs) == 0 {
		return "", fmt.Errorf("%s: %w", topic, ErrNoExamples)
	}
	lines := make([]string, 0, len(exs))
	for _, ex := range exs {
		var compact bytes.Buffer
		if err := json.Compact(&compact, ex.Input); err != nil {
			// ParseExamples already decoded it as an object; unreachable.
			return "", fmt.Errorf("%s: example at line %d: %w", topic, ex.Line, err)
		}
		lines = append(lines, ex.Tool+" "+compact.String())
	}
	return strings.Join(lines, "\n"), nil
}

// article returns the body and the owning tool of the embedded topic named
// topic.
func article(topic string) (body, tool string, err error) {
	if !topicName.MatchString(topic) {
		return "", "", fmt.Errorf("%q: %w", topic, ErrUnknownTopic)
	}
	type candidate struct{ path, tool string }
	var cands []candidate
	if t, op, ok := strings.Cut(topic, "/"); ok {
		cands = []candidate{{"tools/" + t + "/" + op + ".md", t}}
	} else {
		cands = []candidate{{"tools/" + topic + ".md", topic}, {topic + ".md", ""}}
	}
	for _, c := range cands {
		data, rerr := fs.ReadFile(corpus, c.path)
		if errors.Is(rerr, fs.ErrNotExist) {
			continue
		}
		if rerr != nil {
			return "", "", fmt.Errorf("%s: %w", topic, rerr)
		}
		_, b, serr := SplitFrontmatter(data)
		if serr != nil {
			return "", "", fmt.Errorf("%s: %w", topic, serr)
		}
		return b, c.tool, nil
	}
	return "", "", fmt.Errorf("%q: %w", topic, ErrUnknownTopic)
}
