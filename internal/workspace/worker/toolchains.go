package worker

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
)

// toolchainCatalog is the probe binaries of the toolchains an operator may
// name bare in LOOMCYCLE_WORKSPACE_TOOLCHAINS (RFC HH §1). Of several, the
// first one found counts.
var toolchainCatalog = map[string][]string{
	"python": {"python3", "python"},
	"node":   {"node"},
	"go":     {"go"},
	"c":      {"cc", "gcc", "clang"},
	"cpp":    {"c++", "g++", "clang++"},
	"cmake":  {"cmake"},
	"make":   {"make"},
	"rust":   {"cargo"},
	"java":   {"javac"},
	"dotnet": {"dotnet"},
	"ruby":   {"ruby"},
	"php":    {"php"},
}

// DefaultToolchains are what a worker probes for when the operator names
// none: python and node, what every worker reported before RFC HH.
func DefaultToolchains() []engine.Toolchain {
	return []engine.Toolchain{
		{Name: "python", Binaries: toolchainCatalog["python"]},
		{Name: "node", Binaries: toolchainCatalog["node"]},
	}
}

// ParseToolchains reads LOOMCYCLE_WORKSPACE_TOOLCHAINS: a comma-separated
// list of entries, each `name` (its binaries from the built-in catalog) or
// `name=binary[|binary…]`. Empty is nil: the worker then probes for
// DefaultToolchains. A malformed entry is an error naming it, so the worker
// stops at its start rather than probing for something the operator did not
// mean (engine.CheckToolchains holds the rules).
func ParseToolchains(v string) ([]engine.Toolchain, error) {
	var out []engine.Toolchain
	for _, entry := range strings.Split(v, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, bins, named := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		tc := engine.Toolchain{Name: name}
		if named {
			for _, b := range strings.Split(bins, "|") {
				tc.Binaries = append(tc.Binaries, strings.TrimSpace(b))
			}
		} else if tc.Binaries = toolchainCatalog[name]; tc.Binaries == nil {
			return nil, fmt.Errorf("LOOMCYCLE_WORKSPACE_TOOLCHAINS: %q is not a toolchain the catalog knows; name its binaries (%s=binary)", entry, name)
		}
		// Checked one entry at a time, so the error names the entry.
		if err := engine.CheckToolchains([]engine.Toolchain{tc}); err != nil {
			return nil, fmt.Errorf("LOOMCYCLE_WORKSPACE_TOOLCHAINS: %q: %w", entry, err)
		}
		out = append(out, tc)
	}
	// The whole list: the cap and a name listed twice.
	if err := engine.CheckToolchains(out); err != nil {
		return nil, fmt.Errorf("LOOMCYCLE_WORKSPACE_TOOLCHAINS: %w", err)
	}
	return out, nil
}

// DetectToolchains reports which of tcs this machine has on PATH: the host
// engine's probe.
func DetectToolchains(tcs []engine.Toolchain) []string {
	found := []string{}
	for _, tc := range tcs {
		for _, b := range tc.Binaries {
			if _, err := exec.LookPath(b); err == nil {
				found = append(found, tc.Name)
				break
			}
		}
	}
	return found
}

// warnMissing logs each toolchain of tcs the probe did not find (RFC HH D2):
// it is left out, never claimed, and the operator who declared it learns
// why the worker does not report it.
func warnMissing(log *slog.Logger, tcs []engine.Toolchain, found []string) {
	have := map[string]bool{}
	for _, f := range found {
		have[f] = true
	}
	for _, tc := range tcs {
		if !have[tc.Name] {
			log.Warn("a declared toolchain was not found; this worker does not report it", "toolchain", tc.Name, "binaries", tc.Binaries)
		}
	}
}
