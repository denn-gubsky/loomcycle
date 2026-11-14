package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const includeTestBase = "defaults: {provider: mock}\nmodels:\n  m: {provider: mock, model: mock-1}\n"

// An embedded preset's include: was expanded by the layer assembly; the merge
// drops the key. It is not a setting, so it must not surface as an override
// warning or stop a strict load.
func TestLoadLayers_EmbeddedIncludeKeyIsDroppedFromTheMerge(t *testing.T) {
	t.Setenv("LOOMCYCLE_CONFIG_STRICT", "1")
	cfg, err := LoadLayers(
		Layer{Name: "base", Data: []byte(includeTestBase), Embedded: true},
		Layer{Name: "top", Data: []byte("include: [base]\nmodels:\n  n: {provider: mock, model: mock-2}\n"), Embedded: true},
		Layer{Name: "top2", Data: []byte("include: [top, base]\n"), Embedded: true},
	)
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if cfg.Models["m"].Model != "mock-1" || cfg.Models["n"].Model != "mock-2" {
		t.Errorf("models = %+v, want both layers' aliases", cfg.Models)
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "include") {
			t.Errorf("include leaked into a warning: %s", w)
		}
	}
}

// include: in an operator's own config is refused, on every path a file takes
// into the load: alone, layered, and as in-memory bytes. The schema drops an
// unknown top-level key silently, so without the refusal `include: [base]`
// would load nothing and say nothing.
func TestLoadLayers_IncludeInAnOperatorFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	withInclude := filepath.Join(dir, "loomcycle.yaml")
	if err := os.WriteFile(withInclude, []byte("include: [base]\n"+includeTestBase), 0o644); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "plain.yaml")
	if err := os.WriteFile(plain, []byte(includeTestBase), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, layers := range map[string][]Layer{
		"single disk file":         {{Name: withInclude}},
		"disk file over a preset":  {{Name: "base", Data: []byte(includeTestBase), Embedded: true}, {Name: withInclude}},
		"disk file under another":  {{Name: withInclude}, {Name: plain}},
		"in-memory operator layer": {{Name: "base", Data: []byte(includeTestBase), Embedded: true}, {Name: "operator", Data: []byte("include: [base]\n")}},
	} {
		_, err := LoadLayers(layers...)
		if err == nil {
			t.Errorf("%s: loaded; want the include: refused", name)
			continue
		}
		for _, want := range []string{"include:", "LOOMCYCLE_PRESETS"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: err = %v, want it to mention %q", name, err, want)
			}
		}
	}

	// The same files without the key load: the refusal is about the key.
	if _, err := LoadLayers(Layer{Name: plain}); err != nil {
		t.Errorf("plain file: %v", err)
	}
}
