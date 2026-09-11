package scheduler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A `delivery: channel` schedule publishes a tick and starts NO run — the
// whole point of the mode. The RunOnce count is the assertion that matters:
// the old way to reach a channel on a cron was to burn an agent run.
func TestScheduler_ChannelDeliveryPublishesWithoutARun(t *testing.T) {
	enabled := true
	def := scheduleDef{
		Delivery: "channel",
		Channel:  "wave-in",
		Schedule: "0 * * * *",
		Enabled:  &enabled,
		Metadata: map[string]any{"batch": "nightly"},
	}
	sched, fr, _, defID, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	// Declare the channel at global scope, the way the server's resolver does.
	sched.SetChannelScope(func(context.Context, string) (DeclaredChannel, bool) { return DeclaredChannel{Scope: "global"}, true })

	fireT(t, sched)

	if got := len(fr.Calls()); got != 0 {
		t.Fatalf("RunOnce calls = %d, want 0 — a channel tick starts no run", got)
	}
	msgs, _, err := st.ChannelSubscribe(context.Background(), "", "wave-in", store.MemoryScopeGlobal, "", "", 10)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("published %d messages, want 1", len(msgs))
	}
	var got map[string]any
	if err := json.Unmarshal(msgs[0].Payload, &got); err != nil {
		t.Fatalf("decode tick: %v", err)
	}
	if got["schedule_name"] != "sched-test" {
		t.Errorf("schedule_name = %v, want sched-test", got["schedule_name"])
	}
	if got["delivery"] != "channel" {
		t.Errorf("delivery = %v, want channel", got["delivery"])
	}
	if _, ok := got["fired_at"].(string); !ok {
		t.Errorf("tick carries no fired_at: %s", msgs[0].Payload)
	}
	payload, _ := got["payload"].(map[string]any)
	if payload["batch"] != "nightly" {
		t.Errorf("metadata did not reach the tick payload: %s", msgs[0].Payload)
	}

	// A tick is a fire: the state advances and records like any other, or the
	// row re-presents on the next tick and max_fires never retires it.
	state, err := st.ScheduleRunStateGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.LastStatus != "completed" {
		t.Errorf("last_status = %q, want completed", state.LastStatus)
	}
	if state.NextRunAt.Before(time.Now()) {
		t.Errorf("next_run_at = %v, expected the future", state.NextRunAt)
	}
	if state.FireCount != 1 {
		t.Errorf("fire_count = %d, want 1", state.FireCount)
	}
}

// A tick to a channel nobody declared FAILS the fire rather than silently
// publishing nowhere — and still advances, so a broken schedule reports its
// breakage every cycle instead of spinning the sweeper.
func TestScheduler_ChannelDeliveryUndeclaredChannelIsARecordedFailure(t *testing.T) {
	enabled := true
	def := scheduleDef{
		Delivery: "channel",
		Channel:  "ghost",
		Schedule: "0 * * * *",
		Enabled:  &enabled,
	}
	sched, fr, _, defID, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	sched.SetChannelScope(func(context.Context, string) (DeclaredChannel, bool) { return DeclaredChannel{}, false })

	fireT(t, sched)

	if got := len(fr.Calls()); got != 0 {
		t.Fatalf("RunOnce calls = %d, want 0", got)
	}
	state, err := st.ScheduleRunStateGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.LastStatus != "failed" {
		t.Errorf("last_status = %q, want failed", state.LastStatus)
	}
	if state.LastError == "" {
		t.Errorf("a failed tick recorded no error")
	}
	if state.NextRunAt.Before(time.Now()) {
		t.Errorf("a failed tick did not advance next_run_at: %v", state.NextRunAt)
	}
}

// max_fires bounds a channel tick exactly as it bounds a run — a one-shot
// cadence signal is a real use (fire once at T, then retire).
func TestScheduler_ChannelDeliveryRetiresAtMaxFires(t *testing.T) {
	enabled := true
	def := scheduleDef{
		Delivery: "channel",
		Channel:  "wave-in",
		Schedule: "* * * * *",
		Enabled:  &enabled,
		MaxFires: 1,
	}
	sched, _, _, defID, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	sched.SetChannelScope(func(context.Context, string) (DeclaredChannel, bool) { return DeclaredChannel{Scope: "global"}, true })

	fireT(t, sched)

	row, err := st.ScheduleDefGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("def get: %v", err)
	}
	if !row.Retired {
		t.Errorf("max_fires=1 did not retire the def after its first tick")
	}
}

// A disabled channel schedule publishes nothing — the kill switch has to work
// on both delivery modes, and the branch order is what decides that.
func TestScheduler_ChannelDeliveryRespectsDisabled(t *testing.T) {
	disabled := false
	def := scheduleDef{
		Delivery: "channel",
		Channel:  "wave-in",
		Schedule: "0 * * * *",
		Enabled:  &disabled,
	}
	sched, _, _, _, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	sched.SetChannelScope(func(context.Context, string) (DeclaredChannel, bool) { return DeclaredChannel{Scope: "global"}, true })

	fireT(t, sched)

	msgs, _, err := st.ChannelSubscribe(context.Background(), "", "wave-in", store.MemoryScopeGlobal, "", "", 10)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("a disabled schedule published %d ticks, want 0", len(msgs))
	}
}

// A cadence writer must honour the channel's declared retention. A tick every
// minute with neither a TTL nor a bounded-queue cap is half a million rows a
// year on a channel whose operator DID set limits — they just never reached
// the writer.
func TestScheduler_ChannelDeliveryHonoursDeclaredRetention(t *testing.T) {
	enabled := true
	def := scheduleDef{
		Delivery: "channel",
		Channel:  "wave-in",
		Schedule: "0 * * * *",
		Enabled:  &enabled,
	}
	sched, _, _, _, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	sched.SetChannelScope(func(context.Context, string) (DeclaredChannel, bool) {
		return DeclaredChannel{Scope: "global", DefaultTTL: 3600, MaxMessages: 5}, true
	})

	fireT(t, sched)

	msgs, _, err := st.ChannelSubscribe(context.Background(), "", "wave-in", store.MemoryScopeGlobal, "", "", 10)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("published %d, want 1", len(msgs))
	}
	if msgs[0].ExpiresAt.IsZero() {
		t.Errorf("the tick carries no expiry — the channel's default_ttl never reached the writer")
	}
}

// An unknown delivery is refused at decode rather than falling through to the
// run path: firing an agent because of a value nobody could interpret is the
// loudest possible wrong answer.
func TestScheduler_UnknownDeliveryDoesNotFallThroughToARun(t *testing.T) {
	enabled := true
	def := scheduleDef{
		Delivery: "smoke-signal",
		Agent:    "researcher",
		Schedule: "0 * * * *",
		Enabled:  &enabled,
	}
	sched, fr, _, defID, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))

	fireT(t, sched)

	if got := len(fr.Calls()); got != 0 {
		t.Errorf("an unknown delivery fired the agent %d time(s)", got)
	}
	state, err := st.ScheduleRunStateGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.LastStatus != "decode_def" {
		t.Errorf("last_status = %q, want decode_def", state.LastStatus)
	}
}

// A tick to a HELD channel is stored and NOT delivered. Found by running the
// two features together: the tick wrote straight through the store, bypassing
// the publisher where the hold is enforced, so a cron walked past a breakpoint.
func TestScheduler_ChannelDeliveryHonoursAHold(t *testing.T) {
	enabled := true
	def := scheduleDef{
		Delivery: "channel",
		Channel:  "wave-in",
		Schedule: "0 * * * *",
		Enabled:  &enabled,
	}
	sched, _, _, _, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	sched.SetChannelScope(func(context.Context, string) (DeclaredChannel, bool) {
		return DeclaredChannel{Scope: "global", Hold: true}, true
	})

	fireT(t, sched)

	msgs, _, err := st.ChannelSubscribe(context.Background(), "", "wave-in", store.MemoryScopeGlobal, "", "", 10)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("a held channel delivered the tick: %d messages", len(msgs))
	}
	// Stored, not lost: releasing hands it over.
	released, held, err := st.ChannelRelease(context.Background(), "", "wave-in", store.MemoryScopeGlobal, "", 1)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(released) != 1 || held != 0 {
		t.Fatalf("release gave %d leaving %d, want 1 leaving 0 — the tick was not held, it was lost", len(released), held)
	}
	msgs, _, _ = st.ChannelSubscribe(context.Background(), "", "wave-in", store.MemoryScopeGlobal, "", "", 10)
	if len(msgs) != 1 {
		t.Errorf("after release, %d delivered, want 1", len(msgs))
	}
}
