package scheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// timedRunner is a fakeRunner whose passes take a fixed wall-clock time, the
// way a pass that reads a few chats through a slow local extractor does. A pass
// whose ctx ends first is cut; one that outlasts its duration completes.
// forever makes a pass run until its ctx ends, which is a wedged pass.
//
// A cut pass returns NIL, because the real runner does: RunOnce returns nil
// whenever the loop ran, including when it ended with status=failed (see the
// runner.Runner contract), and a code agent cut by its deadline is exactly
// that. A double returning ctx.Err() here let a cut detector keyed on the
// error pass every test and never fire on a live server.
type timedRunner struct {
	*fakeRunner
	take func(in runner.RunInput) (d time.Duration, forever bool)

	mu        sync.Mutex
	completed []string // "tenant/user" of each pass that finished, in order
}

func (r *timedRunner) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	if err := r.fakeRunner.RunOnce(ctx, in, cb); err != nil {
		return err
	}
	d, forever := r.take(in)
	if forever {
		<-ctx.Done()
		return nil // the run ended failed; RunOnce still returns nil
	}
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil // as above
		}
	}
	r.mu.Lock()
	r.completed = append(r.completed, in.TenantID+"/"+in.UserID)
	r.mu.Unlock()
	return nil
}

func (r *timedRunner) completions(target string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.completed {
		if c == target {
			n++
		}
	}
	return n
}

func (l *logCapture) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

const (
	logEscalated  = "goes first next tick with the whole remaining budget"
	logDroppedOut = "dropping it back to normal budgeting"
)

// TestFanout_PassCutByItsShareCompletesOnItsNextTurn is the v1.101.0
// consolidation loop. Two targets split the fire budget in half, and a pass
// that needs more than its half is cut at the same point on every tick: the
// bundled consolidator records nothing a cut pass had not finished, so the next
// pass starts from the same place and is cut again, forever.
//
// A pass the slice cut now goes first next tick with the whole remaining
// budget, so it finishes within two ticks. The slow target is aaa, which the
// rotation puts SECOND on tick two — so finishing also proves it was moved to
// the front rather than merely given a longer slice.
//
// The margins are wide on purpose (the pass is 500ms over its slice and 700ms
// under the whole budget), so a loaded machine cannot flip the outcome.
//
// Fails-before: aaa is cut on both ticks and never completes.
func TestFanout_PassCutByItsShareCompletesOnItsNextTurn(t *testing.T) {
	const (
		fire = 2400 * time.Millisecond // a slice of fire/2 = 1.2s
		slow = 1700 * time.Millisecond
	)
	sched, fr, st, logs := fanoutFixture(t, operatorFanoutDef(nil), func(c *Config) {
		c.FireTimeout = fire
	})
	// No provider resolver: serial, as the code-js consolidator is.
	tr := &timedRunner{fakeRunner: fr, take: func(in runner.RunInput) (time.Duration, bool) {
		if in.TenantID == "aaa" {
			return slow, false
		}
		return 0, false
	}}
	sched.runner = tr
	seedSettledSession(t, st, "aaa", "slow")
	seedSettledSession(t, st, "zzz", "zed")
	row := dueRow(t, st)

	// Tick one: aaa first (rotation 0), cut by its slice; zzz still runs.
	sched.fireOne(context.Background(), row, time.Now())
	if n := tr.completions("aaa/slow"); n != 0 {
		t.Fatalf("aaa completed on its first slice-budgeted turn — the fixture must make the pass longer than the slice to test anything; logs:\n%s", logs.all())
	}
	if n := tr.completions("zzz/zed"); n != 1 {
		t.Fatalf("zzz completed %d time(s) on tick one, want 1; logs:\n%s", n, logs.all())
	}
	if n := logs.count(logEscalated); n != 1 {
		t.Errorf("the cut must be logged once, naming what happens next; got %d line(s); logs:\n%s", n, logs.all())
	}

	// Tick two: the rotation would start with zzz; the escalated aaa goes first
	// and gets the whole budget.
	before := len(fr.Calls())
	sched.fireOne(context.Background(), row, time.Now())
	if calls := callsSince(fr, before); len(calls) == 0 || calls[0] != "aaa/slow" {
		t.Errorf("tick two dispatched %v, want the cut target aaa/slow first, ahead of the rotation; logs:\n%s", calls, logs.all())
	}
	if n := tr.completions("aaa/slow"); n != 1 {
		t.Fatalf("aaa completed %d time(s) after two ticks, want 1 — a pass cut by its share of the budget must get the whole budget on its next turn, or it is cut at the same point forever; logs:\n%s", n, logs.all())
	}
	if !logs.contains("completed on its whole-budget turn") {
		t.Errorf("the escalation clearing must be logged; logs:\n%s", logs.all())
	}
	if got := sched.escalatedTargets(row.DefID); len(got) != 0 {
		t.Errorf("escalated after a completed pass = %v, want none — a completed pass goes back to normal budgeting", got)
	}
}

// TestFanout_WedgedEscalatedPassDropsBackAndDoesNotStarveOthers is the guard on
// escalation. A pass that never finishes, given the whole budget, would hold
// every tick and starve every other target. So an escalated pass the WHOLE
// budget cut drops back to its slice; the cost of a wedged target is at most
// every other tick, and the others still run.
//
// Fails-before: there is no escalation at all, so tick two runs zzz and the
// transitions are never logged.
func TestFanout_WedgedEscalatedPassDropsBackAndDoesNotStarveOthers(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, operatorFanoutDef(nil), func(c *Config) {
		// A 600ms slice: wide enough that target enumeration, which spends the
		// batch's budget too, can never make the batch the earlier deadline.
		c.FireTimeout = 1200 * time.Millisecond
	})
	tr := &timedRunner{fakeRunner: fr, take: func(in runner.RunInput) (time.Duration, bool) {
		return 0, in.TenantID == "aaa"
	}}
	sched.runner = tr
	seedSettledSession(t, st, "aaa", "wedged")
	seedSettledSession(t, st, "zzz", "zed")
	row := dueRow(t, st)

	tick := func() []string {
		before := len(fr.Calls())
		sched.fireOne(context.Background(), row, time.Now())
		return callsSince(fr, before)
	}

	// Tick one: aaa is cut by its slice and escalated; zzz runs behind it.
	if got := strings.Join(tick(), ","); got != "aaa/wedged,zzz/zed" {
		t.Fatalf("tick one dispatched %s, want aaa/wedged,zzz/zed; logs:\n%s", got, logs.all())
	}
	// Tick two: escalated, aaa holds the whole budget — which is what
	// escalation means — and is cut by it, so it drops back.
	if got := strings.Join(tick(), ","); got != "aaa/wedged" {
		t.Fatalf("tick two dispatched %s, want only the escalated aaa/wedged holding the whole budget; logs:\n%s", got, logs.all())
	}
	if n := logs.count(logDroppedOut); n != 1 {
		t.Fatalf("an escalated pass cut by the whole budget must drop back, logged once; got %d line(s); logs:\n%s", n, logs.all())
	}
	// Tick three: back on its slice, so zzz runs again.
	if got := tick(); len(got) != 2 || got[1] != "zzz/zed" {
		t.Errorf("tick three dispatched %v, want zzz to run again — a wedged target must not starve the others; logs:\n%s", got, logs.all())
	}
	if n := tr.completions("zzz/zed"); n != 2 {
		t.Errorf("zzz completed %d time(s) in three ticks, want 2", n)
	}
	// Each transition is one line when it happens: escalated on ticks one and
	// three, dropped back on tick two.
	if n := logs.count(logEscalated); n != 2 {
		t.Errorf("escalation logged %d time(s) over three ticks, want 2; logs:\n%s", n, logs.all())
	}
}

// TestFanout_PassThatCameLastInASpentBudgetIsNotEscalated: a pass cut because
// the BATCH ran out, not its own slice, did not show that it needs more than its
// share, and escalating it would let it jump the queue for having come last.
//
// Three serial targets, each running until cut. aaa and bbb each spend a full
// slice; ccc starts after them, so its slice would end strictly after the batch
// does and the batch's deadline is the one that cuts it.
func TestFanout_PassThatCameLastInASpentBudgetIsNotEscalated(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, operatorFanoutDef(nil), func(c *Config) {
		c.FireTimeout = 1800 * time.Millisecond // three targets: a slice of 600ms
	})
	sched.runner = &timedRunner{fakeRunner: fr, take: func(runner.RunInput) (time.Duration, bool) { return 0, true }}
	for _, tenant := range []string{"aaa", "bbb", "ccc"} {
		seedSettledSession(t, st, tenant, "u")
	}
	row := dueRow(t, st)

	sched.fireOne(context.Background(), row, time.Now())

	if got := strings.Join(dispatched(fr), ","); got != "aaa/u,bbb/u,ccc/u" {
		t.Fatalf("dispatched %s, want all three in tenant order; logs:\n%s", got, logs.all())
	}
	escalated := sched.escalatedTargets(row.DefID)
	for tenant, want := range map[string]bool{"aaa": true, "bbb": true, "ccc": false} {
		if got := escalated[consolidationTarget{TenantID: tenant, Scope: "user", UserID: "u"}]; got != want {
			t.Errorf("tenant %s escalated = %v, want %v — only a pass cut by its OWN slice is escalated; logs:\n%s", tenant, got, want, logs.all())
		}
	}
}
