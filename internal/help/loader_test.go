package help

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSet_BundledOnly(t *testing.T) {
	set, err := LoadSet("")
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	names := set.Names()
	if len(names) == 0 {
		t.Fatal("no bundled topics")
	}
	// Sentinel list of every bundled topic that MUST load. Adding
	// a new file under internal/help/builtin/ requires extending
	// this list — that's load-bearing: an accidental rename or
	// deletion of a shipped topic would otherwise pass CI. Kept
	// in alphabetical order for diff readability.
	want := []string{
		"channel-admin",
		"content-signatures",
		"dynamic-mcp",
		"experimentation",
		"fairness",
		"fan-out-patterns",
		"getting-started",
		"installation",
		"interruption",
		"llm-gateway",
		"loomcycle",
		"memory-reducers",
		"n8n-integration",
		"observability",
		"openai-compat",
		"pause-resume-snapshot",
		"scopes",
		"skills-evolution",
		"sqlite-vec",
		"subagents",
		"system-channels",
		"vector-memory",
		"voyage-embedder",
	}
	for _, w := range want {
		if _, ok := set.Get(w); !ok {
			t.Errorf("bundled topic %q missing (got: %v)", w, names)
		}
	}
	for _, n := range names {
		topic, _ := set.Get(n)
		if topic.Source != "bundled" {
			t.Errorf("topic %q source = %q, want bundled", n, topic.Source)
		}
		if topic.Description == "" {
			t.Errorf("topic %q missing description", n)
		}
		if topic.Content == "" {
			t.Errorf("topic %q missing content", n)
		}
		if topic.Path != "" {
			t.Errorf("bundled topic %q has Path=%q, want empty", n, topic.Path)
		}
	}
}

func TestLoadSet_FilesystemOverlay(t *testing.T) {
	dir := t.TempDir()
	body := "---\nname: my-deployment\ndescription: deployment-specific guidance\n---\nHello world body.\n"
	if err := os.WriteFile(filepath.Join(dir, "my-deployment.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	topic, ok := set.Get("my-deployment")
	if !ok {
		t.Fatal("filesystem topic not loaded")
	}
	if topic.Source != "filesystem" {
		t.Errorf("source = %q, want filesystem", topic.Source)
	}
	if topic.Path == "" {
		t.Error("filesystem topic missing Path")
	}
	if !strings.Contains(topic.Content, "Hello world body") {
		t.Errorf("content = %q", topic.Content)
	}
	// Bundled defaults must still be present alongside.
	if _, ok := set.Get("scopes"); !ok {
		t.Error("bundled scopes topic dropped by filesystem overlay")
	}
}

func TestLoadSet_FilesystemOverridesBundled(t *testing.T) {
	dir := t.TempDir()
	body := "---\nname: scopes\ndescription: operator override\n---\nOperator body.\n"
	if err := os.WriteFile(filepath.Join(dir, "scopes.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	topic, ok := set.Get("scopes")
	if !ok {
		t.Fatal("scopes missing after override")
	}
	if topic.Source != "filesystem" {
		t.Errorf("override source = %q, want filesystem", topic.Source)
	}
	if topic.Description != "operator override" {
		t.Errorf("description = %q (override not applied)", topic.Description)
	}
	if !strings.Contains(topic.Content, "Operator body") {
		t.Errorf("content not from override: %q", topic.Content)
	}
}

func TestLoadSet_NonexistentRoot(t *testing.T) {
	_, err := LoadSet("/this/does/not/exist/loomcycle-help-test")
	if err == nil {
		t.Fatal("expected error for nonexistent root")
	}
}

func TestLoadSet_RootIsFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "regular-file")
	if err := os.WriteFile(f, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSet(f)
	if err == nil {
		t.Fatal("expected error for non-directory root")
	}
}

func TestLoadSet_SubdirSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "subdir.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	set, err := LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	if _, ok := set.Get("subdir"); ok {
		t.Error("subdir mistakenly loaded as topic")
	}
}

func TestParseTopic_NameMismatch(t *testing.T) {
	data := []byte("---\nname: foo\ndescription: x\n---\nbody\n")
	_, err := parseTopic(data, "bar", "")
	if err == nil || !strings.Contains(err.Error(), "doesn't match filename") {
		t.Fatalf("expected name-mismatch error, got %v", err)
	}
}

func TestParseTopic_MissingFrontmatter(t *testing.T) {
	data := []byte("no frontmatter at all\n")
	_, err := parseTopic(data, "x", "")
	if err == nil || !strings.Contains(err.Error(), "missing opening frontmatter") {
		t.Fatalf("expected missing-frontmatter error, got %v", err)
	}
}

func TestParseTopic_MissingClosingFrontmatter(t *testing.T) {
	data := []byte("---\nname: x\ndescription: y\nbody but no closing\n")
	_, err := parseTopic(data, "x", "")
	if err == nil || !strings.Contains(err.Error(), "missing closing frontmatter") {
		t.Fatalf("expected missing-closing-fm error, got %v", err)
	}
}

func TestParseTopic_EmptyBody(t *testing.T) {
	data := []byte("---\nname: x\ndescription: y\n---\n   \n\t\n")
	_, err := parseTopic(data, "x", "")
	if err == nil || !strings.Contains(err.Error(), "empty topic body") {
		t.Fatalf("expected empty-body error, got %v", err)
	}
}

func TestParseTopic_MissingName(t *testing.T) {
	data := []byte("---\ndescription: y\n---\nbody\n")
	_, err := parseTopic(data, "x", "")
	if err == nil || !strings.Contains(err.Error(), "missing `name:`") {
		t.Fatalf("expected missing-name error, got %v", err)
	}
}

func TestParseTopic_MissingDescription(t *testing.T) {
	data := []byte("---\nname: x\n---\nbody\n")
	_, err := parseTopic(data, "x", "")
	if err == nil || !strings.Contains(err.Error(), "missing `description:`") {
		t.Fatalf("expected missing-description error, got %v", err)
	}
}

func TestParseTopic_MalformedYAML(t *testing.T) {
	data := []byte("---\nname: x\n  bad indent: value\n---\nbody\n")
	_, err := parseTopic(data, "x", "")
	if err == nil {
		t.Fatal("expected yaml parse error")
	}
}

func TestLoadSet_FilesystemMalformedTopicSoftSkipped(t *testing.T) {
	dir := t.TempDir()
	// Missing frontmatter → parse error. Loader must SKIP the file
	// (with a log line) rather than failing the whole load — bundled
	// topics are already in the set, and one bad operator file must
	// not kill the runtime.
	if err := os.WriteFile(filepath.Join(dir, "broken.md"), []byte("no frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}
	// And one valid sibling, to confirm the loader keeps going.
	good := "---\nname: good\ndescription: ok\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "good.md"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet should not fail on bad overlay file: %v", err)
	}
	if _, ok := set.Get("broken"); ok {
		t.Error("broken.md should not be present in the set")
	}
	if _, ok := set.Get("good"); !ok {
		t.Error("good.md should still load after a sibling parse error")
	}
	// Bundled defaults must remain intact.
	if _, ok := set.Get("scopes"); !ok {
		t.Error("bundled scopes dropped after malformed overlay")
	}
}

func TestLoadSet_FilesystemSymlinkSkipped(t *testing.T) {
	dir := t.TempDir()
	// A regular target that LOOKS like a valid topic.
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "secret.md")
	body := "---\nname: escape\ndescription: should not be reachable\n---\nsecret body\n"
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Symlink inside the help root pointing at the target outside it.
	link := filepath.Join(dir, "escape.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}
	set, err := LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	if _, ok := set.Get("escape"); ok {
		t.Error("symlinked topic was loaded — symlink traversal must be refused")
	}
}

func TestSet_NilSafe(t *testing.T) {
	var s *Set
	if _, ok := s.Get("anything"); ok {
		t.Error("nil Set.Get should return ok=false")
	}
	if n := s.Names(); n != nil {
		t.Error("nil Set.Names should return nil")
	}
	if a := s.All(); a != nil {
		t.Error("nil Set.All should return nil")
	}
}
