package embedded

import (
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
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
	if len(byProvider["gemini"]) != 4 {
		t.Fatalf("base has gemini aliases %v, want four (lite, flash, pro, pro-preview)", byProvider["gemini"])
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

// A Gemini alias pattern matches chat models only. The matcher is a plain
// glob with no exclusion, and the provider lists image, speech, audio and
// embedding models under the same family names; `gemini-*-pro*` matched
// gemini-3-pro-image-preview, and the newest match wins, so a tier candidate
// could resolve to a model that cannot hold a conversation.
func TestBasePreset_GeminiPatternsMatchChatModelsOnly(t *testing.T) {
	notChat := []string{
		"gemini-3-pro-image-preview", "gemini-2.5-flash-image", "gemini-2.5-flash-image-preview",
		"gemini-2.5-pro-preview-tts", "gemini-2.5-flash-preview-tts",
		"gemini-2.5-flash-native-audio-preview-09-2025", "gemini-2.5-flash-preview-native-audio-dialog",
		"gemini-live-2.5-flash-preview", "gemini-embedding-001", "gemini-embedding-exp-03-07",
		"gemini-2.5-pro-audio", "gemini-4-pro-image",
	}
	chat := map[string]string{ // model -> the alias that must match it
		"gemini-2.5-pro":         "gemini-pro",
		"gemini-3.1-pro-preview": "gemini-pro-preview",
		"gemini-2.5-flash":       "gemini-flash",
		"gemini-2.5-flash-lite":  "gemini-lite",
	}
	for name, data := range map[string][]byte{"base": mustShow(t, "base"), "loomcycle.example.yaml": ExampleYAML()} {
		doc := routingOf(t, name, data)
		patterns := map[string]string{}
		for alias, m := range doc.Models {
			if p, _ := m["provider"].(string); p == "gemini" {
				patterns[alias], _ = m["model_pattern"].(string)
			}
		}
		if len(patterns) < 3 {
			t.Fatalf("%s: gemini patterns %v; the test is not reading them", name, patterns)
		}
		for alias, pattern := range patterns {
			for _, model := range notChat {
				if ok, _ := path.Match(pattern, model); ok {
					t.Errorf("%s: %s (%q) matches %s, which is not a chat model", name, alias, pattern, model)
				}
			}
		}
		for model, alias := range chat {
			if ok, _ := path.Match(patterns[alias], model); !ok {
				t.Errorf("%s: %s (%q) does not match %s", name, alias, patterns[alias], model)
			}
		}
	}
}

func mustShow(t *testing.T, name string) []byte {
	t.Helper()
	data, err := Show(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A glob belongs under model_pattern. Under `model:` it is a literal model id,
// and no provider lists a model called "gpt-oss*": the candidate is skipped,
// silently, like any model the provider does not have. Two of the `local`
// preset's three aliases were written that way, so its low and middle tiers
// had no candidate of their own.
func TestShippedConfigs_NoAliasPutsAGlobUnderModel(t *testing.T) {
	sources := map[string][]byte{"loomcycle.example.yaml": ExampleYAML()}
	for _, u := range Units() {
		sources[u.Name] = u.Data
	}
	aliases := 0
	for name, data := range sources {
		for alias, m := range routingOf(t, name, data).Models {
			aliases++
			if model, _ := m["model"].(string); strings.ContainsAny(model, "*?[") {
				t.Errorf("%s: models.%s has `model: %s`; a glob is only read under model_pattern", name, alias, model)
			}
		}
	}
	if aliases < 20 {
		t.Fatalf("read %d aliases; the test is not reading the configs", aliases)
	}
}
