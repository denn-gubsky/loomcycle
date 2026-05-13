package cancel

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// makeCancellable returns a (ctx, cancelFn) pair plus a boolean
// pointer that flips to true the moment cancelFn is called. Lets
// tests assert "the cancel actually fired" without racing on the
// goroutine that observes ctx.
func makeCancellable() (context.Context, context.CancelCauseFunc, *atomic.Bool) {
	ctx, cancel := context.WithCancelCause(context.Background())
	fired := &atomic.Bool{}
	wrapped := func(cause error) {
		fired.Store(true)
		cancel(cause)
	}
	return ctx, wrapped, fired
}

// Happy path: register an entry, look it up, deregister.
func TestRegistry_Register_Get_Deregister(t *testing.T) {
	r := NewRegistry()
	_, cancelFn, _ := makeCancellable()
	e := Entry{AgentID: "a_1", RunID: "r_1", UserID: "u_1", StartedAt: time.Now()}

	if err := r.Register(e, cancelFn); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := r.Get("a_1")
	if !ok {
		t.Fatal("Get returned ok=false for registered agent")
	}
	if got.RunID != "r_1" || got.UserID != "u_1" {
		t.Errorf("Get returned wrong entry: %+v", got)
	}

	r.Deregister("a_1")
	if _, ok := r.Get("a_1"); ok {
		t.Error("Get should miss after Deregister")
	}
}

// Two simultaneous registrations of the same agent_id is a
// programming error → ErrInUse. The second cancelFn is NOT registered.
//
// EMPIRICAL: removing the `if _, exists` guard in Register lets the
// second registration overwrite the first; the first cancelFn would
// be lost (cancel of agent_1 would fire only the second's cancelFn).
func TestRegistry_DuplicateActive_Conflict(t *testing.T) {
	r := NewRegistry()
	_, c1, fired1 := makeCancellable()
	_, c2, fired2 := makeCancellable()

	if err := r.Register(Entry{AgentID: "a_dup", RunID: "r_1"}, c1); err != nil {
		t.Fatal(err)
	}
	err := r.Register(Entry{AgentID: "a_dup", RunID: "r_2"}, c2)
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("second Register: got %v, want ErrInUse", err)
	}

	// Cancel — only the first should fire.
	r.Cancel("a_dup", "")
	if !fired1.Load() {
		t.Error("first cancelFn did not fire")
	}
	if fired2.Load() {
		t.Error("second cancelFn fired despite Register failure")
	}
}

// After deregistering, the same agent_id can be registered again.
// This is the legitimate "caller reused agent_id after termination"
// case — historical rows in the DB share the id, but only one is
// active at a time.
func TestRegistry_Deregister_AllowsReuse(t *testing.T) {
	r := NewRegistry()
	_, c1, _ := makeCancellable()
	_, c2, fired2 := makeCancellable()

	if err := r.Register(Entry{AgentID: "a_reuse"}, c1); err != nil {
		t.Fatal(err)
	}
	r.Deregister("a_reuse")
	if err := r.Register(Entry{AgentID: "a_reuse"}, c2); err != nil {
		t.Errorf("re-register after Deregister should succeed: %v", err)
	}
	r.Cancel("a_reuse", "")
	if !fired2.Load() {
		t.Error("second cancelFn should fire after re-register + cancel")
	}
}

// Cancel without a reason produces a context.Cause that is exactly
// ErrCancelledByAPI (so errors.Is matches). This is what server.finishRun
// uses to discriminate API-cancel from client-disconnect.
//
// EMPIRICAL: changing the cause in Cancel from ErrCancelledByAPI to
// any other sentinel makes errors.Is(..., ErrCancelledByAPI) return
// false — finishRun would write status=failed instead of cancelled.
func TestRegistry_Cancel_WithoutReason_AttachesAPICause(t *testing.T) {
	r := NewRegistry()
	ctx, cancelFn, _ := makeCancellable()
	_ = r.Register(Entry{AgentID: "a_x"}, cancelFn)

	r.Cancel("a_x", "")
	cause := context.Cause(ctx)
	if !errors.Is(cause, ErrCancelledByAPI) {
		t.Errorf("cause = %v; should match ErrCancelledByAPI via errors.Is", cause)
	}
}

// Cancel with a reason wraps the cause but keeps errors.Is matching
// AND surfaces the reason via ReasonFromCause. The HTTP server uses
// the latter to populate runs.stop_reason.
func TestRegistry_Cancel_WithReason_PreservesIsAndReason(t *testing.T) {
	r := NewRegistry()
	ctx, cancelFn, _ := makeCancellable()
	_ = r.Register(Entry{AgentID: "a_x"}, cancelFn)

	r.Cancel("a_x", "user_clicked_stop")
	cause := context.Cause(ctx)
	if !errors.Is(cause, ErrCancelledByAPI) {
		t.Errorf("errors.Is should still match the sentinel: cause=%v", cause)
	}
	if got := ReasonFromCause(cause); got != "user_clicked_stop" {
		t.Errorf("ReasonFromCause = %q, want user_clicked_stop", got)
	}
}

// Cancel of an unknown agent_id returns ok=false (not an error).
// The HTTP layer maps this to "look in the store for terminal state"
// rather than 404 unconditionally — a recently-finished run might have
// already deregistered.
func TestRegistry_Cancel_UnknownAgent_OkFalse(t *testing.T) {
	r := NewRegistry()
	res, ok := r.Cancel("a_does_not_exist", "")
	if ok {
		t.Errorf("ok = true for unknown agent_id, got result %+v", res)
	}
}

// Cascade: parent + 2 direct children + 1 grandchild. Cancel parent;
// every cancelFn in the tree fires; cascaded list contains all
// non-root agent_ids.
//
// EMPIRICAL: removing the recursive walk (the `for len(queue) > 0`
// loop body) leaves the grandchild's cancelFn unfired and absent
// from cascaded.
func TestRegistry_Cancel_CascadesTree(t *testing.T) {
	r := NewRegistry()
	_, parentCancel, parentFired := makeCancellable()
	_, child1Cancel, child1Fired := makeCancellable()
	_, child2Cancel, child2Fired := makeCancellable()
	_, grandCancel, grandFired := makeCancellable()

	_ = r.Register(Entry{AgentID: "a_parent"}, parentCancel)
	_ = r.Register(Entry{AgentID: "a_child1", ParentAgentID: "a_parent"}, child1Cancel)
	_ = r.Register(Entry{AgentID: "a_child2", ParentAgentID: "a_parent"}, child2Cancel)
	_ = r.Register(Entry{AgentID: "a_grand", ParentAgentID: "a_child1"}, grandCancel)

	res, ok := r.Cancel("a_parent", "shutdown")
	if !ok {
		t.Fatal("Cancel returned ok=false for known parent")
	}
	if !parentFired.Load() || !child1Fired.Load() || !child2Fired.Load() || !grandFired.Load() {
		t.Errorf("not every cancelFn fired: parent=%v c1=%v c2=%v gc=%v",
			parentFired.Load(), child1Fired.Load(), child2Fired.Load(), grandFired.Load())
	}
	wantCascaded := map[string]bool{"a_child1": true, "a_child2": true, "a_grand": true}
	if len(res.Cascaded) != 3 {
		t.Errorf("cascaded len = %d (want 3): %v", len(res.Cascaded), res.Cascaded)
	}
	for _, id := range res.Cascaded {
		if !wantCascaded[id] {
			t.Errorf("unexpected cascaded id: %s", id)
		}
		delete(wantCascaded, id)
	}
	if len(wantCascaded) > 0 {
		t.Errorf("missing from cascaded: %v", wantCascaded)
	}

	// Registry should be empty now (cascade deregistered all).
	if r.Count() != 0 {
		t.Errorf("registry not drained after cascade: %d entries left", r.Count())
	}
}

// Concurrent Cancels of the same agent_id: exactly one returns
// (ok=true), all others return (ok=false). The cancelFn fires
// exactly once. This guards the idempotency claim in the docs.
func TestRegistry_Cancel_ConcurrentIsIdempotent(t *testing.T) {
	r := NewRegistry()
	var fireCount atomic.Int32
	_, cancelFn, _ := makeCancellable()
	wrapped := func(cause error) {
		fireCount.Add(1)
		cancelFn(cause)
	}
	_ = r.Register(Entry{AgentID: "a_c"}, wrapped)

	const N = 20
	var wg sync.WaitGroup
	var oks atomic.Int32
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := r.Cancel("a_c", ""); ok {
				oks.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := oks.Load(); got != 1 {
		t.Errorf("ok count = %d, want exactly 1", got)
	}
	if got := fireCount.Load(); got != 1 {
		t.Errorf("cancelFn fired %d times, want 1", got)
	}
}

// ListByUser returns only entries with the matching UserID.
func TestRegistry_ListByUser(t *testing.T) {
	r := NewRegistry()
	_, c1, _ := makeCancellable()
	_, c2, _ := makeCancellable()
	_, c3, _ := makeCancellable()

	_ = r.Register(Entry{AgentID: "a_a1", UserID: "alice"}, c1)
	_ = r.Register(Entry{AgentID: "a_a2", UserID: "alice"}, c2)
	_ = r.Register(Entry{AgentID: "a_b1", UserID: "bob"}, c3)

	alices := r.ListByUser("alice")
	if len(alices) != 2 {
		t.Errorf("alice has %d entries, want 2", len(alices))
	}
	for _, e := range alices {
		if e.UserID != "alice" {
			t.Errorf("non-alice entry leaked: %+v", e)
		}
	}

	if got := r.ListByUser(""); got != nil {
		t.Errorf("empty userID should return nil, got %d", len(got))
	}
}

// ListChildren returns direct children only — parity with the SQL
// ListRunsByParentAgentID. Recursion is the caller's job.
func TestRegistry_ListChildren_DirectOnly(t *testing.T) {
	r := NewRegistry()
	_, c1, _ := makeCancellable()
	_, c2, _ := makeCancellable()
	_, c3, _ := makeCancellable()
	_ = r.Register(Entry{AgentID: "a_p"}, c1)
	_ = r.Register(Entry{AgentID: "a_c1", ParentAgentID: "a_p"}, c2)
	_ = r.Register(Entry{AgentID: "a_gc", ParentAgentID: "a_c1"}, c3)

	got := r.ListChildren("a_p")
	if len(got) != 1 || got[0].AgentID != "a_c1" {
		t.Errorf("ListChildren(a_p) = %+v, want only [a_c1]", got)
	}
}

// Empty agent_id is a programming error in Register — refused with a
// descriptive error rather than silently storing under "".
func TestRegistry_Register_RejectsEmptyInputs(t *testing.T) {
	r := NewRegistry()
	_, cancelFn, _ := makeCancellable()

	if err := r.Register(Entry{AgentID: ""}, cancelFn); err == nil {
		t.Error("expected error on empty AgentID")
	}
	if err := r.Register(Entry{AgentID: "a_x"}, nil); err == nil {
		t.Error("expected error on nil cancelFn")
	}
}

// TestRegistry_ListAll returns a snapshot of every live entry,
// regardless of user. Deregistered entries vanish from the snapshot.
func TestRegistry_ListAll(t *testing.T) {
	r := NewRegistry()
	_, c1, _ := makeCancellable()
	_, c2, _ := makeCancellable()
	_, c3, _ := makeCancellable()

	if err := r.Register(Entry{AgentID: "a_1", RunID: "r_1", UserID: "alice"}, c1); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Entry{AgentID: "a_2", RunID: "r_2", UserID: "bob"}, c2); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Entry{AgentID: "a_3", RunID: "r_3", UserID: "alice"}, c3); err != nil {
		t.Fatal(err)
	}

	got := r.ListAll()
	if len(got) != 3 {
		t.Fatalf("len(ListAll()) = %d, want 3", len(got))
	}
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.AgentID] = true
	}
	for _, want := range []string{"a_1", "a_2", "a_3"} {
		if !seen[want] {
			t.Errorf("ListAll() missing agent %q", want)
		}
	}

	// Deregister one and confirm the snapshot reflects the change.
	r.Deregister("a_2")
	got = r.ListAll()
	if len(got) != 2 {
		t.Errorf("after Deregister, len(ListAll()) = %d, want 2", len(got))
	}
	for _, e := range got {
		if e.AgentID == "a_2" {
			t.Errorf("deregistered a_2 still in ListAll()")
		}
	}
}

// TestRegistry_ListAll_Empty returns a non-nil empty slice on a
// fresh registry. (Some callers may iterate via range; a nil slice
// also iterates safely, but documenting the contract.)
func TestRegistry_ListAll_Empty(t *testing.T) {
	r := NewRegistry()
	got := r.ListAll()
	if len(got) != 0 {
		t.Errorf("fresh registry returned %d entries, want 0", len(got))
	}
}
