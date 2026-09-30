package snapshot

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// plantedSchedule is one def a test writes into a source store.
type plantedSchedule struct {
	row    store.ScheduleDefRow
	body   map[string]any
	active bool
	fires  int       // real fires recorded before capture
	next   time.Time // next_run_at after those fires; zero = no run state at all
}

// plantSchedules writes each def (in order, so a parent precedes its fork),
// its active pointer, and a run state that has spent `fires` fires.
func plantSchedules(t *testing.T, s store.Store, defs ...plantedSchedule) {
	t.Helper()
	ctx := context.Background()
	for _, d := range defs {
		body, err := json.Marshal(d.body)
		if err != nil {
			t.Fatal(err)
		}
		row := d.row
		row.Definition = body
		if _, err := s.SnapshotRestoreScheduleDef(ctx, row); err != nil {
			t.Fatalf("plant def %s: %v", row.DefID, err)
		}
		if d.active {
			if _, err := s.SnapshotRestoreScheduleDefActive(ctx, store.ScheduleDefActiveEntry{
				TenantID: row.TenantID, Name: row.Name, DefID: row.DefID, PromotedAt: row.CreatedAt, PromotedByAgentID: "planter",
			}); err != nil {
				t.Fatalf("plant active %s: %v", row.DefID, err)
			}
		}
		if d.next.IsZero() {
			continue
		}
		if err := s.ScheduleRunStateSeed(ctx, row.DefID, d.next); err != nil {
			t.Fatalf("seed %s: %v", row.DefID, err)
		}
		for i := 0; i < d.fires; i++ {
			if err := s.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
				DefID: row.DefID, LastRunID: "run_" + row.DefID, LastStatus: "completed", LastError: "",
				LastRunAt: row.CreatedAt.Add(time.Duration(i+1) * time.Minute), NextRunAt: d.next, CountAsFire: true,
			}); err != nil {
				t.Fatalf("record %s: %v", row.DefID, err)
			}
		}
	}
}

func schedBase() time.Time { return time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC) }

// readBody decodes a stored def's body.
func readBody(t *testing.T, s store.Store, defID string) map[string]any {
	t.Helper()
	row, err := s.ScheduleDefGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("ScheduleDefGet(%s): %v", defID, err)
	}
	var m map[string]any
	if err := json.Unmarshal(row.Definition, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func runState(t *testing.T, s store.Store, defID string) store.ScheduleRunStateRow {
	t.Helper()
	st, err := s.ScheduleRunStateGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("ScheduleRunStateGet(%s): %v", defID, err)
	}
	return st
}

func hasWarning(res RestoreResult, parts ...string) bool {
	for _, w := range res.Warnings {
		all := true
		for _, p := range parts {
			if !strings.Contains(w, p) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// Every def comes back under its own tenant with every column, its run state
// with its fire count and pause, and its active pointer — under two tenants
// and the operator layer, with a tenant fork of an operator-layer parent.
func TestScheduleDefs_RoundTripKeepsDefsStateAndPointers(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()
	b := schedBase()
	plantSchedules(t, src,
		plantedSchedule{row: store.ScheduleDefRow{DefID: "sd_tpl", Name: "digest", Version: 1, CreatedAt: b,
			Description: "template", CreatedByAgentID: "op", BootstrappedFromStatic: true},
			body: map[string]any{"agent": "a", "schedule": "0 6 * * *", "tenant_id": "", "enabled": true}, active: true, next: b.Add(time.Hour)},
		plantedSchedule{row: store.ScheduleDefRow{DefID: "sd_acme", TenantID: "acme", Name: "digest", Version: 1, ParentDefID: "sd_tpl",
			CreatedAt: b.Add(time.Minute), CreatedByRunID: "run_author"},
			body: map[string]any{"agent": "a", "schedule": "0 7 * * *", "tenant_id": "acme", "max_fires": 5,
				"operator_key_restricted": true, "isolated": true, "user_credentials_from_env": map[string]string{"jobs": "LOOMCYCLE_JOBS"}},
			active: true, fires: 3, next: b.Add(2 * time.Hour)},
		plantedSchedule{row: store.ScheduleDefRow{DefID: "sd_beta_done", TenantID: "beta", Name: "once", Version: 1, CreatedAt: b, Retired: true},
			body: map[string]any{"agent": "a", "schedule": "0 7 * * *", "max_fires": 1}, active: true, fires: 1, next: b.Add(3 * time.Hour)},
		plantedSchedule{row: store.ScheduleDefRow{DefID: "sd_beta_staged", TenantID: "beta", Name: "once", Version: 2, CreatedAt: b.Add(time.Minute), ParentDefID: "sd_beta_done"},
			body: map[string]any{"agent": "a", "schedule": "0 8 * * *"}},
	)
	pausedUntil := b.Add(48 * time.Hour)
	if err := src.ScheduleRunStatePause(ctx, "sd_acme", pausedUntil); err != nil {
		t.Fatal(err)
	}

	res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{})
	if res.ScheduleDefsRestored != 4 || res.ScheduleDefActiveRestored != 3 || res.ScheduleRunStateRestored != 3 || res.DefsDisabledForCredentials != 0 {
		t.Fatalf("restore counts = defs %d active %d state %d disabled %d; want 4, 3, 3, 0",
			res.ScheduleDefsRestored, res.ScheduleDefActiveRestored, res.ScheduleRunStateRestored, res.DefsDisabledForCredentials)
	}
	counts := res.Counts()
	for key, want := range map[string]int{"schedule_defs": 4, "schedule_def_active": 3, "schedule_run_state": 3, "defs_disabled_for_credentials": 0} {
		if got, ok := counts[key]; !ok || got != want {
			t.Errorf("restored map[%q] = %d (present %v), want %d", key, got, ok, want)
		}
	}

	srcDefs, _ := src.SnapshotReadScheduleDefs(ctx)
	dstDefs, _ := dst.SnapshotReadScheduleDefs(ctx)
	if len(dstDefs) != len(srcDefs) {
		t.Fatalf("dst has %d defs, src %d", len(dstDefs), len(srcDefs))
	}
	for i := range srcDefs {
		sd, dd := srcDefs[i], dstDefs[i]
		if !sd.CreatedAt.Equal(dd.CreatedAt) {
			t.Errorf("%s created_at %v, want %v", sd.DefID, dd.CreatedAt, sd.CreatedAt)
		}
		sd.CreatedAt, dd.CreatedAt = time.Time{}, time.Time{}
		var sb, db map[string]any
		_ = json.Unmarshal(sd.Definition, &sb)
		_ = json.Unmarshal(dd.Definition, &db)
		sd.Definition, dd.Definition = nil, nil
		if !reflect.DeepEqual(sd, dd) {
			t.Errorf("def row differs:\n src %+v\n dst %+v", sd, dd)
		}
		if !reflect.DeepEqual(sb, db) {
			t.Errorf("%s body differs:\n src %v\n dst %v", sd.DefID, sb, db)
		}
	}
	srcState, _ := src.SnapshotReadScheduleRunState(ctx)
	dstState, _ := dst.SnapshotReadScheduleRunState(ctx)
	if len(dstState) != 3 || len(srcState) != 3 {
		t.Fatalf("run states: src %d dst %d, want 3 each", len(srcState), len(dstState))
	}
	for i := range srcState {
		s, d := srcState[i], dstState[i]
		if s.DefID != d.DefID || s.FireCount != d.FireCount || s.LastRunID != d.LastRunID || s.LastStatus != d.LastStatus ||
			!s.NextRunAt.Equal(d.NextRunAt) || !s.LastRunAt.Equal(d.LastRunAt) || !s.PausedUntil.Equal(d.PausedUntil) {
			t.Errorf("run state differs:\n src %+v\n dst %+v", s, d)
		}
	}
	if st := runState(t, dst, "sd_acme"); st.FireCount != 3 || !st.PausedUntil.Equal(pausedUntil) {
		t.Errorf("acme fire_count %d paused %v, want 3 and %v", st.FireCount, st.PausedUntil, pausedUntil)
	}
	if row, err := dst.ScheduleDefGetActive(ctx, "acme", "digest"); err != nil || row.DefID != "sd_acme" {
		t.Errorf("acme active = %v (err %v), want sd_acme", row.DefID, err)
	}
	actives, _ := dst.SnapshotReadScheduleDefActive(ctx)
	for _, a := range actives {
		if a.PromotedByAgentID != "planter" {
			t.Errorf("active %s/%s lost its promoter: %+v", a.TenantID, a.Name, a)
		}
	}
	// An exhausted schedule stays retired.
	if row, _ := dst.ScheduleDefGet(ctx, "sd_beta_done"); !row.Retired {
		t.Error("the exhausted schedule came back unretired")
	}
}

// A re-restore of the same snapshot writes nothing and says nothing new — in
// particular it does not reset a count the target has moved on from.
func TestScheduleDefs_ReRestoreWritesNothingAndKeepsTheCount(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	b := schedBase()
	plantSchedules(t, src, plantedSchedule{
		row:  store.ScheduleDefRow{DefID: "sd_1", Name: "digest", Version: 1, CreatedAt: b},
		body: map[string]any{"agent": "a", "schedule": "0 6 * * *", "max_fires": 5}, active: true, fires: 3, next: b,
	})
	raw := mustCapture(t, src)
	mustRestore(t, dst, raw, RestoreOptions{})
	// The target fires once more after the restore.
	if err := dst.ScheduleRunStateRecordResult(context.Background(), store.ScheduleRunResult{
		DefID: "sd_1", LastStatus: "completed", LastRunAt: b, NextRunAt: b.Add(time.Hour), CountAsFire: true,
	}); err != nil {
		t.Fatal(err)
	}
	res := mustRestore(t, dst, raw, RestoreOptions{})
	if res.ScheduleDefsRestored != 0 || res.ScheduleDefActiveRestored != 0 || res.ScheduleRunStateRestored != 0 || len(res.Warnings) != 0 {
		t.Errorf("re-restore = %+v; want nothing written and no warnings", res)
	}
	if st := runState(t, dst, "sd_1"); st.FireCount != 4 {
		t.Errorf("fire_count after re-restore = %d, want 4 (the target's count stands)", st.FireCount)
	}
}

// V2: a literal user_credentials value never reaches the envelope; its key is
// listed, the carried body is disabled, and the def restores disabled with the
// marker a fork needs — counted and named in a warning.
func TestScheduleDefs_LiteralCredentialsStrippedAndDefRestoredDisabled(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	const literal = "dp2mark-literal-bearer"
	plantSchedules(t, src, plantedSchedule{
		row: store.ScheduleDefRow{DefID: "sd_cred", TenantID: "acme", Name: "digest", Version: 1, CreatedAt: schedBase()},
		body: map[string]any{"agent": "a", "schedule": "0 6 * * *", "enabled": true, "max_fires": 5,
			"operator_key_restricted": true, "isolated": true,
			"user_credentials": map[string]string{"slack": literal, "jobs": literal + "-2"}},
		active: true, fires: 2, next: schedBase(),
	})

	raw := mustCapture(t, src)
	if strings.Contains(string(raw), literal) {
		t.Fatal("the envelope carries a literal user_credentials value")
	}
	var env struct {
		Sections struct {
			ScheduleDefs ScheduleDefsSection `json:"schedule_defs"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Sections.ScheduleDefs.Entries) != 1 {
		t.Fatalf("captured %d schedule defs, want 1", len(env.Sections.ScheduleDefs.Entries))
	}
	e := env.Sections.ScheduleDefs.Entries[0]
	if !reflect.DeepEqual(e.StrippedCredentials, []string{"jobs", "slack"}) {
		t.Errorf("stripped_credentials = %v, want [jobs slack]", e.StrippedCredentials)
	}
	var carried map[string]any
	_ = json.Unmarshal(e.Definition, &carried)
	if carried["enabled"] != false {
		t.Errorf("carried body enabled = %v, want false", carried["enabled"])
	}
	if _, ok := carried["user_credentials"]; ok {
		t.Error("the carried body still has a user_credentials field with every value stripped")
	}
	// Confinement bits and the execution tenant travel verbatim.
	if carried["operator_key_restricted"] != true || carried["isolated"] != true {
		t.Errorf("confinement bits lost in the carried body: %v", carried)
	}

	res := mustRestore(t, dst, raw, RestoreOptions{})
	if res.DefsDisabledForCredentials != 1 || !hasWarning(res, "acme/digest", "DISABLED", "jobs, slack") {
		t.Errorf("disabled count %d warnings %v; want 1 and a warning naming the def and keys", res.DefsDisabledForCredentials, res.Warnings)
	}
	stored := readBody(t, dst, "sd_cred")
	if stored["enabled"] != false {
		t.Errorf("stored enabled = %v, want false", stored["enabled"])
	}
	marker, _ := stored["capture_disabled"].(map[string]any)
	if !reflect.DeepEqual(marker["stripped_credentials"], []any{"jobs", "slack"}) {
		t.Errorf("stored marker = %v, want stripped_credentials [jobs slack]", stored["capture_disabled"])
	}
	if st := runState(t, dst, "sd_cred"); st.FireCount != 2 {
		t.Errorf("fire_count = %d, want 2 (carried while disabled)", st.FireCount)
	}
	// A disabled schedule is not among the ones that will fire.
	if hasWarning(res, "enabled schedule(s) restored ACTIVE") {
		t.Errorf("the copy-not-lease warning counted a disabled schedule: %v", res.Warnings)
	}
}

// V2: a hand-edited envelope that flips enabled back to true while keeping
// stripped_credentials still restores disabled, with the marker.
func TestScheduleDefs_RestoreForcesDisabledOverAHandEditedEnvelope(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantSchedules(t, src, plantedSchedule{
		row:    store.ScheduleDefRow{DefID: "sd_cred", Name: "digest", Version: 1, CreatedAt: schedBase()},
		body:   map[string]any{"agent": "a", "schedule": "0 6 * * *", "user_credentials": map[string]string{"slack": "literal"}},
		active: true, next: schedBase(),
	})
	raw := editSection(t, mustCapture(t, src), "schedule_defs", func(sec map[string]any) {
		e := sec["entries"].([]any)[0].(map[string]any)
		body := e["definition"].(map[string]any)
		body["enabled"] = true
		delete(body, "capture_disabled")
	})
	res := mustRestore(t, dst, raw, RestoreOptions{})
	stored := readBody(t, dst, "sd_cred")
	if stored["enabled"] != false || stored["capture_disabled"] == nil {
		t.Errorf("stored body = %v; want enabled:false and the marker despite the edit", stored)
	}
	if res.DefsDisabledForCredentials != 1 {
		t.Errorf("defs_disabled_for_credentials = %d, want 1", res.DefsDisabledForCredentials)
	}
}

// A second hop (A→B→C) of a def that was never re-enabled lists its keys
// again, so it restores disabled on C too.
func TestScheduleDefs_SecondHopKeepsTheDefDisabled(t *testing.T) {
	a, aClose := newTestStore(t)
	defer aClose()
	bst, bClose := newTestStore(t)
	defer bClose()
	c, cClose := newTestStore(t)
	defer cClose()
	plantSchedules(t, a, plantedSchedule{
		row:    store.ScheduleDefRow{DefID: "sd_cred", Name: "digest", Version: 1, CreatedAt: schedBase()},
		body:   map[string]any{"agent": "a", "schedule": "0 6 * * *", "user_credentials": map[string]string{"slack": "literal"}},
		active: true, fires: 1, next: schedBase(),
	})
	mustRestore(t, bst, mustCapture(t, a), RestoreOptions{})
	res := mustRestore(t, c, mustCapture(t, bst), RestoreOptions{})
	if res.DefsDisabledForCredentials != 1 || !hasWarning(res, "digest", "DISABLED", "slack") {
		t.Errorf("second hop: disabled %d warnings %v; want the def restored disabled again", res.DefsDisabledForCredentials, res.Warnings)
	}
	if stored := readBody(t, c, "sd_cred"); stored["enabled"] != false || stored["capture_disabled"] == nil {
		t.Errorf("second hop body = %v, want disabled and marked", stored)
	}
	if st := runState(t, c, "sd_cred"); st.FireCount != 1 {
		t.Errorf("second hop fire_count = %d, want 1", st.FireCount)
	}
}

// A credential REFERENCE is authored text, not a value: it travels as written,
// is never expanded, and does not disable the def.
func TestScheduleDefs_CredentialReferencesTravelUnexpanded(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	const resolved = "dp2mark-resolved-env-value"
	t.Setenv("LOOMCYCLE_DP2_SCHED_KEY", resolved)
	refs := map[string]string{
		"jobs":  "$cred:jobs-api",
		"slack": "Bearer ${LOOMCYCLE_DP2_SCHED_KEY}",
		"gh":    "$ghapp:ci-bot",
	}
	plantSchedules(t, src, plantedSchedule{
		row:    store.ScheduleDefRow{DefID: "sd_ref", Name: "digest", Version: 1, CreatedAt: schedBase()},
		body:   map[string]any{"agent": "a", "schedule": "0 6 * * *", "user_credentials": refs},
		active: true, next: schedBase(),
	})
	raw := mustCapture(t, src)
	if strings.Contains(string(raw), resolved) {
		t.Fatal("capture expanded an env reference into the envelope")
	}
	if strings.Contains(string(raw), "stripped_credentials") {
		t.Error("a reference was treated as a literal and stripped")
	}
	res := mustRestore(t, dst, raw, RestoreOptions{})
	stored := readBody(t, dst, "sd_ref")
	got, _ := json.Marshal(stored["user_credentials"])
	want, _ := json.Marshal(refs)
	if string(got) != string(want) {
		t.Errorf("references = %s, want %s exactly as written", got, want)
	}
	if _, ok := stored["enabled"]; ok || stored["capture_disabled"] != nil || res.DefsDisabledForCredentials != 0 {
		t.Errorf("a def holding only references was disabled: %v", stored)
	}
}

// The live row stands: an existing def_id keeps its own run state and count,
// an existing pointer keeps pointing where it did, and a different def on the
// same (tenant, name, version) — this instance's own yaml bootstrap — keeps
// its place with a warning, taking the snapshot's run state with it.
func TestScheduleDefs_LiveRowStands(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	b := schedBase()
	plantSchedules(t, src,
		plantedSchedule{row: store.ScheduleDefRow{DefID: "sd_shared", Name: "digest", Version: 1, CreatedAt: b},
			body: map[string]any{"agent": "a", "schedule": "0 6 * * *"}, active: true, fires: 4, next: b},
		plantedSchedule{row: store.ScheduleDefRow{DefID: "sd_src_boot", Name: "nightly", Version: 1, CreatedAt: b, BootstrappedFromStatic: true},
			body: map[string]any{"agent": "a", "schedule": "0 3 * * *"}, active: true, fires: 2, next: b},
	)
	plantSchedules(t, dst,
		plantedSchedule{row: store.ScheduleDefRow{DefID: "sd_shared", Name: "digest", Version: 1, CreatedAt: b},
			body: map[string]any{"agent": "a", "schedule": "0 6 * * *"}, active: true, fires: 1, next: b.Add(time.Hour)},
		plantedSchedule{row: store.ScheduleDefRow{DefID: "sd_dst_boot", Name: "nightly", Version: 1, CreatedAt: b, BootstrappedFromStatic: true},
			body: map[string]any{"agent": "b", "schedule": "0 4 * * *"}, active: true, next: b.Add(time.Hour)},
	)

	res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{})
	if res.ScheduleDefsRestored != 0 || res.ScheduleDefActiveRestored != 0 || res.ScheduleRunStateRestored != 0 {
		t.Errorf("counts = defs %d active %d state %d, want 0 (every live row stands)",
			res.ScheduleDefsRestored, res.ScheduleDefActiveRestored, res.ScheduleRunStateRestored)
	}
	if st := runState(t, dst, "sd_shared"); st.FireCount != 1 {
		t.Errorf("live fire_count = %d, want 1", st.FireCount)
	}
	if row, err := dst.ScheduleDefGetActive(context.Background(), "", "nightly"); err != nil || row.DefID != "sd_dst_boot" {
		t.Errorf("nightly active = %s (err %v), want the target's own sd_dst_boot", row.DefID, err)
	}
	if _, err := dst.ScheduleDefGet(context.Background(), "sd_src_boot"); err == nil {
		t.Error("the snapshot's bootstrapped def was written beside the target's own")
	}
	if !hasWarning(res, "schedule_def nightly v1 (def sd_src_boot)", "live definition stands") {
		t.Errorf("no warning named the colliding bootstrapped def: %v", res.Warnings)
	}
}

// OQ2: restore says once how many enabled schedules it made live — not the
// disabled, retired or unpointed ones — and a re-restore says nothing.
func TestScheduleDefs_WarnsOnceWithTheEnabledCount(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	b := schedBase()
	sched := func(id, tenant, name string, body map[string]any, retired, active bool) plantedSchedule {
		return plantedSchedule{row: store.ScheduleDefRow{DefID: id, TenantID: tenant, Name: name, Version: 1, CreatedAt: b, Retired: retired},
			body: body, active: active, next: b}
	}
	run := func(extra map[string]any) map[string]any {
		m := map[string]any{"agent": "a", "schedule": "0 6 * * *"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	plantSchedules(t, src,
		sched("sd_on_default", "", "a", run(nil), false, true),
		sched("sd_on_explicit", "acme", "b", run(map[string]any{"enabled": true}), false, true),
		sched("sd_off", "acme", "c", run(map[string]any{"enabled": false}), false, true),
		sched("sd_retired", "acme", "d", run(nil), true, true),
		sched("sd_unpointed", "acme", "e", run(nil), false, false),
		sched("sd_stripped", "beta", "f", run(map[string]any{"user_credentials": map[string]string{"k": "v"}}), false, true),
	)
	raw := mustCapture(t, src)
	res := mustRestore(t, dst, raw, RestoreOptions{})
	n := 0
	for _, w := range res.Warnings {
		if strings.Contains(w, "restored ACTIVE") {
			n++
			if !strings.Contains(w, "2 enabled schedule(s)") || !strings.Contains(w, "max_fires") {
				t.Errorf("copy-not-lease warning = %q, want the count 2 and the max_fires overrun", w)
			}
		}
	}
	if n != 1 {
		t.Errorf("copy-not-lease warning emitted %d times, want once: %v", n, res.Warnings)
	}
	if again := mustRestore(t, dst, raw, RestoreOptions{}); hasWarning(again, "restored ACTIVE") {
		t.Errorf("a re-restore that made nothing live warned: %v", again.Warnings)
	}
}

// An old snapshot without the schedule sections restores exactly as before:
// no schedule written, no warning.
func TestScheduleDefs_WithoutSectionRestoresAsBefore(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantSchedules(t, src, plantedSchedule{
		row:  store.ScheduleDefRow{DefID: "sd_1", Name: "digest", Version: 1, CreatedAt: schedBase()},
		body: map[string]any{"agent": "a", "schedule": "0 6 * * *"}, active: true, fires: 1, next: schedBase(),
	})
	raw := withoutSection(t, withoutSection(t, mustCapture(t, src), "schedule_defs"), "schedule_def_active")
	res := mustRestore(t, dst, raw, RestoreOptions{})
	if res.ScheduleDefsRestored != 0 || res.ScheduleDefActiveRestored != 0 || res.ScheduleRunStateRestored != 0 || len(res.Warnings) != 0 {
		t.Errorf("old-format restore = %+v; want no schedules and no warnings", res)
	}
	if defs, _ := dst.SnapshotReadScheduleDefs(context.Background()); len(defs) != 0 {
		t.Errorf("an old-format restore wrote schedule defs %+v", defs)
	}
}
