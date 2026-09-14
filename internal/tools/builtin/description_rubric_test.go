package builtin

import (
	"regexp"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A tool description is the ONLY thing that reaches the model on every single
// request — the Go doc comment above each tool, where most of this knowledge
// already lived, reaches nobody. These tests hold the descriptions to the shape
// that makes a model pick the right tool instead of the plausible one.
//
// The rubric, and why each part is checked mechanically where it can be:
//
//	purpose     a first sentence that stands alone (checked: length + presence)
//	inputs      the arguments and their constraints          (checked: loosely)
//	limits      what it will not do / what it caps           (not mechanical)
//	boundaries  when to use THIS one and not its neighbour   (checked: required)
//
// Boundaries get the hard check because they are the element that goes missing
// first and costs the most: Read vs Glob vs Grep, WebFetch vs HTTP vs WebSearch,
// Memory vs Recall vs History are all pairs a model confuses in the direction
// that wastes an iteration.
//
// The set below is the PRIMITIVE tools. The op-dispatched meta-tools (Memory,
// Document, Channel, Context, History) carry op catalogues whose shape is
// pinned by their own surface tests.
func rubricTools(t *testing.T) []tools.Tool {
	t.Helper()
	return []tools.Tool{
		&Read{}, &Write{}, &Edit{}, &Grep{}, &Glob{}, &NotebookEdit{},
		&HTTP{}, &WebFetch{}, &WebSearch{}, &Bash{}, &Recall{}, &Path{},
	}
}

// firstSentenceOf mirrors the inventory renderer in internal/api/http: it cuts
// at the first ". " and hard-caps at 120 bytes. Duplicated rather than imported
// because api/http imports this package, not the other way round.
func firstSentenceOf(desc string) string {
	d := strings.TrimSpace(desc)
	if i := strings.IndexByte(d, '\n'); i >= 0 {
		d = strings.TrimSpace(d[:i])
	}
	if i := strings.Index(d, ". "); i >= 0 {
		d = d[:i+1]
	}
	return d
}

// TestToolDescriptions_FirstSentenceSurvivesTheInventory pins a constraint the
// rubric does not mention but the runtime imposes: `Context op=tools` renders
// ONLY the first sentence, truncated at 120 bytes with an ellipsis.
//
// A description whose opening sentence runs long is not merely inelegant — it
// reaches the agent's own tool inventory cut mid-clause, which is exactly the
// surface meant to stop a model claiming it lacks a capability it has.
func TestToolDescriptions_FirstSentenceSurvivesTheInventory(t *testing.T) {
	const inventoryCap = 120
	for _, tl := range rubricTools(t) {
		first := firstSentenceOf(tl.Description())
		if first == "" {
			t.Errorf("%s: empty description", tl.Name())
			continue
		}
		if len(first) > inventoryCap {
			t.Errorf("%s: first sentence is %d bytes, over the %d-byte inventory cap — it will "+
				"render truncated in the agent's own tool list:\n  %s",
				tl.Name(), len(first), inventoryCap, first)
		}
		if !strings.HasSuffix(first, ".") {
			t.Errorf("%s: first sentence does not end in a period, so the renderer cannot find "+
				"a sentence boundary and will hard-cut at %d bytes:\n  %s", tl.Name(), inventoryCap, first)
		}
	}
}

// TestToolDescriptions_StateTheirBoundaries is the load-bearing one.
//
// Every tool here has at least one neighbour a model reaches for by mistake,
// and the cost of the mistake is a wasted iteration plus a confusing tool
// result. Saying "do NOT use this for X — use Y" is the cheapest correction
// available, and it has to be in the DESCRIPTION: the usage guide is opt-in
// per agent, so it cannot be relied on.
func TestToolDescriptions_StateTheirBoundaries(t *testing.T) {
	boundary := regexp.MustCompile(`(?i)\bdo not use\b`)
	for _, tl := range rubricTools(t) {
		if !boundary.MatchString(tl.Description()) {
			t.Errorf("%s: the description never says what NOT to use it for. Name the neighbour "+
				"a model would otherwise pick (\"Do NOT use it to …, use <Tool> for that\") — "+
				"that sentence is worth more than any amount of detail about this tool alone.",
				tl.Name())
		}
	}
}

// TestToolDescriptions_AreSubstantive catches the regression this whole change
// undid: a tool shipping with a single clause ("Read a UTF-8 text file from
// disk.") that says nothing about arguments, limits or neighbours.
func TestToolDescriptions_AreSubstantive(t *testing.T) {
	const floor = 250
	for _, tl := range rubricTools(t) {
		if n := len(tl.Description()); n < floor {
			t.Errorf("%s: description is %d bytes, under the %d-byte floor. It is the only text "+
				"that reaches the model on every request — state the inputs, the limits and the "+
				"neighbouring tool, not just the purpose.", tl.Name(), n, floor)
		}
	}
}

// TestToolDescriptions_CiteNoDesignDocs mirrors the rule already enforced on
// the MCP surface: model-visible text must not cite internal RFC letters. They
// mean nothing to an agent and leak the project's private vocabulary into
// every request.
func TestToolDescriptions_CiteNoDesignDocs(t *testing.T) {
	cite := regexp.MustCompile(`RFC [A-Z]{1,2}\b`)
	for _, tl := range rubricTools(t) {
		if m := cite.FindString(tl.Description()); m != "" {
			t.Errorf("%s: description cites %q — model-visible text must not reference internal "+
				"design docs", tl.Name(), m)
		}
	}
}
