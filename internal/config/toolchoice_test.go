package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadYAMLString(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

// A tool_choice that could not mean what it says is refused; the one refusal
// that matters most is a forced choice on every call, which leaves a run no
// way to finish short of its iteration cap.
func TestToolChoiceValidate(t *testing.T) {
	for _, tc := range []struct {
		tc      ToolChoice
		wantErr string
	}{
		{ToolChoice{Mode: "tool", Name: "search"}, ""},
		{ToolChoice{Mode: "required", Until: "until_called"}, ""},
		{ToolChoice{Mode: "none", Until: "always"}, ""},
		{ToolChoice{Mode: "auto"}, ""},
		{ToolChoice{Mode: "required", Until: "always"}, "never give a final answer"},
		{ToolChoice{Mode: "tool", Name: "x", Until: "always"}, "never give a final answer"},
		{ToolChoice{Mode: "tool"}, "needs a tool name"},
		{ToolChoice{Mode: "required", Name: "x"}, "only meaningful with mode"},
		{ToolChoice{Mode: "sometimes"}, "is not one of"},
		{ToolChoice{}, "mode is required"},
		{ToolChoice{Mode: "tool", Name: "x", Until: "later"}, "until"},
		{ToolChoice{Mode: "none", Until: "until_called"}, "no call to wait for"},
	} {
		err := tc.tc.Validate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%+v: unexpected error %v", tc.tc, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%+v: error %v, want one containing %q", tc.tc, err, tc.wantErr)
		}
	}
	if (&ToolChoice{Mode: "tool", Name: "x"}).EffectiveUntil() != ToolChoiceUntilFirstCall {
		t.Error("an unset until must default to first_call")
	}
}

// Per-run REPLACES the agent's choice whole: a per-field merge could pair the
// agent's mode "tool" with nothing, or a run's mode with the agent's tool name.
func TestMergeToolChoice_ReplacesWhole(t *testing.T) {
	agent := &ToolChoice{Mode: "tool", Name: "search", Until: "until_called"}
	run := &ToolChoice{Mode: "required"}
	got := MergeToolChoice(agent, run)
	if got.Mode != "required" || got.Name != "" || got.Until != "" {
		t.Errorf("merge = %+v, want the run's choice exactly", got)
	}
	if MergeToolChoice(agent, nil).Name != "search" {
		t.Error("no per-run choice must keep the agent's")
	}
	got.Mode = "none"
	if run.Mode != "required" {
		t.Error("the merge aliased its input")
	}
}

// yaml carries it and config load validates it.
func TestLoad_ValidatesAgentToolChoice(t *testing.T) {
	cfg, err := loadYAMLString(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  researcher:
    model: claude-sonnet-4-6
    tool_choice: { mode: tool, name: WebSearch, until: until_called }
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if tc := cfg.Agents["researcher"].ToolChoice; tc == nil || tc.Name != "WebSearch" || tc.Until != "until_called" {
		t.Errorf("tool_choice = %+v", tc)
	}
	if _, err := loadYAMLString(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  looper:
    model: claude-sonnet-4-6
    tool_choice: { mode: required, until: always }
`); err == nil || !strings.Contains(err.Error(), "looper") {
		t.Errorf("a never-finishing tool_choice loaded: %v", err)
	}
}
