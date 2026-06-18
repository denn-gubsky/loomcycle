package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// grepCtx attaches a default rw volume rooted at root, the standard ctx the
// Grep tests run under (RFC AH Phase 3: file tools require a bound volume).
func grepCtx(root string) context.Context {
	return ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
}

// makeGrepTree builds a small sandbox with a known structure for the
// Grep tests. Returns the root path; t.TempDir handles cleanup.
//
//	root/
//	  a.go     — contains "func main"
//	  b.go     — contains "TODO: refactor"
//	  c.txt    — contains "hello world"
//	  sub/
//	    d.go   — contains "func main" + "func init"
//	    bin    — binary (NUL byte at start)
func makeGrepTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"a.go":     "package x\n\nfunc main() {}\n",
		"b.go":     "package y\n// TODO: refactor\n",
		"c.txt":    "hello world\nsecond line\n",
		"sub/d.go": "package z\n\nfunc main() {}\nfunc init() {}\n",
	}
	for path, content := range files {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Binary file.
	if err := os.WriteFile(filepath.Join(root, "sub", "bin"), []byte{0, 1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestGrep_FilesWithMatchesIsDefault(t *testing.T) {
	root := makeGrepTree(t)
	g := &Grep{}
	res, _ := g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"func main"}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "a.go") {
		t.Errorf("expected a.go in results, got %q", res.Text)
	}
	if !strings.Contains(res.Text, "sub/d.go") {
		t.Errorf("expected sub/d.go in results, got %q", res.Text)
	}
	// b.go and c.txt don't contain "func main"
	if strings.Contains(res.Text, "b.go") {
		t.Errorf("b.go should not match")
	}
	if strings.Contains(res.Text, "c.txt") {
		t.Errorf("c.txt should not match")
	}
}

func TestGrep_NoMatchesReturnsClearMessage(t *testing.T) {
	root := makeGrepTree(t)
	g := &Grep{}
	res, _ := g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"this-pattern-matches-nothing"}`))
	if res.IsError {
		t.Fatalf("no-match should not be IsError: %s", res.Text)
	}
	if !strings.Contains(res.Text, "no matches") {
		t.Errorf("expected 'no matches' message, got %q", res.Text)
	}
}

func TestGrep_InvalidRegexIsError(t *testing.T) {
	root := makeGrepTree(t)
	g := &Grep{}
	res, _ := g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"["}`))
	if !res.IsError {
		t.Errorf("invalid regex should be IsError: %s", res.Text)
	}
}

// RFC AH Phase 3: an agent bound to no volume must refuse (sandbox-by-default;
// the legacy LOOMCYCLE_READ_ROOT jail is gone).
func TestGrep_NoVolumeRefuses(t *testing.T) {
	g := &Grep{}
	res, _ := g.Execute(context.Background(), json.RawMessage(`{"pattern":"x"}`))
	if !res.IsError {
		t.Errorf("no volume bound should refuse: %s", res.Text)
	}
	if !strings.Contains(res.Text, "no filesystem volume available") {
		t.Errorf("refusal should report no volume, got %q", res.Text)
	}
}

func TestGrep_CaseInsensitive(t *testing.T) {
	root := makeGrepTree(t)
	g := &Grep{}
	res, _ := g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"TODO","case_insensitive":false}`))
	if res.IsError || !strings.Contains(res.Text, "b.go") {
		t.Errorf("case-sensitive TODO should match b.go: %s", res.Text)
	}
	res, _ = g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"todo","case_insensitive":true}`))
	if res.IsError || !strings.Contains(res.Text, "b.go") {
		t.Errorf("case-insensitive todo should match b.go: %s", res.Text)
	}
	// Default case-sensitive: lowercase "todo" shouldn't match.
	res, _ = g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"todo"}`))
	if !strings.Contains(res.Text, "no matches") {
		t.Errorf("default case-sensitive lowercase 'todo' should not match: %s", res.Text)
	}
}

func TestGrep_GlobFilter(t *testing.T) {
	root := makeGrepTree(t)
	g := &Grep{}
	res, _ := g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"hello","glob":"*.go"}`))
	// hello is only in c.txt; glob excludes it.
	if !strings.Contains(res.Text, "no matches") {
		t.Errorf("glob=*.go should exclude c.txt: %s", res.Text)
	}
	res, _ = g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"hello","glob":"*.txt"}`))
	if res.IsError || !strings.Contains(res.Text, "c.txt") {
		t.Errorf("glob=*.txt should include c.txt: %s", res.Text)
	}
}

func TestGrep_ContentModeFormatsLineNumber(t *testing.T) {
	root := makeGrepTree(t)
	g := &Grep{}
	res, _ := g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"func main","output_mode":"content"}`))
	if res.IsError {
		t.Fatalf("content mode err: %s", res.Text)
	}
	// a.go has "func main" on line 3.
	if !strings.Contains(res.Text, "a.go:3:") {
		t.Errorf("expected 'a.go:3:' marker in content mode, got %q", res.Text)
	}
}

func TestGrep_ContentModeWithContextLines(t *testing.T) {
	root := makeGrepTree(t)
	g := &Grep{}
	res, _ := g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"func main","output_mode":"content","-C":1}`))
	if res.IsError {
		t.Fatalf("%s", res.Text)
	}
	// -C=1 means one line before + one after. a.go only has 3 lines
	// so no line 4 exists; assert the before-line. sub/d.go has 4
	// lines and matches on line 3, so it gets both context lines.
	if !strings.Contains(res.Text, "a.go-2-") {
		t.Errorf("expected -C before-context on a.go line 2, got %q", res.Text)
	}
	if !strings.Contains(res.Text, "sub/d.go-2-") || !strings.Contains(res.Text, "sub/d.go-4-") {
		t.Errorf("expected -C context around sub/d.go line 3, got %q", res.Text)
	}
}

func TestGrep_CountModeReportsPerFileCount(t *testing.T) {
	root := makeGrepTree(t)
	g := &Grep{}
	res, _ := g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"func","output_mode":"count"}`))
	if res.IsError {
		t.Fatalf("%s", res.Text)
	}
	// sub/d.go has 2 lines containing "func"; a.go has 1.
	if !strings.Contains(res.Text, "sub/d.go:2") {
		t.Errorf("expected 'sub/d.go:2' in count, got %q", res.Text)
	}
	if !strings.Contains(res.Text, "a.go:1") {
		t.Errorf("expected 'a.go:1' in count, got %q", res.Text)
	}
}

func TestGrep_BinaryFileSkipped(t *testing.T) {
	root := makeGrepTree(t)
	g := &Grep{}
	// The binary file contains \x00\x01\x02\x03 — bytes 1 and 2 are
	// ASCII chars. Search for "\x02" via regex would match bytes;
	// but binary detection should skip the file entirely.
	res, _ := g.Execute(grepCtx(root), json.RawMessage(`{"pattern":".","output_mode":"files_with_matches"}`))
	if res.IsError {
		t.Fatalf("%s", res.Text)
	}
	if strings.Contains(res.Text, "/bin\n") || strings.Contains(res.Text, "sub/bin") {
		t.Errorf("binary file should be skipped, got %q", res.Text)
	}
}

func TestGrep_PathEscapeIsRefused(t *testing.T) {
	root := makeGrepTree(t)
	g := &Grep{}
	res, _ := g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"x","path":"/etc"}`))
	if !res.IsError {
		t.Errorf("path outside root should refuse, got %q", res.Text)
	}
}

func TestGrep_HeadLimitTruncates(t *testing.T) {
	root := t.TempDir()
	// Create 15 files all matching the pattern.
	for i := 0; i < 15; i++ {
		p := filepath.Join(root, "f"+string(rune('0'+i))+".txt")
		if err := os.WriteFile(p, []byte("MATCH\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := &Grep{}
	res, _ := g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"MATCH","head_limit":5}`))
	if res.IsError {
		t.Fatalf("%s", res.Text)
	}
	// Should mention truncation.
	if !strings.Contains(res.Text, "truncated") {
		t.Errorf("expected truncation marker, got %q", res.Text)
	}
}

// Regression: head_limit exactly equal to the result count must NOT
// emit a truncation marker. Previously the post-write check fired on
// the last in-bounds result, flagging truncation that didn't happen.
func TestGrep_HeadLimitExactCountNoTruncation(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		p := filepath.Join(root, "f"+string(rune('0'+i))+".txt")
		if err := os.WriteFile(p, []byte("MATCH\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := &Grep{}
	res, _ := g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"MATCH","head_limit":5}`))
	if res.IsError {
		t.Fatalf("%s", res.Text)
	}
	if strings.Contains(res.Text, "truncated") {
		t.Errorf("head_limit==results should not emit truncation marker, got %q", res.Text)
	}
}

func TestGrep_InvalidGlobFilterIsError(t *testing.T) {
	root := makeGrepTree(t)
	g := &Grep{}
	res, _ := g.Execute(grepCtx(root), json.RawMessage(`{"pattern":"x","glob":"[unclosed"}`))
	if !res.IsError {
		t.Errorf("invalid glob filter must be reported, got %q", res.Text)
	}
	if !strings.Contains(res.Text, "glob") {
		t.Errorf("error message should mention glob, got %q", res.Text)
	}
}
