package teamspec

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
	if ReadsSecretNamespace(value) {
		return fmt.Errorf("the value names the credentials namespace — variables are non-secret by construction, " +
			"and a variable is never expanded, so it would not read the credential either")
	}
	return nil
}

// secretNamespaces are the ${…} namespaces a `vars` state may never bind from.
//
// ${run.credentials.*} and ${run.user_bearer} are FAIL-CLOSED by design:
// unresolved, they drop the whole header rather than emit a placeholder. A vars
// state that could copy one into ${var.x} would convert a fail-closed secret
// into a fail-open plaintext string — and that string is then a legitimate
// Memory key, a prompt fragment, and a value in every transcript, snapshot and
// prompt-cache entry downstream.
//
// Secrets keep their own namespace and their own posture. Variables are
// non-secret BY CONSTRUCTION, which is what lets the expander substitute an
// unresolved one to empty instead of dropping its container.
var secretNamespaces = []string{"${run.credentials.", "${run.user_bearer"}

// ReadsSecretNamespace reports whether a variable's value names a secret
// namespace. One reader for every place a variable is given a value by the
// author or the caller — a vars state's `set`, a declared default, a value
// supplied at start — so the three cannot disagree about what a secret is.
func ReadsSecretNamespace(value string) bool {
	for _, ns := range secretNamespaces {
		if strings.Contains(value, ns) {
			return true
		}
	}
	return false
}
