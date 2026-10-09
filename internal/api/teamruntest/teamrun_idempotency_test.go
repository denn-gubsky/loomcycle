package teamruntest

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// started is what a start of the team answered with.
type started struct {
	RunID        string `json:"run_id"`
	Status       string `json:"status"`
	Name         string `json:"name"`
	DefID        string `json:"def_id"`
	Deduplicated bool   `json:"deduplicated"`
	FinalState   string `json:"final_state"`
	FinalOutput  string `json:"final_output"`
	Steps        []any  `json:"steps"`
}

func startOf(t *testing.T, o outcome) started {
	t.Helper()
	if o.forbidden || o.toolError {
		t.Fatalf("the start was not answered: %+v", o)
	}
	var s started
	if err := json.Unmarshal([]byte(o.text), &s); err != nil || s.RunID == "" {
		t.Fatalf("the start answered %q (%v), want a walk's run", o.text, err)
	}
	return s
}

func keyed(key, mode string) string {
	in := `{"op":"run","name":"solo","idempotency_key":"` + key + `"`
	if mode != "" {
		in += `,"mode":"` + mode + `"`
	}
	return in + `}`
}

// ended waits for the walk's run to end and returns its row.
func (e *env) ended(runID string) store.Run {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		run, err := e.st.GetRun(context.Background(), runID)
		if err != nil {
			e.t.Fatalf("read run %s: %v", runID, err)
		}
		if store.IsTerminalRunStatus(run.Status) {
			return run
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("walk %s did not end; status %s", runID, run.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTeamRun_ASecondStartWithTheKeyStartsNothing — the lost-response case: a
// detached start whose answer never arrived is sent again with its key, and
// is answered with the walk it already started. One walk exists, with one set
// of member runs, on every surface.
func TestTeamRun_ASecondStartWithTheKeyStartsNothing(t *testing.T) {
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			e := newEnv(t, false)
			alice := e.mint("alice", auth.ScopeTenant)

			first := startOf(t, s.call(e, alice, keyed("press-1", "detach")))
			if first.Deduplicated || first.Status != "running" {
				t.Fatalf("the first start = %+v, want a new running walk", first)
			}
			again := startOf(t, s.call(e, alice, keyed("press-1", "detach")))
			if again.RunID != first.RunID || !again.Deduplicated || again.Name != teamName || again.DefID != first.DefID {
				t.Errorf("the retry = %+v, want the first walk %s, marked deduplicated", again, first.RunID)
			}
			e.ended(first.RunID)

			// A key held by a walk that has ended returns that walk, with how
			// it ended — in whatever mode the retry asks.
			for _, mode := range []string{"detach", ""} {
				late := startOf(t, s.call(e, alice, keyed("press-1", mode)))
				if late.RunID != first.RunID || !late.Deduplicated || late.Status != "completed" ||
					late.FinalState != "done" || late.FinalOutput != "done" || late.Steps != nil {
					t.Errorf("a retry (mode %q) after the walk ended = %+v, want the ended walk and no steps", mode, late)
				}
			}
			// The key is resolved before the team is read: a retry is answered
			// with the walk it started even after the team was retired.
			if o := s.call(e, alice, `{"op":"retire","name":"solo","retired":true,"def_id":"`+first.DefID+`"}`); o.forbidden || o.toolError {
				t.Fatalf("retire: %+v", o)
			}
			if o := s.call(e, alice, `{"op":"run","name":"solo"}`); !o.toolError {
				t.Fatalf("a retired team still starts: %+v", o)
			}
			if late := startOf(t, s.call(e, alice, keyed("press-1", "detach"))); late.RunID != first.RunID || !late.Deduplicated {
				t.Errorf("a retry after the team was retired = %+v, want the walk it started", late)
			}
			if n := e.prov.count(); n != 1 {
				t.Errorf("the member ran %d times across five starts with one key, want once", n)
			}
		})
	}
}

// The key belongs to who sent it: another user of the tenant, or another key,
// starts a walk of its own.
func TestTeamRun_AnotherUserOrAnotherKeyStartsItsOwnWalk(t *testing.T) {
	e := newEnv(t, false)
	alice, bob := e.mint("alice", auth.ScopeTenant), e.mint("bob", auth.ScopeTenant)
	call := surfaces[0].call

	mine := startOf(t, call(e, alice, keyed("k", "detach")))
	theirs := startOf(t, call(e, bob, keyed("k", "detach")))
	other := startOf(t, call(e, alice, keyed("k2", "detach")))
	if theirs.RunID == mine.RunID || theirs.Deduplicated {
		t.Errorf("another user's start with the same key = %+v, want its own walk", theirs)
	}
	if other.RunID == mine.RunID || other.Deduplicated {
		t.Errorf("a start with another key = %+v, want its own walk", other)
	}
	for _, id := range []string{mine.RunID, theirs.RunID, other.RunID} {
		e.ended(id)
	}
	if n := e.prov.count(); n != 3 {
		t.Errorf("the member ran %d times, want once per walk", n)
	}
}

// A waited-for start keeps the key too: a retry while it runs, or after, is
// answered with it and walks nothing.
func TestTeamRun_AWaitedStartHoldsItsKey(t *testing.T) {
	e := newEnv(t, false)
	alice := e.mint("alice", auth.ScopeTenant)
	call := surfaces[0].call

	first := startOf(t, call(e, alice, keyed("sync-1", "")))
	if first.Deduplicated || first.Status != "completed" || len(first.Steps) != 1 {
		t.Fatalf("the waited start = %+v, want a completed walk with its steps", first)
	}
	again := startOf(t, call(e, alice, keyed("sync-1", "")))
	if again.RunID != first.RunID || !again.Deduplicated || again.FinalState != "done" {
		t.Errorf("the retry = %+v, want the first walk", again)
	}
	if n := e.prov.count(); n != 1 {
		t.Errorf("the member ran %d times, want once", n)
	}
}

// Requests racing on one key start exactly one walk; every other is answered
// with it.
func TestTeamRun_RacingStartsWithOneKeyStartOneWalk(t *testing.T) {
	e := newEnv(t, false)
	alice := e.mint("alice", auth.ScopeTenant)
	const n = 8
	out := make([]outcome, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = e.teamHTTP(alice, keyed("race", "detach"))
		}()
	}
	wg.Wait()
	ids, fresh := map[string]bool{}, 0
	for _, o := range out {
		s := startOf(t, o)
		ids[s.RunID] = true
		if !s.Deduplicated {
			fresh++
		}
	}
	if len(ids) != 1 || fresh != 1 {
		t.Fatalf("%d racing starts gave %d walks, %d of them reported new; want one walk, reported new once", n, len(ids), fresh)
	}
	for id := range ids {
		e.ended(id)
	}
	if got := e.prov.count(); got != 1 {
		t.Errorf("the member ran %d times, want once", got)
	}
}

// A key that cannot be used is refused before anything starts, in the words a
// run gives a malformed one.
func TestTeamRun_AKeyThatCannotBeUsedIsRefused(t *testing.T) {
	e := newEnv(t, false)
	alice := e.mint("alice", auth.ScopeTenant)
	for input, want := range map[string]string{
		keyed("has space", "detach"):                                             "idempotency_key must match [A-Za-z0-9:._-]{1,200}",
		keyed(strings.Repeat("k", 201), ""):                                      "idempotency_key must match [A-Za-z0-9:._-]{1,200}",
		`{"op":"run","name":"solo","idempotency_key":"k","board_chunk_id":"c1"}`: "does not apply to a board-bound walk",
	} {
		o := e.teamHTTP(alice, input)
		if !o.toolError || !strings.Contains(o.text, want) {
			t.Errorf("%s = %+v, want a refusal containing %q", input, o, want)
		}
	}
	if n, rows := e.prov.count(), e.walkRuns(); n != 0 || rows != 0 {
		t.Errorf("after the refusals: %d model calls and %d walk run rows, want none", n, rows)
	}
}

// A key a caller used to start an agent run names that run, not a walk: the
// two are held apart, so a team start is never answered with an agent run.
func TestTeamRun_ARunsKeyIsNotAWalksKey(t *testing.T) {
	e := newEnv(t, false)
	alice := e.mint("alice", auth.ScopeTenant)
	resp, raw := e.do("/v1/runs", alice, `{"agent":"worker","prompt":"hi","idempotency_key":"shared"}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("POST /v1/runs = %d: %s", resp.StatusCode, raw)
	}
	walk := startOf(t, e.teamHTTP(alice, keyed("shared", "")))
	if walk.Deduplicated || walk.Name != teamName || len(walk.Steps) != 1 {
		t.Errorf("a team start with a key an agent run holds = %+v, want a new walk", walk)
	}
}
