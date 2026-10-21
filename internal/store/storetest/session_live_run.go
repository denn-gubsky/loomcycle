package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// SessionHasLiveRun: a session's live run is a queued one, or a running one
// whose heartbeat (or start) is not older than the cutoff, or one held by a
// runtime pause. The cutoff is the heartbeat sweeper's, so a row it would
// sweep — a crashed process's — is not live.
func testSessionHasLiveRun(t *testing.T, s store.Store) {
	ctx := context.Background()
	fresh := time.Now().Add(-time.Hour)   // everything written now is fresh
	allStale := time.Now().Add(time.Hour) // everything written now is stale
	live := func(sessionID, exclude string, cutoff time.Time) bool {
		t.Helper()
		ok, err := s.SessionHasLiveRun(ctx, sessionID, exclude, cutoff)
		if err != nil {
			t.Fatalf("SessionHasLiveRun: %v", err)
		}
		return ok
	}
	newSession := func() string {
		t.Helper()
		sess, err := s.CreateSession(ctx, "tl", "default", "ann")
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return sess.ID
	}

	empty := newSession()
	if live(empty, "", fresh) {
		t.Error("a session with no runs has a live run")
	}

	sid := newSession()
	running, err := s.CreateRun(ctx, sid, store.RunIdentity{AgentID: "a_live_running"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if !live(sid, "", fresh) {
		t.Error("a running run with a fresh start is not live")
	}
	if live(sid, running.ID, fresh) {
		t.Error("the excluded run is counted")
	}
	if live(sid, "", allStale) {
		t.Error("a running run that never beat and started before the cutoff is live")
	}
	if err := s.UpdateHeartbeat(ctx, running.ID); err != nil {
		t.Fatalf("UpdateHeartbeat: %v", err)
	}
	if !live(sid, "", fresh) || live(sid, "", allStale) {
		t.Error("a beating run is not judged by its heartbeat")
	}
	if err := s.SetRunPauseState(ctx, running.ID, store.PauseStatePaused); err != nil {
		t.Fatalf("SetRunPauseState: %v", err)
	}
	if !live(sid, "", allStale) {
		t.Error("a run held by a runtime pause is not live (it beats no heartbeat on purpose)")
	}
	if err := s.FinishRun(ctx, running.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if live(sid, "", fresh) {
		t.Error("a finished run is live")
	}

	qsid := newSession()
	queued, err := s.CreateQueuedRun(ctx, qsid, store.RunIdentity{AgentID: "a_live_queued"})
	if err != nil {
		t.Fatalf("CreateQueuedRun: %v", err)
	}
	if !live(qsid, "", fresh) || live(qsid, queued.ID, fresh) {
		t.Error("a queued run is not live, or the excluded one is counted")
	}
	if live(qsid, "", allStale) {
		t.Error("a queued run older than the cutoff is live")
	}
	// Another session's live run is not this one's.
	if live(empty, "", fresh) {
		t.Error("another session's run is counted")
	}
}
