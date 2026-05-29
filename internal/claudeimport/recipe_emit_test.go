package claudeimport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteEmittedRecipes_HappyPath(t *testing.T) {
	overlay := t.TempDir()
	target := filepath.Join(overlay, "new.json")
	report := &ImportReport{
		MCPServers: []*MCPEntry{{
			Name:           "new",
			EmitRecipePath: target,
			EmitRecipeJSON: `{"command": "echo"}`,
		}},
	}
	written, err := WriteEmittedRecipes(report, false)
	if err != nil {
		t.Fatalf("WriteEmittedRecipes: %v", err)
	}
	if len(written) != 1 || !strings.Contains(written[0], target) {
		t.Errorf("expected wrote-line for %s, got %v", target, written)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != `{"command": "echo"}` {
		t.Errorf("file contents wrong: %q", got)
	}
}

func TestWriteEmittedRecipes_RefuseOnCollision(t *testing.T) {
	overlay := t.TempDir()
	target := filepath.Join(overlay, "existing.json")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	report := &ImportReport{
		MCPServers: []*MCPEntry{{
			EmitRecipePath: target,
			EmitRecipeJSON: "new",
		}},
	}
	_, err := WriteEmittedRecipes(report, false)
	if err == nil {
		t.Fatal("expected refusal on collision")
	}
	if !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should mention --force: %v", err)
	}
	// File should be untouched.
	got, _ := os.ReadFile(target)
	if string(got) != "original" {
		t.Errorf("file was clobbered without --force: %q", got)
	}
}

func TestWriteEmittedRecipes_ForceClobbers(t *testing.T) {
	overlay := t.TempDir()
	target := filepath.Join(overlay, "existing.json")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	report := &ImportReport{
		MCPServers: []*MCPEntry{{
			EmitRecipePath: target,
			EmitRecipeJSON: `{"command": "new"}`,
		}},
	}
	_, err := WriteEmittedRecipes(report, true)
	if err != nil {
		t.Fatalf("expected force to succeed: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != `{"command": "new"}` {
		t.Errorf("force did not clobber: %q", got)
	}
}

func TestWriteEmittedRecipes_SkipsBlankEntries(t *testing.T) {
	report := &ImportReport{
		MCPServers: []*MCPEntry{
			{Name: "no-emit"}, // EmitRecipePath blank → skip
			{Name: "no-json", EmitRecipePath: "/tmp/x.json"}, // EmitRecipeJSON blank → skip
		},
	}
	written, err := WriteEmittedRecipes(report, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(written) != 0 {
		t.Errorf("expected no writes, got %v", written)
	}
}

func TestWriteEmittedRecipes_EndToEndViaWalk(t *testing.T) {
	// Build a .claude/mcp.json fixture, walk with EmitRecipes,
	// then flush — proves the planning/writing handshake.
	claude := t.TempDir()
	if err := os.WriteFile(filepath.Join(claude, "mcp.json"), []byte(`{
  "mcpServers": {
    "scratch": {
      "command": "scratch-bin",
      "args": ["--port", "9999"]
    }
  }
}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	overlay := t.TempDir()
	report, err := Walk(claude, WalkOptions{
		EmitRecipes: true,
		OverlayRoot: overlay,
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(report.MCPServers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(report.MCPServers))
	}
	if report.MCPServers[0].EmitRecipePath == "" {
		t.Fatal("EmitRecipePath should be populated under EmitRecipes")
	}
	written, err := WriteEmittedRecipes(report, false)
	if err != nil {
		t.Fatalf("WriteEmittedRecipes: %v", err)
	}
	if len(written) != 1 {
		t.Fatalf("expected 1 write, got %d", len(written))
	}
	// Verify the JSON is valid + carries the operator's content.
	raw, err := os.ReadFile(filepath.Join(overlay, "scratch.json"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Errorf("emitted file is not valid JSON: %v", err)
	}
	if probe["command"] != "scratch-bin" {
		t.Errorf("command field wrong in emitted file: %+v", probe)
	}
	if probe["_loomcycle"] == nil {
		t.Errorf("emitted file should carry _loomcycle metadata block: %+v", probe)
	}
}

func TestWriteSkillCopies_CreatesDestinationDir(t *testing.T) {
	src := t.TempDir()
	srcFile := filepath.Join(src, "SKILL.md")
	if err := os.WriteFile(srcFile, []byte("skill body"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dst := t.TempDir()
	dstPath := filepath.Join(dst, "skills", "foo", "SKILL.md")
	report := &ImportReport{
		Skills: []*SkillEntry{{
			Name:            "foo",
			SourcePath:      srcFile,
			DestinationPath: dstPath,
		}},
	}
	written, err := WriteSkillCopies(report, false)
	if err != nil {
		t.Fatalf("WriteSkillCopies: %v", err)
	}
	if len(written) != 1 {
		t.Fatalf("expected 1 write, got %v", written)
	}
	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "skill body" {
		t.Errorf("contents wrong: %q", got)
	}
}

func TestWriteSkillCopies_RefuseOnCollision(t *testing.T) {
	src := t.TempDir()
	srcFile := filepath.Join(src, "SKILL.md")
	os.WriteFile(srcFile, []byte("new"), 0o644)
	dst := t.TempDir()
	dstFile := filepath.Join(dst, "SKILL.md")
	os.WriteFile(dstFile, []byte("original"), 0o644)
	report := &ImportReport{
		Skills: []*SkillEntry{{
			SourcePath: srcFile, DestinationPath: dstFile,
		}},
	}
	_, err := WriteSkillCopies(report, false)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected refusal, got %v", err)
	}
	got, _ := os.ReadFile(dstFile)
	if string(got) != "original" {
		t.Errorf("file was clobbered without --force: %q", got)
	}
}

func TestParentDir(t *testing.T) {
	cases := map[string]string{
		"/a/b/c.txt": "/a/b",
		"a/b":        "a",
		"justfile":   ".",
		"/":          "/",
	}
	for in, want := range cases {
		got := parentDir(in)
		if got != want {
			t.Errorf("parentDir(%q) = %q, want %q", in, got, want)
		}
	}
}
