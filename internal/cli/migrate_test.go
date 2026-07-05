package cli

import (
	"bytes"
	"strings"
	"testing"
)

// migrate up/down/status without any DSN source — should fail with a
// clear "no DSN" message rather than panicking on nil pgxpool.
func TestRunMigrate_UpNoDSN(t *testing.T) {
	t.Setenv("LOOMCYCLE_PG_DSN", "")
	var stdout, stderr bytes.Buffer
	rc := RunMigrate([]string{"up"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "no DSN") {
		t.Errorf("stderr missing 'no DSN': %q", stderr.String())
	}
}

// Unknown migrate verb → rc=2 + helpful message.
func TestRunMigrate_UnknownVerb(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunMigrate([]string{"sideways"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), `unknown migrate verb "sideways"`) {
		t.Errorf("stderr missing diagnostic: %q", stderr.String())
	}
}

func TestRunMigrate_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunMigrate(nil, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("stderr missing usage: %q", stderr.String())
	}
}

// migrate down without --yes refuses (destructive guardrail).
func TestRunMigrate_DownRequiresYes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunMigrate([]string{"down", "--dsn", "postgres://nowhere"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "--yes") {
		t.Errorf("stderr missing --yes hint: %q", stderr.String())
	}
}

// loadPgDSN: --dsn wins over env, env wins over yaml; yaml needs
// backend=postgres + non-empty DSN.
func TestLoadPgDSN_Precedence(t *testing.T) {
	// 1) explicit DSN beats env.
	t.Setenv("LOOMCYCLE_PG_DSN", "from-env")
	got, err := loadPgDSN("", "from-flag")
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-flag" {
		t.Errorf("explicit-flag precedence: got %q", got)
	}

	// 2) env beats yaml.
	got, err = loadPgDSN("", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-env" {
		t.Errorf("env precedence: got %q", got)
	}

	// 3) yaml-only path returns the yaml's DSN.
	t.Setenv("LOOMCYCLE_PG_DSN", "")
	yamlPath := writeTempConfig(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  default:
    system_prompt: "x"
    tools: []
storage:
  backend: postgres
  pg_dsn: from-yaml
`)
	got, err = loadPgDSN(yamlPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-yaml" {
		t.Errorf("yaml precedence: got %q", got)
	}

	// 4) yaml with sqlite backend errors with a pointed message.
	yamlPath2 := writeTempConfig(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  default:
    system_prompt: "x"
    tools: []
`)
	_, err = loadPgDSN(yamlPath2, "")
	if err == nil {
		t.Fatal("expected error for sqlite-backend yaml; got nil")
	}
	if !strings.Contains(err.Error(), "want \"postgres\"") {
		t.Errorf("error doesn't name the fix: %v", err)
	}
}
