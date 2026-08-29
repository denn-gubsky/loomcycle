package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("provider_priority: [mock]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSectionSiblings covers the RFC CK section discovery: loomcycle.*.yaml /
// .yml beside the base, sorted, excluding the base, *.example.yaml, unrelated
// files, and non-regular matches (a directory).
func TestSectionSiblings(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"loomcycle.yaml",              // base — excluded
		"loomcycle.providers.yaml",    // section
		"loomcycle.memory.yaml",       // section
		"loomcycle.agents.yml",        // section (.yml)
		"loomcycle.example.yaml",      // shipped template — MUST be excluded (footgun)
		"loomcycle.local.example.yml", // *.example.yml — excluded
		"notloomcycle.yaml",           // unrelated
		"loomcycle.yml",               // no middle segment → not a section
	} {
		writeFile(t, filepath.Join(dir, n))
	}
	// A directory that matches the glob must be skipped, not sent to LoadLayers.
	if err := os.Mkdir(filepath.Join(dir, "loomcycle.d.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := SectionSiblings(filepath.Join(dir, "loomcycle.yaml"))
	want := []string{
		filepath.Join(dir, "loomcycle.agents.yml"),
		filepath.Join(dir, "loomcycle.memory.yaml"),
		filepath.Join(dir, "loomcycle.providers.yaml"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SectionSiblings = %v, want %v", got, want)
	}
}

// TestSectionSiblings_SkipsBrokenSymlink: a dangling symlink matching the glob is
// skipped (os.Stat errors), not passed to the loader.
func TestSectionSiblings_SkipsBrokenSymlink(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "loomcycle.yaml"))
	writeFile(t, filepath.Join(dir, "loomcycle.real.yaml"))
	if err := os.Symlink(filepath.Join(dir, "gone.yaml"), filepath.Join(dir, "loomcycle.dead.yaml")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	got := SectionSiblings(filepath.Join(dir, "loomcycle.yaml"))
	want := []string{filepath.Join(dir, "loomcycle.real.yaml")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SectionSiblings = %v, want %v (dangling symlink skipped)", got, want)
	}
}

// TestWithSectionSiblings: each loomcycle.yaml is followed by its siblings;
// non-base files pass through; the result is deduped (a base listed twice, or two
// bases sharing a dir, does not double-layer a sibling) with order preserved.
func TestWithSectionSiblings(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "loomcycle.yaml")
	sib := filepath.Join(dir, "loomcycle.providers.yaml")
	other := filepath.Join(dir, "override.yaml")
	for _, f := range []string{base, sib, other} {
		writeFile(t, f)
	}

	// base brings its sibling, then the unrelated override passes through.
	got := WithSectionSiblings([]string{base, other})
	if want := []string{base, sib, other}; !reflect.DeepEqual(got, want) {
		t.Errorf("WithSectionSiblings = %v, want %v", got, want)
	}

	// base listed twice → sibling added once (dedup).
	got = WithSectionSiblings([]string{base, base})
	if want := []string{base, sib}; !reflect.DeepEqual(got, want) {
		t.Errorf("dedup: WithSectionSiblings = %v, want %v", got, want)
	}

	// a non-base file alone → passthrough, no discovery.
	got = WithSectionSiblings([]string{other})
	if want := []string{other}; !reflect.DeepEqual(got, want) {
		t.Errorf("passthrough: WithSectionSiblings = %v, want %v", got, want)
	}
}
