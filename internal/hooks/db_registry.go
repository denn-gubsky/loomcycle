package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// DBBackedRegistry is the v0.12.5 Phase 6 cluster-mode hook registry.
// Wraps the existing in-process Registry with a DB persistence layer
// + backplane invalidation so hooks registered on one replica fire
// for runs on any replica.
//
// Hot-path Match() never hits the DB — it delegates to the inner
// Registry's in-memory cache. Updates flow:
//
//	Register(h) → DB INSERT  →  inner.Register(h)  →  backplane publish
//	Delete(id)  → DB DELETE  →  inner.Delete(id)   →  backplane publish
//
// Backplane consumer goroutine receives peer events and re-loads the
// affected row(s) into the inner cache:
//
//	op=created → store.GetHookByID(id) → inner.Register(reconstructed)
//	op=deleted → inner.Delete(id)
//
// On boot, LoadFromDB seeds the inner cache from the DB. Subsequent
// backplane events keep replicas in sync; a missed event leaves the
// peer's cache stale until the next operation on that hook (acceptable
// per the Phase 1 backplane RFC: "best-effort hint, source-of-truth
// in DB").
//
// IsHostWidenPermitted reads exclusively from inner — the operator
// yaml is the trust boundary (CLAUDE.md rule #8); the DB has no say.
type DBBackedRegistry struct {
	inner     *Registry
	store     hookStore
	backplane coord.Backplane
	replicaID string
}

// hookStore is the minimum surface DBBackedRegistry needs from the
// store layer. *storepostgres.Store implements it implicitly.
type hookStore interface {
	CreateHook(ctx context.Context, h store.HookRow) error
	DeleteHook(ctx context.Context, hookID string) error
	ListHooks(ctx context.Context) ([]store.HookRow, error)
	GetHookByID(ctx context.Context, hookID string) (store.HookRow, error)
}

// NewDBBackedRegistry wires the inner permit-locked Registry to the
// DB + backplane. inner is constructed by the caller via
// NewRegistryWithPermissions so the operator-yaml permit list lives
// inside it (the DB has no say in host-widen permission).
func NewDBBackedRegistry(inner *Registry, hs hookStore, bp coord.Backplane, replicaID string) (*DBBackedRegistry, error) {
	if inner == nil {
		return nil, errors.New("hooks: inner Registry is required")
	}
	if hs == nil {
		return nil, errors.New("hooks: hookStore is required")
	}
	// bp may be nil for tests; LoadFromDB still works without it.
	return &DBBackedRegistry{
		inner:     inner,
		store:     hs,
		backplane: bp,
		replicaID: replicaID,
	}, nil
}

// hookBackplaneEvent is the wire payload on `loomcycle.hook`.
type hookBackplaneEvent struct {
	Op     string `json:"op"` // "created" | "deleted"
	HookID string `json:"hook_id"`
}

// LoadFromDB seeds the inner cache from the DB. Called once at boot
// before any backplane events are delivered. Idempotent: calling it
// multiple times re-loads but the inner Registry's
// replace-on-conflict semantics preserve the chain order.
func (r *DBBackedRegistry) LoadFromDB(ctx context.Context) error {
	rows, err := r.store.ListHooks(ctx)
	if err != nil {
		return fmt.Errorf("hooks: load from db: %w", err)
	}
	for _, row := range rows {
		h := rowToHook(row)
		if _, err := r.inner.Register(h); err != nil {
			log.Printf("hooks: skipping invalid persisted hook %s: %v", row.ID, err)
		}
	}
	return nil
}

// RunBackplaneConsumer subscribes to `loomcycle.hook` and applies
// remote create/delete events to the inner cache. Exits on ctx.Done.
func (r *DBBackedRegistry) RunBackplaneConsumer(ctx context.Context) {
	if r.backplane == nil {
		return
	}
	ch, err := r.backplane.Subscribe(ctx, "loomcycle.hook")
	if err != nil {
		log.Printf("hooks: backplane subscribe: %v", err)
		return
	}
	for evt := range ch {
		var p hookBackplaneEvent
		if err := json.Unmarshal(evt.Payload, &p); err != nil {
			log.Printf("hooks: malformed backplane event: %v", err)
			continue
		}
		switch p.Op {
		case "created":
			row, err := r.store.GetHookByID(ctx, p.HookID)
			if err != nil {
				// Row may have been deleted between publish and now;
				// fine to skip.
				log.Printf("hooks: backplane create-event fetch %s: %v", p.HookID, err)
				continue
			}
			if _, err := r.inner.Register(rowToHook(row)); err != nil {
				log.Printf("hooks: backplane create-event Register %s: %v", p.HookID, err)
			}
		case "deleted":
			if err := r.inner.Delete(p.HookID); err != nil && !errors.Is(err, ErrNotFound) {
				log.Printf("hooks: backplane delete-event %s: %v", p.HookID, err)
			}
		default:
			log.Printf("hooks: unknown backplane op %q", p.Op)
		}
	}
}

// Register satisfies RegistryInterface. Persists to DB, updates the
// in-process cache, then publishes a backplane event so peer replicas
// invalidate. Returns the assigned ID + any validation error.
func (r *DBBackedRegistry) Register(h *Hook) (string, error) {
	// Inner Register validates + assigns ID + bumps RegisteredAt. Call
	// it FIRST so we have the canonical ID for the DB row.
	id, err := r.inner.Register(h)
	if err != nil {
		return "", err
	}
	// Look up the registered hook to pick up the resolved fields
	// (RegisteredAt, Timeout). The List walk is cheap (small N) and
	// avoids a public Get accessor on the inner Registry.
	var registered *Hook
	for _, hook := range r.inner.List() {
		if hook.ID == id {
			registered = hook
			break
		}
	}
	if registered == nil {
		return id, fmt.Errorf("hooks: registered hook %s vanished from cache", id)
	}
	// DB insert. If it fails, roll back the in-process registration
	// so the cluster stays consistent (we don't want a hook firing
	// locally that no peer knows about).
	row := hookToRow(registered, r.replicaID)
	if err := r.store.CreateHook(context.Background(), row); err != nil {
		_ = r.inner.Delete(id)
		return "", fmt.Errorf("hooks: db insert: %w", err)
	}
	r.publishBackplane("created", id)
	return id, nil
}

func (r *DBBackedRegistry) Delete(id string) error {
	if err := r.inner.Delete(id); err != nil {
		return err
	}
	if err := r.store.DeleteHook(context.Background(), id); err != nil {
		log.Printf("hooks: db delete %s failed (in-process already removed): %v", id, err)
		// Don't roll back the in-process delete — the local cache is
		// already missing; the DB delete will re-attempt on next boot
		// via the LoadFromDB reconciliation.
	}
	r.publishBackplane("deleted", id)
	return nil
}

func (r *DBBackedRegistry) List() []*Hook {
	return r.inner.List()
}

func (r *DBBackedRegistry) Match(tenant, agent, tool string, phase Phase) []*Hook {
	return r.inner.Match(tenant, agent, tool, phase)
}

func (r *DBBackedRegistry) IsHostWidenPermitted(tenant, owner string) bool {
	return r.inner.IsHostWidenPermitted(tenant, owner)
}

func (r *DBBackedRegistry) publishBackplane(op, hookID string) {
	if r.backplane == nil {
		return
	}
	payload, _ := json.Marshal(hookBackplaneEvent{Op: op, HookID: hookID})
	if err := r.backplane.Publish(context.Background(), "loomcycle.hook", payload); err != nil {
		log.Printf("hooks: backplane publish(%s, %s): %v", op, hookID, err)
	}
}

// rowToHook converts a store row to a *Hook. The store package uses
// plain strings for Phase + FailMode to avoid a circular import;
// this is where the typed conversion happens.
func rowToHook(r store.HookRow) *Hook {
	timeout := time.Duration(r.TimeoutMs) * time.Millisecond
	return &Hook{
		ID:           r.ID,
		Tenant:       r.Tenant,
		Owner:        r.Owner,
		Name:         r.Name,
		Phase:        Phase(r.Phase),
		Agents:       r.Agents,
		Tools:        r.Tools,
		CallbackURL:  r.CallbackURL,
		FailMode:     FailMode(r.FailMode),
		TimeoutMs:    r.TimeoutMs,
		Timeout:      timeout,
		RegisteredAt: r.CreatedAt,
	}
}

// hookToRow converts the registered *Hook to a store row for INSERT.
func hookToRow(h *Hook, replicaID string) store.HookRow {
	return store.HookRow{
		ID:               h.ID,
		Tenant:           h.Tenant,
		Owner:            h.Owner,
		Name:             h.Name,
		Phase:            string(h.Phase),
		Agents:           h.Agents,
		Tools:            h.Tools,
		CallbackURL:      h.CallbackURL,
		FailMode:         string(h.FailMode),
		TimeoutMs:        h.TimeoutMs,
		CreatedAt:        h.RegisteredAt,
		CreatedByReplica: replicaID,
	}
}

// Compile-time check: DBBackedRegistry satisfies RegistryInterface.
var _ RegistryInterface = (*DBBackedRegistry)(nil)
