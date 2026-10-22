package tools

import (
	"slices"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

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

// WithheldHostNames are the host variables a shell must not receive under
// this narrowing (RFC EW D6): the names in hostAllowed (the operator's
// host-variable allowlist for one tool) that are also among credNames (the
// operator's shell credentials for that tool, whatever command a "cmd:NAME"
// binding ties them to) and that the agent did not keep. Otherwise the
// operator's host value would stand in for a credential the agent gave up,
// the same reason a lapsed credential's host twin is removed. A host variable
// that is not a shell credential name is never withheld, and an unnarrowed
// agent withholds nothing.
func (v ShellCredsPolicyValue) WithheldHostNames(hostAllowed, credNames []string) []string {
	if !v.Set {
		return nil
	}
	var out []string
	for _, n := range hostAllowed {
		if slices.Contains(credNames, n) && !v.Allows(n) && !slices.Contains(out, n) {
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
