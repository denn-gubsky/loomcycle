package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// modelCapsBlock is the documented shape: an Ollama host that runs two models
// one request at a time beside others that run four, one of them named by alias.
const modelCapsBlock = `
providers:
  ollama-local:
    driver: ollama
    lanes:
      chat:
        max_concurrent: 4
        model_caps: { qwen3.6:latest: 1, ornith: 1 }
models:
  ornith: { provider: ollama-local, model: "ornith-1.5:35b" }
`

// The block loads through the real loader (so it validates), keeps the alias
// key as written, and survives a marshal to yaml and json.
func TestProviderLanes_ModelCapsRoundTrip(t *testing.T) {
	cfg, err := LoadLayers(Layer{Name: "op", Data: []byte(modelCapsBlock)})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	want := map[string]int{"qwen3.6:latest": 1, "ornith": 1}
	got := cfg.Providers["ollama-local"]
	if !reflect.DeepEqual(got.Lanes[ModelKindChat].ModelCaps, want) {
		t.Fatalf("model_caps = %v, want %v", got.Lanes[ModelKindChat].ModelCaps, want)
	}
	y, err := yaml.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var fromYAML ProviderConfig
	if err := yaml.Unmarshal(y, &fromYAML); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromYAML, got) {
		t.Errorf("yaml round trip = %+v\nwant              %+v\nyaml:\n%s", fromYAML, got, y)
	}
	j, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(j), `"model_caps":{"ornith":1,"qwen3.6:latest":1}`) {
		t.Errorf("json %s is missing model_caps", j)
	}
	var fromJSON ProviderConfig
	if err := json.Unmarshal(j, &fromJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromJSON, got) {
		t.Errorf("json round trip = %+v\nwant              %+v", fromJSON, got)
	}
}

// A lane that sets no model_caps serialises exactly as before, so the reload
// diff does not see a change in configs that never mention it.
func TestProviderLanes_UnsetModelCapsAreNotSerialised(t *testing.T) {
	pc := ProviderConfig{Driver: "ollama", Lanes: map[string]ProviderLane{ModelKindChat: {MaxConcurrent: 4}}}
	y, err := yaml.Marshal(pc)
	if err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(pc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(y), "model_caps") || strings.Contains(string(j), "model_caps") {
		t.Errorf("model_caps serialised on a lane that does not set it:\nyaml: %sjson: %s", y, j)
	}
}

// ResolveModelCaps keys each cap by the concrete model: an alias of this
// provider becomes its model, a plain name stays.
func TestResolveModelCaps_ResolvesThisProvidersAliases(t *testing.T) {
	models := map[string]ModelRef{
		"ornith":  {Provider: "ollama-local", Model: "ornith-1.5:35b"},
		"bare":    {Model: "gemma4:latest"},
		"cloud-q": {Provider: "ollama", Model: "qwen3.6:cloud"},
	}
	got := ResolveModelCaps(models, "ollama-local", map[string]int{"ornith": 1, "bare": 2, "qwen3.6:latest": 1})
	want := map[string]int{"ornith-1.5:35b": 1, "gemma4:latest": 2, "qwen3.6:latest": 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResolveModelCaps = %v, want %v", got, want)
	}
	if got := ResolveModelCaps(models, "ollama-local", nil); got != nil {
		t.Errorf("ResolveModelCaps(nil) = %v, want nil", got)
	}
	// Two keys naming one model (refused at load) resolve to the lower cap.
	if got := ResolveModelCaps(models, "ollama-local", map[string]int{"ornith": 3, "ornith-1.5:35b": 2}); got["ornith-1.5:35b"] != 2 {
		t.Errorf("colliding keys = %v, want the lower cap 2", got)
	}
}

// laneCfgModels is laneCfg with a models: block.
func laneCfgModels(pc ProviderConfig, models map[string]ModelRef) *Config {
	c := laneCfg(pc)
	c.Models = models
	return c
}

func TestValidate_ModelCapsAccepted(t *testing.T) {
	chat := func(l ProviderLane) map[string]ProviderLane { return map[string]ProviderLane{ModelKindChat: l} }
	cases := []struct {
		name   string
		pc     ProviderConfig
		models map[string]ModelRef
	}{
		{"plain names on a capped lane",
			ProviderConfig{Lanes: chat(ProviderLane{MaxConcurrent: 4, ModelCaps: map[string]int{"qwen3.6:latest": 1, "gpt-oss:20b": 4}})}, nil},
		{"the chat lane capped by the max_concurrent shorthand",
			ProviderConfig{MaxConcurrent: 4, Lanes: chat(ProviderLane{ModelCaps: map[string]int{"qwen3.6:latest": 1}})}, nil},
		{"an alias of this provider",
			ProviderConfig{Lanes: chat(ProviderLane{MaxConcurrent: 4, ModelCaps: map[string]int{"ornith": 1}})},
			map[string]ModelRef{"ornith": {Provider: "ollama-local", Model: "ornith-1.5:35b"}}},
		{"a decision lane",
			ProviderConfig{Lanes: map[string]ProviderLane{ModelKindDecision: {MaxConcurrent: 2, ModelCaps: map[string]int{"nimble": 1}}}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validate(laneCfgModels(tc.pc, tc.models)); err != nil {
				t.Errorf("rejected: %v", err)
			}
		})
	}
}

func TestValidate_ModelCapsRejected(t *testing.T) {
	chat := func(l ProviderLane) ProviderConfig {
		return ProviderConfig{Lanes: map[string]ProviderLane{ModelKindChat: l}}
	}
	cases := []struct {
		name   string
		pc     ProviderConfig
		models map[string]ModelRef
		want   []string
	}{
		{"a cap of 0",
			chat(ProviderLane{MaxConcurrent: 4, ModelCaps: map[string]int{"m": 0}}), nil,
			[]string{"providers.ollama-local.lanes.chat: model_caps.m must be >= 1"}},
		{"a negative cap",
			chat(ProviderLane{MaxConcurrent: 4, ModelCaps: map[string]int{"m": -1}}), nil,
			[]string{"model_caps.m must be >= 1"}},
		{"a cap above the lane's",
			chat(ProviderLane{MaxConcurrent: 2, ModelCaps: map[string]int{"m": 3}}), nil,
			[]string{"model_caps.m (3) is above the lane's max_concurrent (2)"}},
		{"a cap above the shorthand's",
			ProviderConfig{MaxConcurrent: 2, Lanes: map[string]ProviderLane{ModelKindChat: {ModelCaps: map[string]int{"m": 3}}}}, nil,
			[]string{"model_caps.m (3) is above the lane's max_concurrent (2)"}},
		{"model_caps on an uncapped lane",
			chat(ProviderLane{ModelCaps: map[string]int{"m": 1}}), nil,
			[]string{"providers.ollama-local.lanes.chat: model_caps is set but the lane has no max_concurrent"}},
		{"model_caps on a lane the shorthand does not reach",
			ProviderConfig{MaxConcurrent: 4, Lanes: map[string]ProviderLane{ModelKindDecision: {ModelCaps: map[string]int{"m": 1}}}}, nil,
			[]string{"providers.ollama-local.lanes.decision: model_caps is set but the lane has no max_concurrent"}},
		{"an empty key",
			chat(ProviderLane{MaxConcurrent: 4, ModelCaps: map[string]int{" ": 1}}), nil,
			[]string{"model_caps: a key is empty"}},
		{"a model_pattern alias",
			chat(ProviderLane{MaxConcurrent: 4, ModelCaps: map[string]int{"newest": 1}}),
			map[string]ModelRef{"newest": {Provider: "ollama-local", ModelPattern: "qwen*"}},
			[]string{"model_caps.newest is a model_pattern alias"}},
		{"an alias of another provider",
			chat(ProviderLane{MaxConcurrent: 4, ModelCaps: map[string]int{"cloud-q": 1}}),
			map[string]ModelRef{"cloud-q": {Provider: "anthropic", Model: "claude-x"}},
			[]string{`model_caps.cloud-q is an alias of provider "anthropic", not "ollama-local"`}},
		{"an alias and its model both capped",
			chat(ProviderLane{MaxConcurrent: 4, ModelCaps: map[string]int{"ornith": 1, "ornith-1.5:35b": 1}}),
			map[string]ModelRef{"ornith": {Provider: "ollama-local", Model: "ornith-1.5:35b"}},
			[]string{"model_caps.ornith and model_caps.ornith-1.5:35b both name model \"ornith-1.5:35b\""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(laneCfgModels(tc.pc, tc.models))
			if err == nil {
				t.Fatalf("accepted, want an error containing %q", tc.want)
			}
			for _, sub := range tc.want {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not contain %q", err, sub)
				}
			}
		})
	}
}
