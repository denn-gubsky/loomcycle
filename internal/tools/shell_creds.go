package tools

import "github.com/denn-gubsky/loomcycle/internal/config"

// ShellCredsPolicyValue is a run's narrowing of the credentials the Bash tool
// and the Bashbox host-command fallback inject into a command's environment
// (RFC EW). The operator's LOOMCYCLE_BASH_ALLOWED_CREDS and
// LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS say which credentials a shell may
// receive at all; this keeps only some of them for one agent. It never adds a
// name the operator did not list.
//
// The zero value narrows nothing: every credential the operator lists, as
// before the field existed. Set with no Names means none.
type ShellCredsPolicyValue struct {
	// Set is whether the agent narrowed its shell's credentials. False:
	// every credential the operator lists.
	Set bool
	// Names are the credential names the shell may receive, when Set.
	Names []string
}

// ShellCredsFor is the narrowing an agent definition's shell_credentials
// declares: nil (absent) narrows nothing, a list (empty included) keeps only
// its names.
func ShellCredsFor(declared *config.CredentialNames) ShellCredsPolicyValue {
	if declared == nil {
		return ShellCredsPolicyValue{}
	}
	return ShellCredsPolicyValue{Set: true, Names: append([]string{}, (*declared)...)}
}

// Allows reports whether the shell may receive credential name.
func (v ShellCredsPolicyValue) Allows(name string) bool {
	if !v.Set {
		return true
	}
	for _, n := range v.Names {
		if n == name {
			return true
		}
	}
	return false
}

// Filter keeps the names in operator (the operator's list for one tool) that
// the narrowing allows, in the operator's order.
func (v ShellCredsPolicyValue) Filter(operator []string) []string {
	if !v.Set {
		return operator
	}
	var out []string
	for _, n := range operator {
		if v.Allows(n) {
			out = append(out, n)
		}
	}
	return out
}

// NarrowShellCreds is a spawned child's narrowing: the names both its parent
// run and its own definition allow. Either side unset leaves the other's.
func NarrowShellCreds(parent, own ShellCredsPolicyValue) ShellCredsPolicyValue {
	switch {
	case !parent.Set:
		return own
	case !own.Set:
		return parent
	}
	out := ShellCredsPolicyValue{Set: true, Names: []string{}}
	for _, n := range own.Names {
		if parent.Allows(n) {
			out.Names = append(out.Names, n)
		}
	}
	return out
}
