package config

import (
	"strings"
	"testing"
)

// The layer assembly reads presets: from the primary file before the merge
// and marks that layer. The merge drops the key there: it selected layers, it
// is not a setting, and it must not show as an override or stop a strict load.
func TestLoadLayers_PresetsKeyIsDroppedFromThePrimaryLayer(t *testing.T) {
	t.Setenv("LOOMCYCLE_CONFIG_STRICT", "1")
	cfg, err := LoadLayers(
		Layer{Name: "base", Data: []byte(includeTestBase), Embedded: true},
		Layer{Name: "operator.yaml", Data: []byte("presets: [base]\nmodels:\n  n: {provider: mock, model: mock-2}\n"), Primary: true,
			Warnings: []string{"said by the assembler"}},
	)
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if cfg.Models["n"].Model != "mock-2" {
		t.Errorf("models = %+v, want the primary layer's alias", cfg.Models)
	}
	found := false
	for _, w := range cfg.Warnings {
		if w == "said by the assembler" {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings %v lack the one the assembler attached to the layer", cfg.Warnings)
	}
}

// Anywhere but the primary layer the key is refused: in an embedded unit, in
// another operator layer, and in a file loaded without the assembly (which
// would otherwise drop it and load no preset at all).
func TestLoadLayers_PresetsKeyOutsideThePrimaryLayerIsRefused(t *testing.T) {
	for name, layers := range map[string][]Layer{
		"an embedded unit":       {{Name: "unit", Data: []byte("presets: [base]\n" + includeTestBase), Embedded: true}},
		"another operator layer": {{Name: "base", Data: []byte(includeTestBase), Embedded: true}, {Name: "extra.yaml", Data: []byte("presets: [base]\n")}},
		"primary plus another": {{Name: "extra.yaml", Data: []byte("presets: [base]\n" + includeTestBase)},
			{Name: "operator.yaml", Data: []byte("presets: [base]\n"), Primary: true}},
	} {
		_, err := LoadLayers(layers...)
		if err == nil || !strings.Contains(err.Error(), "presets:") {
			t.Errorf("%s: err = %v, want the presets: key refused", name, err)
		}
	}
}
