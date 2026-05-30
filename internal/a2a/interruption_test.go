package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakeInterruptStore records resolve calls and serves a scripted pending
// list, so the resolver is testable without a real DB.
type fakeInterruptStore struct {
	pending     []store.InterruptRow
	listErr     error
	resolveErr  error
	resolvedID  string
	resolvedAns string
	resolveByOK string
}

func (f *fakeInterruptStore) InterruptListByRun(ctx context.Context, runID, statusFilter string) ([]store.InterruptRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.pending, nil
}

func (f *fakeInterruptStore) InterruptResolve(ctx context.Context, interruptID, answer, resolvedBy string, answerMeta json.RawMessage) error {
	if f.resolveErr != nil {
		return f.resolveErr
	}
	f.resolvedID = interruptID
	f.resolvedAns = answer
	f.resolveByOK = resolvedBy
	return nil
}

// TestParkRegistry_PutThenTakeOnce asserts a parked run is returned
// exactly once and the entry is then gone (so a resume consumes it).
func TestParkRegistry_PutThenTakeOnce(t *testing.T) {
	r := newParkRegistry()
	p := &parkedRun{agentID: "task-1"}
	r.put("task-1", p)

	got, ok := r.take("task-1")
	if !ok || got != p {
		t.Fatalf("take = (%v,%v), want the parked run", got, ok)
	}
	if _, ok := r.take("task-1"); ok {
		t.Fatal("second take returned a value; entry should be consumed exactly once")
	}
}

// TestParkRegistry_TakeUnknownReturnsFalse asserts an unknown task id
// yields ok=false (the executor's "unknown-taskId starts a fresh run"
// branch depends on this).
func TestParkRegistry_TakeUnknownReturnsFalse(t *testing.T) {
	r := newParkRegistry()
	if _, ok := r.take("nope"); ok {
		t.Fatal("take on an unregistered task returned ok=true")
	}
}

// TestStoreInterruptResolver_PendingForRun asserts the resolver reports
// the first pending interrupt's id for a run, and (false) when none.
func TestStoreInterruptResolver_PendingForRun(t *testing.T) {
	fs := &fakeInterruptStore{pending: []store.InterruptRow{{InterruptID: "intr-7"}}}
	var notified []string
	r := NewInterruptResolver(fs, func(k string) { notified = append(notified, k) })

	id, ok := r.PendingForRun(context.Background(), "run-1")
	if !ok || id != "intr-7" {
		t.Fatalf("PendingForRun = (%q,%v), want (intr-7,true)", id, ok)
	}

	fs.pending = nil
	if _, ok := r.PendingForRun(context.Background(), "run-1"); ok {
		t.Fatal("PendingForRun returned ok=true with no pending rows")
	}
}

// TestStoreInterruptResolver_ResolveCallsStoreAndNotifies asserts Resolve
// records the answer via the store AND wakes the parked run on the
// "intr:<id>" bus key — the two-step shape the parked loop depends on.
func TestStoreInterruptResolver_ResolveCallsStoreAndNotifies(t *testing.T) {
	fs := &fakeInterruptStore{}
	var notified []string
	r := NewInterruptResolver(fs, func(k string) { notified = append(notified, k) })

	if err := r.Resolve(context.Background(), "intr-7", "the answer"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if fs.resolvedID != "intr-7" || fs.resolvedAns != "the answer" {
		t.Errorf("store resolve = (%q,%q), want (intr-7, the answer)", fs.resolvedID, fs.resolvedAns)
	}
	if fs.resolveByOK != store.InterruptResolvedByAPI {
		t.Errorf("resolved_by = %q, want %q", fs.resolveByOK, store.InterruptResolvedByAPI)
	}
	if len(notified) != 1 || notified[0] != "intr:intr-7" {
		t.Fatalf("notify keys = %v, want one [intr:intr-7]", notified)
	}
}

// TestStoreInterruptResolver_ResolveStoreErrorSkipsNotify asserts a store
// failure short-circuits before the notify (we must not wake a run whose
// answer was not recorded).
func TestStoreInterruptResolver_ResolveStoreErrorSkipsNotify(t *testing.T) {
	fs := &fakeInterruptStore{resolveErr: errors.New("already terminal")}
	var notified []string
	r := NewInterruptResolver(fs, func(k string) { notified = append(notified, k) })

	if err := r.Resolve(context.Background(), "intr-x", "ans"); err == nil {
		t.Fatal("Resolve should return the store error")
	}
	if len(notified) != 0 {
		t.Fatalf("notify fired despite store error: %v", notified)
	}
}

// fakeResolver is a hand-driven InterruptResolver for the executor
// INPUT_REQUIRED tests: it reports a configurable pending id and records
// whether Resolve was called. resolved closes the first time Resolve is
// invoked so a test can synchronise the parked-loop release on it without
// polling a shared field (which would race the executor goroutine).
type fakeResolver struct {
	pendingID    string
	hasPending   bool
	resolveErr   error
	resolveCalls int
	gotAnswer    string
	resolved     chan struct{}
}

func (f *fakeResolver) PendingForRun(ctx context.Context, runID string) (string, bool) {
	return f.pendingID, f.hasPending
}

func (f *fakeResolver) Resolve(ctx context.Context, interruptID, answer string) error {
	f.resolveCalls++
	f.gotAnswer = answer
	if f.resolved != nil {
		close(f.resolved)
	}
	return f.resolveErr
}

var _ InterruptResolver = (*fakeResolver)(nil)
