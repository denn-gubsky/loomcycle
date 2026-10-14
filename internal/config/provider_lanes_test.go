package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// lanesBlock is the documented shape, every new key set.
const lanesBlock = `
providers:
  ollama-local:
    driver: ollama
    max_concurrent: 4
    max_models_in_flight: 1
    models_in_flight_exempt_kinds: [embedder]
    model_switch_max_wait_ms: 30000
    lanes:
      chat:     { max_concurrent: 4, quantum_calls: 4, affinity_hold_ms: 5000 }
      decision: { max_concurrent: 2 }
      embedder: { max_concurrent: 2, background_share: 0.25 }
`

func wantLanesBlock() ProviderConfig {
	return ProviderConfig{
		Driver:                    "ollama",
		MaxConcurrent:             4,
		MaxModelsInFlight:         1,
		ModelSwitchMaxWaitMS:      30000,
		ModelsInFlightExemptKinds: []string{ModelKindEmbedder},
		Lanes: map[string]ProviderLane{
			ModelKindChat:     {MaxConcurrent: 4, QuantumCalls: 4, AffinityHoldMS: 5000},
			ModelKindDecision: {MaxConcurrent: 2},
			ModelKindEmbedder: {MaxConcurrent: 2, BackgroundShare: 0.25},
		},
	}
}

// laneCfg is the smallest config validate() accepts around one provider entry.
func laneCfg(pc ProviderConfig) *Config {
	if pc.Driver == "" {
		pc.Driver = "ollama"
	}
	return &Config{
		Defaults:    Defaults{Provider: "anthropic", Model: "x"},
		Concurrency: Concurrency{MaxConcurrentRuns: 1},
		Providers:   map[string]ProviderConfig{"ollama-local": pc},
	}
}

// The documented block loads through the real loader (so it also validates),
// and survives a marshal back to yaml and to json — the two encodings the
// reload diff and an admin surface use.
func TestProviderLanes_RoundTripKeepsEveryKey(t *testing.T) {
	cfg, err := LoadLayers(Layer{Name: "op", Data: []byte(lanesBlock)})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	want := wantLanesBlock()
	got := cfg.Providers["ollama-local"]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded = %+v\nwant     %+v", got, want)
	}

	y, err := yaml.Marshal(got)
	if err != nil {
		t.Fatalf("yaml marshal: %v", err)
	}
	var fromYAML ProviderConfig
	if err := yaml.Unmarshal(y, &fromYAML); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if !reflect.DeepEqual(fromYAML, want) {
		t.Errorf("yaml round trip = %+v\nwant              %+v\nyaml:\n%s", fromYAML, want, y)
	}

	j, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	for _, key := range []string{`"max_models_in_flight":1`, `"model_switch_max_wait_ms":30000`, `"models_in_flight_exempt_kinds":["embedder"]`, `"lanes":{`,
		`"quantum_calls":4`, `"affinity_hold_ms":5000`, `"background_share":0.25`} {
		if !strings.Contains(string(j), key) {
			t.Errorf("json %s is missing %s", j, key)
		}
	}
	var fromJSON ProviderConfig
	if err := json.Unmarshal(j, &fromJSON); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if !reflect.DeepEqual(fromJSON, want) {
		t.Errorf("json round trip = %+v\nwant              %+v", fromJSON, want)
	}
}

// An entry that sets none of the new keys serialises exactly as before, so the
// reload diff does not see a change in configs that never mention lanes.
func TestProviderLanes_UnsetKeysAreNotSerialised(t *testing.T) {
	pc := ProviderConfig{Driver: "ollama", MaxConcurrent: 4}
	y, err := yaml.Marshal(pc)
	if err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(pc)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"lanes", "max_models_in_flight", "model_switch_max_wait_ms", "models_in_flight_exempt_kinds"} {
		if strings.Contains(string(y), key) || strings.Contains(string(j), key) {
			t.Errorf("%s serialised on an entry that does not set it:\nyaml: %sjson: %s", key, y, j)
		}
	}
}

func TestProviderConfig_EffectiveLanesAppliesMaxConcurrentShorthand(t *testing.T) {
	cases := []struct {
		name string
		pc   ProviderConfig
		want map[string]ProviderLane
	}{
		{"no cap and no lanes is ungated",
			ProviderConfig{}, map[string]ProviderLane{}},
		{"max_concurrent alone is the chat lane",
			ProviderConfig{MaxConcurrent: 4},
			map[string]ProviderLane{ModelKindChat: {MaxConcurrent: 4}}},
		{"max_concurrent equal to the chat lane's cap",
			ProviderConfig{MaxConcurrent: 4, Lanes: map[string]ProviderLane{ModelKindChat: {MaxConcurrent: 4, QuantumCalls: 2}}},
			map[string]ProviderLane{ModelKindChat: {MaxConcurrent: 4, QuantumCalls: 2}}},
		{"max_concurrent fills a chat lane that sets only tuning",
			ProviderConfig{MaxConcurrent: 4, Lanes: map[string]ProviderLane{ModelKindChat: {QuantumCalls: 2, AffinityHoldMS: 100}}},
			map[string]ProviderLane{ModelKindChat: {MaxConcurrent: 4, QuantumCalls: 2, AffinityHoldMS: 100}}},
		{"max_concurrent does not reach the other lanes",
			ProviderConfig{MaxConcurrent: 4, Lanes: map[string]ProviderLane{ModelKindEmbedder: {MaxConcurrent: 2}}},
			map[string]ProviderLane{ModelKindChat: {MaxConcurrent: 4}, ModelKindEmbedder: {MaxConcurrent: 2}}},
		{"a lane with no cap is left out",
			ProviderConfig{Lanes: map[string]ProviderLane{ModelKindChat: {}, ModelKindDecision: {MaxConcurrent: 0}, ModelKindEmbedder: {MaxConcurrent: 3}}},
			map[string]ProviderLane{ModelKindEmbedder: {MaxConcurrent: 3}}},
		{"the documented block",
			wantLanesBlock(), wantLanesBlock().Lanes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.pc.EffectiveLanes(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("EffectiveLanes() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The shorthand must not write through to the configured entry: the reload diff
// marshals the config, and a helper that mutated it would show a change.
func TestProviderConfig_EffectiveLanesLeavesTheConfigUntouched(t *testing.T) {
	pc := ProviderConfig{MaxConcurrent: 4, Lanes: map[string]ProviderLane{ModelKindChat: {QuantumCalls: 2}}}
	pc.EffectiveLanes()[ModelKindDecision] = ProviderLane{MaxConcurrent: 9}
	want := map[string]ProviderLane{ModelKindChat: {QuantumCalls: 2}}
	if !reflect.DeepEqual(pc.Lanes, want) {
		t.Errorf("Lanes after EffectiveLanes = %+v, want %+v", pc.Lanes, want)
	}
}

func TestProviderConfig_ModelSwitchMaxWaitDefaultsOnlyWhenModelsAreCapped(t *testing.T) {
	cases := []struct {
		name string
		pc   ProviderConfig
		want int
	}{
		{"uncapped, unset", ProviderConfig{}, 0},
		{"uncapped, set: nothing waits for a switch", ProviderConfig{ModelSwitchMaxWaitMS: 5000}, 0},
		{"capped, unset: the default", ProviderConfig{MaxModelsInFlight: 1}, 30000},
		{"capped, set", ProviderConfig{MaxModelsInFlight: 2, ModelSwitchMaxWaitMS: 5000}, 5000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.pc.ModelSwitchMaxWaitMSOrDefault(); got != tc.want {
				t.Errorf("ModelSwitchMaxWaitMSOrDefault() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestValidate_ProviderLanesAccepted(t *testing.T) {
	cases := []struct {
		name string
		pc   ProviderConfig
	}{
		{"the documented block", wantLanesBlock()},
		{"max_concurrent equal to lanes.chat.max_concurrent",
			ProviderConfig{MaxConcurrent: 4, Lanes: map[string]ProviderLane{ModelKindChat: {MaxConcurrent: 4}}}},
		{"chat tuning capped by the max_concurrent shorthand",
			ProviderConfig{MaxConcurrent: 4, Lanes: map[string]ProviderLane{ModelKindChat: {QuantumCalls: 4, AffinityHoldMS: 5000, BackgroundShare: 0.5}}}},
		{"an empty lane",
			ProviderConfig{Lanes: map[string]ProviderLane{ModelKindDecision: {}}}},
		{"model_switch_max_wait_ms without max_models_in_flight",
			ProviderConfig{ModelSwitchMaxWaitMS: 1000}},
		{"every kind exempt from max_models_in_flight",
			ProviderConfig{MaxModelsInFlight: 1, ModelsInFlightExemptKinds: []string{ModelKindChat, ModelKindDecision, ModelKindEmbedder}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validate(laneCfg(tc.pc)); err != nil {
				t.Errorf("rejected: %v", err)
			}
		})
	}
}

func TestValidate_ProviderLanesRejected(t *testing.T) {
	lanes := func(kind string, l ProviderLane) ProviderConfig {
		return ProviderConfig{Lanes: map[string]ProviderLane{kind: l}}
	}
	cases := []struct {
		name string
		pc   ProviderConfig
		want []string // every substring must appear
	}{
		{"unknown lane key",
			lanes("rerank", ProviderLane{MaxConcurrent: 1}),
			[]string{"providers.ollama-local", `unknown lane "rerank"`, "[chat decision embedder]"}},
		{"negative max_models_in_flight",
			ProviderConfig{MaxModelsInFlight: -1},
			[]string{"providers.ollama-local: max_models_in_flight must be >= 0"}},
		{"negative model_switch_max_wait_ms",
			ProviderConfig{MaxModelsInFlight: 1, ModelSwitchMaxWaitMS: -1},
			[]string{"providers.ollama-local: model_switch_max_wait_ms must be >= 0"}},
		{"models_in_flight_exempt_kinds without max_models_in_flight",
			ProviderConfig{ModelsInFlightExemptKinds: []string{ModelKindEmbedder}},
			[]string{"providers.ollama-local: models_in_flight_exempt_kinds is set but max_models_in_flight is not"}},
		{"unknown kind in models_in_flight_exempt_kinds",
			ProviderConfig{MaxModelsInFlight: 1, ModelsInFlightExemptKinds: []string{"rerank"}},
			[]string{"providers.ollama-local: models_in_flight_exempt_kinds: unknown kind \"rerank\"", "[chat decision embedder]"}},
		{"negative lane max_concurrent",
			lanes(ModelKindDecision, ProviderLane{MaxConcurrent: -1}),
			[]string{"providers.ollama-local.lanes.decision: max_concurrent must be >= 0"}},
		{"negative quantum_calls",
			lanes(ModelKindChat, ProviderLane{MaxConcurrent: 1, QuantumCalls: -1}),
			[]string{"providers.ollama-local.lanes.chat: quantum_calls must be >= 0"}},
		{"negative affinity_hold_ms",
			lanes(ModelKindChat, ProviderLane{MaxConcurrent: 1, AffinityHoldMS: -1}),
			[]string{"providers.ollama-local.lanes.chat: affinity_hold_ms must be >= 0"}},
		{"negative background_share",
			lanes(ModelKindEmbedder, ProviderLane{MaxConcurrent: 1, BackgroundShare: -0.1}),
			[]string{"providers.ollama-local.lanes.embedder: background_share must be >= 0 and < 1"}},
		{"background_share of 1",
			lanes(ModelKindEmbedder, ProviderLane{MaxConcurrent: 1, BackgroundShare: 1}),
			[]string{"providers.ollama-local.lanes.embedder: background_share must be >= 0 and < 1"}},
		{"quantum_calls on an uncapped lane",
			lanes(ModelKindDecision, ProviderLane{QuantumCalls: 4}),
			[]string{"providers.ollama-local.lanes.decision: quantum_calls is set", "no max_concurrent"}},
		{"affinity_hold_ms on an uncapped lane",
			lanes(ModelKindDecision, ProviderLane{AffinityHoldMS: 5000}),
			[]string{"providers.ollama-local.lanes.decision: affinity_hold_ms is set", "no max_concurrent"}},
		{"background_share on an uncapped lane",
			lanes(ModelKindEmbedder, ProviderLane{BackgroundShare: 0.25}),
			[]string{"providers.ollama-local.lanes.embedder: background_share is set", "no max_concurrent"}},
		{"chat tuning with neither cap",
			lanes(ModelKindChat, ProviderLane{QuantumCalls: 4}),
			[]string{"providers.ollama-local.lanes.chat: quantum_calls is set", "no max_concurrent"}},
		// The shorthand caps the chat lane only: it does not gate the others.
		{"max_concurrent does not cap a decision lane's tuning",
			ProviderConfig{MaxConcurrent: 4, Lanes: map[string]ProviderLane{ModelKindDecision: {QuantumCalls: 4}}},
			[]string{"providers.ollama-local.lanes.decision: quantum_calls is set"}},
		{"max_concurrent and lanes.chat.max_concurrent differ",
			ProviderConfig{MaxConcurrent: 4, Lanes: map[string]ProviderLane{ModelKindChat: {MaxConcurrent: 2}}},
			[]string{"providers.ollama-local: max_concurrent (4)", "lanes.chat.max_concurrent (2)", "differ"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(laneCfg(tc.pc))
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

// builtinLanes stands in for the embedded default-providers layer: operator
// entries merge over it key by key, before the typed decode.
const builtinLanes = `
providers:
  ollama-local:
    driver: ollama
    max_models_in_flight: 1
    lanes:
      chat: { max_concurrent: 4, quantum_calls: 4 }
`

func TestLoadLayers_OperatorLaneKeepsBuiltinSiblingLanes(t *testing.T) {
	op := `
providers:
  ollama-local:
    lanes:
      embedder: { max_concurrent: 2 }
`
	cfg, err := LoadLayers(Layer{Name: "builtin", Data: []byte(builtinLanes)}, Layer{Name: "op", Data: []byte(op)})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	got := cfg.Providers["ollama-local"]
	want := map[string]ProviderLane{
		ModelKindChat:     {MaxConcurrent: 4, QuantumCalls: 4},
		ModelKindEmbedder: {MaxConcurrent: 2},
	}
	if !reflect.DeepEqual(got.Lanes, want) {
		t.Errorf("lanes = %+v, want %+v", got.Lanes, want)
	}
	if got.Driver != "ollama" || got.MaxModelsInFlight != 1 {
		t.Errorf("built-in driver/max_models_in_flight lost: %+v", got)
	}
}

func TestLoadLayers_OperatorLaneFieldKeepsBuiltinLaneCap(t *testing.T) {
	op := `
providers:
  ollama-local:
    model_switch_max_wait_ms: 10000
    lanes:
      chat:
        quantum_calls: 8
`
	cfg, err := LoadLayers(Layer{Name: "builtin", Data: []byte(builtinLanes)}, Layer{Name: "op", Data: []byte(op)})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	got := cfg.Providers["ollama-local"]
	want := map[string]ProviderLane{ModelKindChat: {MaxConcurrent: 4, QuantumCalls: 8}}
	if !reflect.DeepEqual(got.Lanes, want) {
		t.Errorf("lanes = %+v, want %+v", got.Lanes, want)
	}
	if got.MaxModelsInFlight != 1 || got.ModelSwitchMaxWaitMS != 10000 {
		t.Errorf("max_models_in_flight/model_switch_max_wait_ms = %d/%d, want 1/10000", got.MaxModelsInFlight, got.ModelSwitchMaxWaitMS)
	}
}

// The shorthand and the lane cap are checked on the MERGED entry: a built-in
// max_concurrent and an operator lanes.chat.max_concurrent that disagree are
// refused at load, not resolved silently in favour of one layer.
func TestLoadLayers_ShorthandAndChatLaneCapAcrossLayersMustAgree(t *testing.T) {
	builtin := "providers:\n  ollama-local:\n    driver: ollama\n    max_concurrent: 4\n"
	op := "providers:\n  ollama-local:\n    lanes:\n      chat: { max_concurrent: 2 }\n"
	_, err := LoadLayers(Layer{Name: "builtin", Data: []byte(builtin)}, Layer{Name: "op", Data: []byte(op)})
	if err == nil || !strings.Contains(err.Error(), "max_concurrent (4)") || !strings.Contains(err.Error(), "lanes.chat.max_concurrent (2)") {
		t.Errorf("got %v, want an error naming both caps", err)
	}
}

// A hot reload decides what to apply from the section diff, so a lanes-only
// edit has to show up as a changed providers section.
func TestChangedSections_ProviderLaneEditFlagsProviders(t *testing.T) {
	oldCfg := laneCfg(ProviderConfig{MaxConcurrent: 4})
	newCfg := laneCfg(ProviderConfig{MaxConcurrent: 4, Lanes: map[string]ProviderLane{ModelKindEmbedder: {MaxConcurrent: 2}}})
	changed, err := ChangedSections(oldCfg, newCfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != "providers" {
		t.Errorf("changed = %v, want [providers]", changed)
	}
}
