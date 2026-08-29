package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestConfigDirLayers: LOOMCYCLE_CONFIG_DIR enumerates *.yaml/*.yml directly
// under the dir, sorted lexically; ignores non-YAML files and subdirectories.
func TestConfigDirLayers(t *testing.T) {
	dir := t.TempDir()
	// Create out of order to prove the lexical sort.
	for _, n := range []string{"30-z.yaml", "10-a.yaml", "20-b.yml"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("provider_priority: [mock]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Noise that must be ignored: a non-YAML file and a subdirectory.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "ignored.yaml"), []byte("x: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := configDirLayers(dir)
	if err != nil {
		t.Fatalf("configDirLayers: %v", err)
	}
	want := []string{
		filepath.Join(dir, "10-a.yaml"),
		filepath.Join(dir, "20-b.yml"),
		filepath.Join(dir, "30-z.yaml"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d files %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %s, want %s (lexical order)", i, got[i], want[i])
		}
	}
}

// TestConfigDirLayers_EmptyAndMissing: an empty dir → nil (no error); a missing
// dir → error (the operator's typo is surfaced, not silently skipped).
func TestConfigDirLayers_EmptyAndMissing(t *testing.T) {
	empty := t.TempDir()
	got, err := configDirLayers(empty)
	if err != nil {
		t.Fatalf("empty dir should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty dir should yield no files, got %v", got)
	}

	if _, err := configDirLayers(filepath.Join(empty, "does-not-exist")); err == nil {
		t.Errorf("a missing LOOMCYCLE_CONFIG_DIR should error (caller exits)")
	}
}
