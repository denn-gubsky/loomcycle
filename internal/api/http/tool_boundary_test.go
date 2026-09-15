package http

import (
	"strings"
	"testing"

	lctools "github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The injected inventory used to carry only a 120-byte first sentence, so the
// clause saying which tool to reach for INSTEAD was truncated away — which is
// why every bundle hand-wrote a "choosing among them" section that drifted from
// the descriptions it duplicated.
func TestFormatToolLine_CarriesTheBoundary(t *testing.T) {
	for _, tc := range []struct {
		tool lctools.Tool
		want string
	}{
		{&builtin.Grep{}, "that is Glob"},
		{&builtin.Glob{}, "that is Grep"},
		{&builtin.WebFetch{}, "use WebSearch"},
		{&builtin.Read{}, "use Glob"},
	} {
		line := formatToolLine(tc.tool.Name(), tc.tool.Description(), "read")
		if !strings.Contains(line, tc.want) {
			t.Errorf("%s: inventory line does not name the neighbour (%q):\n%s",
				tc.tool.Name(), tc.want, line)
		}
		// The boundary goes on its own continuation line — which tool to reach
		// for instead is a different question from what this one does.
		if !strings.Contains(line, "\n  ") {
			t.Errorf("%s: boundary not on a continuation line:\n%s", tc.tool.Name(), line)
		}
		// The summary survives unchanged alongside it.
		if !strings.HasPrefix(line, "- "+tc.tool.Name()) {
			t.Errorf("%s: line no longer starts with the tool name:\n%s", tc.tool.Name(), line)
		}
	}
}

// Verbatim, not paraphrased: the clause already names the neighbour, and
// rewriting it would be this layer inventing guidance the author did not write.
func TestBoundarySentence_IsVerbatim(t *testing.T) {
	const desc = "Find files by NAME. Do NOT use it to search file CONTENTS — that is Grep. More prose here."
	got := boundarySentence(desc, toolBoundaryMaxDesc)
	if got != "Do NOT use it to search file CONTENTS — that is Grep." {
		t.Errorf("clause = %q", got)
	}
}

func TestBoundarySentence_AbsentOrOversizedYieldsNothing(t *testing.T) {
	if got := boundarySentence("A tool with no stated boundary at all.", toolBoundaryMaxDesc); got != "" {
		t.Errorf("invented a boundary: %q", got)
	}
	// A clause too long to trust to truncation is dropped rather than clipped:
	// half a sentence naming half a tool is worse than no guidance.
	long := "X. Do NOT use it " + strings.Repeat("because ", 40) + "use Y."
	if got := boundarySentence(long, toolBoundaryMaxDesc); got != "" {
		t.Errorf("emitted an oversized clause (%d bytes)", len(got))
	}
}

// A description with no boundary must render exactly as it did before, so tools
// that predate the rubric are unaffected.
func TestFormatToolLine_NoBoundaryIsUnchanged(t *testing.T) {
	line := formatToolLine("Legacy", "Does a thing. And another thing.", "read")
	if strings.Contains(line, "\n") {
		t.Errorf("added a continuation line with no boundary to show:\n%s", line)
	}
	if line != "- Legacy (read) — Does a thing." {
		t.Errorf("line = %q", line)
	}
}
