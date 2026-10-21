//go:build unix

package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A rolling update that starts the new replica before it stops the old one
// boots nothing after the old one parks its runs, so the boot-time resume pass
// never sees them. The running replica's periodic resume sweep picks them up
// once the stopped replica has gone.
//
// Needs Postgres (cluster mode): LOOMCYCLE_TEST_PG_DSN, with a role that may
// create databases — each run gets its own, migrated by the binary.
func TestBoot_ClusterReplicaResumesRunsAStoppedReplicaParked(t *testing.T) {
	dsn := os.Getenv("LOOMCYCLE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LOOMCYCLE_TEST_PG_DSN not set")
	}
	b := newShutdownPauseBoot(t)
	clusterDSN := freshClusterDatabase(t, dsn)
	cluster := func(replica string) []string {
		return []string{
			"LOOMCYCLE_STORAGE_BACKEND=postgres",
			"LOOMCYCLE_PG_DSN=" + clusterDSN,
			"LOOMCYCLE_PG_AUTOMIGRATE=1",
			"LOOMCYCLE_REPLICA_ID=" + replica,
			"LOOMCYCLE_REPLICAS_SWEEP_INTERVAL_MS=500",
		}
	}

	old := b.startOn(t, freePort(t), cluster("f1-old")...)
	// The new replica is up before the old one stops, as with maxSurge.
	newer := b.startOn(t, freePort(t), cluster("f1-new")...)
	old.startRun(t, "cluster-pause-1")
	pgid := waitForSleeper(t, b.volume, old)
	shutDownParked(t, old, pgid)

	pool := clusterPool(t, clusterDSN)
	var runID string
	for deadline := time.Now().Add(60 * time.Second); ; {
		var status, pauseState string
		err := pool.QueryRow(context.Background(),
			`SELECT id, status, COALESCE(pause_state, '') FROM runs WHERE agent_id = $1`, "cluster-pause-1").
			Scan(&runID, &status, &pauseState)
		if err != nil {
			t.Fatalf("read run: %v", err)
		}
		if status == "completed" {
			break
		}
		if status == "failed" || status == "cancelled" || time.Now().After(deadline) {
			t.Fatalf("the parked run on the running replica = status %q pause_state %q, want it resumed to completed\n--- new replica ---\n%s",
				status, pauseState, newer.out.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	var replica string
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(replica_id, '') FROM runs WHERE id = $1`, runID).Scan(&replica); err != nil {
		t.Fatal(err)
	}
	if replica != "f1-new" {
		t.Errorf("the resumed run's replica = %q, want f1-new", replica)
	}
}

// freshClusterDatabase creates an empty database for one test and returns its
// DSN; it is dropped when the test ends.
func freshClusterDatabase(t *testing.T, dsn string) string {
	t.Helper()
	admin := clusterPool(t, dsn)
	name := fmt.Sprintf("lc_f1_%d", time.Now().UnixNano())
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Skipf("cannot create a test database (needs CREATEDB): %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func clusterPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
