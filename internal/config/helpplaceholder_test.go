package config

import (
	"strings"
	"testing"
)

// TestHelpPlaceholder_UnusableTopicIsBootError: the {{help:<topic>}} family
// (RFC GL P4) is a closed set — the topics this binary embeds that have call
// examples. Anything else fails config load with the placeholder named,
// rather than rendering nothing in a prompt that looks wired.
func TestHelpPlaceholder_UnusableTopicIsBootError(t *testing.T) {
	for _, tc := range []struct{ prompt, want string }{
		{`"{{help:Document/no_such_op}}"`, "{{help:Document/no_such_op}}"},
		{`"{{help:no-such-topic}}"`, "LOOMCYCLE_HELP_ROOT"},
		{`"{{help:loomcycle}}"`, "no call examples"},
		{`"{{help:Document/search/extra}}"`, "{{help:Document/search/extra}}"},
		{`"{{help:../loader}}"`, "not a help topic name"},
		{`"{{help:{{identity}}"`, "not a help topic name"},
		{`"{{help:}}"`, "not a help topic name"},
	} {
		_, err := Load(writeAgentPrompt(t, tc.prompt))
		if err == nil {
			t.Errorf("%s loaded cleanly; want a boot error", tc.prompt)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error should mention %q: %v", tc.prompt, tc.want, err)
		}
	}
}

func TestHelpPlaceholder_KnownTopicAndEscapedLoad(t *testing.T) {
	for _, prompt := range []string{
		`"{{help:Document/search}}"`,
		// What Context op=help resolves: another case, `.` or a space as the
		// separator, an alias.
		`"{{help:document.SEARCH}} {{help:Document search}}"`,
		`"{{help:Path/ls}} and {{help:scopes}}"`,
		`"docs say \\{{help:no-such-topic}} is a literal"`,
	} {
		if _, err := Load(writeAgentPrompt(t, prompt)); err != nil {
			t.Errorf("prompt %s should load: %v", prompt, err)
		}
	}
}

// TestHelpPlaceholder_OverBudgetIsBootError: a prompt whose examples would
// overflow the render budget is refused at load, so an accepted prompt always
// renders whole. Repeats count: each occurrence renders.
func TestHelpPlaceholder_OverBudgetIsBootError(t *testing.T) {
	// knowledge-base renders ~2.4 KiB; four of them pass 8 KiB.
	prompt := `"` + strings.Repeat("{{help:knowledge-base}} ", 4) + `"`
	_, err := Load(writeAgentPrompt(t, prompt))
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("err = %v, want an over-budget refusal", err)
	}
	if err := ValidateHelpPlaceholders("{{help:knowledge-base}}"); err != nil {
		t.Fatalf("one copy is within budget: %v", err)
	}
}
