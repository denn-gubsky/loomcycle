package teamgraph

import "fmt"

// ValidateVarName checks one variable name against the grammar a definition's
// own `vars`, `set` and `capture` keys are held to. Exported for a definition
// that names a team's variables without being one — a schedule or a webhook
// that starts a walk — so its keys are checked by the rule the team's are,
// rather than by a copy of it.
func ValidateVarName(name string) error {
	if !varNameRe.MatchString(name) {
		return fmt.Errorf("variable name %q must match [a-zA-Z0-9_-]{1,64}", name)
	}
	return nil
}
