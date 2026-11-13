package config

import (
	"strings"
	"testing"
)

// Each malformed tool_modes is refused at load, with a message naming the agent
// and what is wrong. Ignored instead, a mode on the wrong tool (or a misspelt
// one) would boot an agent the operator believes is read-only and is not.
func TestLoad_RefusesAMalformedToolMode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools string
		modes string
		want  []string
	}{
		{"a tool that takes no mode", "[Document, Memory]", "{Memory: read}", []string{`"Memory"`, "cannot be given a mode", "Document, Path"}},
		{"an unknown mode", "[Document]", "{Document: readonly}", []string{"tool_modes.Document", `"readonly"`, `"read"`}},
		{"a write mode", "[Path]", "{Path: write}", []string{"tool_modes.Path", `"write"`}},
		{"a tool not in tools", "[Path]", "{Document: read}", []string{"tool_modes.Document", "not in tools"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAMLString(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  reader:
    model: claude-sonnet-4-6
    tools: `+tc.tools+`
    tool_modes: `+tc.modes+`
`)
			if err == nil {
				t.Fatal("expected the load to be refused")
			}
			for _, want := range append([]string{`"reader"`}, tc.want...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q: %v", want, err)
				}
			}
		})
	}
}

func TestLoad_ToolModesReachTheAgent(t *testing.T) {
	cfg, err := loadYAMLString(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  reader:
    model: claude-sonnet-4-6
    tools: [Document, Path]
    tool_modes: {Document: read, Path: read}
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := cfg.Agents["reader"].ToolModes
	if got["Document"] != ToolModeRead || got["Path"] != ToolModeRead || len(got) != 2 {
		t.Fatalf("tool_modes = %v, want Document and Path read", got)
	}
}

func TestValidateToolModes_GrantFollowsTheToolsGlob(t *testing.T) {
	if err := ValidateToolModes(map[string]string{"Document": "read"}, []string{"Doc*"}); err != nil {
		t.Fatalf("a glob granting Document should accept its mode: %v", err)
	}
	if err := ValidateToolModes(nil, nil); err != nil {
		t.Fatalf("absent tool_modes refused: %v", err)
	}
}
