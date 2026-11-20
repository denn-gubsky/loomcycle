package help

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/denn-gubsky/loomcycle/internal/help/builtin"
)

// Example is one call example in a help topic. The parser lives with the
// embedded corpus (internal/help/builtin) so the config layer, which validates
// a {{help:<topic>}} placeholder at load, reads examples through the same code.
type Example = builtin.Example

// Examples returns the call examples in t — see builtin.ParseExamples for the
// grammar: ```json fences under a `## Examples` heading, `json result` fences
// excluded, an optional tool= attribute.
func (t *Topic) Examples() ([]Example, error) {
	if t == nil {
		return nil, nil
	}
	return builtin.ParseExamples(t.Name, t.Content, t.Tool)
}

// designDocCite matches an internal design-document citation ("RFC DL",
// "RFC-AK"). Help is model-visible text, and a citation there is a reference
// the model cannot follow.
var designDocCite = regexp.MustCompile(`\bRFC[ -][A-Z]{1,2}\b`)

// Lint returns the authoring-rule problems with t, independent of any tool's
// schema: examples are mandatory where a model needs them, an operation
// article's examples call that operation, a section article has an example
// calling its tool, and no design-doc citations. Nil
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
	// A tool article with operation or section articles is an index; its
	// examples live one level down. Every other tool article, and every operation
	// or section article, is where a model lands to learn the call, so it must
	// show one.
	isChild := t.IsOpArticle() || t.IsSectionArticle()
	isIndex := len(s.OpsOf(t.Tool)) > 0 || len(s.SectionsOf(t.Tool)) > 0
	needsExample := isChild || (t.Tool != "" && !isIndex)
	if needsExample && len(exs) == 0 {
		probs = append(probs, "no call example (add a ```json block under a `## Examples` heading)")
	}
	// A section has no op to check, but its examples must still call its own
	// tool: a section whose only examples call another tool teaches nothing
	// about this one.
	if t.IsSectionArticle() && len(exs) > 0 {
		calls := false
		for _, ex := range exs {
			calls = calls || ex.Tool == t.Tool
		}
		if !calls {
			probs = append(probs, fmt.Sprintf("no example calls %s (a section article's examples show calls to its own tool)", t.Tool))
		}
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
