package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// These tests drive a team's own schedules on a clock the test owns, and read
// what the store holds: a tick is a message in the team's own channel, and
// none lands once the walk that armed the schedule is over.

// fakeClock is a walkClock the test advances. Every After registers a waiter;
// afters counts them, so a test can tell a timer has gone round its loop.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	afters  int
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.afters++
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock and fires every waiter now due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = kept
}

// waitAfters blocks until the clock has been asked for n waits in all.
func (c *fakeClock) waitAfters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := c.afters
		c.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the clock was asked for %d wait(s) within 5s, want %d", c.afters, n)
}

// tickingTeam declares its own `ticks` channel (scope given) and a schedule
// ticking into it every 10s; its entry Starter reads `ticks`, waiting waitMS.
func tickingTeam(scope string, waitMS int) string {
	return `{"entry":"wave",
	  "local":{"channels":{"ticks":{"scope":"` + scope + `"}},
	    "schedules":{"clock":{"schedule":"@every 10s","channel":"./ticks"}}},
	  "states":[{"state":"wave","handler":{"kind":"starter","source":{"channel":"./ticks","wait_ms":` + strconv.Itoa(waitMS) + `},
	    "fanout":{"agent":"reviewer","per":"message","max":1}}},
	    {"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"wave","to":"done","on":"success"}]}`
}

// newScheduleHarness is a channel harness on a fake clock whose Starters may
// wait up to 30s for a message.
func newScheduleHarness(t *testing.T) (*channelHarness, *fakeClock) {
	t.Helper()
	h := newChannelHarness(t, nil)
	h.srv.cfg().Env.ChannelsLongPollCapMS = 30000
	clock := newFakeClock()
	h.srv.walkClock = clock
	return h, clock
}

// detach starts a detached walk of `name` as `as` and returns its run id.
func (h *channelHarness) detach(as func(context.Context) context.Context, name string) string {
	h.t.Helper()
	code, out := h.teamDef(as, `{"op":"run","name":"`+name+`","input":"go","mode":"detach"}`)
	runID, _ := out["run_id"].(string)
	if code != http.StatusOK || runID == "" {
		h.t.Fatalf("detach %s: HTTP %d %v", name, code, out)
	}
	return runID
}

// waitRun blocks until the run is no longer running and returns its status.
func (h *channelHarness) waitRun(runID string) store.RunStatus {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, err := h.st.GetRun(context.Background(), runID)
		if err == nil && run.Status != store.RunRunning {
			return run.Status
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("run %s did not end within 10s", runID)
	return ""
}

// assertNoTickAfterTheWalk advances the clock well past several ticks and
// checks the channel still holds exactly `want` messages and no timer is armed.
func assertNoTickAfterTheWalk(t *testing.T, h *channelHarness, clock *fakeClock, want int) {
	t.Helper()
	if n := h.srv.walkTimers.Load(); n != 0 {
		t.Errorf("%d schedule timer(s) still armed after the walk ended", n)
	}
	for i := 0; i < 5; i++ {
		clock.Advance(10 * time.Second)
	}
	time.Sleep(20 * time.Millisecond) // give a timer that wrongly survived a moment to publish
	if got := h.stored("_team/clocked/ticks", store.MemoryScopeTenant, ""); len(got) != want {
		t.Errorf("the team's channel holds %d message(s) after the walk ended, want %d — a tick landed after it", len(got), want)
	}
}

// A running walk's schedule ticks into the team's own channel, the walk's
// Starter reads the tick, and once the walk completes no tick follows.
func TestTeamLocalSchedule_TicksIntoTheWalksChannelAndStopsWhenItCompletes(t *testing.T) {
	h, clock := newScheduleHarness(t)
	h.seed("tdf_clocked_1", "clocked", tickingTeam("tenant", 30000))

	runID := h.detach(acmeUser("alice"), "clocked")
	clock.waitAfters(t, 1) // the timer is armed and waiting
	if n := h.srv.walkTimers.Load(); n != 1 {
		t.Fatalf("a walk of a team with one schedule arms %d timer(s), want 1", n)
	}
	clock.Advance(10 * time.Second)

	if st := h.waitRun(runID); st != store.RunCompleted {
		t.Fatalf("the walk ended %s, want completed (its Starter fed by the tick)", st)
	}
	got := h.stored("_team/clocked/ticks", store.MemoryScopeTenant, "")
	if len(got) != 1 {
		t.Fatalf("one tick, stored under the team's channel: %d message(s)", len(got))
	}
	var tick map[string]any
	if err := json.Unmarshal(got[0].Payload, &tick); err != nil {
		t.Fatalf("tick payload: %v", err)
	}
	if tick["schedule_name"] != "./clock" || tick["delivery"] != "channel" || tick["fired_at"] != "2026-10-05T12:00:10Z" {
		t.Errorf("tick = %v, want the schedule tick shape naming ./clock", tick)
	}
	if got[0].PublishedByUserID != "alice" {
		t.Errorf("the tick is published as %q, want the walk's user", got[0].PublishedByUserID)
	}
	assertNoTickAfterTheWalk(t, h, clock, 1)
}

// A walk that fails — here its Starter's wait runs out — stops its schedules.
func TestTeamLocalSchedule_StopsWhenTheWalkFails(t *testing.T) {
	h, clock := newScheduleHarness(t)
	h.seed("tdf_clocked_1", "clocked", tickingTeam("tenant", 50))

	runID := h.detach(acmeUser("alice"), "clocked")
	if st := h.waitRun(runID); st != store.RunFailed {
		t.Fatalf("the walk ended %s, want failed (nothing arrived within its wait)", st)
	}
	assertNoTickAfterTheWalk(t, h, clock, 0)
}

// A cancelled walk stops its schedules.
func TestTeamLocalSchedule_StopsWhenTheWalkIsCancelled(t *testing.T) {
	h, clock := newScheduleHarness(t)
	h.seed("tdf_clocked_1", "clocked", tickingTeam("tenant", 30000))

	runID := h.detach(acmeUser("alice"), "clocked")
	clock.waitAfters(t, 1)
	alice := acmeUser("alice")(context.Background())
	if stopped, isWalk, err := h.srv.cancelTeamWalk(alice, runID, "enough"); !stopped || !isWalk || err != nil {
		t.Fatalf("cancel the walk: stopped=%v walk=%v err=%v", stopped, isWalk, err)
	}
	if st := h.waitRun(runID); st != store.RunCancelled {
		t.Fatalf("the walk ended %s, want cancelled", st)
	}
	assertNoTickAfterTheWalk(t, h, clock, 0)
}

// schedWalkCtx is a ctx shaped like a walk's: the team's scope, a run id, and the
// run identity of a user in tenant acme.
func schedWalkCtx(sc store.TeamScope, runID, user string) context.Context {
	ctx := store.WithTeamScope(context.Background(), sc)
	ctx = tools.WithRunID(ctx, runID)
	ctx = auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: user, Scopes: []string{auth.ScopeTenant}})
	return tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "team:clocked", TenantID: "acme", UserID: user})
}

func mustTeamDef(t *testing.T, raw string) teamgraph.Definition {
	t.Helper()
	d, err := teamgraph.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Two walks of one team running at once each arm their own timer: a
// user-scoped channel gets each walk's tick under its own user, and a
// tenant-scoped one both walks' ticks.
func TestTeamLocalSchedule_TwoWalksEachTick(t *testing.T) {
	for _, scope := range []string{"user", "tenant"} {
		t.Run(scope, func(t *testing.T) {
			h, clock := newScheduleHarness(t)
			raw := tickingTeam(scope, 30000)
			sc := h.seed("tdf_clocked_1", "clocked", raw)
			def := mustTeamDef(t, raw)

			disarmA, err := h.srv.armWalkTriggers(schedWalkCtx(sc, "r_a", "alice"), def)
			if err != nil {
				t.Fatal(err)
			}
			disarmB, err := h.srv.armWalkTriggers(schedWalkCtx(sc, "r_b", "bob"), def)
			if err != nil {
				t.Fatal(err)
			}
			clock.waitAfters(t, 2)
			if n := h.srv.walkTimers.Load(); n != 2 {
				t.Fatalf("two walks arm %d timer(s), want 2", n)
			}
			clock.Advance(10 * time.Second)
			clock.waitAfters(t, 4) // each timer published and is waiting again
			disarmA()
			disarmB()

			if scope == "user" {
				for _, who := range []string{"alice", "bob"} {
					if got := h.stored("_team/clocked/ticks", store.MemoryScopeUser, who); len(got) != 1 || got[0].PublishedByUserID != who {
						t.Errorf("%s's walk's tick under its own user: %+v", who, got)
					}
				}
			} else if got := h.stored("_team/clocked/ticks", store.MemoryScopeTenant, ""); len(got) != 2 {
				t.Errorf("both walks' ticks land in the shared channel: %d message(s), want 2", len(got))
			}
			if n := h.srv.walkTimers.Load(); n != 0 {
				t.Errorf("%d timer(s) left after both disarms", n)
			}
		})
	}
}

// While the runtime is paused a team's schedules publish nothing; after the
// resume the next tick publishes. Skipped ticks are not made up.
func TestTeamLocalSchedule_PauseStopsTickingAndResumeRestartsIt(t *testing.T) {
	h, clock := newScheduleHarness(t)
	raw := tickingTeam("tenant", 30000)
	sc := h.seed("tdf_clocked_1", "clocked", raw)
	mgr := pause.NewManager(h.st, 200*time.Millisecond)
	h.srv.SetPauseManager(mgr)

	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, "r_a", "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	defer disarm()
	clock.waitAfters(t, 1)

	if _, err := mgr.Pause(context.Background(), 100*time.Millisecond); err != nil {
		t.Fatalf("pause: %v", err)
	}
	clock.Advance(10 * time.Second)
	clock.waitAfters(t, 2) // the tick came due and went round
	clock.Advance(10 * time.Second)
	clock.waitAfters(t, 3)
	if got := h.stored("_team/clocked/ticks", store.MemoryScopeTenant, ""); len(got) != 0 {
		t.Fatalf("%d tick(s) published while the runtime was paused, want none", len(got))
	}

	if _, err := mgr.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	clock.Advance(10 * time.Second)
	clock.waitAfters(t, 4)
	if got := h.stored("_team/clocked/ticks", store.MemoryScopeTenant, ""); len(got) != 1 {
		t.Fatalf("after the resume the next tick publishes: %d message(s), want 1", len(got))
	}
}

// A schedule with a payload publishes exactly that payload.
func TestTeamLocalSchedule_PublishesItsPayload(t *testing.T) {
	h, clock := newScheduleHarness(t)
	raw := strings.Replace(tickingTeam("tenant", 30000), `"channel":"./ticks"}}`, `"channel":"./ticks","payload":{"task":"sweep"}}}`, 1)
	sc := h.seed("tdf_clocked_1", "clocked", raw)
	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, "r_a", "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	clock.waitAfters(t, 1)
	clock.Advance(10 * time.Second)
	clock.waitAfters(t, 2)
	disarm()
	got := h.stored("_team/clocked/ticks", store.MemoryScopeTenant, "")
	if len(got) != 1 || string(got[0].Payload) != `{"task":"sweep"}` {
		t.Fatalf("the tick publishes the schedule's payload: %+v", got)
	}
}

// A schedule that could never publish refuses the walk at arm time instead of
// failing on every tick, and arms nothing.
func TestArmWalkTriggers_RefusesAScheduleThatCouldNeverPublish(t *testing.T) {
	h, _ := newScheduleHarness(t)
	raw := tickingTeam("user", 30000)
	sc := h.seed("tdf_clocked_1", "clocked", raw)
	def := mustTeamDef(t, raw)

	for what, ctx := range map[string]context.Context{
		"no user for a user-scoped channel": schedWalkCtx(sc, "r_a", ""),
		"another tenant's walk": tools.WithRunIdentity(schedWalkCtx(sc, "r_a", "alice"),
			tools.RunIdentityValue{AgentID: "team:clocked", TenantID: "globex", UserID: "alice"}),
		"outside every walk": tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme", UserID: "alice"}),
	} {
		if _, err := h.srv.armWalkTriggers(ctx, def); err == nil {
			t.Errorf("%s: want a refusal", what)
		}
	}
	if n := h.srv.walkTimers.Load(); n != 0 {
		t.Errorf("a refused arm left %d timer(s)", n)
	}
	// A team without schedules arms nothing and needs nothing.
	disarm, err := h.srv.armWalkTriggers(context.Background(), mustTeamDef(t, starterTeam))
	if err != nil {
		t.Fatalf("a team without schedules: %v", err)
	}
	disarm()
}
