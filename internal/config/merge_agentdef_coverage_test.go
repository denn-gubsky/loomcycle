package config

import (
	"reflect"
	"testing"
)

// TestMergeAgentDef_CoversEveryAgentDefField is the sibling of the substrate
// overlay's drift test, guarding the same user-visible failure from the other
// direction: an operator writes yaml and the yaml is silently discarded.
//
// mergeAgentDef merges an MD-discovered agent with its yaml `agents:` override.
// A field absent from it is not a compile error and produces no warning — the
// operator's declaration is simply dropped. Twelve were missing when this test
// was written (sampling, compaction, volumes, history_scope, search_providers,
// unbounded_iterations, run_timeout_seconds, disable_context, retry_attempts,
// and three of the *_def_scopes gates), so an operator overriding any of them
// on an MD-declared agent got no effect and no diagnostic.
//
// Adding a field to `exempt` is the conscious decision this test forces.
func TestMergeAgentDef_CoversEveryAgentDefField(t *testing.T) {
	exempt := map[string]string{
		// A REMOVED-field tombstone (see AgentDef.SkillDefScopes): skill
		// authoring is governed by the `skills:` pattern allowlist now, so
		// merging it would resurrect a dead gate.
		"SkillDefScopes": "removed-field tombstone; superseded by the skills: allowlist",
		// `yaml:"-"` — derived at config load from SystemPrompt, never
		// operator-declarable, so there is nothing for an override to carry.
		"SystemPromptBase": "derived at load from SystemPrompt; not operator-declarable",
	}

	merged := mergedAgentDefFields(t)
	defType := reflect.TypeOf(AgentDef{})
	for i := 0; i < defType.NumField(); i++ {
		name := defType.Field(i).Name
		if _, ok := exempt[name]; ok {
			continue
		}
		if !merged[name] {
			t.Errorf("AgentDef.%s is never assigned by mergeAgentDef — a yaml `agents:` override of it on an MD-discovered agent is SILENTLY DISCARDED.\n"+
				"Either merge it (string: != \"\"; int: != 0; slice: != nil; bool: build up; pointer: nil check) "+
				"OR add %q to this test's exempt map with the reason it cannot be merged.", name, name)
		}
	}
}

// mergedAgentDefFields reports which AgentDef fields mergeAgentDef actually
// writes, by exercising it: merge a zero base against an override with every
// field set to a non-zero value, then diff. This beats parsing the source —
// it cannot be fooled by a field that is mentioned but never assigned, and it
// proves the merge semantics really fire.
func mergedAgentDefFields(t *testing.T) map[string]bool {
	t.Helper()
	override := nonZeroAgentDef()
	got := mergeAgentDef(AgentDef{}, override)

	out := map[string]bool{}
	gv, ov := reflect.ValueOf(got), reflect.ValueOf(override)
	for i := 0; i < gv.NumField(); i++ {
		if reflect.DeepEqual(gv.Field(i).Interface(), ov.Field(i).Interface()) {
			out[gv.Type().Field(i).Name] = true
		}
	}
	return out
}

// nonZeroAgentDef returns an AgentDef with every field set to a distinctive
// non-zero value, so "did the merge carry this field over?" is answerable by
// comparison. Kept exhaustive by the test above: a new field defaults to its
// zero value here, which equals the zero base, so it registers as NOT merged
// and fails loudly rather than passing by accident.
func nonZeroAgentDef() AgentDef {
	retries := 3
	return AgentDef{
		Provider:              "anthropic",
		Model:                 "claude-x",
		Code:                  "function run(){}",
		SystemPrompt:          "prompt",
		SystemPromptBase:      "base",
		SystemPromptFile:      "", // mutually exclusive with SystemPrompt; see below
		Tools:                 []string{"Read"},
		Skills:                []string{"a/*"},
		MaxTokens:             111,
		MaxIterations:         22,
		UnboundedIterations:   true,
		MaxConcurrentChildren: 7,
		RunTimeoutSeconds:     33,
		Tier:                  "low",
		Effort:                "high",
		Sampling:              &Sampling{Temperature: floatPtr(0.5)},
		Compaction:            &Compaction{Enabled: boolPtr(true), KeepLastN: intPtr(4)},
		Volumes:               []string{"workspace"},
		Providers:             []string{"anthropic"},
		SearchProviders:       []string{"brave"},
		Models:                map[string][]TierCandidate{"low": {{Provider: "anthropic", Model: "m"}}},
		MemoryScopes:          []string{"user"},
		MemoryQuotaBytes:      1024,
		SqlScopes:             []string{"user"},
		SqlQuotaBytes:         2048,
		MemoryBackend:         "inprocess",
		CoreBlocks:            []CoreBlock{{Label: "human", Scope: "user"}},
		InheritCoreBlocks:     true,
		MemoryInjectMaxTokens: 512,
		MemoryProtocol:        true,
		MemoryConsolidation:   true,
		MemoryIndexMaxBytes:   4096,
		MemoryRoots:           "lazy",
		Channels:              AgentChannelACL{Publish: []string{"p"}, Subscribe: []string{"s"}},
		AgentDefScopes:        []string{"any"},
		ScheduleDefScopes:     []string{"any"},

		A2AServerCardDefScopes: []string{"any"},
		A2AAgentDefScopes:      []string{"any"},
		SkillDefScopes:         []string{"any"},
		VolumeDefScopes:        []string{"any"},
		EvaluationScopes:       []string{"read_any"},
		HistoryScope:           []string{"user"},
		DisableContext:         true,
		Interruption:           AgentInterruptionACL{Enabled: true, Kinds: []string{"question"}, MaxPending: 2},
		RetryAttempts:          &retries,
	}
}

func floatPtr(f float64) *float64 { return &f }
func boolPtr(b bool) *bool        { return &b }
func intPtr(i int) *int           { return &i }

// TestMergeAgentDef_PerFieldSemantics pins the non-obvious merge rules the
// coverage test above cannot express: pointer blocks merge PER FIELD (a yaml
// override setting only temperature must not wipe the MD's top_p), and
// RetryAttempts is a pointer precisely so an explicit "force 0" survives.
func TestMergeAgentDef_PerFieldSemantics(t *testing.T) {
	base := AgentDef{
		Sampling:   &Sampling{Temperature: floatPtr(0.2), TopP: floatPtr(0.9)},
		Compaction: &Compaction{Enabled: boolPtr(true), KeepLastN: intPtr(8)},
	}
	zero := 0
	override := AgentDef{
		Sampling:      &Sampling{Temperature: floatPtr(0.7)},
		Compaction:    &Compaction{KeepLastN: intPtr(2)},
		RetryAttempts: &zero,
	}
	got := mergeAgentDef(base, override)

	if got.Sampling == nil || got.Sampling.Temperature == nil || *got.Sampling.Temperature != 0.7 {
		t.Errorf("sampling temperature not overridden: %+v", got.Sampling)
	}
	if got.Sampling.TopP == nil || *got.Sampling.TopP != 0.9 {
		t.Errorf("sampling top_p was wiped by an override that only set temperature: %+v", got.Sampling)
	}
	if got.Compaction == nil || got.Compaction.KeepLastN == nil || *got.Compaction.KeepLastN != 2 {
		t.Errorf("compaction keep_last_n not overridden: %+v", got.Compaction)
	}
	if got.Compaction.Enabled == nil || !*got.Compaction.Enabled {
		t.Errorf("compaction enabled was wiped by an override that only set keep_last_n: %+v", got.Compaction)
	}
	if got.RetryAttempts == nil || *got.RetryAttempts != 0 {
		t.Errorf("an explicit `retry_attempts: 0` override was dropped: %v — the pointer exists so \"force no retries\" is distinguishable from \"unset\"", got.RetryAttempts)
	}
}
