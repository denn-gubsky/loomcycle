package builtin

import (
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// teamIssueHelpPlaceholder refuses a node prompt whose {{help:<topic>}}
// placeholder cannot render (RFC GL P4).
const teamIssueHelpPlaceholder = "help_placeholder"

// helpPlaceholderIssues holds every prompt text a team's nodes carry — the
// node's system_prompt and input_template, and a starter's prompt.system and
// prompt.input — to the rule an agent's system prompt is held to at
// authoring: each {{help:<topic>}} names a built-in topic with call examples,
// within the budget. Each text is judged alone because each is expanded in
// its own pass with its own budget.
//
// Authoring only (create/fork, through authoringIssues): a stored team is
// never refused over a topic a later binary dropped; the placeholder renders
// nothing there.
func helpPlaceholderIssues(def teamgraph.Definition) []teamIssue {
	var issues []teamIssue
	for i, st := range def.States {
		h := st.Handler
		fields := []struct{ name, text string }{{"system_prompt", h.SystemPrompt}, {"input_template", h.InputTemplate}}
		if h.Prompt != nil {
			fields = append(fields, struct{ name, text string }{"prompt.system", h.Prompt.System},
				struct{ name, text string }{"prompt.input", h.Prompt.Input})
		}
		for _, f := range fields {
			if err := config.ValidateHelpPlaceholders(f.text); err != nil {
				issues = append(issues, teamIssue{
					Kind: teamIssueHelpPlaceholder, Severity: severityRefused,
					Path: fmt.Sprintf("states[%d].handler.%s", i, f.name), State: st.ID, Field: f.name,
					Detail: fmt.Sprintf("team definition: state %q `%s`: %v", st.ID, f.name, err),
				})
			}
		}
	}
	return issues
}
