package builtin

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// Releases across replicas: two replicas sharing one store must not take releases at
// the same time. The in-process release lock does not reach another process, so these
// tests stand a second Document in for the second replica: its own locks, the same
// store, and on Postgres its own connection pool.

// otherReplica is a second Document over d's store. On SQLite it shares d's manager
// (a SQLite scope is one file with one connection; replicas share Postgres, not
// SQLite), on Postgres it opens its own, as another process would.
func otherReplica(t *testing.T, d *Document) *Document {
	t.Helper()
	mgr := d.SqlMem
	if strings.HasSuffix(t.Name(), "/postgres") {
		m, err := sqlmem.NewPostgres(context.Background(), sqlmem.Config{
			PgDSN: os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN"), StatementTimeoutMS: 30000, MaxRows: 1000})
		if err != nil {
			t.Fatalf("second replica's manager: %v", err)
		}
		t.Cleanup(func() { _ = m.Close() })
		mgr = m
	}
	return &Document{Store: d.Store, SqlMem: mgr, Bus: d.Bus}
}

// raceWindow is how long a replica's hook gives the other replica to run before it
// carries on. Long enough for an unlocked release to finish; a locked one waits it out.
const raceWindow = 1500 * time.Millisecond

// runAlongside starts fn on another goroutine and returns a hook that waits for it to
// finish or for raceWindow, whichever comes first, the first time it is called.
func runAlongside(fn func()) (hook func(), wait func()) {
	done := make(chan struct{})
	var once sync.Once
	hook = func() {
		once.Do(func() {
			go func() { defer close(done); fn() }()
			select {
			case <-done:
			case <-time.After(raceWindow):
			}
		})
	}
	return hook, func() { <-done }
}

// TestOntologyRelease_TwoReplicasTakeDistinctConsecutiveCompleteReleases: a replica
// taking a release while another is between writing its entries and marking it
// complete waits for it, so the two releases have seqs 1 and 2, both complete, each
// with every entry its header counts.
func TestOntologyRelease_TwoReplicasTakeDistinctConsecutiveCompleteReleases(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := releaseFixtureOn(t, d, base)
		d2 := otherReplica(t, d)
		var second OntologyRelease
		var secondErr error
		hook, wait := runAlongside(func() { second, secondErr = d2.TakeOntologyRelease(f.ctx, OntologyReleaseApply, nil) })
		d.afterReleaseBatch = func() error { hook(); return nil }
		first, err := d.TakeOntologyRelease(f.ctx, OntologyReleaseConfirm, nil)
		d.afterReleaseBatch = nil
		wait()
		if err != nil || secondErr != nil {
			t.Fatalf("take: first %v, second %v", err, secondErr)
		}
		seqs := []int{int(first.Seq), int(second.Seq)}
		sort.Ints(seqs)
		if seqs[0] != 1 || seqs[1] != 2 {
			t.Errorf("seqs = %v, want 1 and 2", seqs)
		}
		rels := listedReleases(t, d, f.ctx)
		if len(rels) != 2 {
			t.Fatalf("listed %+v, want two complete releases", rels)
		}
		for _, r := range rels {
			if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_release_entries WHERE release_seq = ?`, r.Seq); n != r.Entries || n == 0 {
				t.Errorf("release %d (%s) holds %d entries, its header says %d", r.Seq, r.Reason, n, r.Entries)
			}
		}
		if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_releases WHERE complete = 0`); n != 0 {
			t.Errorf("%d incomplete headers stayed", n)
		}
	})
}

// TestOntologyRelease_LostLeaseStopsTheRelease: a release whose lease another replica
// took (after it expired under a slow copy) stops at its next renewal and leaves the
// release incomplete, rather than completing over the other replica's.
func TestOntologyRelease_LostLeaseStopsTheRelease(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := releaseFixtureOn(t, d, base)
		key := mustTenantKey(t, d, f.ctx)
		d.afterReleaseBatch = func() error {
			return d.exec(f.ctx, key, `UPDATE document_leases SET owner = 'other-replica', expires_at = ? WHERE name = ?`,
				time.Now().Add(time.Minute).UnixNano(), ontologyReleaseLease)
		}
		_, err := d.TakeOntologyRelease(f.ctx, OntologyReleaseConfirm, nil)
		d.afterReleaseBatch = nil
		if !errors.Is(err, ErrOntologyReleaseLeaseLost) {
			t.Fatalf("take = %v, want ErrOntologyReleaseLeaseLost", err)
		}
		if rels := listedReleases(t, d, f.ctx); len(rels) != 0 {
			t.Errorf("listed %+v, want none: the release did not complete", rels)
		}
		if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM document_leases WHERE name = ? AND owner = 'other-replica'`, ontologyReleaseLease); n != 1 {
			t.Error("the stopped release released the other replica's lease")
		}
	})
}

// TestOntologyRelease_TwoReplicasTakeOneCatchUpForOneContentVersion: two replicas
// catching up the same content version at once take one catch_up between them; the one
// that waited finds the version released and takes none.
func TestOntologyRelease_TwoReplicasTakeOneCatchUpForOneContentVersion(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := releaseFixtureOn(t, d, base)
		d2 := otherReplica(t, d)
		var tookSecond bool
		var secondErr error
		hook, wait := runAlongside(func() { _, tookSecond, secondErr = d2.CatchUpOntologyRelease(f.ctx) })
		d.duringReleaseCopy = hook
		_, tookFirst, err := d.CatchUpOntologyRelease(f.ctx)
		d.duringReleaseCopy = nil
		wait()
		if err != nil || secondErr != nil {
			t.Fatalf("catch_up: first %v, second %v", err, secondErr)
		}
		if !tookFirst {
			t.Error("the first replica took no catch_up")
		}
		if tookSecond {
			t.Error("the second replica took a catch_up of a content version already released")
		}
		if rels := listedReleases(t, d, f.ctx); len(rels) != 1 || rels[0].Reason != OntologyReleaseCatchUp ||
			rels[0].ContentVersion != currentContentVersion(t, d, f.ctx) {
			t.Errorf("listed %+v, want one catch_up of the version in force", rels)
		}
	})
}
