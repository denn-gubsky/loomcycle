package cli

import (
	"bytes"
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
    allowed_tools: []
  classifier:
    model: cheap
    system_prompt: "Classify each input."
    allowed_tools: []
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
