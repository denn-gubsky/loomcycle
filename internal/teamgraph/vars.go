package teamgraph

import (
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/teamspec"
)

// validateVars checks a definition's `vars` block: the declared names and
// their defaults.
func validateVars(out *issues, vars map[string]string) {
	if len(vars) > teamspec.MaxVars {
		out.top("vars", "team definition: vars declares %d variables, more than the maximum %d", len(vars), teamspec.MaxVars)
	}
	for _, name := range sortedKeys(vars) {
		path := PathKey("vars", name)
		if teamspec.ValidateVarName(name) != nil {
			out.top(path, "team definition: vars key %q must match [a-zA-Z0-9_-]{1,64}", name)
		}
		if err := teamspec.CheckVarValue(vars[name]); err != nil {
			out.top(path, "team definition: vars %q: %v", name, err)
		}
	}
}

// CheckStartVars checks the values a caller supplies when it starts a walk
// against what the team declares. Only a declared name is accepted: a name the
// team does not declare is one no state was written to read, so carrying it
// would be a typo that changes nothing and says nothing.
func CheckStartVars(d Definition, supplied map[string]string) error {
	for _, name := range sortedKeys(supplied) {
		if _, declared := d.Vars[name]; !declared {
			if len(d.Vars) == 0 {
				return fmt.Errorf("vars: %q is not a variable of this team — it declares none (a team lists the "+
					"variables a run may set in its definition's `vars`)", name)
			}
			return fmt.Errorf("vars: %q is not a variable of this team (declared: %s)", name, strings.Join(sortedKeys(d.Vars), ", "))
		}
		if err := teamspec.CheckVarValue(supplied[name]); err != nil {
			return fmt.Errorf("vars %q: %w", name, err)
		}
	}
	return nil
}
