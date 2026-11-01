package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const statementMethodsCodeJSHead = `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  linker:
    provider: code-js
    code: "function run(){ return {final_text: 'ok'}; }"
    tools: [Document]
`

const statementMethodsModelHead = `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  writer:
    model: claude-sonnet-4-6
    tools: [Document]
`

// A code-js agent's statement_methods reach its definition as declared.
func TestLoad_StatementMethodsReachACodeJSAgent(t *testing.T) {
	for _, tc := range []struct{ name, field, want string }{
		{"absent", "", ""},
		{"empty", "    statement_methods: []\n", ""},
		{"rule", "    statement_methods: [rule]\n", "rule"},
		{"both", "    statement_methods: [rule, extractor]\n", "rule,extractor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadYAMLString(t, statementMethodsCodeJSHead+tc.field)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(cfg.Agents["linker"].StatementMethods, ","); got != tc.want {
				t.Errorf("StatementMethods = %q, want %q", got, tc.want)
			}
		})
	}
}

// A model-driven agent may not hold the grant: a rule: stamp claims code made
// the relation, and a model is not code. Refused at load, naming the agent.
func TestLoad_RefusesStatementMethodsOnAModelAgent(t *testing.T) {
	_, err := loadYAMLString(t, statementMethodsModelHead+"    statement_methods: [rule]\n")
	if err == nil {
		t.Fatal("a model-driven agent with statement_methods loaded")
	}
	for _, w := range []string{`"writer"`, "statement_methods", "code-js"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error does not mention %q: %v", w, err)
		}
	}
	// An empty list grants nothing, so it is no claim.
	if _, err := loadYAMLString(t, statementMethodsModelHead+"    statement_methods: []\n"); err != nil {
		t.Errorf("an empty statement_methods on a model agent was refused: %v", err)
	}
}

// An MD-declared code-js agent carries the grant too, and an MD model agent is
// held to the same rule as yaml.
func TestLoad_StatementMethodsFromAgentMarkdown(t *testing.T) {
	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, "agents")
	if err := os.Mkdir(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgentMD(t, agentsDir, "linker", "---\nname: linker\nprovider: code-js\ncode: \"function run(){ return {final_text: 'x'}; }\"\nstatement_methods: [rule]\n---\n")
	yamlPath := filepath.Join(tmp, "c.yaml")
	if err := os.WriteFile(yamlPath, []byte("defaults: { provider: anthropic, model: claude-sonnet-4-6 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOMCYCLE_AGENTS_ROOT", agentsDir)
	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Agents["linker"].StatementMethods, ","); got != "rule" {
		t.Errorf("MD StatementMethods = %q, want rule", got)
	}
	writeAgentMD(t, agentsDir, "writer", "---\nname: writer\nmodel: claude-sonnet-4-6\nstatement_methods: [rule]\n---\nbody\n")
	if _, err := Load(yamlPath); err == nil || !strings.Contains(err.Error(), "statement_methods") {
		t.Errorf("an MD model agent with statement_methods loaded: %v", err)
	}
}

func TestValidateStatementMethods_RefusesAnUnknownOrRepeatedMethod(t *testing.T) {
	for _, tc := range []struct {
		provider string
		in       []string
		ok       bool
	}{
		{"code-js", nil, true},
		{"code-js", []string{}, true},
		{"code-js", []string{"rule"}, true},
		{"code-js", []string{"extractor", "rule"}, true},
		{"code-js", []string{"judge"}, true},
		{"code-js", []string{"rule", "extractor", "judge"}, true},
		{"code-js", []string{"judge", "judge"}, false},
		{"anthropic", []string{"judge"}, false},
		{"code-js", []string{"manual"}, false},
		{"code-js", []string{"Rule"}, false},
		{"code-js", []string{"rule", "rule"}, false},
		{"anthropic", []string{"rule"}, false},
		{"", []string{"extractor"}, false},
		{"anthropic", nil, true},
	} {
		if err := ValidateStatementMethods(tc.provider, tc.in); (err == nil) != tc.ok {
			t.Errorf("ValidateStatementMethods(%q, %v) = %v, want ok=%v", tc.provider, tc.in, err, tc.ok)
		}
	}
}
