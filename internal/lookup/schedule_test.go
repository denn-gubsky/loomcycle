package lookup_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// stubScheduleStore is a minimal in-memory ScheduleStore for the
// equivalence tests. Sufficient for unit-level coverage of the
// resolver chain.
type stubScheduleStore struct {
	defs map[string]store.ScheduleDefRow // keyed by name (active)
}

// ScheduleDefGetActive ignores tenantID — these resolver tests exercise
// the precedence/equivalence logic with the shared "" tenant; per-tenant
// isolation is covered by the store contract test.
func (s *stubScheduleStore) ScheduleDefGetActive(_ context.Context, _, name string) (store.ScheduleDefRow, error) {
	if row, ok := s.defs[name]; ok {
		return row, nil
	}
	return store.ScheduleDefRow{}, &store.ErrNotFound{Kind: "schedule_def_active", ID: name}
}

// TestSchedule_EquivalenceYamlVsSubstrate pins the architectural
// contract: loading the same content via the yaml path
// (cfg.ScheduledRuns) vs the substrate path (schedule_defs row,
// decoded by lookup.Schedule) MUST produce equivalent
// config.ScheduledRun structs after normalization.
//
// Catches drift bugs the way TestAgent_EquivalenceYamlVsSubstrate
// does for AgentDef. The two paths must stay in lockstep.
func TestSchedule_EquivalenceYamlVsSubstrate(t *testing.T) {
	// Seed schedule — representative content shape with on_complete +
	// per-tier defaults + required_credentials.
	yamlSchedule := config.ScheduledRun{
		Agent: "job-search-batch",
		Prompt: []config.ScheduledRunSegment{
			{Role: "user", Content: []config.ScheduledRunSegmentContent{
				{Type: "trusted-text", Text: "run the search"},
			}},
		},
		UserTierSchedules:   map[string]string{"low": "0 6 1 * *", "high": "0 6 * * *"},
		RequiredCredentials: []string{"jobs", "slack"},
		Timezone:            "Europe/Berlin",
		Enabled:             true,
		CatchUpMax:          0,
		OnComplete: []config.ScheduledRunHook{
			{Kind: "channel.publish", Channel: "notifications/alice"},
		},
	}

	// Persist via the substrate shape (snake_case json tags via
	// lookup.SubstrateScheduleDef).
	enabled := yamlSchedule.Enabled
	substrateShape := lookup.SubstrateScheduleDef{
		Agent:               yamlSchedule.Agent,
		UserTierSchedules:   yamlSchedule.UserTierSchedules,
		RequiredCredentials: yamlSchedule.RequiredCredentials,
		Timezone:            yamlSchedule.Timezone,
		Enabled:             &enabled,
		CatchUpMax:          yamlSchedule.CatchUpMax,
	}
	substrateShape.Prompt = make([]lookup.SubstratePromptSegment, len(yamlSchedule.Prompt))
	for i, seg := range yamlSchedule.Prompt {
		substrateShape.Prompt[i].Role = seg.Role
		substrateShape.Prompt[i].Content = make([]lookup.SubstratePromptSegmentContent, len(seg.Content))
		for j, c := range seg.Content {
			substrateShape.Prompt[i].Content[j].Type = c.Type
			substrateShape.Prompt[i].Content[j].Text = c.Text
		}
	}
	substrateShape.OnComplete = make([]lookup.SubstrateScheduleHook, len(yamlSchedule.OnComplete))
	for i, h := range yamlSchedule.OnComplete {
		substrateShape.OnComplete[i] = lookup.SubstrateScheduleHook{Kind: h.Kind, Channel: h.Channel}
	}
	defJSON, err := json.Marshal(substrateShape)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Resolve via the substrate path.
	ss := &stubScheduleStore{
		defs: map[string]store.ScheduleDefRow{
			"job-search-template": {
				DefID:      "sd_template_v1",
				Name:       "job-search-template",
				Version:    1,
				Definition: defJSON,
				CreatedAt:  time.Now(),
			},
		},
	}
	resolved, ok := lookup.Schedule(context.Background(), ss, &config.Config{}, "", "job-search-template")
	if !ok {
		t.Fatal("resolver returned !ok")
	}

	// Equivalence: every field that crosses the wire must arrive intact.
	if resolved.Agent != yamlSchedule.Agent {
		t.Errorf("Agent: got %q want %q", resolved.Agent, yamlSchedule.Agent)
	}
	if !reflect.DeepEqual(resolved.UserTierSchedules, yamlSchedule.UserTierSchedules) {
		t.Errorf("UserTierSchedules mismatch:\n got %v\nwant %v", resolved.UserTierSchedules, yamlSchedule.UserTierSchedules)
	}
	if !reflect.DeepEqual(resolved.RequiredCredentials, yamlSchedule.RequiredCredentials) {
		t.Errorf("RequiredCredentials mismatch:\n got %v\nwant %v", resolved.RequiredCredentials, yamlSchedule.RequiredCredentials)
	}
	if resolved.Timezone != yamlSchedule.Timezone {
		t.Errorf("Timezone: got %q want %q", resolved.Timezone, yamlSchedule.Timezone)
	}
	if !reflect.DeepEqual(resolved.Prompt, yamlSchedule.Prompt) {
		t.Errorf("Prompt mismatch:\n got %v\nwant %v", resolved.Prompt, yamlSchedule.Prompt)
	}
	if len(resolved.OnComplete) != 1 || resolved.OnComplete[0].Channel != "notifications/alice" {
		t.Errorf("OnComplete mismatch: %v", resolved.OnComplete)
	}
}

// TestSchedule_StaticBeforeSubstrate pins precedence: when a name
// exists in BOTH cfg.ScheduledRuns AND the substrate's active
// pointer, the static yaml wins.
func TestSchedule_StaticBeforeSubstrate(t *testing.T) {
	cfg := &config.Config{
		ScheduledRuns: map[string]config.ScheduledRun{
			"my-sched": {Agent: "yaml-agent", Schedule: "0 0 * * *", Timezone: "UTC"},
		},
	}
	ss := &stubScheduleStore{
		defs: map[string]store.ScheduleDefRow{
			"my-sched": {
				DefID:      "sd_substrate_v1",
				Name:       "my-sched",
				Definition: json.RawMessage(`{"agent":"substrate-agent","schedule":"0 12 * * *"}`),
			},
		},
	}
	got, ok := lookup.Schedule(context.Background(), ss, cfg, "", "my-sched")
	if !ok {
		t.Fatal("resolver returned !ok")
	}
	if got.Agent != "yaml-agent" {
		t.Errorf("Agent = %q, want yaml-agent (static must win)", got.Agent)
	}
}

// TestSchedule_NormalizesTimezone pins the empty-Timezone → "UTC"
// normalization (RFC E sharp edge: timezone optional, UTC default).
func TestSchedule_NormalizesTimezone(t *testing.T) {
	ss := &stubScheduleStore{
		defs: map[string]store.ScheduleDefRow{
			"no-tz": {
				DefID:      "sd_notz_v1",
				Name:       "no-tz",
				Definition: json.RawMessage(`{"agent":"demo","schedule":"0 0 * * *"}`),
			},
		},
	}
	got, ok := lookup.Schedule(context.Background(), ss, &config.Config{}, "", "no-tz")
	if !ok {
		t.Fatal("resolver returned !ok")
	}
	if got.Timezone != "UTC" {
		t.Errorf("Timezone = %q, want UTC (normalizer should default)", got.Timezone)
	}
}

// TestSchedule_DriftDetection pins the SubstrateScheduleDef field set
// against an explicit `want` enumeration. A field added to or removed
// from SubstrateScheduleDef without updating this enumeration fails
// CI — mirrors TestAgent_DriftDetection.
//
// The complementary direction (a field added to mergedScheduleDef in
// the builtin package but not mirrored here) lives in the builtin
// package alongside the tool. See
// TestMergedScheduleDef_DriftDetection_VsLookupSubstrateScheduleDef
// once that ships in the follow-up tool PR.
func TestSchedule_DriftDetection(t *testing.T) {
	want := map[string]bool{
		"agent":                     true,
		"prompt":                    true,
		"schedule":                  true,
		"user_tier_schedules":       true,
		"required_credentials":      true,
		"timezone":                  true,
		"enabled":                   true,
		"catch_up_max":              true,
		"user_id":                   true,
		"user_tier":                 true,
		"user_credentials_from_env": true,
		"on_complete":               true,
		"metadata":                  true, // non-secret agent metadata (PR 2/2)
		"tenant_id":                 true, // tenant the fired run executes as (RFC N follow-up)
	}
	have := scheduleJsonTagsOf(reflect.TypeOf(lookup.SubstrateScheduleDef{}))
	for tag := range want {
		if !have[tag] {
			t.Errorf("SubstrateScheduleDef missing json tag %q (must mirror config.ScheduledRun yaml tag)", tag)
		}
	}
	for tag := range have {
		if !want[tag] {
			t.Errorf("SubstrateScheduleDef has json tag %q not in expected set — if this field was deliberately added, update the `want` map in this test to confirm the addition was conscious", tag)
		}
	}
}

func scheduleJsonTagsOf(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		for j, c := range tag {
			if c == ',' {
				tag = tag[:j]
				break
			}
		}
		out[tag] = true
	}
	return out
}
