package auditchain_test

import (
	"context"
	"database/sql"
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
