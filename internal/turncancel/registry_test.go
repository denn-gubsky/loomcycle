package turncancel

import (
	"context"
	"errors"
	"testing"
)

var errTest = errors.New("turn cancelled by operator")

// TestRegistry_CancelFiresArmedTokenWithCause verifies Arm→Cancel delivers the
// cause to the armed context and reports that a token was armed.
func TestRegistry_CancelFiresArmedTokenWithCause(t *testing.T) {
	r := NewRegistry()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	r.Arm("run-1", cancel)

	if !r.IsArmed("run-1") {
		t.Fatal("IsArmed false right after Arm")
	}
	if got := r.Cancel("run-1", errTest); !got {
		t.Fatal("Cancel returned false for an armed run")
	}
	if err := ctx.Err(); err == nil {
		t.Fatal("armed context not cancelled after Cancel")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, errTest) {
		t.Fatalf("cause = %v, want %v", cause, errTest)
	}
}

// TestRegistry_CancelUnarmedReturnsFalse verifies a Cancel with no armed token
// (never armed / already fired / already disarmed) reports false so the handler
// can 409 it.
func TestRegistry_CancelUnarmedReturnsFalse(t *testing.T) {
	r := NewRegistry()
	if r.Cancel("missing", errTest) {
		t.Fatal("Cancel returned true for an unarmed run")
	}
}

// TestRegistry_CancelIsIdempotent verifies a second Cancel after the token fired
// finds nothing armed — a repeated stop click / a cancel racing the turn end is
// a no-op, not a double-fire.
func TestRegistry_CancelIsIdempotent(t *testing.T) {
	r := NewRegistry()
	_, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	r.Arm("run-1", cancel)

	if !r.Cancel("run-1", errTest) {
		t.Fatal("first Cancel returned false")
	}
	if r.IsArmed("run-1") {
		t.Fatal("token still armed after Cancel")
	}
	if r.Cancel("run-1", errTest) {
		t.Fatal("second Cancel returned true (not idempotent)")
	}
}

// TestRegistry_DisarmClearsArmedState verifies the disarm returned by Arm removes
// the token so a subsequent Cancel is a no-op (the loop disarms before parking).
func TestRegistry_DisarmClearsArmedState(t *testing.T) {
	r := NewRegistry()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	disarm := r.Arm("run-1", cancel)

	disarm()
	if r.IsArmed("run-1") {
		t.Fatal("IsArmed true after disarm")
	}
	if r.Cancel("run-1", errTest) {
		t.Fatal("Cancel fired after disarm")
	}
	if ctx.Err() != nil {
		t.Fatal("context cancelled despite disarm (no Cancel)")
	}
}

// TestRegistry_StaleDisarmDoesNotClobberReArmedToken verifies the core
// generation invariant: after a turn re-arms, the PREVIOUS turn's disarm must not
// delete the new turn's live token (a missed disarm can't disable the run).
func TestRegistry_StaleDisarmDoesNotClobberReArmedToken(t *testing.T) {
	r := NewRegistry()
	_, cancel1 := context.WithCancelCause(context.Background())
	defer cancel1(nil)
	disarm1 := r.Arm("run-1", cancel1)

	// Next turn re-arms without the caller having disarmed turn 1 (overwrite).
	ctx2, cancel2 := context.WithCancelCause(context.Background())
	defer cancel2(nil)
	r.Arm("run-1", cancel2)

	// The stale disarm from turn 1 must be a no-op.
	disarm1()
	if !r.IsArmed("run-1") {
		t.Fatal("stale disarm cleared the re-armed token")
	}
	if !r.Cancel("run-1", errTest) {
		t.Fatal("Cancel found no armed token after re-arm")
	}
	if context.Cause(ctx2) == nil || !errors.Is(context.Cause(ctx2), errTest) {
		t.Fatalf("re-armed token (turn 2) not the one fired: cause=%v", context.Cause(ctx2))
	}
}

// TestRegistry_RunsAreIndependent verifies tokens are keyed by run_id — cancelling
// one run does not disturb another.
func TestRegistry_RunsAreIndependent(t *testing.T) {
	r := NewRegistry()
	ctxA, cancelA := context.WithCancelCause(context.Background())
	defer cancelA(nil)
	ctxB, cancelB := context.WithCancelCause(context.Background())
	defer cancelB(nil)
	r.Arm("run-a", cancelA)
	r.Arm("run-b", cancelB)

	r.Cancel("run-a", errTest)
	if ctxA.Err() == nil {
		t.Fatal("run-a not cancelled")
	}
	if ctxB.Err() != nil {
		t.Fatal("run-b cancelled by a run-a cancel")
	}
	if !r.IsArmed("run-b") {
		t.Fatal("run-b disarmed by a run-a cancel")
	}
}
