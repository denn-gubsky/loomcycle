package auditchain_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auditchain"
)

// savedHead is a tenant head as a database writer could keep it: from a
// backup, a replica, or the trailer of an export.
type savedHead struct {
	seq       int64
	hash, mac string
}

func saveHead(t *testing.T, db *sql.DB, tenant string) savedHead {
	t.Helper()
	var h savedHead
	if err := db.QueryRow(`SELECT seq, hash, mac FROM audit_heads WHERE tenant_id = ?`, tenant).Scan(&h.seq, &h.hash, &h.mac); err != nil {
		t.Fatal(err)
	}
	return h
}

// rollBack puts a tenant chain back to an earlier, genuinely signed head:
// every row past it deleted, the head restored. Each remaining row and the
// head still verify on their own.
func rollBack(t *testing.T, db *sql.DB, tenant string, h savedHead) {
	t.Helper()
	exec(t, db, `DELETE FROM audit_events WHERE tenant_id = ? AND seq > ?`, tenant, h.seq)
	exec(t, db, `UPDATE audit_heads SET seq = ?, hash = ?, mac = ? WHERE tenant_id = ?`, h.seq, h.hash, h.mac, tenant)
}

func record(t *testing.T, sink audit.Sink, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := sink.Record(audit.Event{Action: "acl_written", TargetTenant: "t1", ActorSubject: "alice",
			TargetName: "acl:" + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
}

// A clean shutdown checkpoints each chain it wrote, so a rollback of a
// tenant chain past it fails verification — even though every row and the
// restored head still carry valid signatures.
func TestAuditVerify_ARollbackPastTheShutdownCheckpointIsABreak(t *testing.T) {
	s, db := chainFixture(t, 0)
	sink, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	record(t, sink, 3)
	old := saveHead(t, db, "t1")
	record(t, sink, 3) // erase-intent rows, a token mint... what a rollback would hide
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rep := verify(t, s, testKeys); !rep.OK {
		t.Fatalf("the untouched chain: %+v", rep)
	}

	rollBack(t, db, "t1", old)

	rep := verify(t, s, testKeys)
	if rep.OK {
		t.Fatalf("a chain rolled back to an earlier signed head verifies: %+v", rep)
	}
	if !strings.Contains(rep.Reason, "checkpoint") {
		t.Errorf("break %q does not name the checkpoint", rep.Reason)
	}
}

// Rows written since the last checkpoint are what a rollback can still
// remove undetected; the row count bounds them.
func TestAuditVerify_ARollbackPastARowCountCheckpointIsABreak(t *testing.T) {
	s, db := chainFixture(t, 0)
	sink, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	sink.SetCheckpointPolicy(4, time.Hour)
	record(t, sink, 2)
	old := saveHead(t, db, "t1")
	record(t, sink, 2) // the 4th row checkpoints seq 4

	rollBack(t, db, "t1", old)

	rep := verify(t, s, testKeys)
	if rep.OK || !strings.Contains(rep.Reason, "checkpoint") {
		t.Fatalf("a rollback past the row-count checkpoint: %+v", rep)
	}
	if rep.BreakSeq != old.seq+1 {
		t.Errorf("break at %d, want %d (the first row removed)", rep.BreakSeq, old.seq+1)
	}
}

// A rollback that stays at or past the last checkpoint is the stated limit;
// an untouched chain with checkpoints verifies, and so does every chain
// after more appends.
func TestAuditVerify_CheckpointsKeepAnUntouchedChainVerifiable(t *testing.T) {
	s, _ := chainFixture(t, 0)
	sink, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	sink.SetCheckpointPolicy(2, time.Hour)
	record(t, sink, 7)
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rep := verify(t, s, testKeys); !rep.OK || rep.Rows != 7 {
		t.Fatalf("an untouched chain with checkpoints: %+v", rep)
	}
	op, err := auditchain.Verify(context.Background(), s, testKeys, "")
	if err != nil || !op.OK {
		t.Fatalf("the operator chain: %+v, %v", op, err)
	}
}

// A retention prune of the operator chain removes checkpoint rows; their
// signed anchor carries the newest per tenant, so a rollback is still caught.
func TestAuditVerify_CheckpointsSurviveTheOperatorChainsPrune(t *testing.T) {
	s, db := chainFixture(t, 0)
	sink, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	sink.SetCheckpointPolicy(2, time.Hour)
	record(t, sink, 2)
	old := saveHead(t, db, "t1")
	record(t, sink, 2)
	if n, err := pruneForTest(context.Background(), s, "", time.Now().Add(time.Hour)); err != nil || n == 0 {
		t.Fatalf("prune the operator chain: %d, %v", n, err)
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE tenant_id = ''`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("operator rows left after the prune: %d, %v", left, err)
	}

	rollBack(t, db, "t1", old)

	if rep := verify(t, s, testKeys); rep.OK || !strings.Contains(rep.Reason, "checkpoint") {
		t.Fatalf("a rollback after the operator chain was pruned: %+v", rep)
	}
}

// A tenant chain whose row at a checkpoint's position was replaced — the
// chain rebuilt from there under the key — no longer holds the checkpointed
// hash.
func TestAuditVerify_ARowThatDiffersFromItsCheckpointIsABreak(t *testing.T) {
	s, db := chainFixture(t, 0)
	sink, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	sink.SetCheckpointPolicy(3, time.Hour)
	record(t, sink, 2)
	old := saveHead(t, db, "t1")
	record(t, sink, 1) // checkpoints seq 3
	rollBack(t, db, "t1", old)
	// The key holder (or a replica with it) appends a different row 3.
	other, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Record(audit.Event{Action: "auth_login", TargetTenant: "t1"}); err != nil {
		t.Fatal(err)
	}
	rep := verify(t, s, testKeys)
	if rep.OK || rep.BreakSeq != 3 || !strings.Contains(rep.Reason, "checkpoint") {
		t.Fatalf("a row replaced at a checkpoint's position: %+v", rep)
	}
}

// After a rollback the running deployment goes on appending to the rolled-
// back head, and in time checkpoints that (forged) history too. The newer
// checkpoint must not stand in for the older, genuine one: every checkpoint
// whose row is still in the chain is compared.
func TestAuditVerify_ALaterCheckpointDoesNotLaunderARollback(t *testing.T) {
	s, db := chainFixture(t, 0)
	sink, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	sink.SetCheckpointPolicy(4, time.Hour)
	record(t, sink, 2)
	old := saveHead(t, db, "t1")
	record(t, sink, 2) // checkpoints seq 4
	rollBack(t, db, "t1", old)
	// A replica that never saw the rolled-back rows goes on writing; its
	// checkpoint at seq 6 covers the forged history.
	other, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	other.SetCheckpointPolicy(4, time.Hour)
	record(t, other, 4)
	rep := verify(t, s, testKeys)
	if rep.OK || rep.BreakSeq != 4 || !strings.Contains(rep.Reason, "checkpoint") {
		t.Fatalf("a rollback followed by a newer checkpoint: %+v", rep)
	}
}

// A retention prune of the tenant chain past a checkpoint must not sign an
// anchor over history that contradicts it: the prune is refused, and the
// break stays visible to Verify.
func TestAuditPrune_RefusesToFoldARollbackPastACheckpoint(t *testing.T) {
	s, db := chainFixture(t, 0)
	sink, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	sink.SetCheckpointPolicy(4, time.Hour)
	record(t, sink, 2)
	old := saveHead(t, db, "t1")
	record(t, sink, 2) // checkpoints seq 4
	rollBack(t, db, "t1", old)
	other, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	record(t, other, 4)

	if _, err := pruneForTest(context.Background(), s, "t1", time.Now().Add(time.Hour)); !errors.Is(err, auditchain.ErrChainBroken) {
		t.Fatalf("prune over a rolled-back chain: err = %v, want ErrChainBroken", err)
	}
	if rep := verify(t, s, testKeys); rep.OK {
		t.Fatalf("the rollback verifies after the refused prune: %+v", rep)
	}
}

// Checkpoint, prune, then a rollback to just after the anchor: the newest
// checkpoint lies past the restored head.
func TestAuditVerify_ARollbackToJustAfterAnAnchorIsABreak(t *testing.T) {
	s, db := chainFixture(t, 0)
	sink, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	sink.SetCheckpointPolicy(4, time.Hour)
	record(t, sink, 6)   // checkpoints seq 4
	cutoff := time.Now() // the rows recorded after it stay
	if n, err := pruneForTest(context.Background(), s, "t1", cutoff); err != nil || n != 6 {
		t.Fatalf("prune: %d, %v", n, err)
	}
	record(t, sink, 1)
	old := saveHead(t, db, "t1")
	record(t, sink, 1) // checkpoints seq 8
	rollBack(t, db, "t1", old)
	if rep := verify(t, s, testKeys); rep.OK || rep.BreakSeq != 8 || !strings.Contains(rep.Reason, "checkpoint") {
		t.Fatalf("a rollback to just after the anchor: %+v", rep)
	}
}

// An anchor sealed before checkpoints existed (released versions) carries no
// checkpoints key: its state must still decode, verify and be pruned past.
// The state below is byte-for-byte what those versions sealed for an
// operator chain holding one opening.
func TestAuditVerify_AnAnchorSealedBeforeCheckpointsVerifiesAndPrunes(t *testing.T) {
	s, db := chainFixture(t, 3) // no checkpoint: under the row count, never closed
	ctx := context.Background()
	cutoff := time.Now()
	for _, tenant := range []string{"", "t1"} {
		if n, err := pruneForTest(ctx, s, tenant, cutoff); err != nil || n == 0 {
			t.Fatalf("prune %q: %d, %v", tenant, n, err)
		}
	}
	var state string
	if err := db.QueryRow(`SELECT anchor_state FROM audit_heads WHERE tenant_id = ''`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != `{"opened":{"t1":1},"rotated":true}` {
		t.Fatalf("operator anchor state = %s, want the released shape", state)
	}
	sink, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	record(t, sink, 2)
	for _, tenant := range []string{"", "t1"} {
		if rep, err := auditchain.Verify(ctx, s, testKeys, tenant); err != nil || !rep.OK {
			t.Fatalf("verify %q over a released-shape anchor: %+v, %v", tenant, rep, err)
		}
		if _, err := pruneForTest(ctx, s, tenant, time.Now()); err != nil {
			t.Fatalf("prune %q past a released-shape anchor: %v", tenant, err)
		}
	}
}

// The operator chain's prune carries checkpoints forward in its anchor. It
// must not drop one a rollback contradicts: roll a tenant back, let the
// deployment write past 64 newer checkpoints over the forged history, prune
// the operator chain, and the genuine checkpoint must still catch it.
func TestAuditVerify_CarryKeepsTheCheckpointARollbackContradicts(t *testing.T) {
	s, db := chainFixture(t, 0)
	sink, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	sink.SetCheckpointPolicy(1, time.Hour)
	record(t, sink, 2)
	old := saveHead(t, db, "t1")
	record(t, sink, 2) // genuine checkpoints at 3 and 4
	rollBack(t, db, "t1", old)
	other, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	other.SetCheckpointPolicy(1, time.Hour)
	for i := 0; i < 70; i++ { // 70 checkpoints of the forged history
		record(t, other, 1)
	}
	if n, err := pruneForTest(context.Background(), s, "", time.Now()); err != nil || n == 0 {
		t.Fatalf("prune the operator chain: %d, %v", n, err)
	}
	if rep := verify(t, s, testKeys); rep.OK {
		t.Fatalf("a rollback after the operator chain's prune dropped its checkpoint: %+v", rep)
	}
}

// Carried checkpoints are dropped once the tenant chain's own signed anchor
// has passed them (the tenant prune that moved it checked them), so the
// operator anchor's state stays bounded by retention.
func TestAuditPrune_CarryDropsOnlyCheckpointsBelowTheTenantAnchor(t *testing.T) {
	s, db := chainFixture(t, 0)
	sink, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	sink.SetCheckpointPolicy(2, time.Hour)
	record(t, sink, 6) // checkpoints at 2, 4 and 6
	cutoff := time.Now()
	record(t, sink, 2) // checkpoint at 8, after the cutoff
	if n, err := pruneForTest(context.Background(), s, "t1", cutoff); err != nil || n != 6 {
		t.Fatalf("prune t1: %d, %v", n, err)
	}
	if n, err := pruneForTest(context.Background(), s, "", time.Now()); err != nil || n == 0 {
		t.Fatalf("prune the operator chain: %d, %v", n, err)
	}
	var state string
	if err := db.QueryRow(`SELECT anchor_state FROM audit_heads WHERE tenant_id = ''`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	var st struct {
		Checkpoints map[string][]auditchain.Checkpoint `json:"checkpoints"`
	}
	if err := json.Unmarshal([]byte(state), &st); err != nil {
		t.Fatal(err)
	}
	var seqs []int64
	for _, cp := range st.Checkpoints["t1"] {
		seqs = append(seqs, cp.Seq)
	}
	if len(seqs) != 2 || seqs[0] != 6 || seqs[1] != 8 {
		t.Errorf("carried checkpoints of t1 at %v, want [6 8] (at or above its anchor at 6)", seqs)
	}
	if rep := verify(t, s, testKeys); !rep.OK {
		t.Errorf("t1 after both prunes: %+v", rep)
	}
}

// A shutdown whose drain runs out of time still checkpoints the chains.
func TestAuditStoreSink_CloseCheckpointsEvenWhenTheDrainTimesOut(t *testing.T) {
	s, db := chainFixture(t, 0)
	sink, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	sink.StartAsync(8)
	record(t, sink, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the drain's budget is already spent
	_ = sink.Close(ctx)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE tenant_id = '' AND action = 'audit_chain_checkpoint'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("checkpoints after a timed-out drain = %d, %v; want 1", n, err)
	}
}
