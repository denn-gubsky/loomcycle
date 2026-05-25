package coord

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

func TestValidateReplicaID_AcceptsUUID(t *testing.T) {
	// A representative UUID4 — middle char in version field is 4, third
	// group leads with 8/9/a/b. ValidateReplicaID is regex-only and does
	// not check the version digit semantics; pass a real one anyway so
	// the test documents the expected shape.
	if err := ValidateReplicaID("3f9b0a2e-1234-4abc-89ef-0123456789ab"); err != nil {
		t.Errorf("UUID4 rejected: %v", err)
	}
}

func TestValidateReplicaID_AcceptsShortLabels(t *testing.T) {
	for _, id := range []string{"a", "replica-a", "lc_1", "R-2", "x123"} {
		if err := ValidateReplicaID(id); err != nil {
			t.Errorf("label %q rejected: %v", id, err)
		}
	}
}

func TestValidateReplicaID_RejectsInvalid(t *testing.T) {
	cases := []string{
		"",                  // empty
		"-leads-with-dash",  // first char must be alnum
		"_leads-with-under", // first char must be alnum
		"has space",
		"has/slash",
		"has.dot",
		"has:colon",
		"has;semi",
		strings.Repeat("a", 65), // too long
	}
	for _, id := range cases {
		if err := ValidateReplicaID(id); err == nil {
			t.Errorf("invalid id %q accepted", id)
		}
	}
}

// TestReplicaIDPatternsAreInSync is the drift catcher for the
// duplicated regex in internal/config and internal/coord. Both
// packages own a copy because the import graph forbids config →
// coord (main.go composes the two; config has to validate at Load
// independently). The two validators must produce the same accept/
// reject decision on every input; this test pins that invariant on
// a corpus designed to catch every realistic divergence shape
// (length boundary, first-char rules, allowed-char set, common
// invalid shapes).
func TestReplicaIDPatternsAreInSync(t *testing.T) {
	corpus := []string{
		// Accept cases
		"a",
		"A",
		"0",
		"replica-a",
		"replica_a",
		"r1",
		"R-2-3",
		"3f9b0a2e-1234-4abc-89ef-0123456789ab",
		strings.Repeat("a", 64), // length boundary — should accept
		// Reject cases
		"",
		"-leading-dash",
		"_leading-under",
		"has space",
		"has/slash",
		"has.dot",
		"has:colon",
		"has;semi",
		"has,comma",
		"has\"quote",
		"has'apos",
		strings.Repeat("a", 65), // length boundary — should reject
	}
	for _, id := range corpus {
		coordErr := ValidateReplicaID(id)
		configErr := config.ValidateReplicaID(id)
		coordOK := coordErr == nil
		configOK := configErr == nil
		if coordOK != configOK {
			t.Errorf("DRIFT on %q: coord accept=%v err=%v / config accept=%v err=%v",
				id, coordOK, coordErr, configOK, configErr)
		}
	}
}

// ---- Postgres-gated tests ----

func pgDSNFromEnv(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LOOMCYCLE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LOOMCYCLE_TEST_PG_DSN not set; skipping coord integration tests.")
	}
	return dsn
}

// freshReplicasTable opens a pgxpool against the DSN and resets the
// `replicas` table to an empty state. Migrations are applied via the
// storepostgres package on the operator's database; the tests assume
// they've already been run (matches the pattern in internal/store/postgres).
//
// Tests that need isolation from concurrent runs should namespace their
// replica IDs (e.g. `TestX_<rand>`) — replicas is a tiny operational
// table; per-test schemas would be overkill.
func freshReplicasTable(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("dial pg: %v", err)
	}
	// Tests do NOT TRUNCATE the table — that would race with other
	// concurrent test packages also touching `replicas`. Each test
	// inserts a unique-named row and cleans up after itself.
	t.Cleanup(func() { pool.Close() })
	return pool
}

func TestReplicaStore_UpsertListDelete(t *testing.T) {
	pool := freshReplicasTable(t, pgDSNFromEnv(t))
	store := NewReplicaStore(pool)
	id := "test-rs-" + time.Now().Format("150405.000")
	ctx := context.Background()
	if err := store.UpsertReplica(ctx, id, "host-a", "v0.12.0-test"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort — pool may already be closed by t.Cleanup ordering.
		_ = store.DeleteReplica(context.Background(), id)
	})

	got, err := store.ListReplicas(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, r := range got {
		if r.ID == id {
			found = true
			if r.Hostname != "host-a" {
				t.Errorf("hostname = %q, want host-a", r.Hostname)
			}
			if r.Version != "v0.12.0-test" {
				t.Errorf("version = %q, want v0.12.0-test", r.Version)
			}
			if r.StartedAt.IsZero() || r.LastHeartbeatAt.IsZero() {
				t.Errorf("zero timestamps: %+v", r)
			}
			break
		}
	}
	if !found {
		t.Fatalf("inserted replica %s not present in list (got %d rows)", id, len(got))
	}

	// Delete + re-list — row should be gone.
	if err := store.DeleteReplica(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got2, err := store.ListReplicas(ctx)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	for _, r := range got2 {
		if r.ID == id {
			t.Fatalf("delete left row present: %+v", r)
		}
	}
}

func TestReplicaStore_UpsertUpdatesHeartbeat(t *testing.T) {
	pool := freshReplicasTable(t, pgDSNFromEnv(t))
	store := NewReplicaStore(pool)
	id := "test-hb-" + time.Now().Format("150405.000")
	ctx := context.Background()
	if err := store.UpsertReplica(ctx, id, "host-b", "v0.12.0-test"); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteReplica(context.Background(), id) })

	rows1, _ := store.ListReplicas(ctx)
	var first time.Time
	for _, r := range rows1 {
		if r.ID == id {
			first = r.LastHeartbeatAt
		}
	}
	if first.IsZero() {
		t.Fatal("first upsert row not found")
	}

	// Sleep enough that Postgres's now() advances measurably even on
	// fast hosts. 50ms is well below the 30s heartbeat tick but enough
	// to register at microsecond timestamp resolution.
	time.Sleep(50 * time.Millisecond)
	if err := store.UpsertReplica(ctx, id, "host-b", "v0.12.0-test"); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	rows2, _ := store.ListReplicas(ctx)
	var second time.Time
	for _, r := range rows2 {
		if r.ID == id {
			second = r.LastHeartbeatAt
		}
	}
	if !second.After(first) {
		t.Errorf("second heartbeat %v not after first %v", second, first)
	}
}

func TestHeartbeat_RunExitsOnContextDone(t *testing.T) {
	pool := freshReplicasTable(t, pgDSNFromEnv(t))
	store := NewReplicaStore(pool)
	id := "test-run-" + time.Now().Format("150405.000")
	hb := NewHeartbeat(store, HeartbeatConfig{
		ReplicaID:       id,
		Hostname:        "host-c",
		Version:         "v0.12.0-test",
		Interval:        50 * time.Millisecond,
		ShutdownTimeout: 2 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hb.Run(ctx)
		close(done)
	}()
	// Give the goroutine time to register the initial row.
	time.Sleep(100 * time.Millisecond)
	rows, _ := store.ListReplicas(context.Background())
	registered := false
	for _, r := range rows {
		if r.ID == id {
			registered = true
		}
	}
	if !registered {
		t.Fatal("heartbeat goroutine did not insert initial row")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not exit within 3s of ctx cancel")
	}

	// After ctx.Done, the shutdown DELETE should have removed the row.
	rows2, _ := store.ListReplicas(context.Background())
	for _, r := range rows2 {
		if r.ID == id {
			t.Errorf("shutdown DELETE did not remove row: %+v", r)
		}
	}
}
