package workspace

// MaxToolchains bounds the toolchains a worker probes for and so reports
// (LOOMCYCLE_WORKSPACE_TOOLCHAINS, RFC HH §1). It sits inside the
// dispatcher's bound on a Hello's list, so a worker within it is never
// refused for its length.
const MaxToolchains = 32

// MaxToolchainNameLen bounds a toolchain name, and a probe binary's name.
const MaxToolchainNameLen = 32

// ValidToolchainName reports whether name may name a toolchain: 1 to
// MaxToolchainNameLen characters, a lowercase ASCII letter first, then
// lowercase letters, digits, '.', '+', '_' or '-' (^[a-z][a-z0-9.+_-]{0,31}$).
//
// The same rule checks a probe binary's name (RFC HH §1). It admits no '/',
// no whitespace and nothing a shell reads specially, which is what lets a
// worker write validated names into its probe script unquoted; and the
// dispatcher refuses a Hello naming a toolchain that fails it.
func ValidToolchainName(name string) bool {
	if name == "" || len(name) > MaxToolchainNameLen || name[0] < 'a' || name[0] > 'z' {
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
