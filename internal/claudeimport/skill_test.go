package claudeimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, name, body string, extras map[string]string) {
	t.Helper()
	d := filepath.Join(root, "skills", name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile SKILL.md: %v", err)
	}
	for fname, content := range extras {
		if err := os.WriteFile(filepath.Join(d, fname), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", fname, err)
		}
	}
}

func TestWalkSkills_SingleFile(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "yaml-fence", `---
allowed-tools: Read, Edit
---
Skill body.
`, nil)
	report, err := Walk(root, WalkOptions{SkillsDest: "/dest/skills"})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(report.Skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(report.Skills))
	}
	s := report.Skills[0]
	if s.Name != "yaml-fence" {
		t.Errorf("name: got %q", s.Name)
	}
	if !strings.HasSuffix(s.SourcePath, "skills/yaml-fence/SKILL.md") {
		t.Errorf("source: %q", s.SourcePath)
	}
	if s.DestinationPath != "/dest/skills/yaml-fence/SKILL.md" {
		t.Errorf("dest: %q", s.DestinationPath)
	}
	if s.MultiFile {
		t.Errorf("single-file skill should not be flagged multi-file")
	}
	if len(report.Warnings) != 0 {
		t.Errorf("no warnings expected, got %v", report.Warnings)
	}
}

func TestWalkSkills_MultiFileFlagged(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "complex", "body\n", map[string]string{
		"helper.md":   "support",
		"examples.md": "examples",
	})
	report, err := Walk(root, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	s := report.Skills[0]
	if !s.MultiFile {
		t.Errorf("expected MultiFile=true")
	}
	if len(s.SupplementaryAny) != 2 {
		t.Errorf("expected 2 supplementary files, got %v", s.SupplementaryAny)
	}
	// Verify a warning about Approach A bundling fired.
	found := false
	for _, w := range report.Warnings {
		if strings.Contains(w, "multi-file") && strings.Contains(w, "Approach A") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected multi-file warning mentioning Approach A: %v", report.Warnings)
	}
}

func TestWalkSkills_MissingSkillMD(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "skills", "no-skill-md")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(d, "README.md"), []byte("nope"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	report, err := Walk(root, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(report.Skills) != 0 {
		t.Errorf("expected 0 skills, got %d", len(report.Skills))
	}
	if len(report.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(report.Warnings))
	}
	if !strings.Contains(report.Warnings[0], "no SKILL.md") {
		t.Errorf("warning should mention missing SKILL.md: %q", report.Warnings[0])
	}
}

func TestWalkSkills_HiddenSubdirectoriesIgnored(t *testing.T) {
	root := t.TempDir()
	// .git inside skills/ should not become a skill.
	if err := os.MkdirAll(filepath.Join(root, "skills", ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeSkill(t, root, "real", "body\n", nil)
	report, _ := Walk(root, WalkOptions{})
	if len(report.Skills) != 1 {
		t.Fatalf("expected 1 skill (hidden ignored), got %d", len(report.Skills))
	}
	if report.Skills[0].Name != "real" {
		t.Errorf("expected 'real' skill, got %q", report.Skills[0].Name)
	}
}

func TestWalkSkills_NoSkillsDir(t *testing.T) {
	root := t.TempDir()
	// .claude/ exists but no skills/ subdirectory at all.
	report, err := Walk(root, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(report.Skills) != 0 {
		t.Errorf("expected 0 skills, got %d", len(report.Skills))
	}
	if len(report.Warnings) != 0 {
		t.Errorf("no warnings expected, got %v", report.Warnings)
	}
}

func TestWalkSkills_OrderedDeterministically(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "zebra", "z\n", nil)
	writeSkill(t, root, "alpha", "a\n", nil)
	writeSkill(t, root, "mike", "m\n", nil)
	report, _ := Walk(root, WalkOptions{})
	if len(report.Skills) != 3 {
		t.Fatalf("expected 3 skills, got %d", len(report.Skills))
	}
	want := []string{"alpha", "mike", "zebra"}
	for i, n := range want {
		if report.Skills[i].Name != n {
			t.Errorf("at %d: got %q want %q", i, report.Skills[i].Name, n)
		}
	}
}
