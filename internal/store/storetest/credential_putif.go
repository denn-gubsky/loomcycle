package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential/credentialdef"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A conditional credential write (RFC GB: an OAuth credential's state, kept
// by replicas that race to refresh it) lands only over the definition the
// caller read: a stale prev is refused and leaves the newer row in place.
// What a read returned is always a valid prev, whatever the backend did to
// the text (Postgres re-serialises jsonb).
func testCredentialDefPutIfWritesOnlyWhenDefinitionUnchanged(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := credentialdef.Row{TenantID: "t", Scope: "tenant", Name: "docs", Backend: "inline",
		Definition: json.RawMessage(`{"usage":"oauth","state_version":1}`)}
	if _, err := s.CredentialDefPut(ctx, row); err != nil {
		t.Fatalf("put: %v", err)
	}
	read, err := s.CredentialDefGet(ctx, "t", "tenant", "", "docs")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	next := read
	next.Definition = json.RawMessage(`{"usage":"oauth","state_version":2}`)
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	next.ExpiresAt = &exp
	if ok, err := s.CredentialDefPutIf(ctx, next, read.Definition); err != nil || !ok {
		t.Fatalf("PutIf over the row as read: ok=%v err=%v, want written", ok, err)
	}
	stale := read
	stale.Definition = json.RawMessage(`{"usage":"oauth","state_version":99}`)
	if ok, err := s.CredentialDefPutIf(ctx, stale, read.Definition); err != nil || ok {
		t.Fatalf("PutIf over a stale read: ok=%v err=%v, want refused", ok, err)
	}
	got, err := s.CredentialDefGet(ctx, "t", "tenant", "", "docs")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !jsonEqual(got.Definition, `{"usage":"oauth","state_version":2}`) {
		t.Fatalf("after a refused PutIf the row is %s, want the written one", got.Definition)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, exp)
	}
	if !got.CreatedAt.Equal(read.CreatedAt) {
		t.Errorf("created_at moved: %v -> %v", read.CreatedAt, got.CreatedAt)
	}
	// The key is the row's: a PutIf never writes another bucket's row of the
	// same name, nor creates one.
	other := got
	other.TenantID = "u"
	if _, err := s.CredentialDefPutIf(ctx, other, got.Definition); err == nil {
		t.Error("PutIf on another tenant's (absent) row did not report not found")
	}
	if _, err := s.CredentialDefGet(ctx, "u", "tenant", "", "docs"); err == nil {
		t.Error("PutIf created a row")
	}

	// Of N writers racing over the same read, exactly one lands.
	base, _ := s.CredentialDefGet(ctx, "t", "tenant", "", "docs")
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := base
			r.Definition = json.RawMessage(`{"usage":"oauth","state_version":3,"writer":` + string(rune('0'+i)) + `}`)
			if ok, err := s.CredentialDefPutIf(ctx, r, base.Definition); err != nil {
				t.Errorf("racing PutIf: %v", err)
			} else if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("%d racing writers landed, want exactly 1", wins.Load())
	}
}

// A PutIf over a row that is gone reports *store.ErrNotFound, so the caller
// can tell a deleted credential from a lost race.
func testCredentialDefPutIfMissingRowIsNotFound(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := credentialdef.Row{TenantID: "t", Scope: "user", ScopeID: "alice", Name: "gone", Backend: "inline",
		Definition: json.RawMessage(`{"usage":"oauth"}`)}
	_, err := s.CredentialDefPutIf(ctx, row, row.Definition)
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v, want *store.ErrNotFound", err)
	}
}

// Listing every credential row spans every tenant and scope, in key order:
// what the snapshot capture walks.
func testCredentialDefListAllSpansTenantsAndScopes(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, r := range []credentialdef.Row{
		{TenantID: "b", Scope: "tenant", Name: "x"},
		{TenantID: "a", Scope: "user", ScopeID: "u1", Name: "y"},
		{TenantID: "a", Scope: "agent", ScopeID: "ag", Name: "z"},
		{TenantID: "", Scope: "tenant", Name: "w"},
	} {
		r.Backend, r.Definition = "inline", json.RawMessage(`{"value":"v"}`)
		if _, err := s.CredentialDefPut(ctx, r); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	got, err := s.CredentialDefListAll(ctx)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	var keys []string
	for _, r := range got {
		keys = append(keys, r.TenantID+"/"+r.Scope+"/"+r.ScopeID+"/"+r.Name)
	}
	want := []string{"/tenant//w", "a/agent/ag/z", "a/user/u1/y", "b/tenant//x"}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Errorf("list all = %v, want %v", keys, want)
	}
}
