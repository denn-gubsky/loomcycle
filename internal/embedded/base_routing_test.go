package embedded

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

type routingDoc struct {
	Models           map[string]map[string]any `yaml:"models"`
	ProviderPriority []string                  `yaml:"provider_priority"`
	Tiers            map[string][]yaml.Node    `yaml:"tiers"`
	UserTiers        map[string]struct {
		ProviderPriority []string `yaml:"provider_priority"`
	} `yaml:"user_tiers"`
}

func routingOf(t *testing.T, name string, data []byte) routingDoc {
	t.Helper()
	var d routingDoc
	if err := yaml.Unmarshal(data, &d); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return d
}

func unitRouting(t *testing.T, name string) routingDoc {
	t.Helper()
	data, err := Show(name)
	if err != nil {
		t.Fatal(err)
	}
	return routingOf(t, name, data)
}

// base wires every provider the built-in layer declares a key for, so that a
// deployment does not restate the matrix: each has an alias, is in
// provider_priority, and has a candidate in every tier it can serve. Gemini
// was missing from all three, and the cloud deployment carried sixty lines to
// make up for it.
func TestBasePreset_GeminiIsWiredLikeTheOtherProviders(t *testing.T) {
	base := unitRouting(t, "base")
	byProvider := map[string][]string{}
	for alias, m := range base.Models {
		p, _ := m["provider"].(string)
		byProvider[p] = append(byProvider[p], alias)
	}
	if len(byProvider["gemini"]) != 3 {
		t.Fatalf("base has gemini aliases %v, want three (lite, flash, pro)", byProvider["gemini"])
	}
	inPriority := func(list []string) bool {
		for _, p := range list {
			if p == "gemini" {
				return true
			}
		}
		return false
	}
	if !inPriority(base.ProviderPriority) {
		t.Errorf("provider_priority %v does not name gemini: its aliases would never be chosen", base.ProviderPriority)
	}
	if !inPriority(base.UserTiers["default"].ProviderPriority) {
		t.Errorf("user_tiers.default.provider_priority %v does not name gemini: the default user tier would never reach it", base.UserTiers["default"].ProviderPriority)
	}
	for _, tier := range []string{"low", "middle", "high"} {
		found := false
		for _, c := range base.Tiers[tier] {
			if p, _ := base.Models[c.Value]["provider"].(string); p == "gemini" {
				found = true
			}
		}
		if !found {
			t.Errorf("tiers.%s has no gemini candidate", tier)
		}
	}
}

// Every tier candidate base names is an alias base defines: a candidate that
// names nothing is skipped by the resolver without a word.
func TestBasePreset_EveryTierCandidateIsAnAliasItDefines(t *testing.T) {
	base := unitRouting(t, "base")
	n := 0
	for tier, cands := range base.Tiers {
		for _, c := range cands {
			n++
			if c.Kind != yaml.ScalarNode {
				continue // an inline {provider, model} pair names no alias
			}
			if _, ok := base.Models[c.Value]; !ok {
				t.Errorf("tiers.%s names %q, which base does not define", tier, c.Value)
			}
		}
	}
	if n < 9 {
		t.Fatalf("read %d tier candidates from base; the test is not reading the tiers", n)
	}
}

// An alias that both base and local define means the same model in both.
// local-coder named two different models, so selecting `local` silently
// changed what an agent pinned to that alias ran.
func TestPresets_AnAliasInBothBaseAndLocalIsTheSameModel(t *testing.T) {
	base, local := unitRouting(t, "base"), unitRouting(t, "local")
	shared := 0
	for alias, lm := range local.Models {
		bm, ok := base.Models[alias]
		if !ok {
			continue
		}
		shared++
		if !reflect.DeepEqual(bm, lm) {
			t.Errorf("alias %q: base has %v, local has %v", alias, bm, lm)
		}
	}
	if shared == 0 {
		t.Skip("base and local share no alias")
	}
}

// The cloud deployment's overlay restates nothing base supplies with the same
// value. A restated copy is how the two drifted: the copy was updated, base
// was not, and every other deployment kept the old ids.
func TestCloudOverlay_RestatesNothingBaseSuppliesIdentically(t *testing.T) {
	path := filepath.Join("..", "..", "cloud-deployment", "config", "loomcycle.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no cloud overlay in this tree: %v", err)
	}
	base, cloud := unitRouting(t, "base"), routingOf(t, path, data)
	if len(cloud.Models) == 0 {
		t.Fatal("read no models from the cloud overlay; the test is not reading it")
	}
	for alias, cm := range cloud.Models {
		if bm, ok := base.Models[alias]; ok && reflect.DeepEqual(bm, cm) {
			t.Errorf("models.%s is identical to base's; delete it from the overlay", alias)
		}
	}
	if len(cloud.ProviderPriority) > 0 && reflect.DeepEqual(cloud.ProviderPriority, base.ProviderPriority) {
		t.Error("provider_priority is identical to base's; delete it from the overlay")
	}
}
