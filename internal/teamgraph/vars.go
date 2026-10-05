package teamgraph

import (
	"fmt"
	"strings"
)

// The bounds on a team's declared variables. Nothing bounded a variable's
// value before this (a `capture` binds whatever an agent answered), so these
// are new numbers rather than borrowed ones: enough for a list of settings
// and a paragraph of text each, small enough that a start request cannot use
// the block to carry a document into every prompt of a walk.
const (
	MaxVars          = 64
	MaxVarValueBytes = 4096
)

// validateVars checks a definition's `vars` block: the declared names and
// their defaults.
func validateVars(vars map[string]string) error {
	if len(vars) > MaxVars {
		return fmt.Errorf("team definition: vars declares %d variables, more than the maximum %d", len(vars), MaxVars)
	}
	for _, name := range sortedKeys(vars) {
		if !varNameRe.MatchString(name) {
			return fmt.Errorf("team definition: vars key %q must match [a-zA-Z0-9_-]{1,64}", name)
		}
		if err := CheckVarValue(vars[name]); err != nil {
			return fmt.Errorf("team definition: vars %q: %w", name, err)
		}
	}
	return nil
}

// CheckVarValue is what a variable's value must satisfy when it is WRITTEN
// DOWN — as a declared default, or by a caller starting a walk. Both are held
// to one rule so a start cannot supply what a definition could not declare.
//
// The value is a LITERAL. Nothing expands it: prompt assembly substitutes a
// variable in a single pass and never reads what it wrote, so a ${var.other}
// or a ${now.date} inside a value reaches the prompt as those characters. (The
// one exception is a `vars` state that copies it: `set: {b: "${var.a}"}` runs
// the built-in ${now.*} / ${team.*} tokens over its result, so one of those
// inside a is resolved in b. They are a fixed, non-secret set.)
//
// {{ and }} are refused here although prompt assembly drops such a value
// anyway: there it is dropped silently, mid-walk, and the prompt goes out
// with a blank where the value was. A value somebody typed is refused while
// they can still fix it.
func CheckVarValue(value string) error {
	if len(value) > MaxVarValueBytes {
		return fmt.Errorf("the value is %d bytes, more than the maximum %d", len(value), MaxVarValueBytes)
	}
	if strings.Contains(value, "{{") || strings.Contains(value, "}}") {
		return fmt.Errorf("the value contains {{ or }} — a variable may not carry a prompt placeholder")
	}
	if readsSecretNamespace(value) {
		return fmt.Errorf("the value names the credentials namespace — variables are non-secret by construction, " +
			"and a variable is never expanded, so it would not read the credential either")
	}
	return nil
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
		if err := CheckVarValue(supplied[name]); err != nil {
			return fmt.Errorf("vars %q: %w", name, err)
		}
	}
	return nil
}
