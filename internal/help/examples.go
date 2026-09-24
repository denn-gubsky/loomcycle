package help

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Example is one call example in a help topic: the ARGUMENTS object a model
// would pass to Tool. Examples are written as the argument object alone —
// never a {name, arguments} envelope or a tagged block — because an envelope
// is exactly the shape a small model copies into its reply as text instead of
// making the call.
type Example struct {
	// Tool is the tool the example calls: the fence's `tool=` attribute, else
	// the article's own tool.
	Tool string
	// Input is the argument object, verbatim.
	Input json.RawMessage
	// Line is the 1-based line of the opening fence within the topic body, for
	// error messages.
	Line int
}

// Examples returns the call examples in t. A call example is a fenced block
// whose info string starts with `json`, inside a `## Examples` section (the
// heading may carry a suffix: "## Examples — tenant scope"). Within that
// section a fence tagged `json result` shows what came back and is not a call.
// A fence may name its tool (```json tool=Memory), which is how a topic that
// is not a tool article — the scopes primer — carries checked examples.
//
// A malformed example is reported as an error rather than skipped: an example
// that does not parse teaches nothing and should not reach a model.
func (t *Topic) Examples() ([]Example, error) {
	if t == nil {
		return nil, nil
	}
	var (
		out       []Example
		inSection bool
		inFence   bool
		isCall    bool
		fenceTool string
		fenceLine int
		body      []string
	)
	for i, ln := range strings.Split(t.Content, "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "```") {
			if !inFence {
				inFence = true
				fenceLine = i + 1
				body = body[:0]
				isCall, fenceTool = false, ""
				info := strings.Fields(strings.TrimPrefix(trimmed, "```"))
				if inSection && len(info) > 0 && info[0] == "json" {
					isCall = true
					for _, attr := range info[1:] {
						switch {
						case attr == "result":
							isCall = false
						case strings.HasPrefix(attr, "tool="):
							fenceTool = strings.TrimPrefix(attr, "tool=")
						}
					}
				}
				continue
			}
			inFence = false
			if !isCall {
				continue
			}
			raw := strings.Join(body, "\n")
			var obj map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &obj); err != nil {
				return nil, fmt.Errorf("%s: example at line %d is not a JSON object: %w", t.Name, fenceLine, err)
			}
			tool := fenceTool
			if tool == "" {
				tool = t.Tool
			}
			if tool == "" {
				return nil, fmt.Errorf("%s: example at line %d names no tool (add tool=<Name> to the fence)", t.Name, fenceLine)
			}
			out = append(out, Example{Tool: tool, Input: json.RawMessage(raw), Line: fenceLine})
			continue
		}
		if inFence {
			body = append(body, ln)
			continue
		}
		if strings.HasPrefix(ln, "## ") {
			inSection = strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(ln, "## ")), "Examples")
		}
	}
	if inFence {
		return nil, fmt.Errorf("%s: unclosed code fence opened at line %d", t.Name, fenceLine)
	}
	return out, nil
}

// designDocCite matches an internal design-document citation ("RFC DL",
// "RFC-AK"). Help is model-visible text, and a citation there is a reference
// the model cannot follow.
var designDocCite = regexp.MustCompile(`\bRFC[ -][A-Z]{1,2}\b`)

// Lint returns the authoring-rule problems with t, independent of any tool's
// schema: examples are mandatory where a model needs them, an operation
// article's examples call that operation, and no design-doc citations. Nil
// means clean. The bundled corpus is held to this by tests; an operator
// article that fails it still loads, with the problems logged.
//
// Checking an example against the tool's live input schema is a separate,
// stricter check that needs the tool itself (internal/tools/builtin tests).
func (s *Set) Lint(t *Topic) []string {
	var probs []string
	if loc := designDocCite.FindString(t.Content + " " + t.Description); loc != "" {
		probs = append(probs, fmt.Sprintf("cites a design document (%q); help is read by models, which cannot follow it", loc))
	}
	exs, err := t.Examples()
	if err != nil {
		return append(probs, err.Error())
	}
	// A tool article with operation articles is an index; its examples live one
	// level down. Every other tool article, and every operation article, is where
	// a model lands to learn the call, so it must show one.
	needsExample := t.IsOpArticle() || (t.Tool != "" && len(s.OpsOf(t.Tool)) == 0)
	if needsExample && len(exs) == 0 {
		probs = append(probs, "no call example (add a ```json block under a `## Examples` heading)")
	}
	if t.IsOpArticle() {
		for _, ex := range exs {
			if ex.Tool != t.Tool {
				continue
			}
			var probe struct {
				Op string `json:"op"`
			}
			_ = json.Unmarshal(ex.Input, &probe)
			if probe.Op != t.Op {
				probs = append(probs, fmt.Sprintf("example at line %d calls op %q, but this article documents %q", ex.Line, probe.Op, t.Op))
			}
		}
	}
	return probs
}
