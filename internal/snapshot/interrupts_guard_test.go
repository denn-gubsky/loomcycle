package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestCapture_PausedRunOnAPendingInterruptIsAFinding: a paused run cannot be
// parked on a pending interrupt today, which is why no section carries them.
// If one ever is, capture must say so — in its warnings, the envelope's
// capture_findings and every restore of it — rather than carry the run and
// silently drop what it waits on. A resolved interrupt, and one on a run that
// is not paused, are not findings.
func TestCapture_PausedRunOnAPendingInterruptIsAFinding(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	newRun := func(paused bool) string {
		t.Helper()
		sess, err := s.CreateSession(ctx, "acme", "asker", "alice")
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a", UserID: "alice", TenantID: "acme"})
		if err != nil {
			t.Fatal(err)
		}
		if paused {
			if err := s.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
				t.Fatal(err)
			}
		}
		return run.ID
	}
	n := 0
	ask := func(runID string) string {
		t.Helper()
		n++
		id, err := s.InterruptCreate(ctx, store.InterruptRow{InterruptID: fmt.Sprintf("int_%d", n), RunID: runID, Kind: store.InterruptKindQuestion,
			Status: store.InterruptStatusPending, Question: "proceed?", Priority: "normal", CreatedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	parked := newRun(true)
	ask(parked)
	answered := newRun(true)
	if err := s.InterruptResolve(ctx, ask(answered), "yes", "alice", nil); err != nil {
		t.Fatalf("InterruptResolve: %v", err)
	}
	ask(newRun(false))

	got, err := CaptureReport(ctx, s, CaptureOptions{})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	var hits []string
	for _, w := range got.Warnings {
		if strings.Contains(w, "pending interrupt") {
			hits = append(hits, w)
		}
	}
	if len(hits) != 1 || !strings.Contains(hits[0], parked) {
		t.Fatalf("capture warnings about pending interrupts = %v; want exactly one, naming run %s", hits, parked)
	}
	var env struct {
		Sections struct {
			CaptureFindings *CaptureFindingsSection `json:"capture_findings"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(got.JSON, &env); err != nil {
		t.Fatal(err)
	}
	if env.Sections.CaptureFindings == nil || len(env.Sections.CaptureFindings.Entries) != 1 {
		t.Fatalf("capture_findings = %+v, want the one finding", env.Sections.CaptureFindings)
	}
	f := env.Sections.CaptureFindings.Entries[0]
	if f.Section != "paused_runs" || f.Name != parked || f.TenantID != "acme" || f.Field != "interrupts" || f.Detector != detectorPendingInterrupt {
		t.Errorf("finding = %+v", f)
	}

	dst, dstClose := newTestStore(t)
	defer dstClose()
	res, err := Restore(ctx, dst, got.JSON, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reemitted := false
	for _, w := range res.Warnings {
		reemitted = reemitted || (strings.HasPrefix(w, "capture finding: ") && strings.Contains(w, parked) && strings.Contains(w, "pending interrupt"))
	}
	if !reemitted {
		t.Errorf("restore did not re-emit the finding: %v", res.Warnings)
	}
}

// A failed interrupt count fails the capture: a guard that skips when it
// cannot look is no guard.
func TestCapture_InterruptGuardFailsClosed(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "acme", "asker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a", UserID: "alice", TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("count failed")
	if _, _, err := Capture(ctx, failingInterruptCount{Store: s, err: boom}, CaptureOptions{}); !errors.Is(err, boom) {
		t.Errorf("Capture err = %v, want the count failure", err)
	}
}

type failingInterruptCount struct {
	store.Store
	err error
}

func (f failingInterruptCount) InterruptCountPendingByRun(context.Context, string) (int, error) {
	return 0, f.err
}
