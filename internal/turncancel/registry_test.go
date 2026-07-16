package turncancel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

var errTest = errors.New("turn cancelled by operator")

// causeForTest mirrors loop.TurnCancelCause: it wraps errTest so errors.Is holds
// and the reason string is recoverable, without importing internal/loop.
func causeForTest(reason string) error {
	if reason == "" {
		return errTest
	}
	return fmt.Errorf("%w: %s", errTest, reason)
}

func newReg() *Registry {
	r := NewRegistry()
	r.SetCauseFor(causeForTest)
	return r
}

// fakeCluster is a stub ClusterCanceller recording the delegated CancelRemote.
type fakeCluster struct {
	calls  int
	runID  string
	reason string
	found  bool
	err    error
}

func (f *fakeCluster) CancelRemote(_ context.Context, runID, reason string) (bool, error) {
	f.calls++
	f.runID, f.reason = runID, reason
	return f.found, f.err
}

// TestRegistry_CancelLocalFiresArmedTokenWithCause verifies Arm→CancelLocal
// delivers the causeFor-built cause (carrying the reason) to the armed context
// and reports that a token was armed.
func TestRegistry_CancelLocalFiresArmedTokenWithCause(t *testing.T) {
	r := newReg()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	r.Arm("run-1", cancel)

	if !r.IsArmed("run-1") {
		t.Fatal("IsArmed false right after Arm")
	}
	if got := r.CancelLocal("run-1", "too slow"); !got {
		t.Fatal("CancelLocal returned false for an armed run")
	}
	if err := ctx.Err(); err == nil {
		t.Fatal("armed context not cancelled after CancelLocal")
	}
	cause := context.Cause(ctx)
	if !errors.Is(cause, errTest) {
		t.Fatalf("cause = %v, want wrap of errTest", cause)
	}
	if !strings.Contains(cause.Error(), "too slow") {
		t.Fatalf("cause %q did not carry the operator reason", cause.Error())
	}
}

// TestRegistry_CancelLocalUnarmedReturnsFalse verifies a CancelLocal with no
// armed token (never armed / already fired / already disarmed) reports false so
// the handler can 409 it.
func TestRegistry_CancelLocalUnarmedReturnsFalse(t *testing.T) {
	r := newReg()
	if r.CancelLocal("missing", "x") {
		t.Fatal("CancelLocal returned true for an unarmed run")
	}
}

// TestRegistry_CancelLocalIsIdempotent verifies a second CancelLocal after the
// token fired finds nothing armed — a repeated stop click / a cancel racing the
// turn end is a no-op, not a double-fire.
func TestRegistry_CancelLocalIsIdempotent(t *testing.T) {
	r := newReg()
	_, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	r.Arm("run-1", cancel)

	if !r.CancelLocal("run-1", "") {
		t.Fatal("first CancelLocal returned false")
	}
	if r.IsArmed("run-1") {
		t.Fatal("token still armed after CancelLocal")
	}
	if r.CancelLocal("run-1", "") {
		t.Fatal("second CancelLocal returned true (not idempotent)")
	}
}

// TestRegistry_DisarmClearsArmedState verifies the disarm returned by Arm removes
// the token so a subsequent CancelLocal is a no-op (the loop disarms before parking).
func TestRegistry_DisarmClearsArmedState(t *testing.T) {
	r := newReg()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	disarm := r.Arm("run-1", cancel)

	disarm()
	if r.IsArmed("run-1") {
		t.Fatal("IsArmed true after disarm")
	}
	if r.CancelLocal("run-1", "") {
		t.Fatal("CancelLocal fired after disarm")
	}
	if ctx.Err() != nil {
		t.Fatal("context cancelled despite disarm (no CancelLocal)")
	}
}

// TestRegistry_StaleDisarmDoesNotClobberReArmedToken verifies the core
// generation invariant: after a turn re-arms, the PREVIOUS turn's disarm must not
// delete the new turn's live token (a missed disarm can't disable the run).
func TestRegistry_StaleDisarmDoesNotClobberReArmedToken(t *testing.T) {
	r := newReg()
	ctx1, cancel1 := context.WithCancelCause(context.Background())
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
	if !r.CancelLocal("run-1", "") {
		t.Fatal("CancelLocal found no armed token after re-arm")
	}
	// Turn 2's ctx is the one fired; turn 1's is left untouched.
	if ctx2.Err() == nil || !errors.Is(context.Cause(ctx2), errTest) {
		t.Fatalf("re-armed token (turn 2) not the one fired: cause=%v", context.Cause(ctx2))
	}
	if ctx1.Err() != nil {
		t.Fatal("turn 1 ctx fired despite being overwritten")
	}
}

// TestRegistry_RunsAreIndependent verifies tokens are keyed by run_id — cancelling
// one run does not disturb another.
func TestRegistry_RunsAreIndependent(t *testing.T) {
	r := newReg()
	ctxA, cancelA := context.WithCancelCause(context.Background())
	defer cancelA(nil)
	ctxB, cancelB := context.WithCancelCause(context.Background())
	defer cancelB(nil)
	r.Arm("run-a", cancelA)
	r.Arm("run-b", cancelB)

	r.CancelLocal("run-a", "")
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

// TestRegistry_CancelLocalRefusesWithoutCauseFor verifies the fail-safe: a
// registry with no causeFor wired refuses to fire (a wrong/nil cause would
// terminate the run) and LEAVES the token armed for a later, correctly-wired fire.
func TestRegistry_CancelLocalRefusesWithoutCauseFor(t *testing.T) {
	r := NewRegistry() // no SetCauseFor
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	r.Arm("run-1", cancel)

	if r.CancelLocal("run-1", "x") {
		t.Fatal("CancelLocal fired without a causeFor")
	}
	if ctx.Err() != nil {
		t.Fatal("context cancelled despite no causeFor")
	}
	if !r.IsArmed("run-1") {
		t.Fatal("token dropped without firing (must stay armed for a later fire)")
	}
}

// TestRegistry_CancelDelegatesToClusterOnLocalMiss verifies the P3a delegation:
// a Cancel that misses the local armed map routes to the ClusterCanceller (the
// owning-replica route) and returns its found result.
func TestRegistry_CancelDelegatesToClusterOnLocalMiss(t *testing.T) {
	r := newReg()
	fc := &fakeCluster{found: true}
	r.SetClusterCanceller(fc)

	fired, err := r.Cancel(context.Background(), "remote-run", "operator stop")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !fired {
		t.Fatal("Cancel did not report the remote fire")
	}
	if fc.calls != 1 || fc.runID != "remote-run" || fc.reason != "operator stop" {
		t.Fatalf("CancelRemote calls=%d runID=%q reason=%q, want 1/remote-run/operator stop", fc.calls, fc.runID, fc.reason)
	}
}

// TestRegistry_CancelSingleProcessLocalMissIsFalse verifies byte-identical P1
// behaviour: with no ClusterCanceller wired, a local miss returns (false, nil)
// and never touches the cluster path.
func TestRegistry_CancelSingleProcessLocalMissIsFalse(t *testing.T) {
	r := newReg()
	fired, err := r.Cancel(context.Background(), "ghost", "x")
	if fired || err != nil {
		t.Fatalf("Cancel(ghost) = (%v,%v), want (false,nil)", fired, err)
	}
}

// TestRegistry_CancelLocalHitDoesNotDelegate verifies a locally-armed run fires
// LOCALLY and never reaches the cluster (no needless remote round-trip).
func TestRegistry_CancelLocalHitDoesNotDelegate(t *testing.T) {
	r := newReg()
	fc := &fakeCluster{found: true}
	r.SetClusterCanceller(fc)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	r.Arm("run-1", cancel)

	fired, err := r.Cancel(context.Background(), "run-1", "stop")
	if err != nil || !fired {
		t.Fatalf("Cancel(local) = (%v,%v), want (true,nil)", fired, err)
	}
	if fc.calls != 0 {
		t.Fatalf("cluster CancelRemote called %d times for a local hit, want 0", fc.calls)
	}
	if ctx.Err() == nil {
		t.Fatal("local armed token not fired")
	}
}

// TestRegistry_CancelPropagatesClusterError verifies a cluster error surfaces to
// the caller (the handler maps it to 500) rather than being swallowed as a miss.
func TestRegistry_CancelPropagatesClusterError(t *testing.T) {
	r := newReg()
	boom := errors.New("backplane down")
	r.SetClusterCanceller(&fakeCluster{err: boom})
	fired, err := r.Cancel(context.Background(), "remote-run", "")
	if fired {
		t.Fatal("Cancel reported fired on a cluster error")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}
