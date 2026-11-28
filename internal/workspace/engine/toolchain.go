package engine

import (
	"fmt"
	"slices"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/toolchainname"
)

// MaxToolchainBinaries bounds the alternative programs one toolchain is
// probed by, so an operator's setting cannot grow the probe script without
// limit.
const MaxToolchainBinaries = 8

// Toolchain is one toolchain a worker looks for (RFC HH §1): it is reported
// under Name when any of Binaries is found, the first one found counting.
type Toolchain struct {
	Name     string
	Binaries []string
}

// CheckToolchains reports the first entry of tcs that may not be probed: a
// name or a binary outside toolchainname.Valid, a toolchain with no
// binary or more than MaxToolchainBinaries, a name listed twice, or more than
// toolchainname.Max entries. Every name and binary that passes is safe
// to write into a shell script unquoted.
func CheckToolchains(tcs []Toolchain) error {
	if len(tcs) > toolchainname.Max {
		return fmt.Errorf("%d toolchains listed, at most %d", len(tcs), toolchainname.Max)
	}
	seen := map[string]bool{}
	for _, tc := range tcs {
		if !toolchainname.Valid(tc.Name) {
			return fmt.Errorf("toolchain name %q is not 1 to %d of a-z 0-9 . + _ - starting with a letter", tc.Name, toolchainname.MaxLen)
		}
		if seen[tc.Name] {
			return fmt.Errorf("toolchain %q is listed twice", tc.Name)
		}
		seen[tc.Name] = true
		if len(tc.Binaries) == 0 || len(tc.Binaries) > MaxToolchainBinaries {
			return fmt.Errorf("toolchain %q: 1 to %d probe binaries, got %d", tc.Name, MaxToolchainBinaries, len(tc.Binaries))
		}
		for _, b := range tc.Binaries {
			if !toolchainname.Valid(b) {
				return fmt.Errorf("toolchain %q: binary %q is not 1 to %d of a-z 0-9 . + _ - starting with a letter, with no path", tc.Name, b, toolchainname.MaxLen)
			}
		}
	}
	return nil
}

// ToolchainScript is the shell script that probes a session for tcs: one
// line per toolchain, printing its name when any of its binaries is on PATH.
// It is built only from entries CheckToolchains passes, so nothing the
// operator typed reaches the shell unchecked.
func ToolchainScript(tcs []Toolchain) (string, error) {
	if err := CheckToolchains(tcs); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, tc := range tcs {
		// `a || b && c` groups as `(a || b) && c` in sh: equal precedence,
		// left to right.
		for i, bin := range tc.Binaries {
			if i > 0 {
				b.WriteString(" || ")
			}
			fmt.Fprintf(&b, "command -v %s >/dev/null 2>&1", bin)
		}
		fmt.Fprintf(&b, " && echo %s\n", tc.Name)
	}
	// The script's status is the probe's, not the last toolchain's absence.
	b.WriteString("true\n")
	return b.String(), nil
}

// FoundToolchains takes from a probe's output only the names of tcs, in the
// order tcs lists them, each once: the image's own output (a login banner,
// an entrypoint's chatter) never becomes a toolchain.
func FoundToolchains(out []byte, tcs []Toolchain) []string {
	printed := strings.Fields(string(out))
	found := []string{}
	for _, tc := range tcs {
		if slices.Contains(printed, tc.Name) {
			found = append(found, tc.Name)
		}
	}
	return found
}
