package changesub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
