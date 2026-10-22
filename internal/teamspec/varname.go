package teamspec

import (
	"fmt"
	"regexp"
)

// varNameRe is the variable-name charset, the same [a-zA-Z0-9_-]{1,64} the
// credentials validator enforces and the expander matches — so a name that
// validates here is a name the expander can actually resolve.
var varNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

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
