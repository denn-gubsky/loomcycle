package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// A schedule restored from a snapshot without its literal credentials comes
// back disabled, with a capture_disabled marker listing the stripped keys and
// the fire count it had spent. These tests pin how a fork re-enables it: only
// by re-supplying every listed key, and never with its max_fires budget back.

// plantCaptureDisabled writes a def in the shape a snapshot restore leaves it:
// enabled:false, the marker, no literal values for the stripped keys, active,
// with a run state that has already spent fireCount fires.
func plantCaptureDisabled(t *testing.T, st store.Store, name string, stripped []string, fireCount int) string {
	t.Helper()
	ctx := context.Background()
	body, err := json.Marshal(map[string]any{
		"agent": "job-search-batch", "schedule": "0 6 * * *", "max_fires": 5, "enabled": false,
		"user_credentials_from_env": map[string]string{"telegram": "LOOMCYCLE_TG"},
		"capture_disabled":          map[string]any{"stripped_credentials": stripped},
	})
	if err != nil {
		t.Fatal(err)
	}
	defID := "sd_restored_" + name
	if _, err := st.SnapshotRestoreScheduleDef(ctx, store.ScheduleDefRow{
		DefID: defID, Name: name, Version: 1, Definition: body, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("plant def: %v", err)
	}
	if _, err := st.SnapshotRestoreScheduleDefActive(ctx, store.ScheduleDefActiveEntry{Name: name, DefID: defID}); err != nil {
		t.Fatalf("plant active: %v", err)
	}
	if _, err := st.SnapshotRestoreScheduleRunState(ctx, store.ScheduleRunStateRow{
		DefID: defID, NextRunAt: time.Now().Add(time.Hour), FireCount: fireCount,
	}); err != nil {
		t.Fatalf("plant run state: %v", err)
	}
	return defID
}

// forkResult runs a fork and returns the new def's decoded body and fire count.
func forkResult(t *testing.T, tool *ScheduleDef, ctx context.Context, input string) (string, mergedScheduleDef, int) {
	t.Helper()
	res, _ := tool.Execute(ctx, json.RawMessage(input))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	defID, _ := decodeResult(t, res.Text)["def_id"].(string)
	row, err := tool.Store.ScheduleDefGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("get fork %s: %v", defID, err)
	}
	var def mergedScheduleDef
	if err := json.Unmarshal(row.Definition, &def); err != nil {
		t.Fatalf("decode fork body: %v", err)
	}
	st, err := tool.Store.ScheduleRunStateGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("fork %s has no run state: %v", defID, err)
	}
	return defID, def, st.FireCount
}

func enabledOf(d mergedScheduleDef) bool { return d.Enabled == nil || *d.Enabled }

// V2b: a fork that re-supplies every stripped key and sets enabled:true
// re-enables the schedule — and starts at the parent's count, not zero, so a
// max_fires:5 schedule that fired 3 times has 2 left.
func TestScheduleDefTool_ForkOfCaptureDisabledDefInheritsFireCount(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()
	plantCaptureDisabled(t, tool.Store, "digest", []string{"jobs", "slack"}, 3)
	allowEnvCredential(t, tool.Cfg, "LOOMCYCLE_SLACK")

	_, def, count := forkResult(t, tool, ctx, `{"op":"fork","name":"digest","overlay":{"enabled":true,
		"user_credentials":{"jobs":"j-new"},"user_credentials_from_env":{"slack":"LOOMCYCLE_SLACK"}}}`)
	if def.CaptureDisabled != nil {
		t.Errorf("marker = %+v, want cleared (every stripped key re-supplied)", def.CaptureDisabled)
	}
	if !enabledOf(def) {
		t.Error("fork is disabled; with every key re-supplied the overlay's enabled:true stands")
	}
	if count != 3 {
		t.Errorf("fork fire_count = %d, want 3 (inherited; re-enabling must not reset max_fires)", count)
	}
}

// A fork that re-supplies only SOME keys stays disabled — whatever its overlay
// says — keeps the marker minus the keys it supplied, and still inherits the
// count; a later fork supplying the rest clears it, again at the same count.
func TestScheduleDefTool_PartialResupplyStaysDisabledAndKeepsMarker(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()
	plantCaptureDisabled(t, tool.Store, "digest", []string{"jobs", "slack"}, 3)

	_, def, count := forkResult(t, tool, ctx, `{"op":"fork","name":"digest","overlay":{"enabled":true,"user_credentials":{"jobs":"j-new"}}}`)
	if def.CaptureDisabled == nil || !reflect.DeepEqual(def.CaptureDisabled.StrippedCredentials, []string{"slack"}) {
		t.Fatalf("marker = %+v, want [slack] (jobs was re-supplied)", def.CaptureDisabled)
	}
	if enabledOf(def) {
		t.Error("a partially re-supplied fork is enabled; it must stay disabled until every key is back")
	}
	if count != 3 {
		t.Errorf("partial fork fire_count = %d, want 3", count)
	}

	_, def, count = forkResult(t, tool, ctx, `{"op":"fork","name":"digest","overlay":{"enabled":true,"user_credentials":{"slack":"s-new"}}}`)
	if def.CaptureDisabled != nil || !enabledOf(def) {
		t.Errorf("second fork: marker %+v enabled %v, want cleared and enabled", def.CaptureDisabled, enabledOf(def))
	}
	if def.UserCredentials["jobs"] != "j-new" || def.UserCredentials["slack"] != "s-new" {
		t.Errorf("credentials = %v, want both re-supplied values", def.UserCredentials)
	}
	if count != 3 {
		t.Errorf("second fork fire_count = %d, want 3", count)
	}
}

// An env source the parent ALREADY had does not count as re-supplying a
// stripped literal: the parent was authored to fire with the literal.
func TestScheduleDefTool_ParentEnvSourceDoesNotClearMarker(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()
	plantCaptureDisabled(t, tool.Store, "digest", []string{"telegram"}, 1)

	_, def, _ := forkResult(t, tool, ctx, `{"op":"fork","name":"digest","overlay":{"enabled":true}}`)
	if def.CaptureDisabled == nil || enabledOf(def) {
		t.Errorf("marker %+v enabled %v; the parent's own env source must not clear the marker", def.CaptureDisabled, enabledOf(def))
	}
}

// The marker is server authority: an overlay can neither set it on create nor
// clear it on fork.
func TestScheduleDefTool_OverlayCannotSetOrClearCaptureDisabled(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"fresh","overlay":{"agent":"job-search-batch","schedule":"0 6 * * *",
		"capture_disabled":{"stripped_credentials":["x"]}}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	row, err := tool.Store.ScheduleDefGetActive(context.Background(), "", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	var created mergedScheduleDef
	if err := json.Unmarshal(row.Definition, &created); err != nil {
		t.Fatal(err)
	}
	if created.CaptureDisabled != nil {
		t.Errorf("create stored an overlay-supplied marker %+v", created.CaptureDisabled)
	}

	plantCaptureDisabled(t, tool.Store, "digest", []string{"jobs"}, 2)
	_, def, _ := forkResult(t, tool, ctx, `{"op":"fork","name":"digest","overlay":{"enabled":true,"capture_disabled":{"stripped_credentials":[]}}}`)
	if def.CaptureDisabled == nil || enabledOf(def) {
		t.Errorf("an overlay cleared the marker: %+v enabled %v", def.CaptureDisabled, enabledOf(def))
	}
}

// An ordinary fork of an unmarked def — a per-user fork of a template —
// starts at zero, as it always has.
func TestScheduleDefTool_OrdinaryForkStartsAtZero(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"plain","overlay":{"agent":"job-search-batch","schedule":"0 6 * * *","max_fires":5}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	parentID, _ := decodeResult(t, res.Text)["def_id"].(string)
	for i := 0; i < 3; i++ {
		if err := tool.Store.ScheduleRunStateRecordResult(context.Background(), store.ScheduleRunResult{
			DefID: parentID, LastStatus: "completed", LastRunAt: time.Now(), NextRunAt: time.Now().Add(time.Hour), CountAsFire: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, _, count := forkResult(t, tool, ctx, `{"op":"fork","name":"plain","overlay":{"user_id":"bob"}}`)
	if count != 0 {
		t.Errorf("ordinary fork fire_count = %d, want 0", count)
	}
}

// A hook edit is a new version too. On a marked def it keeps the marker (it
// supplies no credentials) and must carry the count, or a hook edit would be a
// way to reset the budget.
func TestScheduleDefTool_HookEditOfCaptureDisabledDefKeepsCountAndMarker(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()
	parentID := plantCaptureDisabled(t, tool.Store, "digest", []string{"jobs"}, 4)

	_, def, count := forkResult(t, tool, ctx, `{"op":"add_hook","def_id":"`+parentID+`","hook":{"kind":"memory.set","scope":"user","key":"k"}}`)
	if def.CaptureDisabled == nil || enabledOf(def) {
		t.Errorf("hook edit: marker %+v enabled %v, want kept and disabled", def.CaptureDisabled, enabledOf(def))
	}
	if count != 4 {
		t.Errorf("hook edit fire_count = %d, want 4", count)
	}
}

// V2b end to end: a schedule with a literal credential is captured, restored
// onto another store, and re-enabled there by a fork that re-supplies the key.
// The fork starts at the source's count, fires, and is disabled no more.
func TestScheduleDefTool_SnapshotRestoredDefReEnabledByForkKeepsCount(t *testing.T) {
	src, err := sqlite.Open(filepath.Join(t.TempDir(), "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	bg := context.Background()
	body := json.RawMessage(`{"agent":"job-search-batch","schedule":"0 6 * * *","max_fires":5,"enabled":true,"user_credentials":{"jobs":"literal-jobs-token"}}`)
	if _, err := src.ScheduleDefCreate(bg, store.ScheduleDefRow{DefID: "sd_src", Name: "digest", Definition: body}); err != nil {
		t.Fatal(err)
	}
	if err := src.ScheduleDefSetActive(bg, "", "digest", "sd_src", "t"); err != nil {
		t.Fatal(err)
	}
	if err := src.ScheduleRunStateSeed(bg, "sd_src", time.Now()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := src.ScheduleRunStateRecordResult(bg, store.ScheduleRunResult{
			DefID: "sd_src", LastStatus: "completed", LastRunAt: time.Now(), NextRunAt: time.Now().Add(time.Hour), CountAsFire: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, raw, err := snapshot.Capture(bg, src, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()
	if _, err := snapshot.Restore(bg, tool.Store, raw, snapshot.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	_, def, count := forkResult(t, tool, ctx, `{"op":"fork","name":"digest","overlay":{"enabled":true,"user_credentials":{"jobs":"new-jobs-token"}}}`)
	if def.CaptureDisabled != nil || !enabledOf(def) {
		t.Errorf("re-enabling fork: marker %+v enabled %v, want cleared and enabled", def.CaptureDisabled, enabledOf(def))
	}
	if count != 3 {
		t.Errorf("re-enabling fork fire_count = %d, want 3 — 2 of max_fires 5 left", count)
	}
}

// A fork of a marked def that is NOT promoted still gets its run state with
// the inherited count, so no later seed can start it at zero.
func TestScheduleDefTool_UnpromotedForkOfCaptureDisabledDefStillCarriesCount(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()
	plantCaptureDisabled(t, tool.Store, "digest", []string{"jobs"}, 2)

	forkID, _, count := forkResult(t, tool, ctx, `{"op":"fork","name":"digest","promote":false,"overlay":{"enabled":true,"user_credentials":{"jobs":"j"}}}`)
	if count != 2 {
		t.Errorf("unpromoted fork fire_count = %d, want 2", count)
	}
	// A run-now style seed afterwards keeps the carried count.
	if err := tool.Store.ScheduleRunStateSeed(context.Background(), forkID, time.Now()); err != nil {
		t.Fatal(err)
	}
	st, err := tool.Store.ScheduleRunStateGet(context.Background(), forkID)
	if err != nil || st.FireCount != 2 {
		t.Errorf("after a later seed fire_count = %d (err %v), want 2", st.FireCount, err)
	}
}

// A create on a name whose current def is capture-disabled writes a new
// version of it, so it follows the fork's rules: a create supplying only some
// stripped keys stays disabled with the rest listed, and it inherits the fire
// count — create is not a way around either.
func TestScheduleDefTool_CreateOverCaptureDisabledPartialStaysDisabled(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()
	plantCaptureDisabled(t, tool.Store, "digest", []string{"jobs", "slack"}, 3)

	_, def, count := forkResult(t, tool, ctx, `{"op":"create","name":"digest","overlay":{"agent":"job-search-batch",
		"schedule":"0 6 * * *","max_fires":5,"enabled":true,"user_credentials":{"jobs":"j-new"}}}`)
	if def.CaptureDisabled == nil || !reflect.DeepEqual(def.CaptureDisabled.StrippedCredentials, []string{"slack"}) {
		t.Fatalf("marker = %+v, want [slack] still missing", def.CaptureDisabled)
	}
	if enabledOf(def) {
		t.Error("a create that supplied only some stripped keys is enabled")
	}
	if count != 3 {
		t.Errorf("create fire_count = %d, want 3 (inherited from the superseded def)", count)
	}
}

func TestScheduleDefTool_CreateOverCaptureDisabledFullClearsAndKeepsCount(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()
	plantCaptureDisabled(t, tool.Store, "digest", []string{"jobs", "slack"}, 3)
	allowEnvCredential(t, tool.Cfg, "LOOMCYCLE_SLACK")

	_, def, count := forkResult(t, tool, ctx, `{"op":"create","name":"digest","overlay":{"agent":"job-search-batch",
		"schedule":"0 6 * * *","max_fires":5,"enabled":true,
		"user_credentials":{"jobs":"j-new"},"user_credentials_from_env":{"slack":"LOOMCYCLE_SLACK"}}}`)
	if def.CaptureDisabled != nil || !enabledOf(def) {
		t.Errorf("marker %+v enabled %v; want cleared and enabled (every key supplied)", def.CaptureDisabled, enabledOf(def))
	}
	if count != 3 {
		t.Errorf("create fire_count = %d, want 3 (re-enabling must not reset max_fires)", count)
	}
}

// An ordinary create — no marked def on the name — starts at zero as before.
func TestScheduleDefTool_CreateOverUnmarkedNameStartsAtZero(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()
	_, def, count := forkResult(t, tool, ctx, `{"op":"create","name":"digest","overlay":{"agent":"job-search-batch",
		"schedule":"0 6 * * *","max_fires":5,"enabled":true}}`)
	if def.CaptureDisabled != nil || count != 0 {
		t.Errorf("marker %+v fire_count %d; want no marker and 0", def.CaptureDisabled, count)
	}
}
