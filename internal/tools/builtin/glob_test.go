package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// globCtx attaches a default rw volume rooted at root, the standard ctx the
// Glob tests run under (RFC AH Phase 3: file tools require a bound volume).
func globCtx(root string) context.Context {
	return ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
}

// makeGlobTree builds a known structure for the Glob tests.
//
//	root/
//	  a.go
//	  b.txt
//	  src/
//	    main.go
//	    pkg/
//	      util.go
//	      data.tsx
//	  vendor/
//	    third.go
func makeGlobTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		"a.go",
		"b.txt",
		"src/main.go",
		"src/pkg/util.go",
		"src/pkg/data.tsx",
		"vendor/third.go",
	}
	for _, p := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestGlob_DoubleStarRecursive(t *testing.T) {
	root := makeGlobTree(t)
	g := &Glob{}
	res, _ := g.Execute(globCtx(root), json.RawMessage(`{"pattern":"**/*.go"}`))
	if res.IsError {
		t.Fatalf("err: %s", res.Text)
	}
	for _, want := range []string{"a.go", "src/main.go", "src/pkg/util.go", "vendor/third.go"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("expected %q in results, got %q", want, res.Text)
		}
	}
	// .tsx + .txt must NOT appear in *.go results.
	if strings.Contains(res.Text, "data.tsx") {
		t.Errorf("data.tsx should not match *.go: %s", res.Text)
	}
	if strings.Contains(res.Text, "b.txt") {
		t.Errorf("b.txt should not match *.go: %s", res.Text)
	}
}

func TestGlob_SingleSegmentPattern(t *testing.T) {
	root := makeGlobTree(t)
	g := &Glob{}
	res, _ := g.Execute(globCtx(root), json.RawMessage(`{"pattern":"*.go"}`))
	if res.IsError {
		t.Fatalf("err: %s", res.Text)
	}
	if !strings.Contains(res.Text, "a.go") {
		t.Errorf("expected a.go in results, got %q", res.Text)
	}
	// `*.go` is single-segment — must NOT match nested files.
	if strings.Contains(res.Text, "src/main.go") {
		t.Errorf("single-segment *.go must not recurse, got %q", res.Text)
	}
}

func TestGlob_MultiSegmentPattern(t *testing.T) {
	root := makeGlobTree(t)
	g := &Glob{}
	res, _ := g.Execute(globCtx(root), json.RawMessage(`{"pattern":"src/**/*.tsx"}`))
	if res.IsError {
		t.Fatalf("err: %s", res.Text)
	}
	if !strings.Contains(res.Text, "src/pkg/data.tsx") {
		t.Errorf("expected src/pkg/data.tsx, got %q", res.Text)
	}
	// Must not include .go files from src/.
	if strings.Contains(res.Text, "src/main.go") || strings.Contains(res.Text, "util.go") {
		t.Errorf("**/*.tsx must exclude .go files: %s", res.Text)
	}
}

func TestGlob_MtimeDescOrdering(t *testing.T) {
	root := t.TempDir()
	// Write three files with deliberate mtime gaps.
	paths := []string{"old.go", "mid.go", "new.go"}
	for _, p := range paths {
		full := filepath.Join(root, p)
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	// old.go = 2h ago, mid.go = 1h ago, new.go = now.
	if err := os.Chtimes(filepath.Join(root, "old.go"), now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "mid.go"), now.Add(-1*time.Hour), now.Add(-1*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "new.go"), now, now); err != nil {
		t.Fatal(err)
	}

	g := &Glob{}
	res, _ := g.Execute(globCtx(root), json.RawMessage(`{"pattern":"*.go"}`))
	if res.IsError {
		t.Fatalf("err: %s", res.Text)
	}
	// Find positions; new.go must come before mid.go before old.go.
	newPos := strings.Index(res.Text, "new.go")
	midPos := strings.Index(res.Text, "mid.go")
	oldPos := strings.Index(res.Text, "old.go")
	if newPos < 0 || midPos < 0 || oldPos < 0 {
		t.Fatalf("missing files in output: %q", res.Text)
	}
	if !(newPos < midPos && midPos < oldPos) {
		t.Errorf("expected mtime DESC ordering (new < mid < old), got:\n%s", res.Text)
	}
}

func TestGlob_MaxResultsTruncation(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 7; i++ {
		p := filepath.Join(root, "f"+string(rune('a'+i))+".go")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := &Glob{MaxResults: 3}
	res, _ := g.Execute(globCtx(root), json.RawMessage(`{"pattern":"*.go"}`))
	if res.IsError {
		t.Fatalf("err: %s", res.Text)
	}
	if !strings.Contains(res.Text, "truncated at max_results=3") {
		t.Errorf("expected truncation marker, got %q", res.Text)
	}
	// 7 total minus 3 shown = 4 should be in the "X total matches" line.
	if !strings.Contains(res.Text, "7 total") {
		t.Errorf("expected total-matches count in truncation marker, got %q", res.Text)
	}
}

func TestGlob_NoMatches(t *testing.T) {
	root := makeGlobTree(t)
	g := &Glob{}
	res, _ := g.Execute(globCtx(root), json.RawMessage(`{"pattern":"**/*.rs"}`))
	if res.IsError {
		t.Fatalf("err: %s", res.Text)
	}
	if !strings.Contains(res.Text, "no matches") {
		t.Errorf("expected no-matches message, got %q", res.Text)
	}
}

// TestGlob_AbsoluteInRootPattern is the exp7 R1 regression: an absolute
// pattern pointing inside the sandbox root must match. The matcher compares
// against root-relative paths, so the leading "/" used to become an empty
// first segment that never matched — an absolute in-root pattern silently
// returned "no matches". It is now relativized to the walk root.
// FAIL-BEFORE: returns "no matches".
func TestGlob_AbsoluteInRootPattern(t *testing.T) {
	root := makeGlobTree(t)
	g := &Glob{}
	abs := root + "/**/*.go"
	in, _ := json.Marshal(map[string]string{"pattern": abs})
	res, _ := g.Execute(globCtx(root), in)
	if res.IsError {
		t.Fatalf("err: %s", res.Text)
	}
	for _, want := range []string{"a.go", "src/main.go", "src/pkg/util.go", "vendor/third.go"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("absolute in-root pattern %q: expected %q in results, got %q", abs, want, res.Text)
		}
	}
	if strings.Contains(res.Text, "no matches") {
		t.Errorf("absolute in-root pattern returned no matches: %q", res.Text)
	}
}

// TestGlob_AbsolutePatternOutsideRootMatchesNothing: an absolute pattern that
// points outside the sandbox root matches nothing — it must not walk or leak
// files outside the root.
func TestGlob_AbsolutePatternOutsideRootMatchesNothing(t *testing.T) {
	root := makeGlobTree(t)
	g := &Glob{}
	res, _ := g.Execute(globCtx(root), json.RawMessage(`{"pattern":"/etc/**/*.conf"}`))
	if res.IsError {
		t.Fatalf("err: %s", res.Text)
	}
	if !strings.Contains(res.Text, "no matches") {
		t.Errorf("absolute out-of-root pattern must match nothing, got %q", res.Text)
	}
}

func TestGlob_PathEscapeRejected(t *testing.T) {
	root := makeGlobTree(t)
	g := &Glob{}
	res, _ := g.Execute(globCtx(root), json.RawMessage(`{"pattern":"*","path":"/etc"}`))
	if !res.IsError {
		t.Errorf("path outside root must refuse, got %q", res.Text)
	}
}

// RFC AH Phase 3: an agent bound to no volume must refuse (sandbox-by-default;
// the legacy LOOMCYCLE_READ_ROOT jail is gone).
func TestGlob_NoVolumeRefuses(t *testing.T) {
	g := &Glob{}
	res, _ := g.Execute(context.Background(), json.RawMessage(`{"pattern":"*"}`))
	if !res.IsError {
		t.Errorf("no volume bound must refuse, got %q", res.Text)
	}
	if !strings.Contains(res.Text, "no filesystem volume available") {
		t.Errorf("refusal must report no volume, got %q", res.Text)
	}
}

// Regression: a syntactically invalid glob pattern (e.g. `[unclosed`)
// must surface as IsError, not silently yield "no matches" via the
// per-file filepath.Match's swallowed ErrBadPattern.
func TestGlob_InvalidPatternIsError(t *testing.T) {
	root := makeGlobTree(t)
	g := &Glob{}
	res, _ := g.Execute(globCtx(root), json.RawMessage(`{"pattern":"[unclosed"}`))
	if !res.IsError {
		t.Errorf("invalid pattern must return IsError, got %q", res.Text)
	}
	if !strings.Contains(res.Text, "invalid glob pattern") {
		t.Errorf("expected 'invalid glob pattern' in error, got %q", res.Text)
	}
}

func TestGlob_PathSubdirScopes(t *testing.T) {
	root := makeGlobTree(t)
	g := &Glob{}
	// Scope to src/ — vendor/third.go must not appear.
	res, _ := g.Execute(globCtx(root), json.RawMessage(`{"pattern":"**/*.go","path":"src"}`))
	if res.IsError {
		t.Fatalf("err: %s", res.Text)
	}
	if strings.Contains(res.Text, "vendor") {
		t.Errorf("path=src must scope out vendor/, got %q", res.Text)
	}
	if !strings.Contains(res.Text, "main.go") || !strings.Contains(res.Text, "pkg/util.go") {
		t.Errorf("expected src/-relative matches, got %q", res.Text)
	}
}
