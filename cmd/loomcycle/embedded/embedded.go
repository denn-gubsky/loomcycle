// Package embedded ships v0.11.1 first-run assets bundled into the
// binary so `loomcycle init` can write them to the operator's config
// directory without reading from a source checkout that may not be
// present (Homebrew installs, go install from the tagged tarball,
// container images).
//
// Lives in its own package (not in main) so internal/cli can import
// it — internal packages can't import main packages.
package embedded

import _ "embed"

// loomcycle.example.yaml — the canonical heavily-commented config.
// A symlink at the repo root keeps existing references working
// (config tests, docs that point at the GitHub raw URL).
//
//go:embed loomcycle.example.yaml
var exampleYAML []byte

// CONFIGURATION.md — operator reference covering file layout, env
// vars, yaml structure, troubleshooting. Written alongside the yaml
// so the operator has one local landing pad after init.
//
//go:embed CONFIGURATION.md
var configurationDoc []byte

// ExampleYAML returns the bundled loomcycle.example.yaml bytes.
// Used by `loomcycle init` to write the user's starter config.
func ExampleYAML() []byte { return exampleYAML }

// ConfigurationDoc returns the bundled CONFIGURATION.md bytes.
// Used by `loomcycle init` to write the operator reference.
func ConfigurationDoc() []byte { return configurationDoc }
