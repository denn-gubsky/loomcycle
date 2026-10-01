package changesub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

type fakeStore struct {
	changes []store.MemoryChange
	cursors map[string]int64
}

func (f *fakeStore) GetMemoryChangesSince(_ context.Context, tenantID string, afterSeq int64, limit int) ([]store.MemoryChange, error) {
	var out []store.MemoryChange
	for _, ch := range f.changes {
		if ch.TenantID == tenantID && ch.Seq > afterSeq {
			out = append(out, ch)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeStore) GetMemoryChangesSinceAllTenants(_ context.Context, afterSeq int64, limit int) ([]store.MemoryChange, error) {
	var out []store.MemoryChange
	for _, ch := range f.changes {
		if ch.Seq > afterSeq {
			out = append(out, ch)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeStore) GetChangeSubscriptionCursor(_ context.Context, name string) (int64, error) {
	return f.cursors[name], nil
}

func (f *fakeStore) SetChangeSubscriptionCursor(_ context.Context, name string, seq int64) error {
	f.cursors[name] = seq
	return nil
}

func seeded() *fakeStore {
	return &fakeStore{
		cursors: map[string]int64{},
		changes: []store.MemoryChange{
			{Seq: 1, TenantID: "acme", Type: store.MemoryChangeSet, Scope: store.MemoryScopeAgent, ScopeID: "a1", Key: "k1"},
			{Seq: 2, TenantID: "acme", Type: store.DocumentChangeUpdated, Scope: store.MemoryScopeAgent, ScopeID: "a1", ChunkID: "CID"},
			{Seq: 3, TenantID: "globex", Type: store.MemoryChangeSet, Scope: store.MemoryScopeUser, ScopeID: "u1", Key: "z"},
		},
	}
}

const testSecret = "s3cr3t"

func testSecretFor(string) (string, error) { return testSecret, nil }

func TestDeliver_SignsBatchAndAdvancesCursor(t *testing.T) {
	fs := seeded()
	var gotSig string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-Loomcycle-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(fs, testSecretFor, nil)
	sub := Subscription{Name: "s1", CallbackURL: srv.URL, TenantID: "acme", SecretEnv: "SECRET", Client: srv.Client()}
	d.RunOnce(context.Background(), []Subscription{sub})

	// acme has seqs 1,2 (globex's 3 is another tenant) → cursor advances to 2.
	if fs.cursors["s1"] != 2 {
		t.Errorf("cursor = %d, want 2", fs.cursors["s1"])
	}
	// The signature verifies against the body with the shared secret.
	if want := sign(testSecret, gotBody); want != gotSig {
		t.Errorf("signature = %q, want %q", gotSig, want)
	}
	// The body is the value-free batch (both acme changes, no value field).
	var batch deliveryBatch
	if err := json.Unmarshal(gotBody, &batch); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if batch.Subscription != "s1" || len(batch.Changes) != 2 {
		t.Errorf("batch = %+v", batch)
	}
	if batch.Changes[0].Key != "k1" || batch.Changes[1].ChunkID != "CID" {
		t.Errorf("batch changes = %+v", batch.Changes)
	}
}

func TestDeliver_FiltersButStillAdvancesCursor(t *testing.T) {
	fs := seeded()
	var count int
	var delivered []store.MemoryChange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b deliveryBatch
		_ = json.NewDecoder(r.Body).Decode(&b)
		delivered = append(delivered, b.Changes...)
		count++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(fs, testSecretFor, nil)
	// Only memory.* changes → the document change (seq 2) is skipped, but the
	// cursor still advances over it (no re-scan).
	sub := Subscription{Name: "s1", CallbackURL: srv.URL, TenantID: "acme", Kinds: []string{"memory"}, Client: srv.Client()}
	d.RunOnce(context.Background(), []Subscription{sub})

	if len(delivered) != 1 || delivered[0].Type != store.MemoryChangeSet {
		t.Errorf("delivered = %+v, want only the memory.set", delivered)
	}
	if fs.cursors["s1"] != 2 {
		t.Errorf("cursor = %d, want 2 (advanced past the skipped document change)", fs.cursors["s1"])
	}
}

func TestDeliver_RetriesThenAdvances(t *testing.T) {
	fs := seeded()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway) // fail the first attempt
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(fs, testSecretFor, nil)
	sub := Subscription{Name: "s1", CallbackURL: srv.URL, TenantID: "acme", Client: srv.Client()}
	d.RunOnce(context.Background(), []Subscription{sub})

	if atomic.LoadInt32(&hits) < 2 {
		t.Errorf("expected a retry (hits=%d)", hits)
	}
	if fs.cursors["s1"] != 2 {
		t.Errorf("cursor = %d, want 2 after the retry succeeded", fs.cursors["s1"])
	}
}

func TestDeliver_FailureLeavesCursor(t *testing.T) {
	fs := seeded()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // always fail
	}))
	defer srv.Close()

	d := New(fs, testSecretFor, nil)
	sub := Subscription{Name: "s1", CallbackURL: srv.URL, TenantID: "acme", Client: srv.Client()}
	d.RunOnce(context.Background(), []Subscription{sub})

	// At-least-once: a failed delivery must NOT advance the cursor.
	if fs.cursors["s1"] != 0 {
		t.Errorf("cursor = %d, want 0 (delivery failed, must retry next tick)", fs.cursors["s1"])
	}
}

// collector is a callback that records every delivered change.
func collector(t *testing.T) (*httptest.Server, func() []store.MemoryChange) {
	t.Helper()
	var mu sync.Mutex
	var got []store.MemoryChange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b deliveryBatch
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		got = append(got, b.Changes...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []store.MemoryChange {
		mu.Lock()
		defer mu.Unlock()
		return append([]store.MemoryChange(nil), got...)
	}
}

// TestDeliver_OperatorSubscriptionDeliversEveryTenantsChanges is the reported
// bug. A subscription with no tenant_id is the operator's (only yaml declares
// subscriptions), but it read tenant "" exactly — and nobody who signs in writes
// there: the legacy bearer writes to "default", minted tokens to their own
// tenants. It delivered nothing. It is now the operator's feed of every tenant,
// each change naming its tenant.
//
// Fails-before: zero changes delivered, cursor stays 0.
func TestDeliver_OperatorSubscriptionDeliversEveryTenantsChanges(t *testing.T) {
	fs := seeded()
	fs.changes = append(fs.changes, store.MemoryChange{Seq: 4, TenantID: "default", Type: store.MemoryChangeSet, Scope: store.MemoryScopeUser, ScopeID: "alice", Key: "tea"})
	srv, delivered := collector(t)

	d := New(fs, testSecretFor, nil)
	d.RunOnce(context.Background(), []Subscription{{Name: "ops", CallbackURL: srv.URL, Client: srv.Client()}})

	var got []string
	for _, ch := range delivered() {
		got = append(got, fmt.Sprintf("%d:%s", ch.Seq, ch.TenantID))
	}
	if want := "1:acme,2:acme,3:globex,4:default"; strings.Join(got, ",") != want {
		t.Fatalf("operator subscription delivered %v, want %s", got, want)
	}
	if fs.cursors["ops"] != 4 {
		t.Errorf("cursor = %d, want 4", fs.cursors["ops"])
	}
}

// TestDeliver_OperatorSubscriptionKeepsItsFilters: sweeping every tenant widens
// WHICH tenants, not what the scope/kinds filter lets through.
func TestDeliver_OperatorSubscriptionKeepsItsFilters(t *testing.T) {
	fs := seeded()
	srv, delivered := collector(t)

	d := New(fs, testSecretFor, nil)
	d.RunOnce(context.Background(), []Subscription{{Name: "ops", CallbackURL: srv.URL, Scope: "user", Client: srv.Client()}})

	got := delivered()
	if len(got) != 1 || got[0].TenantID != "globex" || got[0].Scope != store.MemoryScopeUser {
		t.Fatalf("delivered %+v, want only globex's user-scope change", got)
	}
}

// TestDeliver_TenantSubscriptionNeverSeesAnotherTenant: a subscription naming a
// tenant is confined to it, whatever other tenants write.
func TestDeliver_TenantSubscriptionNeverSeesAnotherTenant(t *testing.T) {
	fs := seeded()
	fs.changes = append(fs.changes, store.MemoryChange{Seq: 4, TenantID: "", Type: store.MemoryChangeSet, Scope: store.MemoryScopeAgent, ScopeID: "op", Key: "k"})
	srv, delivered := collector(t)

	d := New(fs, testSecretFor, nil)
	d.RunOnce(context.Background(), []Subscription{{Name: "g", CallbackURL: srv.URL, TenantID: "globex", Client: srv.Client()}})

	for _, ch := range delivered() {
		if ch.TenantID != "globex" {
			t.Errorf("globex's subscription delivered tenant %q's change %+v", ch.TenantID, ch)
		}
	}
	if n := len(delivered()); n != 1 {
		t.Errorf("delivered %d change(s), want globex's 1", n)
	}
}
