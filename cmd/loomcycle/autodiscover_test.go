package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// layerNamesOf collects the Name of each layer in order.
func layerNamesOf(layers []config.Layer) []string {
	out := make([]string, len(layers))
	for i, l := range layers {
		out[i] = l.Name
	}
	return out
}

func layersContain(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestAssembleConfigLayers_ReGlobsSectionSiblings proves the RFC CK reload
// follow-up: assembleConfigLayers re-globs a base's loomcycle.*.yaml siblings on
// every call, so a section file added (or removed) after boot is picked up by a
// runtime reload with no restart. NO_DEFAULT_PROVIDERS=1 drops the constant
// providers.default layer so the layer count is exactly the disk files.
func TestAssembleConfigLayers_ReGlobsSectionSiblings(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "loomcycle.yaml")
	if err := os.WriteFile(base, []byte("provider_priority: [mock]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOMCYCLE_PRESETS", "")
	t.Setenv("LOOMCYCLE_CONFIG_DIR", "")
	t.Setenv("LOOMCYCLE_NO_DEFAULT_PROVIDERS", "1")

	sib := filepath.Join(dir, "loomcycle.memory.yaml")

	before, err := assembleConfigLayers(nil, []string{base}, false)
	if err != nil {
		t.Fatalf("assemble before: %v", err)
	}
	if names := layerNamesOf(before); layersContain(names, sib) {
		t.Fatalf("sibling %s present before it was created: %v", sib, names)
	}

	// Add a section sibling and RE-assemble: the re-glob sees it, no restart.
	if err := os.WriteFile(sib, []byte("provider_priority: [mock]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := assembleConfigLayers(nil, []string{base}, false)
	if err != nil {
		t.Fatalf("assemble after: %v", err)
	}
	if names := layerNamesOf(after); !layersContain(names, sib) {
		t.Fatalf("re-glob did not pick up added sibling %s: %v", sib, names)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("adding one sibling should add exactly one layer: before=%d after=%d", len(before), len(after))
	}

	// Remove the sibling and RE-assemble: it drops back out.
	if err := os.Remove(sib); err != nil {
		t.Fatal(err)
	}
	gone, err := assembleConfigLayers(nil, []string{base}, false)
	if err != nil {
		t.Fatalf("assemble gone: %v", err)
	}
	if names := layerNamesOf(gone); layersContain(names, sib) {
		t.Fatalf("removed sibling %s still present after re-glob: %v", sib, names)
	}
}

// TestAssembleConfigLayers_MissingExplicitFile: an explicit config file that does
// not exist yields an ERROR, not an os.Exit — the property the runtime reload
// relies on to reject a now-broken assembly and keep the running config.
func TestAssembleConfigLayers_MissingExplicitFile(t *testing.T) {
	t.Setenv("LOOMCYCLE_PRESETS", "")
	t.Setenv("LOOMCYCLE_CONFIG_DIR", "")
	if _, err := assembleConfigLayers(nil, []string{filepath.Join(t.TempDir(), "nope.yaml")}, false); err == nil {
		t.Fatal("expected an error for a missing explicit config file, got nil")
	}
}
