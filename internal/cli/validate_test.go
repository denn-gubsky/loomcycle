package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimalConfigYAML = `
defaults:
  provider: anthropic
  model: claude-sonnet-4-6
agents:
  default:
    system_prompt: "You are a helpful assistant."
    tools: []
`

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "loomcycle.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write tempfile: %v", err)
	}
	return path
}

// Happy path: a minimal valid config returns 0 and an "OK" footer.
func TestRunValidate_Clean(t *testing.T) {
	path := writeTempConfig(t, minimalConfigYAML)
	var stdout, stderr bytes.Buffer
	rc := RunValidate([]string{path}, &stdout, &stderr)
	if rc != 0 {
		t.Errorf("rc=%d, want 0; stderr=%q", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "OK") {
		t.Errorf("stdout missing OK footer: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Storage backend  : sqlite") {
		t.Errorf("stdout missing storage backend line: %q", stdout.String())
	}
}

// Missing path arg → usage to stderr, rc=2.
func TestRunValidate_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunValidate(nil, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("stderr missing usage line: %q", stderr.String())
	}
}

// Unknown file → rc=2, error message names the path.
func TestRunValidate_MissingFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunValidate([]string{"/no/such/file.yaml"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "config:") {
		t.Errorf("stderr missing config error: %q", stderr.String())
	}
}

// Postgres backend without a DSN must surface in the validate
// output (so operators see "you set backend=postgres but didn't
// supply a DSN" before the runtime fails to start).
func TestRunValidate_PostgresMissingDSN(t *testing.T) {
	path := writeTempConfig(t, minimalConfigYAML+`
storage:
  backend: postgres
`)
	var stdout, stderr bytes.Buffer
	rc := RunValidate([]string{path}, &stdout, &stderr)
	if rc != 0 {
		t.Errorf("rc=%d, want 0 (validate doesn't dial PG)", rc)
	}
	if !strings.Contains(stdout.String(), "(empty") {
		t.Errorf("stdout should flag empty DSN: %q", stdout.String())
	}
}

// maskDSN must scrub the password between user: and @.
func TestMaskDSN(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"postgres://alice:secret@host:5432/db", "postgres://alice:***@host:5432/db"},
		{"postgres://alice@host:5432/db", "postgres://alice@host:5432/db"},
		{"postgres://host:5432/db", "postgres://host:5432/db"},
		{"postgres://alice:@host/db", "postgres://alice:@host/db"}, // empty pw stays
		// libpq keyword form: the password value is now masked (was printed
		// verbatim before the fix).
		{"host=localhost user=alice password=secret", "host=localhost user=alice password=***"},
		{"host=db user=lc password='pw with spaces' dbname=x", "host=db user=lc password=*** dbname=x"},
		{"host=db user=alice dbname=loomcycle", "host=db user=alice dbname=loomcycle"}, // no pw → unchanged
	}
	for _, c := range cases {
		got := maskDSN(c.in)
		if got != c.want {
			t.Errorf("maskDSN(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A TIER-ONLY agent is not a config error. It is how every bundled agent preset
// is written (chat, memory, document-agent, agent-teams all name a `tier:` and
// pin nothing), and before the fix validate exited 2 on all of them — naming an
// agent the running server resolves without trouble. Because validate returns at
// the first agent, everything after it (MCP servers, the OK footer) went
// unreported too, so the assertions below cover the whole output, not just the
// exit code.
func TestRunValidate_TierOnlyAgentIsNotAnError(t *testing.T) {
	path := writeTempConfig(t, `
tiers:
  middle: [{ provider: anthropic, model: claude-sonnet-4-6 }]
user_tiers:
  default:
    provider_priority: [anthropic]
agents:
  extractor:
    tier: middle
    system_prompt: "extract"
    tools: []
mcp_servers:
  peer:
    transport: http
    url: https://example.invalid/mcp
`)
	var stdout, stderr bytes.Buffer
	if rc := RunValidate([]string{path}, &stdout, &stderr); rc != 0 {
		t.Fatalf("rc = %d, want 0; stderr = %q", rc, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"extractor", "tier:middle", "MCP servers", "OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

// The other half of the same rule: an agent with NEITHER a pin nor a tier has no
// way to reach a model, and the resolver refuses it at run time too — so validate
// must keep failing on it. Without this, "stop failing on tier agents" is one
// sloppy edit away from "stop checking agents".
func TestRunValidate_AgentWithNeitherPinNorTierStillFails(t *testing.T) {
	path := writeTempConfig(t, `
agents:
  orphan:
    system_prompt: "nothing routes me"
    tools: []
`)
	var stdout, stderr bytes.Buffer
	if rc := RunValidate([]string{path}, &stdout, &stderr); rc == 0 {
		t.Fatalf("rc = 0, want non-zero; stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "orphan") {
		t.Errorf("stderr should name the offending agent, got %q", stderr.String())
	}
}

// A tier agent in a config that ALSO sets `defaults:` must still report its tier.
// The runtime takes the tier path before it looks at a pin or at defaults, so
// resolving the pin path first and treating the tier as a fallback would print a
// provider the server never uses for this agent — a wrong answer, which is worse
// than the refusal it replaced.
func TestRunValidate_TierWinsOverDefaultsInTheReport(t *testing.T) {
	path := writeTempConfig(t, `
defaults:
  provider: anthropic
  model: claude-sonnet-4-6
tiers:
  low: [{ provider: openai, model: gpt-5-mini }]
user_tiers:
  default:
    provider_priority: [openai]
agents:
  judge:
    tier: low
    system_prompt: "judge"
    tools: []
`)
	var stdout, stderr bytes.Buffer
	if rc := RunValidate([]string{path}, &stdout, &stderr); rc != 0 {
		t.Fatalf("rc = %d, want 0; stderr = %q", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "tier:low") {
		t.Errorf("expected the tier reported for a tier agent, got:\n%s", out)
	}
	if strings.Contains(out, "claude-sonnet-4-6") {
		t.Errorf("reported the defaults model for an agent the server routes by tier:\n%s", out)
	}
}
