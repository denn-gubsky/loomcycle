package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAgentPrompt(t *testing.T, prompt string) string {
	t.Helper()
	yamlPath := filepath.Join(t.TempDir(), "c.yaml")
	body := `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  a:
    model: claude-sonnet-4-6
    system_prompt: ` + prompt + `
`
	if err := os.WriteFile(yamlPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return yamlPath
}

// TestToolPlaceholder_DisallowedToolIsBootError is the load-bearing half of the
// allowlist. A {{tool:Bash}} that merely rendered to nothing would leave the
// operator debugging a prompt that silently dropped a line; more importantly, the
// allowlist can only be stated as a BOUND if naming a tool outside it is refused
// rather than ignored.
func TestToolPlaceholder_DisallowedToolIsBootError(t *testing.T) {
	for _, ref := range []string{"Bash.run", "Write.file", "Agent.spawn", "Memory.add"} {
		_, err := Load(writeAgentPrompt(t, `"do it {{tool:`+ref+`}}"`))
		if err == nil {
			t.Errorf("{{tool:%s}} loaded cleanly; a non-allowlisted tool call must fail boot", ref)
			continue
		}
		if !strings.Contains(err.Error(), ref) {
			t.Errorf("error should name the offending ref %q: %v", ref, err)
		}
		if !strings.Contains(err.Error(), "Context.tools") {
			t.Errorf("error should list what IS allowed so the operator can fix it: %v", err)
		}
	}
}

// TestToolPlaceholder_TypoIsBootError: an allowlisted tool with a misspelled op is
// the likeliest real mistake, and it must not degrade to a silently empty section.
func TestToolPlaceholder_TypoIsBootError(t *testing.T) {
	_, err := Load(writeAgentPrompt(t, `"{{tool:Context.tols}}"`))
	if err == nil {
		t.Fatal("expected a boot error for a misspelled op")
	}
	if !strings.Contains(err.Error(), "Context.tols") {
		t.Errorf("error should name the offending ref: %v", err)
	}
}

// TestToolPlaceholder_BareToolIsBootError: every allowlisted tool is
// op-discriminated, so there is no defensible default op to assume.
func TestToolPlaceholder_BareToolIsBootError(t *testing.T) {
	if _, err := Load(writeAgentPrompt(t, `"{{tool:Context}}"`)); err == nil {
		t.Fatal("expected a boot error for a bare {{tool:Context}} with no op")
	}
}

// TestToolPlaceholder_AllowlistedAndEscapedLoad confirms the valid cases load: an
// allowlisted ref, a case variant of it, and an escaped placeholder (a literal,
// which must not be validated as a call).
func TestToolPlaceholder_AllowlistedAndEscapedLoad(t *testing.T) {
	for _, prompt := range []string{
		`"{{tool:Context.tools}}"`,
		`"{{tool:context.tools}}"`,
		`"docs say \\{{tool:Bash.run}} is not allowed"`,
	} {
		if _, err := Load(writeAgentPrompt(t, prompt)); err != nil {
			t.Errorf("prompt %s should load: %v", prompt, err)
		}
	}
}
