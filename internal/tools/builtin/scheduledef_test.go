package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// scheduleDefFixture builds a ScheduleDef tool over in-memory SQLite +
// a stub Config with one yaml template. Returns a ctx with a
// permissive policy (scopes=[any]); per-test code overrides for
// scope-specific cases.
func scheduleDefFixture(t *testing.T) (*ScheduleDef, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		Agents: map[string]config.AgentDef{
			"job-search-batch": {Provider: "anthropic", Model: "claude-haiku-4-5"},
		},
		ScheduledRuns: map[string]config.ScheduledRun{
			"job-search-template": {
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
			},
		},
	}
	tool := &ScheduleDef{
		Store:               s,
		Cfg:                 cfg,
		MaxDefinitionBytes:  131072,
		MaxDescriptionBytes: 8192,
	}
	ctx := tools.WithAgentName(context.Background(), "scheduler-orchestrator")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithScheduleDefPolicy(ctx, tools.ScheduleDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: "scheduler-orchestrator",
	})
	return tool, ctx, func() { _ = s.Close() }
}

func TestScheduleDefTool_CreateRefusedOverStaticName(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"job-search-template","overlay":{"agent":"x","schedule":"0 0 * * *"}}`))
	if !res.IsError {
		t.Fatalf("create over static name should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "static cfg.ScheduledRuns") {
		t.Errorf("refusal should mention static; got %s", res.Text)
	}
}

func TestScheduleDefTool_CreateNewName(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"adhoc-sched","overlay":{"agent":"job-search-batch","schedule":"0 9 * * 1","user_id":"alice"},"description":"weekly digest"}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["name"] != "adhoc-sched" {
		t.Errorf("name = %v, want adhoc-sched", out["name"])
	}
	if out["version"].(float64) != 1 {
		t.Errorf("version = %v, want 1", out["version"])
	}
	if out["promoted"].(bool) != true {
		t.Errorf("create default promote = false; want true")
	}
}

func TestScheduleDefTool_CreateRefusesInvalidCron(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad","overlay":{"agent":"job-search-batch","schedule":"not a cron","user_id":"alice"}}`))
	if !res.IsError {
		t.Fatalf("invalid cron should refuse")
	}
	if !strings.Contains(res.Text, "invalid cron") {
		t.Errorf("refusal should mention cron failure; got %s", res.Text)
	}
}

func TestScheduleDefTool_CreateRefusesMissingAgent(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"noagent","overlay":{"schedule":"0 0 * * *"}}`))
	if !res.IsError {
		t.Fatalf("create without agent should refuse")
	}
}

func TestScheduleDefTool_ForkBootstrapsTemplate(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	// Fork the yaml template — first fork bootstraps v1 from yaml,
	// then forks v2 with the user's credentials overlay.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"job-search-template","overlay":{"user_id":"alice","user_credentials":{"jobs":"x","slack":"y"}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	// First fork: bootstrap v1 + fork v2. So the returned row is v2.
	if out["name"] != "job-search-template" {
		t.Errorf("name = %v, want job-search-template", out["name"])
	}
	if v := out["version"].(float64); v != 2 {
		t.Errorf("version = %v, want 2 (v1 bootstrap + v2 fork)", v)
	}
	if out["promoted"].(bool) != true {
		t.Errorf("fork default promote = false; want true (schedules auto-promote per RFC E)")
	}
}

func TestScheduleDefTool_ForkRefusesMissingRequiredCredentials(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	// Template requires credentials {jobs, slack}; supply only jobs.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"job-search-template","overlay":{"user_id":"alice","user_credentials":{"jobs":"x"}}}`))
	if !res.IsError {
		t.Fatalf("fork with missing required cred should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "required credential") || !strings.Contains(res.Text, "slack") {
		t.Errorf("refusal should name the missing key; got %s", res.Text)
	}
}

func TestScheduleDefTool_ForkPartialCredentialMerge(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	// First fork with full credentials.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"job-search-template","overlay":{"user_id":"alice","user_credentials":{"jobs":"j1","slack":"s1"}}}`))
	if res.IsError {
		t.Fatalf("first fork: %s", res.Text)
	}
	out1 := decodeResult(t, res.Text)
	v1DefID := out1["def_id"].(string)

	// Second fork rotates ONLY slack — substrate must merge with v2's
	// credentials map so jobs is preserved.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"job-search-template","parent_def_id":"`+v1DefID+`","overlay":{"user_credentials":{"slack":"s2"}}}`))
	if res.IsError {
		t.Fatalf("rotation fork: %s", res.Text)
	}
	// Verify the v3 row's persisted definition includes both jobs
	// (preserved from v2) + slack (rotated from v2's s1 → s2).
	// ScheduleDefRow.Definition is json.RawMessage; once decoded
	// through decodeResult's map[string]any path it nests directly
	// as a sub-map (no base64 wrapping).
	out2 := decodeResult(t, res.Text)
	if out2["parent_def_id"].(string) != v1DefID {
		t.Errorf("parent_def_id = %q, want %q (rotation should chain)", out2["parent_def_id"], v1DefID)
	}
	def, ok := out2["definition"].(map[string]any)
	if !ok {
		t.Fatalf("definition not a JSON object: %T %v", out2["definition"], out2["definition"])
	}
	creds, ok := def["user_credentials"].(map[string]any)
	if !ok {
		t.Fatalf("user_credentials missing or not a map: %T %v", def["user_credentials"], def["user_credentials"])
	}
	if creds["jobs"] != "j1" {
		t.Errorf("jobs not preserved from parent fork; got %v, want j1 (RFC E §6: partial merge)", creds["jobs"])
	}
	if creds["slack"] != "s2" {
		t.Errorf("slack not rotated by overlay; got %v, want s2", creds["slack"])
	}
}

// TestScheduleDefTool_ForkPreservesEnabledWhenOverlayOmits regresses
// a self-review bug: applyOverlay used to unconditionally clobber
// d.Enabled with ov.Enabled. The fork-rotate-credentials path
// supplies only `user_credentials` in the overlay → ov.Enabled
// decodes to the zero value (false) → enabled=true template
// silently became enabled=false in the fork. Fixed by treating
// Enabled as pointer (*bool) so absence in overlay leaves the
// parent's value alone; explicit true/false in overlay overrides.
func TestScheduleDefTool_ForkPreservesEnabledWhenOverlayOmits(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	// Fork the yaml template (Enabled:true) supplying only credentials.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"job-search-template","overlay":{"user_id":"alice","user_credentials":{"jobs":"x","slack":"y"}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	def, ok := out["definition"].(map[string]any)
	if !ok {
		t.Fatalf("definition not a JSON object: %T", out["definition"])
	}
	// The yaml template sets enabled:true; the fork's overlay omits
	// it; the merged def MUST stay enabled. A false here means the
	// overlay's zero-value boolean clobbered the parent — the exact
	// bug this test guards against.
	if enabled, _ := def["enabled"].(bool); !enabled {
		t.Errorf("enabled=%v in fork — overlay zero-value clobbered the template's enabled:true", def["enabled"])
	}
}

func TestScheduleDefTool_NoScopesIsDefaultDeny(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	// Drop the permissive policy.
	ctx = tools.WithScheduleDefPolicy(ctx, tools.ScheduleDefPolicyValue{})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"x","overlay":{"agent":"job-search-batch","schedule":"0 0 * * *","user_id":"alice"}}`))
	if !res.IsError {
		t.Fatalf("empty scopes should default-deny")
	}
	if !strings.Contains(res.Text, "default-deny") {
		t.Errorf("refusal should mention default-deny; got %s", res.Text)
	}
}

func TestScheduleDefTool_NamedScope(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	// Restrict to named:adhoc-sched only.
	ctx = tools.WithScheduleDefPolicy(ctx, tools.ScheduleDefPolicyValue{
		Scopes: []string{"named:adhoc-sched"},
	})
	// Creating the allowed name → ok.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"adhoc-sched","overlay":{"agent":"job-search-batch","schedule":"0 0 * * *","user_id":"alice"}}`))
	if res.IsError {
		t.Fatalf("named scope should allow matching name; got %s", res.Text)
	}
	// Creating a different name → refuse.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"other","overlay":{"agent":"job-search-batch","schedule":"0 0 * * *","user_id":"alice"}}`))
	if !res.IsError {
		t.Fatalf("named scope should refuse non-matching name")
	}
}

func TestScheduleDefTool_RetireRoundTrip(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	// Create one.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"retire-test","overlay":{"agent":"job-search-batch","schedule":"0 0 * * *","user_id":"alice"}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	defID := out["def_id"].(string)

	// Retire it.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":true}`))
	if res.IsError {
		t.Fatalf("retire: %s", res.Text)
	}
	out = decodeResult(t, res.Text)
	if out["retired"].(bool) != true {
		t.Errorf("retired = %v, want true", out["retired"])
	}

	// Un-retire.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":false}`))
	if res.IsError {
		t.Fatalf("un-retire: %s", res.Text)
	}
	out = decodeResult(t, res.Text)
	if out["retired"].(bool) != false {
		t.Errorf("retired = %v, want false", out["retired"])
	}
}

func TestScheduleDefTool_ListReturnsVersions(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	// Create 3 versions via create+fork+fork.
	for i := 0; i < 3; i++ {
		op := `create`
		if i > 0 {
			op = `fork`
		}
		_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"`+op+`","name":"multi","overlay":{"agent":"job-search-batch","schedule":"0 0 * * *","user_id":"alice"}}`))
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"multi"}`))
	if res.IsError {
		t.Fatalf("list: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	versions := out["versions"].([]any)
	if len(versions) != 3 {
		t.Errorf("got %d versions, want 3", len(versions))
	}
}

// TestMergedScheduleDef_DriftDetection_VsLookupSubstrateScheduleDef
// pins json-tag parity between mergedScheduleDef (this package — the
// substrate-write shape) and lookup.SubstrateScheduleDef (the
// substrate-read adapter). A field added to either side without the
// matching addition fails this test instead of silently dropping at
// the JSON boundary.
//
// Two fields are intentionally NOT mirrored: `user_credentials` only
// exists in mergedScheduleDef (forks store credentials in the
// substrate row; the lookup path returns a config.ScheduledRun which
// does NOT have credentials — credentials flow into RunInput at the
// scheduler's sweeper-fire seam, not through the config layer).
// Listed in the exempt set with explicit rationale.
func TestMergedScheduleDef_DriftDetection_VsLookupSubstrateScheduleDef(t *testing.T) {
	exempt := map[string]bool{
		// user_credentials lives only in the substrate-write shape;
		// the read side (config.ScheduledRun) has no credentials
		// field. The scheduler's RunInput build step reads it from
		// the SubstrateScheduleDef adapter at sweeper-fire time —
		// but that's a future PR's runtime path, not a config-layer
		// concern. Keeping credentials out of config.ScheduledRun
		// also prevents accidental yaml-side credential exposure.
		"user_credentials": true,
	}

	mergedTags := scheduleJsonTagsOf(reflect.TypeOf(mergedScheduleDef{}))
	substrateTags := scheduleJsonTagsOf(reflect.TypeOf(lookup.SubstrateScheduleDef{}))

	for tag := range mergedTags {
		if exempt[tag] {
			continue
		}
		if !substrateTags[tag] {
			t.Errorf("mergedScheduleDef has json tag %q but lookup.SubstrateScheduleDef does not — either mirror it on the lookup side OR add %q to the exempt set with a justifying comment",
				tag, tag)
		}
	}
	for tag := range substrateTags {
		if !mergedTags[tag] {
			t.Errorf("lookup.SubstrateScheduleDef has json tag %q but mergedScheduleDef does not — substrate-write is the source-of-truth shape; remove from SubstrateScheduleDef OR add the field to mergedScheduleDef in this package",
				tag)
		}
	}
}

// scheduleJsonTagsOf mirrors jsonTagsOfFields in agentdef_test.go;
// duplicated here because that helper is package-private and reusing
// it would require widening test-helper visibility.
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
