package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const agentsConfigYAML = `
defaults:
  provider: anthropic
  model: claude-sonnet-4-6
models:
  cheap: { provider: anthropic, model: claude-haiku-4-5 }
agents:
  default:
    system_prompt: "You are a helpful assistant."
    tools: []
  classifier:
    model: cheap
    system_prompt: "Classify each input."
    tools: []
    max_tokens: 4096
`

func TestRunAgentsList_HumanFormat(t *testing.T) {
	path := writeTempConfig(t, agentsConfigYAML)
	var stdout, stderr bytes.Buffer
	rc := RunAgents([]string{"list", "--config", path}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc=%d, stderr=%q", rc, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"default", "classifier",
		"provider     : anthropic",
		"model        : claude-sonnet-4-6",
		"model        : claude-haiku-4-5",
		"max_tokens   : 4096",
		"max_tokens   : default",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\n--- got ---\n%s", want, out)
		}
	}
}

func TestRunAgentsList_JSONFormat(t *testing.T) {
	path := writeTempConfig(t, agentsConfigYAML)
	var stdout, stderr bytes.Buffer
	rc := RunAgents([]string{"list", "--config", path, "--json"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc=%d, stderr=%q", rc, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		`"name": "default"`,
		`"name": "classifier"`,
		`"provider": "anthropic"`,
		`"model": "claude-haiku-4-5"`,
		`"max_tokens": 4096`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\n--- got ---\n%s", want, out)
		}
	}
}

func TestRunAgents_UnknownVerb(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunAgents([]string{"badverb"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), `unknown agents verb "badverb"`) {
		t.Errorf("stderr missing usage hint: %q", stderr.String())
	}
}

func TestRunAgents_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunAgents(nil, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("stderr missing usage: %q", stderr.String())
	}
}

// `agents list --json` must stay PARSEABLE for an agent routed at run time. Before
// the fix it bailed at the first tier-only agent, having already written that
// agent's opening brace — so the array was never closed and every consumer got a
// syntax error rather than a message. The parse below is the assertion; the field
// checks are what a consumer would then read.
func TestRunAgentsList_TierOnlyAgent_JSONStaysParseable(t *testing.T) {
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
`)
	var stdout, stderr bytes.Buffer
	if rc := RunAgents([]string{"list", "--json", "--config", path}, &stdout, &stderr); rc != 0 {
		t.Fatalf("rc = %d, want 0; stderr = %q", rc, stderr.String())
	}
	var got []struct {
		Name     string `json:"name"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Tier     string `json:"tier"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON (%v):\n%s", err, stdout.String())
	}
	if len(got) != 1 || got[0].Name != "extractor" {
		t.Fatalf("unexpected agents: %+v", got)
	}
	if got[0].Tier != "middle" {
		t.Errorf("tier = %q, want \"middle\"", got[0].Tier)
	}
	// The placeholder the human table prints must not leak into a machine field.
	if got[0].Provider != "" || got[0].Model != "" {
		t.Errorf("provider/model = %q/%q, want both empty for a tier-routed agent",
			got[0].Provider, got[0].Model)
	}
}
