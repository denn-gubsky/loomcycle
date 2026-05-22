package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/skills"
)

func TestRunHashAgent_PrintsSameHashAsInProcessSign(t *testing.T) {
	dir := t.TempDir()
	md := filepath.Join(dir, "researcher.md")
	body := `---
name: researcher
description: thorough investigator
allowed_tools: [Read, WebFetch]
max_tokens: 8192
max_iterations: 32
---
be thorough
`
	if err := os.WriteFile(md, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := RunHash([]string{"agent", md}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("RunHash exit %d; stderr=%s", code, stderr.String())
	}
	got := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(got, "sha256:") || len(got) != 71 {
		t.Errorf("malformed hash %q", got)
	}

	// Recompute the hash via the same Go code the server uses on the
	// inbound substrate path. Equality here is the load-bearing
	// guarantee — CLI hash MUST equal server hash for matching content.
	set, err := agents.LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	a, _ := set.Get("researcher")
	want := agents.Sign(agents.FromYAMLAgent(a))
	if got != want {
		t.Errorf("CLI hash %s != in-process hash %s — drift between code paths", got, want)
	}
}

func TestRunHashSkill_AcceptsSKILLMdPath(t *testing.T) {
	skillRoot := t.TempDir()
	skillDir := filepath.Join(skillRoot, "summariser")
	if err := os.Mkdir(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := filepath.Join(skillDir, "SKILL.md")
	body := `---
name: summariser
allowed-tools: [Read]
---
Summarise the input concisely.
`
	if err := os.WriteFile(md, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := RunHash([]string{"skill", md}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("RunHash skill exit %d; stderr=%s", code, stderr.String())
	}
	got := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(got, "sha256:") {
		t.Errorf("malformed: %q", got)
	}
	// Equality with in-process sign.
	set, _ := skills.LoadSet(skillRoot)
	sk, _ := set.Get("summariser")
	want := skills.Sign(skills.FromSkill(sk))
	if got != want {
		t.Errorf("CLI vs in-process drift: %s vs %s", got, want)
	}
}

func TestRunHashSkill_AcceptsSkillDirPath(t *testing.T) {
	skillRoot := t.TempDir()
	skillDir := filepath.Join(skillRoot, "voice-applier")
	_ = os.Mkdir(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: voice-applier\n---\nbe terse\n"), 0o600)

	var stdout, stderr bytes.Buffer
	code := RunHash([]string{"skill", skillDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("RunHash skill exit %d; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); !strings.HasPrefix(got, "sha256:") {
		t.Errorf("expected hash, got %q", got)
	}
}

func TestRunHash_UnknownVerb(t *testing.T) {
	var stderr bytes.Buffer
	code := RunHash([]string{"foo"}, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestRunHash_NoArgs(t *testing.T) {
	var stderr bytes.Buffer
	code := RunHash(nil, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestRunHashAgent_MissingFile(t *testing.T) {
	var stderr bytes.Buffer
	code := RunHash([]string{"agent", "/nonexistent/whatever.md"}, &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit for missing file")
	}
}
