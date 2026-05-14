package cases

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestLoadAll_AllBundledCasesParse verifies every shipped case file
// parses cleanly. Catches schema drift in the YAML on save.
func TestLoadAll_AllBundledCasesParse(t *testing.T) {
	root := repoRoot(t)
	cases, err := LoadAll(root, "")
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(cases) != 16 {
		t.Errorf("expected 16 cases (8 low + 8 middle), got %d", len(cases))
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.ID] {
			t.Errorf("duplicate case ID: %s", c.ID)
		}
		seen[c.ID] = true
		if c.Expected.Semantic.Threshold <= 0 || c.Expected.Semantic.Threshold > 100 {
			t.Errorf("%s: semantic threshold %d out of 1..100", c.ID, c.Expected.Semantic.Threshold)
		}
	}
}

// TestLoadAll_TierFilter verifies the per-tier filter loads only the
// matching directory.
func TestLoadAll_TierFilter(t *testing.T) {
	root := repoRoot(t)
	low, err := LoadAll(root, "low")
	if err != nil {
		t.Fatalf("LoadAll low: %v", err)
	}
	if len(low) != 8 {
		t.Errorf("expected 8 low-tier cases, got %d", len(low))
	}
	for _, c := range low {
		if c.Tier != "low" {
			t.Errorf("tier filter leaked: got %s case in low slice", c.Tier)
		}
	}
}

// repoRoot resolves the bench/ directory by walking up from this test
// file. Avoids hard-coding paths. The test file lives at
// bench/internal/cases/cases_test.go, so bench/ is two levels up.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// bench/internal/cases/cases_test.go -> bench/
	return filepath.Join(filepath.Dir(file), "..", "..")
}
