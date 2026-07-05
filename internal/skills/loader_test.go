package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Happy path: a skill with full frontmatter + body parses into all
// four fields. Verifies the directory name becomes the canonical Name
// and the closing --- correctly demarcates the body.
func TestLoadSet_Basic(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "voice-applier")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# voice-applier\n\nApply the voice rules.\n"
	content := "---\n" +
		"name: voice-applier\n" +
		"description: Apply voice rules to prose.\n" +
		"allowed-tools:\n" +
		"  - Read\n" +
		"  - Write\n" +
		"  - Edit\n" +
		"---\n\n" + body
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	set, err := LoadSet(root)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	sk, ok := set.Get("voice-applier")
	if !ok {
		t.Fatalf("voice-applier not loaded; names=%v", set.Names())
	}
	if sk.Name != "voice-applier" {
		t.Errorf("Name = %q", sk.Name)
	}
	if sk.Description != "Apply voice rules to prose." {
		t.Errorf("Description = %q", sk.Description)
	}
	if len(sk.Tools) != 3 || sk.Tools[0] != "Read" {
		t.Errorf("Tools = %v", sk.Tools)
	}
	if !strings.HasPrefix(sk.Body, "\n# voice-applier") {
		t.Errorf("Body = %q (expected to start with body markdown after closing ---)", sk.Body)
	}
}

// Empty SkillsRoot returns an empty (non-nil) Set so callers can Get()
// without crashing. LOOMCYCLE_SKILLS_ROOT is optional; agents that
// don't list skills should keep working.
func TestLoadSet_EmptyRoot(t *testing.T) {
	set, err := LoadSet("")
	if err != nil {
		t.Fatalf("LoadSet(\"\"): %v", err)
	}
	if set == nil {
		t.Fatal("expected non-nil Set on empty root")
	}
	if _, ok := set.Get("anything"); ok {
		t.Errorf("unexpected hit on empty Set")
	}
	if names := set.Names(); len(names) != 0 {
		t.Errorf("Names() = %v, want []", names)
	}
}

// Missing root directory is an error — almost certainly a misconfigured
// LOOMCYCLE_SKILLS_ROOT, which we surface loudly.
func TestLoadSet_MissingRoot(t *testing.T) {
	_, err := LoadSet(filepath.Join(t.TempDir(), "no-such-dir"))
	if err == nil {
		t.Fatal("expected error for missing root")
	}
}

// Drift detection: frontmatter `name:` disagreeing with the directory
// name is the kind of silent breakage that bites operators. Refuse to
// load, with a message that names both the file and the conflict.
func TestLoadSet_NameMismatch(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "voice-applier")
	os.MkdirAll(dir, 0o755)
	content := "---\nname: cv-voice-applier\ndescription: Wrong name.\n---\nbody\n"
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600)

	_, err := LoadSet(root)
	if err == nil || !strings.Contains(err.Error(), "frontmatter name") {
		t.Errorf("expected frontmatter-name error, got %v", err)
	}
}

// A SKILL.md with no frontmatter is body-only. Name falls back to the
// directory name. Tools defaults to nil (skill needs no tools).
func TestLoadSet_NoFrontmatter(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "raw-skill")
	os.MkdirAll(dir, 0o755)
	body := "Just a plain markdown body.\n"
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600)

	set, err := LoadSet(root)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	sk, ok := set.Get("raw-skill")
	if !ok {
		t.Fatal("raw-skill not loaded")
	}
	if sk.Body != body {
		t.Errorf("Body = %q", sk.Body)
	}
	if sk.Tools != nil {
		t.Errorf("Tools = %v, want nil", sk.Tools)
	}
}

// An opening "---\n" with no closing "---" is malformed. Refuse to load
// rather than treating the rest of the file as body — silent acceptance
// would hide a YAML-frontmatter syntax bug.
func TestLoadSet_UnclosedFrontmatter(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "broken")
	os.MkdirAll(dir, 0o755)
	content := "---\nname: broken\nno closing line below\nbody continues...\n"
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600)

	_, err := LoadSet(root)
	if err == nil || !strings.Contains(err.Error(), "no closing") {
		t.Errorf("expected unclosed-frontmatter error, got %v", err)
	}
}

// Subdirectories without a SKILL.md are skipped silently. A skill may
// stage auxiliary content (a references/ folder, a fixtures dir, etc.)
// alongside SKILL.md without breaking discovery.
func TestLoadSet_SkipsDirsWithoutSkillMd(t *testing.T) {
	root := t.TempDir()
	// Real skill
	good := filepath.Join(root, "good-skill")
	os.MkdirAll(good, 0o755)
	os.WriteFile(filepath.Join(good, "SKILL.md"), []byte("body\n"), 0o600)
	// Aux directory (no SKILL.md)
	aux := filepath.Join(root, "auxiliary")
	os.MkdirAll(aux, 0o755)
	os.WriteFile(filepath.Join(aux, "README.md"), []byte("aux\n"), 0o600)

	set, err := LoadSet(root)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	if _, ok := set.Get("good-skill"); !ok {
		t.Error("good-skill not loaded")
	}
	if _, ok := set.Get("auxiliary"); ok {
		t.Error("auxiliary should not be loaded as a skill")
	}
}

// CRLF line endings in a SKILL.md — produced when authors edit on
// Windows or paste into editors that default to CRLF — must parse the
// same as LF.
func TestLoadSet_CRLFLineEndings(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "windows-skill")
	os.MkdirAll(dir, 0o755)
	content := "---\r\nname: windows-skill\r\nallowed-tools:\r\n  - Read\r\n---\r\nbody\r\n"
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600)

	set, err := LoadSet(root)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	sk, ok := set.Get("windows-skill")
	if !ok {
		t.Fatal("windows-skill not loaded")
	}
	if len(sk.Tools) != 1 || sk.Tools[0] != "Read" {
		t.Errorf("Tools = %v", sk.Tools)
	}
	if !strings.Contains(sk.Body, "body") {
		t.Errorf("Body = %q", sk.Body)
	}
}

// Get() and Names() are safe on a nil receiver. The config layer holds
// a *Set that may be nil when LOOMCYCLE_SKILLS_ROOT is unset; callers
// shouldn't have to check before every lookup.
func TestSet_NilSafe(t *testing.T) {
	var s *Set
	if _, ok := s.Get("anything"); ok {
		t.Error("nil Get should miss")
	}
	if names := s.Names(); names != nil {
		t.Errorf("nil Names = %v", names)
	}
}
