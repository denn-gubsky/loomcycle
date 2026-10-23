// Package inboxtest builds a run context with an inbox for tests of the
// producers that capture into it (MCP results, HTTP, WebFetch, the File tool).
package inboxtest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// Store is an in-memory inbox.Store.
type Store struct {
	mu        sync.Mutex
	Ephemeral []store.EphemeralVolumeDefRow
	// Volumes are persistent volume names by tenant, "tenant/name".
	Volumes map[string]bool
}

func (s *Store) EphemeralVolumeCreate(_ context.Context, row store.EphemeralVolumeDefRow) (store.EphemeralVolumeDefRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row.CreatedAt = time.Now()
	s.Ephemeral = append(s.Ephemeral, row)
	return row, nil
}

func (s *Store) EphemeralVolumeListByRun(_ context.Context, root string) ([]store.EphemeralVolumeDefRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.EphemeralVolumeDefRow
	for _, r := range s.Ephemeral {
		if r.RootRunID == root {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Store) VolumeDefGetByName(_ context.Context, tenant, name string) (store.VolumeDefRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Volumes[tenant+"/"+name] {
		return store.VolumeDefRow{TenantID: tenant, Name: name}, nil
	}
	return store.VolumeDefRow{}, &store.ErrNotFound{Kind: "volume_def", ID: name}
}

// Run is a test run tree with an inbox.
type Run struct {
	Ctx    context.Context
	Set    *tools.EphemeralVolumeSet
	Store  *Store
	Tree   *inbox.Tree
	mu     sync.Mutex
	events []providers.Event
}

// Events returns the events the run's emitter received.
func (r *Run) Events() []providers.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]providers.Event(nil), r.events...)
}

// New returns a run tree rooted at rootRunID whose dynamic root is a fresh
// temporary directory. svc fields left zero take the test's defaults.
func New(t *testing.T, rootRunID string, svc inbox.Service) *Run {
	t.Helper()
	if svc.DynamicRoot == "" {
		svc.DynamicRoot = t.TempDir()
	}
	return build(rootRunID, svc)
}

// NewWithoutDynamicRoot returns a run tree in a deployment with no
// dynamic_root volume.
func NewWithoutDynamicRoot(t *testing.T, rootRunID string) *Run {
	t.Helper()
	return build(rootRunID, inbox.Service{})
}

func build(rootRunID string, svc inbox.Service) *Run {
	r := &Run{Set: tools.NewEphemeralVolumeSet(), Store: &Store{Volumes: map[string]bool{}}, Tree: inbox.NewTree()}
	if svc.Store == nil {
		svc.Store = r.Store
	}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t1", UserID: "u1", RootRunID: rootRunID})
	ctx = tools.WithRunID(ctx, rootRunID)
	ctx = tools.WithEphemeralVolumes(ctx, r.Set)
	ctx = tools.WithEventEmitter(ctx, func(ev providers.Event) {
		r.mu.Lock()
		r.events = append(r.events, ev)
		r.mu.Unlock()
	})
	r.Ctx = inbox.With(ctx, svc, r.Tree)
	return r
}
