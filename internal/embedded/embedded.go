// Package embedded ships v0.11.1 first-run assets bundled into the
// binary so `loomcycle init` can write them to the operator's config
// directory without reading from a source checkout that may not be
// present (Homebrew installs, go install from the tagged tarball,
// container images).
//
// Lives under internal/, not under cmd/loomcycle, because internal/cli and
// internal/api/http import it: a surface package importing from the entry
// layer is an upward edge (RFC EX), and internal packages can't import main
// packages anyway.
package embedded

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

// loomcycle.example.yaml — the canonical heavily-commented config.
// A symlink at the repo root keeps existing references working
// (config tests, docs that point at the GitHub raw URL).
//
//go:embed loomcycle.example.yaml
var exampleYAML []byte

// README.md — per-machine quickstart written next to the yaml so the
// operator has one local landing pad after init. Covers file layout,
// env vars, troubleshooting. Renamed from an earlier CONFIGURATION.md
// draft so it doesn't collide with the repo's existing
// docs/CONFIGURATION.md (the provider-routing deep-dive); operators
// instinctively look for a README in their config directory.
//
//go:embed README.md
var readmeDoc []byte

// ExampleYAML returns the bundled loomcycle.example.yaml bytes.
// Used by `loomcycle init` to write the user's starter config.
func ExampleYAML() []byte { return exampleYAML }

// LocalReadme returns the bundled per-machine README.md bytes.
// Used by `loomcycle init` to write the per-machine quickstart.
func LocalReadme() []byte { return readmeDoc }

// profiles/<name>.yaml — the short config `loomcycle init` writes for a
// profile: what an operator of that profile supplies over the preset of the
// same name. Each is a byte copy of examples/profiles/<name>/loomcycle.yaml,
// the copy an operator reads in the repository; go:embed cannot reach outside
// this directory, and a symlink there would break for anyone who copies the
// example directory. TestProfileYAML_MatchesTheExampleAnOperatorCopies holds
// the two together.
//
//go:embed profiles/*.yaml
var profileFS embed.FS

// DefaultProfile is the profile `loomcycle init` writes when none is named.
const DefaultProfile = "personal"

// Profiles returns the profile names, sorted.
func Profiles() []string {
	entries, err := profileFS.ReadDir("profiles")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".yaml"); ok && !e.IsDir() {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ProfileYAML returns the config `loomcycle init` writes for a profile. The
// profile's preset has the same name: the file is used with
// LOOMCYCLE_PRESETS=<name>.
func ProfileYAML(name string) ([]byte, error) {
	data, err := profileFS.ReadFile("profiles/" + name + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("unknown profile %q (want one of: %s)", name, strings.Join(Profiles(), ", "))
	}
	return data, nil
}
