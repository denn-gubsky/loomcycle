// Package chaintest is the store-backed audit chain's contract, run against
// both store backends from their own tests. It lives with the chain rather
// than in storetest: the chain sits on the store, so storetest importing it
// would point the layering the wrong way.
package chaintest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auditchain"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ReplicaFactory opens TWO handles on one fresh, empty database — what two
// replicas of a deployment hold — and a cleanup that releases both.
type ReplicaFactory func(t *testing.T) (a, b store.Store, cleanup func())

// RunAudit runs the store-backed audit stream contract (RFC FZ Part E).
func RunAudit(t *testing.T, factory ReplicaFactory) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(*testing.T, store.Store, store.Store)
	}{
		{"EveryReplicaWritesOneChainPerTenant", testAuditEveryReplicaWritesOneChainPerTenant},
		{"PruneKeepsTheChainVerifiable", testAuditPruneKeepsTheChainVerifiable},
		{"HeadAndListRoundTrip", testAuditHeadAndListRoundTrip},
		{"UnsafeTextIsStoredEscapedAndHashed", testAuditUnsafeTextIsStoredEscapedAndHashed},
		{"PruneHandsTheSealEveryRowAndAbortsOnItsError", testAuditPruneHandsTheSealEveryRowAndAbortsOnItsError},
		{"PruneToTheHeadReSignsTheHead", testAuditPruneToTheHeadReSignsTheHead},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b, cleanup := factory(t)
			defer cleanup()
			tc.fn(t, a, b)
		})
	}
}

var auditTestKeys = auditchain.ChainKeys{Current: []byte("0123456789abcdef0123456789abcdef")}

// Two replicas appending to the same tenants at once — each through its own
// handle, so nothing in-process serializes them — must leave one unbroken
// chain per tenant holding every event: no fork (two rows on one head), no
// gap, no lost append.
func testAuditEveryReplicaWritesOneChainPerTenant(t *testing.T, a, b store.Store) {
	ctx := context.Background()
	const perWriter = 15
	tenants := []string{"t1", "t2", ""}
	sinks := make([]audit.Sink, 0, 2)
	for _, s := range []store.Store{a, b} {
		sink, err := auditchain.NewStoreSink(s, auditTestKeys)
		if err != nil {
			t.Fatal(err)
		}
		sinks = append(sinks, sink)
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(sinks)*len(tenants)*2*perWriter)
	for si, sink := range sinks {
		for _, tenant := range tenants {
			for w := 0; w < 2; w++ {
				wg.Add(1)
				go func(sink audit.Sink, si, w int, tenant string) {
					defer wg.Done()
					for i := 0; i < perWriter; i++ {
						if err := sink.Record(audit.Event{Action: "auth_login", ActorTenant: tenant,
							ActorSubject: fmt.Sprintf("r%d-w%d-%d", si, w, i)}); err != nil {
							errs <- err
							return
						}
					}
				}(sink, si, w, tenant)
			}
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("append: %v", err)
	}
	for _, tenant := range tenants {
		want := int64(len(sinks) * 2 * perWriter)
		if tenant == "" {
			// Plus one opening record per tenant chain, written exactly once
			// however many replicas raced to the first append.
			want += 2
		}
		for _, s := range []store.Store{a, b} {
			rep, err := auditchain.Verify(ctx, s, auditTestKeys, tenant)
			if err != nil {
				t.Fatalf("verify %q: %v", tenant, err)
			}
			if !rep.OK || rep.Rows != want || rep.HeadSeq != want {
				t.Errorf("tenant %q: report %+v, want an unbroken chain of %d rows", tenant, rep, want)
			}
		}
	}
	reg, op, err := auditchain.ReadRegistry(ctx, b, auditTestKeys)
	if err != nil || !op.OK {
		t.Fatalf("ReadRegistry: %+v, %v", op, err)
	}
	if reg.Openings["t1"] != 1 || reg.Openings["t2"] != 1 || len(reg.Openings) != 2 {
		t.Errorf("chain openings = %v, want t1 and t2 once each", reg.Openings)
	}
	got, err := a.AuditTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint([]string{"", "t1", "t2"}) {
		t.Errorf("AuditTenants = %q", got)
	}
}

// A retention prune removes the old rows and leaves a signed anchor, so the
// rows left — and rows appended after — still verify from it.
func testAuditPruneKeepsTheChainVerifiable(t *testing.T, a, b store.Store) {
	ctx := context.Background()
	appendAt := func(s store.Store, ts time.Time, subject string) {
		t.Helper()
		sink, err := auditchain.NewStoreSink(s, auditTestKeys)
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Record(audit.Event{TS: ts, Action: "acl_edit", ActorTenant: "t1", ActorSubject: subject}); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)
	for i := 0; i < 4; i++ {
		appendAt(a, old.Add(time.Duration(i)*time.Hour), fmt.Sprintf("old-%d", i))
	}
	recent := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		appendAt(b, recent.Add(time.Duration(i)*time.Hour), fmt.Sprintf("new-%d", i))
	}
	n, err := auditchain.Prune(ctx, b, auditTestKeys, "t1", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || n != 4 {
		t.Fatalf("AuditPrune = %d, %v; want 4 rows removed", n, err)
	}
	appendAt(a, recent.Add(24*time.Hour), "after-prune")
	rep, err := auditchain.Verify(ctx, a, auditTestKeys, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK || rep.AnchorSeq != 4 || rep.HeadSeq != 8 || rep.Rows != 4 {
		t.Errorf("after prune: %+v, want OK from anchor 4 to head 8 over 4 rows", rep)
	}
	// Pruning what is left (every row older than now) leaves an empty chain
	// whose anchor is its head, and that still verifies.
	if _, err := auditchain.Prune(ctx, a, auditTestKeys, "t1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rep, err = auditchain.Verify(ctx, b, auditTestKeys, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK || rep.AnchorSeq != 8 || rep.HeadSeq != 8 || rep.Rows != 0 {
		t.Errorf("after pruning everything: %+v, want OK with anchor = head = 8", rep)
	}
	// Pruning the operator chain carries t1's opening in its signed anchor
	// state, so t1 is still known to exist with no opening row left.
	if n, err := auditchain.Prune(ctx, b, auditTestKeys, "", time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("pruning the operator chain = %d, %v; want its one opening removed", n, err)
	}
	if head, err := a.AuditHeadGet(ctx, ""); err != nil || !strings.Contains(head.AnchorState, `"t1":1`) {
		t.Errorf("operator head after the prune = %+v, %v; want t1's opening in the anchor state", head, err)
	}
	if rep, err := auditchain.Verify(ctx, a, auditTestKeys, "t1"); err != nil || !rep.OK {
		t.Errorf("t1 after the operator chain's prune: %+v, %v", rep, err)
	}
	// A prune of a tenant with no chain does nothing.
	if n, err := a.AuditPrune(ctx, "nobody", time.Now(), 0, auditTestKeys.SealAnchor()); err != nil || n != 0 {
		t.Errorf("prune of an absent chain = %d, %v", n, err)
	}
}

// The row the store hands back, the head and the listed rows carry exactly
// what was sealed, to the nanosecond.
func testAuditHeadAndListRoundTrip(t *testing.T, a, b store.Store) {
	ctx := context.Background()
	if _, err := a.AuditHeadGet(ctx, "t1"); err == nil {
		t.Fatal("AuditHeadGet on an empty store: want not found")
	}
	ts := time.Date(2026, 11, 5, 10, 11, 12, 987654321, time.UTC)
	row := auditchain.RowFor(audit.Event{TS: ts, Action: "auth_login", TargetTenant: "t1", ActorSubject: "alice",
		TargetSubject: "bob"}, []byte(`{"action":"auth_login"}`))
	rows, err := a.AuditAppend(ctx, []store.AuditEventRow{row}, auditTestKeys.Seal(), nil)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	written := rows[0]
	if written.Seq != 1 || written.PrevHash != "" || written.Hash == "" || written.MAC == "" {
		t.Fatalf("written = %+v", written)
	}
	got, err := b.AuditEventsList(ctx, "t1", 0, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("AuditEventsList = %v, %v", got, err)
	}
	if got[0] != written {
		t.Errorf("listed row = %+v\nwant          %+v", got[0], written)
	}
	head, err := b.AuditHeadGet(ctx, "t1")
	if err != nil || head.Seq != 1 || head.Hash != written.Hash || head.MAC == "" || head.AnchorSeq != 0 {
		t.Errorf("head = %+v, %v", head, err)
	}
}

// A NUL or an invalid UTF-8 byte in a recorded field — a member's denied
// path with %00 in it, a model-chosen definition name — must still leave a
// row (Postgres refuses NUL and invalid UTF-8 in text), and the hash must
// cover exactly the bytes stored, so the field is stored escaped.
func testAuditUnsafeTextIsStoredEscapedAndHashed(t *testing.T, a, b store.Store) {
	ctx := context.Background()
	sink, err := auditchain.NewStoreSink(a, auditTestKeys)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Record(audit.Event{Action: "acl_denied", ActorTenant: "t1", ActorSubject: "mal\x00lory\x00",
		TargetName: "doc:/a\x00b\xff\\c"}); err != nil {
		t.Fatalf("an event with NUL and invalid UTF-8 was not recorded: %v", err)
	}
	row := auditchain.RowFor(audit.Event{TS: time.Now(), Action: "acl_denied", ActorTenant: "t1"}, []byte("{\"x\":\"\x00\xfe\"}"))
	if _, err := a.AuditAppend(ctx, []store.AuditEventRow{row}, auditTestKeys.Seal(), nil); err != nil {
		t.Fatalf("a payload with NUL and invalid UTF-8 was not stored: %v", err)
	}
	rows, err := b.AuditEventsList(ctx, "t1", 0, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	if rows[0].Actor != `mal\x00lory\x00` || rows[0].Subject != `doc:/a\x00b\xff\\c` {
		t.Errorf("stored actor %q subject %q, want the reversible escapes", rows[0].Actor, rows[0].Subject)
	}
	if rows[1].Payload != `{"x":"\x00\xfe"}` {
		t.Errorf("stored payload %q, want the reversible escapes", rows[1].Payload)
	}
	rep, err := auditchain.Verify(ctx, b, auditTestKeys, "t1")
	if err != nil || !rep.OK || rep.Rows != 2 {
		t.Errorf("verify = %+v, %v", rep, err)
	}
}

// The store side of a prune: inside its transaction it hands the seal the
// locked head (the previous anchor and its state) and every row it would
// remove, in order and complete, and a seal error removes nothing. The seal
// does the verifying (auditchain.ChainKeys.SealAnchor); this pins that each
// backend gives it the whole range to verify.
func testAuditPruneHandsTheSealEveryRowAndAbortsOnItsError(t *testing.T, a, b store.Store) {
	ctx := context.Background()
	sink, err := auditchain.NewStoreSink(a, auditTestKeys)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := sink.Record(audit.Event{TS: base.Add(time.Duration(i) * time.Hour), Action: "acl_edit", ActorTenant: "t1"}); err != nil {
			t.Fatal(err)
		}
	}
	refuse := func(store.AuditHead, []store.AuditEventRow) (string, string, string, error) {
		return "", "", "", errors.New("refused")
	}
	if n, err := b.AuditPrune(ctx, "t1", base.Add(24*time.Hour), 0, refuse); err == nil || n != 0 {
		t.Fatalf("prune with a refusing seal = %d, %v; want an error and nothing removed", n, err)
	}
	if rows, _ := a.AuditEventsList(ctx, "t1", 0, 0); len(rows) != 5 {
		t.Fatalf("after a refused prune %d rows remain, want 5", len(rows))
	}
	// A limit of 3 takes rows 1-3, then 4-5: each time the seal sees the
	// previous anchor and exactly the rows removed.
	var seen [][]int64
	var anchors []int64
	inner := auditTestKeys.SealAnchor()
	record := func(prev store.AuditHead, pruned []store.AuditEventRow) (string, string, string, error) {
		var seqs []int64
		for _, r := range pruned {
			seqs = append(seqs, r.Seq)
		}
		seen = append(seen, seqs)
		anchors = append(anchors, prev.AnchorSeq)
		return inner(prev, pruned)
	}
	for _, want := range []int{3, 2} {
		if n, err := b.AuditPrune(ctx, "t1", base.Add(24*time.Hour), 3, record); err != nil || n != want {
			t.Fatalf("prune = %d, %v; want %d", n, err, want)
		}
	}
	if fmt.Sprint(seen) != "[[1 2 3] [4 5]]" || fmt.Sprint(anchors) != "[0 3]" {
		t.Errorf("the seal saw rows %v after anchors %v; want [[1 2 3] [4 5]] after [0 3]", seen, anchors)
	}
	if rep, err := auditchain.Verify(ctx, a, auditTestKeys, "t1"); err != nil || !rep.OK || rep.AnchorSeq != 5 {
		t.Errorf("after the batched prune: %+v, %v", rep, err)
	}
}

// A prune that reaches the head writes the head MAC the seal returns, in the
// same transaction, so a chain pruned under a new key carries its head under
// that key too.
func testAuditPruneToTheHeadReSignsTheHead(t *testing.T, a, b store.Store) {
	ctx := context.Background()
	sink, err := auditchain.NewStoreSink(a, auditTestKeys)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := sink.Record(audit.Event{TS: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Action: "acl_edit", ActorTenant: "t1"}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := a.AuditHeadGet(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	rotated := auditchain.ChainKeys{Current: []byte("a-rotated-in-audit-key-32-bytes!"), Previous: auditTestKeys.Current}
	if n, err := auditchain.Prune(ctx, b, rotated, "t1", time.Now()); err != nil || n != 2 {
		t.Fatalf("prune to the head = %d, %v", n, err)
	}
	after, err := a.AuditHeadGet(ctx, "t1")
	if err != nil || after.MAC == before.MAC || after.Seq != before.Seq || after.Hash != before.Hash {
		t.Errorf("head after a prune to it under a new key = %+v (before %+v), %v; want the same position re-signed", after, before, err)
	}
}
