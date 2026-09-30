package snapshot

import (
	"reflect"
	"testing"
)

// The `restored` map every transport sends is RestoreResult.Counts(). It must
// carry every counter the result has — the five def counts that no transport
// reported are what a hand-kept list loses — and keep the keys the connector's
// map already sent, since a published key is never renamed.

func TestRestoreCounts_CarriesEveryCounter(t *testing.T) {
	var r RestoreResult
	rv := reflect.ValueOf(&r).Elem()
	ints := 0
	for i := 0; i < rv.NumField(); i++ {
		if rv.Field(i).Kind() == reflect.Int {
			ints++
			rv.Field(i).SetInt(int64(100 + i)) // distinct, so a crossed wire shows
		}
	}
	if ints < 20 {
		t.Fatalf("only %d int counters on RestoreResult; the reflection stopped matching", ints)
	}
	got := r.Counts()
	if len(got) != ints {
		t.Errorf("Counts has %d keys, RestoreResult has %d counters: %v", len(got), ints, got)
	}
	for key, field := range map[string]string{
		"skill_defs":                "SkillDefsRestored",
		"team_def_active":           "TeamDefActiveRestored",
		"hook_defs":                 "HookDefsRestored",
		"mcp_server_defs":           "MCPServerDefsRestored",
		"channel_defs":              "ChannelDefsRestored",
		"sqlmem_scopes":             "SqlMemScopesRestored",
		"synthesized_sessions":      "SynthesizedSessions",
		"mcp_server_defs_activated": "MCPServerDefsActivated",
		"paused_runs_resumed":       "PausedRunsResumed",
		"mcp_server_defs_refused":   "MCPServerDefsRefused",
		"channel_defs_refused":      "ChannelDefsRefused",
	} {
		if want := int(rv.FieldByName(field).Int()); got[key] != want {
			t.Errorf("Counts()[%q] = %d, want %s = %d", key, got[key], field, want)
		}
	}
}

func TestRestoreCounts_KeepsThePublishedKeys(t *testing.T) {
	// The keys the connector's restored map sent before it carried every
	// counter. Renaming one breaks a caller that reads it.
	published := []string{"agent_defs", "agent_def_active", "memory", "channel_messages", "channel_cursors",
		"evaluations", "paused_runs", "transcript_events", "interaction_history"}
	got := RestoreResult{}.Counts()
	for _, k := range published {
		if _, ok := got[k]; !ok {
			t.Errorf("Counts() dropped the published key %q (have %v)", k, got)
		}
	}
}
