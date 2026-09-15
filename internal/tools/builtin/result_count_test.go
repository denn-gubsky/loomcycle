package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func runTool(t *testing.T, tool tools.Tool, ctx context.Context, args string) tools.Result {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

func wantCount(t *testing.T, res tools.Result, want int) {
	t.Helper()
	if res.Count == nil {
		t.Fatalf("Count is nil — the tool counted nothing, which is a DIFFERENT "+
			"statement from counting zero; text was %q", res.Text)
	}
	if *res.Count != want {
		t.Errorf("Count = %d, want %d (text: %q)", *res.Count, want, res.Text)
	}
}

// A search that ran and matched nothing must be reportable as such WITHOUT
// reading the prose. Count 0 — not nil — is the whole point.
func TestGrep_NoMatchesCountsZeroAndReadsAsSuccess(t *testing.T) {
	root := makeGrepTree(t)
	res := runTool(t, &Grep{}, grepCtx(root), `{"pattern":"zzz-nothing-matches-this"}`)

	if res.IsError {
		t.Fatal("an empty result is a SUCCESS, not an error")
	}
	wantCount(t, res, 0)

	// The text has to say the search ran. "no matches" alone is a fragment an
	// agent can read as either outcome.
	low := strings.ToLower(res.Text)
	if !strings.Contains(low, "completed successfully") {
		t.Errorf("empty text does not state the search ran: %q", res.Text)
	}
}

func TestGrep_CountMatchesTheMode(t *testing.T) {
	root := makeGrepTree(t)
	ctx := grepCtx(root)

	// "func main" appears in a.go and sub/d.go — two files, two lines.
	for _, tc := range []struct {
		mode string
		want int
		why  string
	}{
		{"files_with_matches", 2, "two files contain it"},
		{"count", 2, "one line per matching file"},
		{"content", 2, "two matching lines"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			res := runTool(t, &Grep{}, ctx,
				`{"pattern":"func main","output_mode":"`+tc.mode+`"}`)
			wantCount(t, res, tc.want)
		})
	}
}

func TestGlob_NoMatchesCountsZeroAndReadsAsSuccess(t *testing.T) {
	root := makeGrepTree(t)
	res := runTool(t, &Glob{}, grepCtx(root), `{"pattern":"**/*.nothing"}`)

	if res.IsError {
		t.Fatal("an empty list is a legitimate answer, not a failure")
	}
	wantCount(t, res, 0)
	if !strings.Contains(strings.ToLower(res.Text), "completed successfully") {
		t.Errorf("empty text does not state the search ran: %q", res.Text)
	}
}

func TestGlob_CountsWhatItReturned(t *testing.T) {
	root := makeGrepTree(t)
	res := runTool(t, &Glob{}, grepCtx(root), `{"pattern":"**/*.go"}`)

	// a.go, b.go, sub/d.go
	wantCount(t, res, 3)

	// The count must agree with what was actually rendered, or it is worse
	// than no count at all.
	lines := 0
	for _, l := range strings.Split(strings.TrimSpace(res.Text), "\n") {
		if strings.HasSuffix(l, ".go") {
			lines++
		}
	}
	if lines != *res.Count {
		t.Errorf("Count = %d but %d paths were rendered:\n%s", *res.Count, lines, res.Text)
	}
}

// An out-of-root absolute pattern cannot match inside the sandbox. That is a
// legitimate empty answer, not a refusal, and must count zero like any other.
func TestGlob_OutOfRootPatternIsEmptyNotError(t *testing.T) {
	root := makeGrepTree(t)
	res := runTool(t, &Glob{}, grepCtx(root), `{"pattern":"/etc/*.conf"}`)

	if res.IsError {
		t.Fatal("an unmatchable path is an empty result, not an error")
	}
	wantCount(t, res, 0)
}
