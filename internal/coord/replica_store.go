package coord

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Replica is one row from the replicas heartbeat table.
type Replica struct {
	ID              string    `json:"id"`
	Hostname        string    `json:"hostname"`
	StartedAt       time.Time `json:"started_at"`
	LastHeartbeatAt time.Time `json:"last_heartbeat_at"`
	Version         string    `json:"version"`
}

// ReplicaStore is read/write access to the v0.12.0 replicas table.
// Takes a *pgxpool.Pool directly — bypasses the store.Store interface
// because the table is Postgres-specific (SQLite refuses to start in
// cluster mode).
type ReplicaStore struct {
	pool *pgxpool.Pool
}

// NewReplicaStore wraps an existing pgxpool. The pool is not owned;
// closing ReplicaStore does not close the pool.
func NewReplicaStore(pool *pgxpool.Pool) *ReplicaStore {
	return &ReplicaStore{pool: pool}
}

// UpsertReplica is the heartbeat write — INSERT on first call, UPDATE
// every subsequent. Bumps last_heartbeat_at to now() on every call.
// hostname + version are only persisted on the INSERT row; subsequent
// upserts leave them unchanged (a replica's hostname/version doesn't
// change between restarts; UPDATE would also overwrite if we wanted).
func (s *ReplicaStore) UpsertReplica(ctx context.Context, id, hostname, version string) error {
	if id == "" {
		return errors.New("replica id is empty")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO replicas (id, hostname, version, started_at, last_heartbeat_at)
		VALUES ($1, $2, $3, now(), now())
		ON CONFLICT (id) DO UPDATE SET last_heartbeat_at = now()
	`, id, hostname, version)
	if err != nil {
		return fmt.Errorf("upsert replica %s: %w", id, err)
	}
	return nil
}

// DeleteReplica removes a replica's row. Called on graceful shutdown;
// also exposed for the Phase 5 replicas TTL sweeper to reap stale rows.
func (s *ReplicaStore) DeleteReplica(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("replica id is empty")
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM replicas WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete replica %s: %w", id, err)
	}
	return nil
}

// ListReplicas returns every row, ordered by started_at ascending.
// Backs the /healthz cluster view + Phase 7's /ui/cluster admin page.
func (s *ReplicaStore) ListReplicas(ctx context.Context) ([]Replica, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, hostname, started_at, last_heartbeat_at, version
		FROM replicas
		ORDER BY started_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list replicas: %w", err)
	}
	defer rows.Close()
	var out []Replica
	for rows.Next() {
		var r Replica
		if err := rows.Scan(&r.ID, &r.Hostname, &r.StartedAt, &r.LastHeartbeatAt, &r.Version); err != nil {
			return nil, fmt.Errorf("scan replica row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate replicas: %w", err)
	}
	return out, nil
}

// HeartbeatConfig configures the periodic upsert loop.
type HeartbeatConfig struct {
	ReplicaID string
	Hostname  string
	Version   string

	// Interval between heartbeats. Default 30s. The Phase 5 replicas
	// sweeper marks rows stale at ~2× this interval, so shortening it
	// here also shortens dead-replica detection latency.
	Interval time.Duration

	// ShutdownTimeout caps the DELETE issued on graceful shutdown.
	// Default 5s. A separate context is used so the parent ctx (which
	// is already cancelled at this point) doesn't prevent the cleanup.
	ShutdownTimeout time.Duration
}

const (
	defaultHeartbeatInterval = 30 * time.Second
	defaultShutdownTimeout   = 5 * time.Second
)

// Heartbeat is the background goroutine that keeps this replica's row
// current. Owns the upsert tick and the shutdown DELETE.
type Heartbeat struct {
	store *ReplicaStore
	cfg   HeartbeatConfig
}

func NewHeartbeat(store *ReplicaStore, cfg HeartbeatConfig) *Heartbeat {
	if cfg.Interval == 0 {
		cfg.Interval = defaultHeartbeatInterval
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}
	return &Heartbeat{store: store, cfg: cfg}
}

// Run blocks until ctx is cancelled. Upserts immediately, then every
// cfg.Interval. On ctx.Done, issues a final DELETE with its own
// cfg.ShutdownTimeout context (the parent ctx is cancelled by then).
//
// A failed UPSERT logs a warning and continues — a stale row will be
// reaped by the Phase 5 sweeper. A failed final DELETE logs but does
// not propagate; the row will likewise age out via the sweeper.
func (h *Heartbeat) Run(ctx context.Context) {
	// First upsert: synchronous, before the ticker starts. Ensures the
	// row is visible in /healthz immediately on boot.
	if err := h.store.UpsertReplica(ctx, h.cfg.ReplicaID, h.cfg.Hostname, h.cfg.Version); err != nil {
		log.Printf("coord: initial replica upsert failed: %v", err)
	}
	t := time.NewTicker(h.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Fresh context so the cancellation doesn't immediately
			// kill the DELETE.
			shutCtx, cancel := context.WithTimeout(context.Background(), h.cfg.ShutdownTimeout)
			if err := h.store.DeleteReplica(shutCtx, h.cfg.ReplicaID); err != nil {
				log.Printf("coord: shutdown DELETE for replica %s failed: %v", h.cfg.ReplicaID, err)
			}
			cancel()
			return
		case <-t.C:
			if err := h.store.UpsertReplica(ctx, h.cfg.ReplicaID, h.cfg.Hostname, h.cfg.Version); err != nil {
				log.Printf("coord: replica upsert failed: %v (will retry in %s)", err, h.cfg.Interval)
			}
		}
	}
}

// replicaIDPattern validates LOOMCYCLE_REPLICA_ID values.
// Accepts either a UUID4 or a short alphanumeric label (operator-
// supplied, ≤ 64 chars). The character class is deliberately narrow:
// LISTEN channel names are quoted by pgx so most input would survive,
// but constraining it here means a replica_id can also serve as a
// log-line key, a cluster admin label, and a row PK without escaping
// concerns at any of those sites.
var replicaIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidateReplicaID enforces the format. Called once at config-load.
// Empty replica_id is the "single-replica mode" sentinel and is
// accepted by callers above this layer; the validation here is for
// non-empty values only.
func ValidateReplicaID(id string) error {
	if id == "" {
		return errors.New("replica id is empty")
	}
	if !replicaIDPattern.MatchString(id) {
		return fmt.Errorf("replica id %q: must match [A-Za-z0-9][A-Za-z0-9_-]{0,63}", id)
	}
	return nil
}
