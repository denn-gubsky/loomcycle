package auditchain_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auditchain"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

var testKeys = auditchain.ChainKeys{Current: []byte("an-audit-chain-key-of-32-bytes!!")}

// chainFixture is a SQLite store holding one tenant's chain of n rows, plus a
// raw connection to the same file: the database writer of the threat model.
func chainFixture(t *testing.T, n int) (*sqlite.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.db")
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sink, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := sink.Record(audit.Event{Action: "acl_edit", TargetTenant: "t1", ActorSubject: "alice",
			TargetName: "acl:" + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return s, raw
}

func verify(t *testing.T, s *sqlite.Store, keys auditchain.ChainKeys) auditchain.VerifyReport {
	t.Helper()
	rep, err := auditchain.Verify(context.Background(), s, keys, "t1")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return rep
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// rehashFrom recomputes the chain from row `from` onward the way a database
// writer without the key can: every hash and prev_hash made consistent, the
// MACs left as they were (or forged under a key that is not the deployment's).
func rehashFrom(t *testing.T, s *sqlite.Store, db *sql.DB, from int64, forge []byte) {
	t.Helper()
	ctx := context.Background()
	rows, err := s.AuditEventsList(ctx, "t1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	prev := ""
	for _, r := range rows {
		if r.Seq >= from {
			r.PrevHash = prev
			r.Hash = auditchain.RowHash(prev, r)
			mac := r.MAC
			if forge != nil {
				// The forger signs with a key of its own.
				_, _ = auditchain.ChainKeys{Current: forge}.Seal()(&r)
				mac = r.MAC
			}
			exec(t, db, `UPDATE audit_events SET prev_hash = ?, hash = ?, mac = ? WHERE tenant_id = 't1' AND seq = ?`,
				r.PrevHash, r.Hash, mac, r.Seq)
		}
		prev = r.Hash
	}
	exec(t, db, `UPDATE audit_heads SET hash = ? WHERE tenant_id = 't1'`, prev)
}

func TestAudit_ATamperedRowFailsVerification(t *testing.T) {
	t.Run("untouched", func(t *testing.T) {
		s, _ := chainFixture(t, 5)
		if rep := verify(t, s, testKeys); !rep.OK || rep.Rows != 5 {
			t.Fatalf("an untouched chain: %+v", rep)
		}
	})
	cases := []struct {
		name      string
		tamper    func(t *testing.T, s *sqlite.Store, db *sql.DB)
		wantSeq   int64
		wantInWhy string
	}{
		{"payload edited", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `UPDATE audit_events SET payload = '{"action":"nothing"}' WHERE tenant_id = 't1' AND seq = 3`)
		}, 3, "content"},
		{"actor edited", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `UPDATE audit_events SET actor = 'mallory' WHERE tenant_id = 't1' AND seq = 2`)
		}, 2, "content"},
		{"a row deleted", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `DELETE FROM audit_events WHERE tenant_id = 't1' AND seq = 3`)
		}, 3, "missing"},
		{"two rows reordered", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `UPDATE audit_events SET seq = -1 WHERE tenant_id = 't1' AND seq = 2`)
			exec(t, db, `UPDATE audit_events SET seq = 2 WHERE tenant_id = 't1' AND seq = 3`)
			exec(t, db, `UPDATE audit_events SET seq = 3 WHERE tenant_id = 't1' AND seq = -1`)
		}, 2, "follow"},
		{"payload edited and the chain recomputed without the key", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `UPDATE audit_events SET payload = '{"action":"nothing"}' WHERE tenant_id = 't1' AND seq = 3`)
			rehashFrom(t, s, db, 3, nil)
		}, 3, "signature"},
		{"chain recomputed and signed under another key", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `UPDATE audit_events SET subject = 'nobody' WHERE tenant_id = 't1' AND seq = 4`)
			rehashFrom(t, s, db, 4, []byte("not-the-deployment-key-32-bytes!"))
		}, 4, "signature"},
		{"the newest rows cut off", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `DELETE FROM audit_events WHERE tenant_id = 't1' AND seq >= 4`)
		}, 4, "ends"},
		{"the oldest rows deleted as if pruned", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			var hash, mac string
			if err := db.QueryRow(`SELECT hash, mac FROM audit_events WHERE tenant_id = 't1' AND seq = 2`).Scan(&hash, &mac); err != nil {
				t.Fatal(err)
			}
			exec(t, db, `DELETE FROM audit_events WHERE tenant_id = 't1' AND seq <= 2`)
			// The row's own MAC is the only signature at hand; it is not an
			// anchor's.
			exec(t, db, `UPDATE audit_heads SET anchor_seq = 2, anchor_hash = ?, anchor_mac = ? WHERE tenant_id = 't1'`, hash, mac)
		}, 2, "anchor"},
		{"the oldest rows deleted without an anchor", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `DELETE FROM audit_events WHERE tenant_id = 't1' AND seq <= 2`)
		}, 1, "missing"},
		{"the head deleted, its rows kept", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `DELETE FROM audit_heads WHERE tenant_id = 't1'`)
		}, 1, "head"},
		{"the head reset to zero, its rows kept", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `UPDATE audit_heads SET seq = 0, hash = '', mac = '' WHERE tenant_id = 't1'`)
		}, 1, "head"},
		{"the whole chain deleted", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `DELETE FROM audit_events WHERE tenant_id = 't1'`)
			exec(t, db, `DELETE FROM audit_heads WHERE tenant_id = 't1'`)
		}, 1, "disappeared"},
		{"the operator chain deleted", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `DELETE FROM audit_events WHERE tenant_id = ''`)
			exec(t, db, `DELETE FROM audit_heads WHERE tenant_id = ''`)
		}, 1, "no record"},
		{"the chain and its opening record deleted", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `DELETE FROM audit_events WHERE tenant_id = 't1'`)
			exec(t, db, `DELETE FROM audit_heads WHERE tenant_id = 't1'`)
			exec(t, db, `DELETE FROM audit_events WHERE tenant_id = '' AND action = 'audit_chain_opened' AND subject = 't1'`)
		}, 1, "operator chain"},
		{"an opening record forged into the operator chain", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `INSERT INTO audit_events (tenant_id, seq, ts, actor, action, subject, payload, prev_hash, hash, mac)
				SELECT '', seq + 1, ts, actor, action, subject, payload, hash, hash, mac FROM audit_events
				WHERE tenant_id = '' AND seq = (SELECT MAX(seq) FROM audit_events WHERE tenant_id = '')`)
			exec(t, db, `UPDATE audit_heads SET seq = seq + 1 WHERE tenant_id = ''`)
		}, 1, "operator chain"},
		{"the operator chain marked as pruned", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `DELETE FROM audit_events WHERE tenant_id = ''`)
			exec(t, db, `UPDATE audit_heads SET anchor_seq = seq, anchor_hash = hash, anchor_mac = mac WHERE tenant_id = ''`)
		}, 1, "operator chain"},
		{"an opened chain's rows deleted and its head zeroed", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `DELETE FROM audit_events WHERE tenant_id = 't1'`)
			exec(t, db, `UPDATE audit_heads SET seq = 0, hash = '', mac = '' WHERE tenant_id = 't1'`)
		}, 1, "head"},
		{"the whole chain deleted and a new one begun", func(t *testing.T, s *sqlite.Store, db *sql.DB) {
			exec(t, db, `DELETE FROM audit_events WHERE tenant_id = 't1'`)
			exec(t, db, `DELETE FROM audit_heads WHERE tenant_id = 't1'`)
			sink, err := auditchain.NewStoreSink(s, testKeys)
			if err != nil {
				t.Fatal(err)
			}
			if err := sink.Record(audit.Event{Action: "auth_login", TargetTenant: "t1"}); err != nil {
				t.Fatal(err)
			}
		}, 1, "opened again"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, db := chainFixture(t, 5)
			tc.tamper(t, s, db)
			rep := verify(t, s, testKeys)
			if rep.OK {
				t.Fatalf("a tampered chain verifies: %+v", rep)
			}
			if rep.BreakSeq != tc.wantSeq || !strings.Contains(rep.Reason, tc.wantInWhy) {
				t.Errorf("break at %d (%q), want at %d mentioning %q", rep.BreakSeq, rep.Reason, tc.wantSeq, tc.wantInWhy)
			}
		})
	}
}

// A row signed before a key rotation still verifies with the old key named
// as the previous one, and fails once neither key is the one it was signed
// under.
func TestAuditVerify_AcceptsTheRotatedOutKey(t *testing.T) {
	s, _ := chainFixture(t, 3)
	newKey := []byte("the-rotated-in-audit-key-32bytes")
	if rep := verify(t, s, auditchain.ChainKeys{Current: newKey, Previous: testKeys.Current}); !rep.OK {
		t.Errorf("with the old key as previous: %+v", rep)
	}
	if rep := verify(t, s, auditchain.ChainKeys{Current: newKey}); rep.OK {
		t.Errorf("with neither key: verifies, %+v", rep)
	}
}

// Every event goes to the chain of the tenant it acted on, else the actor's,
// else the operator chain; and a StoreSink refuses to exist without a key.
func TestAuditStoreSink_ChainsByTenantAndNeedsAKey(t *testing.T) {
	for _, tc := range []struct {
		ev   audit.Event
		want string
	}{
		{audit.Event{TargetTenant: "acme", ActorTenant: ""}, "acme"},
		{audit.Event{ActorTenant: "acme"}, "acme"},
		{audit.Event{}, ""},
	} {
		if got := auditchain.ChainTenant(tc.ev); got != tc.want {
			t.Errorf("ChainTenant(%+v) = %q, want %q", tc.ev, got, tc.want)
		}
	}
	if _, err := auditchain.NewStoreSink(&sqlite.Store{}, auditchain.ChainKeys{}); err == nil {
		t.Error("NewStoreSink without a key: want an error")
	}
	var _ store.AuditStore = (*sqlite.Store)(nil)
}

// SafeText leaves clean text alone and escapes the rest reversibly; an
// escaped field is clean, so a second pass changes nothing.
func TestSafeText_EscapesNULAndInvalidUTF8Reversibly(t *testing.T) {
	for in, want := range map[string]string{
		"plain/ünïcode":      "plain/ünïcode",
		`back\slash`:         `back\slash`,
		"a\x00b":             `a\x00b`,
		"a\xffb\\c":          `a\xffb\\c`,
		"\xe2\x82":           `\xe2\x82`,
		"ok\x00\xc3\xa9\xc3": `ok\x00é\xc3`,
	} {
		got := auditchain.SafeText(in)
		if got != want {
			t.Errorf("SafeText(%q) = %q, want %q", in, got, want)
		}
		if again := auditchain.SafeText(got); again != got {
			t.Errorf("SafeText is not idempotent on %q: %q", got, again)
		}
	}
}

// A rotated-out key verifies only the rows signed before the rotation: once a
// row verifies under the current key, a later row signed with the previous
// key is a forgery by whoever still holds it.
func TestAuditVerify_PreviousKeyStopsAtTheRotationPoint(t *testing.T) {
	s, _ := chainFixture(t, 3)
	newKey := []byte("the-rotated-in-audit-key-32bytes")
	record := func(key []byte, subject string) {
		t.Helper()
		sink, err := auditchain.NewStoreSink(s, auditchain.ChainKeys{Current: key})
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Record(audit.Event{Action: "acl_edit", TargetTenant: "t1", TargetName: subject}); err != nil {
			t.Fatal(err)
		}
	}
	record(newKey, "after-rotation-1")
	record(newKey, "after-rotation-2")
	keys := auditchain.ChainKeys{Current: newKey, Previous: testKeys.Current}
	if rep := verify(t, s, keys); !rep.OK || rep.Rows != 5 {
		t.Fatalf("rows before and after the rotation: %+v", rep)
	}
	record(testKeys.Current, "forged-with-the-old-key")
	rep := verify(t, s, keys)
	if rep.OK || rep.BreakSeq != 6 || !strings.Contains(rep.Reason, "previous key") {
		t.Errorf("a row signed with the rotated-out key after the rotation: %+v, want a break at 6 naming the previous key", rep)
	}
}

// interleavingReader appends a new tenant's first event between the
// operator chain's read and the tenant head's read, as a live server can.
type interleavingReader struct {
	*sqlite.Store
	sink   audit.Sink
	tenant string
	done   bool
}

func (r *interleavingReader) AuditHeadGet(ctx context.Context, tenant string) (store.AuditHead, error) {
	if tenant == r.tenant && !r.done {
		r.done = true
		if err := r.sink.Record(audit.Event{Action: "auth_login", ActorTenant: tenant}); err != nil {
			return store.AuditHead{}, err
		}
	}
	return r.Store.AuditHeadGet(ctx, tenant)
}

// A tenant whose first append commits between the operator chain's read
// and its own head's read is not reported broken: verification re-reads the
// operator chain for the opening before calling it missing.
func TestAuditVerify_ANewChainOpenedDuringTheWalkVerifies(t *testing.T) {
	s, _ := chainFixture(t, 2)
	sink, err := auditchain.NewStoreSink(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	r := &interleavingReader{Store: s, sink: sink, tenant: "t2"}
	rep, err := auditchain.Verify(context.Background(), r, testKeys, "t2")
	if err != nil || !r.done {
		t.Fatalf("Verify: %v (interleaved: %v)", err, r.done)
	}
	if !rep.OK || rep.Rows != 1 {
		t.Errorf("a chain opened during the walk: %+v, want it verified", rep)
	}
}

// After the operator chain is pruned, its anchor still carries the openings
// it pruned, so a tenant chain deleted later is still caught.
func TestAuditVerify_OpeningsSurviveTheOperatorChainsPrune(t *testing.T) {
	s, db := chainFixture(t, 3)
	ctx := context.Background()
	if n, err := pruneForTest(ctx, s, "", time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("pruning the operator chain: %d, %v", n, err)
	}
	if rep := verify(t, s, testKeys); !rep.OK {
		t.Fatalf("t1 after the operator chain's prune: %+v", rep)
	}
	exec(t, db, `DELETE FROM audit_events WHERE tenant_id = 't1'`)
	exec(t, db, `DELETE FROM audit_heads WHERE tenant_id = 't1'`)
	rep := verify(t, s, testKeys)
	if rep.OK || !strings.Contains(rep.Reason, "disappeared") {
		t.Errorf("t1 deleted after the operator chain's prune: %+v, want it reported disappeared", rep)
	}
	// Dropping the carried opening from the anchor breaks its signature.
	exec(t, db, `UPDATE audit_heads SET anchor_state = '' WHERE tenant_id = ''`)
	if rep := verify(t, s, testKeys); rep.OK || !strings.Contains(rep.Reason, "operator chain") {
		t.Errorf("the operator anchor's carried openings dropped: %+v, want the operator chain broken", rep)
	}
}

// A prune must not launder tampering: it re-signs an anchor over what it
// removes, so it verifies the range first and refuses a chain that does
// not verify, leaving the rows (and the break) in place.
func TestAuditPrune_RefusesAChainThatDoesNotVerify(t *testing.T) {
	ctx := context.Background()
	t.Run("a tampered row", func(t *testing.T) {
		s, db := chainFixture(t, 4)
		exec(t, db, `UPDATE audit_events SET payload = '{"action":"nothing"}' WHERE tenant_id = 't1' AND seq = 2`)
		if n, err := pruneForTest(ctx, s, "t1", time.Now().Add(time.Hour)); err == nil || n != 0 {
			t.Errorf("prune of a tampered chain = %d, %v; want a refusal", n, err)
		}
		if rep := verify(t, s, testKeys); rep.OK || rep.BreakSeq != 2 {
			t.Errorf("after the refused prune: %+v, want the break at 2 still reported", rep)
		}
	})
	t.Run("a tampered anchor state", func(t *testing.T) {
		s, db := chainFixture(t, 2)
		if _, err := pruneForTest(ctx, s, "", time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		sink, err := auditchain.NewStoreSink(s, testKeys)
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Record(audit.Event{Action: "auth_login", ActorTenant: "t2"}); err != nil {
			t.Fatal(err)
		}
		// Forge an opening for a tenant that never existed.
		exec(t, db, `UPDATE audit_heads SET anchor_state = '{"opened":{"ghost":1,"t1":1}}' WHERE tenant_id = ''`)
		if n, err := pruneForTest(ctx, s, "", time.Now().Add(time.Hour)); err == nil || n != 0 {
			t.Errorf("prune over a forged anchor state = %d, %v; want a refusal", n, err)
		}
		head, err := s.AuditHeadGet(ctx, "")
		if err != nil || !strings.Contains(head.AnchorState, "ghost") {
			t.Fatalf("operator head = %+v, %v", head, err)
		}
		if rep := verify(t, s, testKeys); rep.OK || !strings.Contains(rep.Reason, "operator chain") {
			t.Errorf("after the refused prune: %+v, want the operator chain still broken", rep)
		}
	})
}

// With no anchor (seq 0) the anchor's hash, MAC and state must be empty: a
// state planted there is never carried or trusted.
func TestAuditVerify_NoAnchorMeansEmptyAnchorFields(t *testing.T) {
	s, db := chainFixture(t, 2)
	ctx := context.Background()
	exec(t, db, `UPDATE audit_heads SET anchor_state = '{"opened":{"ghost":1}}' WHERE tenant_id = ''`)
	rep, err := auditchain.Verify(ctx, s, testKeys, "")
	if err != nil || rep.OK || !strings.Contains(rep.Reason, "anchor") {
		t.Errorf("an anchor state with no anchor: %+v, %v; want a break naming the anchor", rep, err)
	}
	if n, err := pruneForTest(ctx, s, "", time.Now().Add(time.Hour)); err == nil || n != 0 {
		t.Errorf("prune over a planted anchor state = %d, %v; want a refusal", n, err)
	}
}

// The rotation point survives a prune: the anchor carries "rotated", so a
// row signed with the previous key after the prune still fails.
func TestAuditVerify_RotationPointSurvivesAPrune(t *testing.T) {
	s, _ := chainFixture(t, 3)
	ctx := context.Background()
	newKey := []byte("the-rotated-in-audit-key-32bytes")
	keys := auditchain.ChainKeys{Current: newKey, Previous: testKeys.Current}
	record := func(key []byte) {
		t.Helper()
		sink, err := auditchain.NewStoreSink(s, auditchain.ChainKeys{Current: key})
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Record(audit.Event{Action: "acl_edit", TargetTenant: "t1"}); err != nil {
			t.Fatal(err)
		}
	}
	record(newKey)
	record(newKey)
	if n, err := auditchain.Prune(ctx, s, keys, "t1", time.Now().Add(time.Hour)); err != nil || n != 5 {
		t.Fatalf("prune across the rotation = %d, %v", n, err)
	}
	record(testKeys.Current) // a forgery with the rotated-out key
	rep, err := auditchain.Verify(ctx, s, keys, "t1")
	if err != nil || rep.OK || rep.BreakSeq != 6 {
		t.Errorf("a previous-key row after a pruned rotation point: %+v, %v; want a break at 6", rep, err)
	}
}
