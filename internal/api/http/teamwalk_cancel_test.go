package http

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A lead holding only its TeamDef call can end a poll-mode walk with TeamDef
// cancel: the walk's run and its member's run end cancelled with the
// parent's reason, the lead does not wait for it, and the answer reports it
// cancelled.
func TestTeamWalkPoll_TeamDefCancelEndsTheWalkAndItsMember(t *testing.T) {
	prov := &scriptedParent{release: make(chan struct{})}
	var st store.Store
	prov.script = []func(providers.Request) []providers.Event{
		fixed(toolCallEv("tu_1", "TeamDef", `{"op":"run","name":"rev","input":"a","mode":"poll"}`)),
		func(req providers.Request) []providers.Event {
			// Cancelled once its member runs, so the cancel has a member to reach.
			walkID := runIDIn(lastToolText(req))
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				if runs, _, err := st.ListRunsByWalk(context.Background(), "", walkID, 50, ""); err == nil && len(runs) > 1 {
					break
				}
			}
			return toolCallEv("tu_2", "TeamDef", `{"op":"cancel","run_ids":["`+walkID+`"]}`)
		},
		fixed(answer("cancelled it")),
	}
	srv, st := newPollWalkServer(t, prov)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	defer cancelAllRuns(srv)
	defer prov.unblock()
	// Bounded: a lead whose cancel did nothing waits for the walk, whose
	// member is held until the cleanup — so a failure fails here, not at the
	// package timeout.
	var stream string
	select {
	case stream = <-startLead(t, ts):
	case <-time.After(30 * time.Second):
		t.Fatal("the lead never finished: it is still waiting for the walk it cancelled")
	}
	if strings.Contains(stream, "event: awaiting_children") {
		t.Error("the lead waited for a walk it had cancelled")
	}
	calls := prov.leadCalls()
	if len(calls) != 3 {
		t.Fatalf("the lead made %d calls, want 3", len(calls))
	}
	walkID := runIDIn(lastToolText(calls[1]))
	if res := lastToolText(calls[2]); !strings.Contains(res, `"state":"cancelled"`) || !strings.Contains(res, `"run_id":"`+walkID+`"`) {
		t.Errorf("TeamDef cancel answered %s", res)
	}
	walk := waitWalkRunStatus(t, st, walkID, store.RunCancelled)
	if !strings.Contains(walk.StopReason, "cancelled by its parent") {
		t.Errorf("walk stop reason = %q", walk.StopReason)
	}
	runs, _, err := st.ListRunsByWalk(context.Background(), "", walkID, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	members := 0
	for _, m := range runs {
		if m.ID == walkID {
			continue
		}
		members++
		waitWalkRunStatus(t, st, m.ID, store.RunCancelled)
	}
	if members != 1 {
		t.Errorf("the cancelled walk had %d members, want 1", members)
	}
}
