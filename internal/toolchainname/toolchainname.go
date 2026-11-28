// Package toolchainname is the rule for a workspace toolchain's name (RFC
// HH): stdlib only, so the agent grant in config and the workspace worker,
// dispatcher and tool check names with one function.
package toolchainname

// Max bounds the toolchains a worker probes for and so reports
// (LOOMCYCLE_WORKSPACE_TOOLCHAINS, RFC HH §1), and the toolchains an open or
// an agent's grant requires. It sits inside the dispatcher's bound on a
// Hello's list, so a worker within it is never refused for its length.
const Max = 32

// MaxLen bounds a toolchain name, and a probe binary's name.
const MaxLen = 32

// Valid reports whether name may name a toolchain: 1 to MaxLen characters,
// a lowercase ASCII letter first, then lowercase letters, digits, '.', '+',
// '_' or '-' (^[a-z][a-z0-9.+_-]{0,31}$).
//
// The same rule checks a probe binary's name (RFC HH §1). It admits no '/',
// no whitespace and nothing a shell reads specially, which is what lets a
// worker write validated names into its probe script unquoted; and the
// dispatcher refuses a Hello naming a toolchain that fails it.
func Valid(name string) bool {
	if name == "" || len(name) > MaxLen || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '+', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}
