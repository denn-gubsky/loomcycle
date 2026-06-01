package codejs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCodeJS_Compiler_RejectsPathTraversal pins the host-side containment
// floor: an agent name must not escape CodeRoot. Regression-grade — a real
// index.js placed OUTSIDE the root must NOT be compiled via "../evil"; on the
// unfixed compiler.load (filepath.Join with no containment) it was read +
// compiled successfully.
func TestCodeJS_Compiler_RejectsPathTraversal(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "agent_code")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// A valid code-agent sitting OUTSIDE CodeRoot, reachable only by traversal.
	outside := filepath.Join(base, "evil")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "index.js"), []byte("function run(){return {final_text:'pwned'}}"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := newCompiler(root)
	if _, err := c.load("../evil"); err == nil {
		t.Fatal(`load("../evil") compiled an index.js OUTSIDE CodeRoot — containment floor missing`)
	}
	for _, bad := range []string{"a/b", `a\b`, "..", ".", "../../etc/hosts"} {
		if _, err := c.load(bad); err == nil {
			t.Errorf("load(%q) was not rejected by the containment floor", bad)
		}
	}
	// A plain name still resolves under root (no false positive).
	if err := os.MkdirAll(filepath.Join(root, "ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok", "index.js"), []byte("function run(){return {}}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.load("ok"); err != nil {
		t.Errorf("load(%q) for a valid in-root agent failed: %v", "ok", err)
	}
}
