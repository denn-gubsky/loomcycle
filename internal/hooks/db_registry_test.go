package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// stubHookStore is the in-memory hookStore fake.
type stubHookStore struct {
	rows         map[string]store.HookRow
	createErr    error
	deleteErr    error
	createCalls  atomic.Int32
	deleteCalls  atomic.Int32
	getByIDCalls atomic.Int32
}

func newStubHookStore() *stubHookStore {
	return &stubHookStore{rows: map[string]store.HookRow{}}
}

func (s *stubHookStore) CreateHook(_ context.Context, h store.HookRow) error {
	s.createCalls.Add(1)
	if s.createErr != nil {
		return s.createErr
	}
	s.rows[h.ID] = h
	return nil
}

func (s *stubHookStore) DeleteHook(_ context.Context, id string) error {
	s.deleteCalls.Add(1)
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.rows, id)
	return nil
}

func (s *stubHookStore) ListHooks(_ context.Context) ([]store.HookRow, error) {
	out := make([]store.HookRow, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out, nil
}

func (s *stubHookStore) GetHookByID(_ context.Context, id string) (store.HookRow, error) {
	s.getByIDCalls.Add(1)
	r, ok := s.rows[id]
	if !ok {
		return store.HookRow{}, &store.ErrNotFound{Kind: "hook", ID: id}
	}
	return r, nil
}

// stubBackplane is the in-process backplane for tests.
type stubBackplane struct {
	publishCount atomic.Int32
	feedCh       chan coord.Event
}

func newStubBackplane() *stubBackplane {
	return &stubBackplane{feedCh: make(chan coord.Event, 16)}
}

func (s *stubBackplane) Publish(_ context.Context, _ string, _ []byte) error {
	s.publishCount.Add(1)
	return nil
}

func (s *stubBackplane) Subscribe(_ context.Context, _ string) (<-chan coord.Event, error) {
	return s.feedCh, nil
}

func (s *stubBackplane) Close() error { return nil }

func sampleHook() *Hook {
	return &Hook{
		Owner:       "app-A",
		Name:        "test-hook",
		Phase:       PhasePre,
		Tools:       []string{"Read"},
		CallbackURL: "https://example.com/hook",
	}
}

func TestDBBackedRegistry_Register_PersistsAndPublishes(t *testing.T) {
	inner := NewRegistry()
	hs := newStubHookStore()
	bp := newStubBackplane()
	r, err := NewDBBackedRegistry(inner, hs, bp, "rep-a")
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}

	id, err := r.Register(sampleHook())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if id == "" {
		t.Fatal("empty id returned")
	}
	if hs.createCalls.Load() != 1 {
		t.Errorf("store.CreateHook calls = %d, want 1", hs.createCalls.Load())
	}
	if bp.publishCount.Load() != 1 {
		t.Errorf("backplane.Publish calls = %d, want 1", bp.publishCount.Load())
	}
	if got := len(r.List()); got != 1 {
		t.Errorf("List() len = %d, want 1", got)
	}
}

func TestDBBackedRegistry_Register_RollsBackOnDBError(t *testing.T) {
	inner := NewRegistry()
	hs := newStubHookStore()
	hs.createErr = errors.New("simulated db failure")
	bp := newStubBackplane()
	r, _ := NewDBBackedRegistry(inner, hs, bp, "rep-a")

	_, err := r.Register(sampleHook())
	if err == nil {
		t.Fatal("expected error from db insert")
	}
	if got := len(r.List()); got != 0 {
		t.Errorf("inner cache should be rolled back on db error, got %d entries", got)
	}
	if bp.publishCount.Load() != 0 {
		t.Error("backplane.Publish fired despite db error")
	}
}

func TestDBBackedRegistry_Delete_RemovesAndPublishes(t *testing.T) {
	inner := NewRegistry()
	hs := newStubHookStore()
	bp := newStubBackplane()
	r, _ := NewDBBackedRegistry(inner, hs, bp, "rep-a")

	id, err := r.Register(sampleHook())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	bp.publishCount.Store(0) // reset for the delete-only assertion

	if err := r.Delete(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := len(r.List()); got != 0 {
		t.Errorf("List() len = %d, want 0 after delete", got)
	}
	if hs.deleteCalls.Load() != 1 {
		t.Errorf("store.DeleteHook calls = %d, want 1", hs.deleteCalls.Load())
	}
	if bp.publishCount.Load() != 1 {
		t.Errorf("backplane.Publish calls = %d, want 1 (delete event)", bp.publishCount.Load())
	}
}

func TestDBBackedRegistry_LoadFromDB_SeedsInnerCache(t *testing.T) {
	inner := NewRegistry()
	hs := newStubHookStore()
	// Seed DB rows directly. The Tenant is set so we can assert it survives the
	// rowToHook conversion (RFC AF: a dropped Tenant on reload would silently
	// un-scope a tenant hook to global, firing on every tenant's runs).
	hs.rows["hook_x"] = store.HookRow{
		ID: "hook_x", Tenant: "tenant-a", Owner: "app", Name: "n", Phase: "pre",
		Agents: []string{}, Tools: []string{"Read"},
		CallbackURL: "https://example.com",
		FailMode:    "open",
	}
	r, _ := NewDBBackedRegistry(inner, hs, nil, "rep-a")

	if err := r.LoadFromDB(context.Background()); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	got := r.List()
	if len(got) != 1 {
		t.Fatalf("List() len = %d, want 1 (seeded from db)", len(got))
	}
	if got[0].Tenant != "tenant-a" {
		t.Errorf("reloaded hook Tenant = %q, want tenant-a (rowToHook dropped the tenant)", got[0].Tenant)
	}
}

func TestDBBackedRegistry_BackplaneCreatedEvent_HydratesCache(t *testing.T) {
	// Replica A: registers a hook (DB has the row).
	// Replica B: receives a `created` backplane event → fetches from
	// DB → inserts into its own inner cache.
	inner := NewRegistry()
	hs := newStubHookStore()
	bp := newStubBackplane()
	r, _ := NewDBBackedRegistry(inner, hs, bp, "rep-b")

	// Seed the DB row directly (simulating replica A's insert), with a tenant
	// so the backplane-hydration path's rowToHook is asserted to carry it.
	hs.rows["hook_remote"] = store.HookRow{
		ID: "hook_remote", Tenant: "tenant-b", Owner: "app", Name: "n", Phase: "pre",
		Agents: []string{}, Tools: []string{"Read"},
		CallbackURL: "https://example.com",
		FailMode:    "open",
	}

	// Fire backplane consumer in background, then feed an event.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.RunBackplaneConsumer(ctx)

	payload, _ := json.Marshal(hookBackplaneEvent{Op: "created", HookID: "hook_remote"})
	bp.feedCh <- coord.Event{Topic: "loomcycle.hook", Payload: payload}

	// Poll for hydration with a small sleep so the consumer goroutine
	// has a chance to run.
	for i := 0; i < 100; i++ {
		if len(r.List()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	hydrated := r.List()
	if len(hydrated) != 1 {
		t.Fatalf("inner cache should have hydrated from DB, got %d entries", len(hydrated))
	}
	if hydrated[0].Tenant != "tenant-b" {
		t.Errorf("backplane-hydrated hook Tenant = %q, want tenant-b", hydrated[0].Tenant)
	}
}

func TestDBBackedRegistry_BackplaneDeletedEvent_EvictsFromCache(t *testing.T) {
	inner := NewRegistry()
	hs := newStubHookStore()
	bp := newStubBackplane()
	r, _ := NewDBBackedRegistry(inner, hs, bp, "rep-b")

	// Seed an entry directly in inner (simulating prior LoadFromDB
	// or backplane create).
	id, _ := inner.Register(sampleHook())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.RunBackplaneConsumer(ctx)

	payload, _ := json.Marshal(hookBackplaneEvent{Op: "deleted", HookID: id})
	bp.feedCh <- coord.Event{Topic: "loomcycle.hook", Payload: payload}

	for i := 0; i < 100; i++ {
		if len(r.List()) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(r.List()); got != 0 {
		t.Errorf("inner cache should be empty after delete event, got %d entries", got)
	}
}

// Compile-time: stub satisfies the hookStore interface so the
// constructor accepts it.
var _ hookStore = (*stubHookStore)(nil)
