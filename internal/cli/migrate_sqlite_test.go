package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denn-gubsky/loomcycle/internal/store"
	storepostgres "github.com/denn-gubsky/loomcycle/internal/store/postgres"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// pgDSNFromEnv returns the test-fixture DSN or skips. Mirrors the
// pattern in internal/store/postgres/postgres_test.go.
func pgDSNFromEnv(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LOOMCYCLE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LOOMCYCLE_TEST_PG_DSN not set; skipping. Run `make pg-up` first.")
	}
	return dsn
}

var migrateSqliteSchemaCounter atomic.Uint64

// freshPgSchema spins up a per-test schema in the test fixture, runs
// migrations, and returns a Pool + DSN-with-search-path + cleanup.
// Same pattern as internal/store/postgres/postgres_test.go's helper
// but isolated in this package because cli/ can't import internal
// test code from another package.
func freshPgSchema(t *testing.T, dsn string) (string, func()) {
	t.Helper()

	n := migrateSqliteSchemaCounter.Add(1)
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, t.Name())
	if len(clean) > 30 {
		clean = clean[:30]
	}
	schema := fmt.Sprintf("clim_%s_%d", clean, n)

	bootstrap, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer bootstrap.Close()
	if _, err := bootstrap.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	scopedDSN := dsn
	sep := "?"
	if strings.Contains(scopedDSN, "?") {
		sep = "&"
	}
	scopedDSN = scopedDSN + sep + "search_path=" + schema

	if err := storepostgres.MigrateUp(scopedDSN); err != nil {
		// best-effort cleanup
		conn2, _ := pgxpool.New(context.Background(), dsn)
		if conn2 != nil {
			_, _ = conn2.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
			conn2.Close()
		}
		t.Fatalf("migrate up: %v", err)
	}

	cleanup := func() {
		conn, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Logf("cleanup dial: %v", err)
			return
		}
		defer conn.Close()
		if _, err := conn.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Logf("drop schema: %v", err)
		}
	}
	return scopedDSN, cleanup
}

// End-to-end: populate a sqlite DB, copy to postgres, verify row
// counts + transcripts. The postgres adapter's contract test already
// covers the destination side; this test exercises the copy code
// path itself (type conversion, idempotency, seq preservation,
// verification phase).
func TestMigrateSqliteToPostgres_EndToEnd(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	scopedDSN, cleanup := freshPgSchema(t, dsn)
	defer cleanup()

	// Build a sqlite source with a meaningful mix: 3 sessions, 5
	// runs (one per parent + 2 sub-agent rows), each run has
	// several events of varying types.
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := storesqlite.Open(srcPath)
	if err != nil {
		t.Fatalf("open sqlite src: %v", err)
	}
	ctx := context.Background()

	sessA, _ := src.CreateSession(ctx, "tenant-1", "agent-a", "alice")
	sessB, _ := src.CreateSession(ctx, "tenant-1", "agent-b", "alice")
	sessC, _ := src.CreateSession(ctx, "tenant-2", "agent-a", "bob")

	runA1, _ := src.CreateRun(ctx, sessA.ID, store.RunIdentity{AgentID: "a_top1", UserID: "alice"})
	runA2, _ := src.CreateRun(ctx, sessA.ID, store.RunIdentity{AgentID: "a_top2", ParentAgentID: "a_top1", ParentRunID: runA1.ID, UserID: "alice"})
	runB1, _ := src.CreateRun(ctx, sessB.ID, store.RunIdentity{AgentID: "a_top3", UserID: "alice"})
	runC1, _ := src.CreateRun(ctx, sessC.ID, store.RunIdentity{AgentID: "a_top4", UserID: "bob"})
	runC2, _ := src.CreateRun(ctx, sessC.ID, store.RunIdentity{}) // legacy-shape run with no identity

	for _, run := range []store.Run{runA1, runA2, runB1, runC1, runC2} {
		for i := 0; i < 4; i++ {
			payload := []byte(fmt.Sprintf(`{"i":%d,"run":"%s"}`, i, run.ID))
			if err := src.AppendEvent(ctx, run.ID, "text", payload); err != nil {
				t.Fatalf("append event: %v", err)
			}
		}
	}
	_ = src.UpdateHeartbeat(ctx, runA1.ID)
	_ = src.FinishRun(ctx, runA1.ID, store.RunCompleted, "end_turn",
		store.Usage{InputTokens: 100, OutputTokens: 50, Model: "claude-sonnet-4-6"}, "")
	_ = src.FinishRun(ctx, runC2.ID, store.RunCancelled, "user_stopped", store.Usage{}, "")
	_ = src.Close()

	// Run the copy.
	args := []string{
		"sqlite-to-postgres",
		"--src", srcPath,
		"--dst", scopedDSN,
		"--batch", "10", // small batch to exercise the multi-flush path
	}
	var stdout, stderr bytes.Buffer
	rc := RunMigrate(args, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc=%d\nstdout:\n%s\nstderr:\n%s", rc, stdout.String(), stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"sessions: 3 copied",
		"runs: 5 copied",
		"events: 20 copied",
		"verifying row counts",
		"sessions: src=3 dst=3 OK",
		"runs: src=5 dst=5 OK",
		"events: src=20 dst=20 OK",
		"DONE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\n--- got ---\n%s", want, out)
		}
	}

	// Re-running on the same DB must be idempotent: same row counts,
	// no errors. ON CONFLICT DO NOTHING + setval(seq) make this safe.
	var stdout2, stderr2 bytes.Buffer
	rc2 := RunMigrate(args, &stdout2, &stderr2)
	if rc2 != 0 {
		t.Fatalf("idempotent re-run rc=%d\nstdout:\n%s\nstderr:\n%s", rc2, stdout2.String(), stderr2.String())
	}
	out2 := stdout2.String()
	if !strings.Contains(out2, "events: src=20 dst=20 OK") {
		t.Errorf("idempotent re-run row counts: %q", out2)
	}

	// Open the destination via the Store adapter and assert the
	// migrated data round-trips: identity fields, terminal statuses,
	// transcript ordering.
	dst, err := storepostgres.Open(ctx, storepostgres.Config{DSN: scopedDSN})
	if err != nil {
		t.Fatalf("open pg dst: %v", err)
	}
	defer dst.Close()

	gotA1, err := dst.GetRunByAgentID(ctx, "a_top1")
	if err != nil {
		t.Fatalf("GetRunByAgentID a_top1: %v", err)
	}
	if gotA1.Status != store.RunCompleted {
		t.Errorf("a_top1 status: got %q, want completed", gotA1.Status)
	}
	if gotA1.Model != "claude-sonnet-4-6" {
		t.Errorf("a_top1 model: got %q", gotA1.Model)
	}
	if gotA1.LastHeartbeatAt.IsZero() {
		t.Errorf("a_top1 last_heartbeat_at: should be non-zero (we called UpdateHeartbeat)")
	}

	gotA2, _ := dst.GetRunByAgentID(ctx, "a_top2")
	if gotA2.ParentAgentID != "a_top1" || gotA2.ParentRunID != runA1.ID {
		t.Errorf("a_top2 parent fields lost: %+v", gotA2)
	}

	transcript, err := dst.GetTranscript(ctx, sessA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 8 {
		t.Errorf("sessA transcript len: got %d, want 8 (2 runs × 4 events)", len(transcript))
	}
	for i := 1; i < len(transcript); i++ {
		if transcript[i].Seq <= transcript[i-1].Seq {
			t.Errorf("seq not ascending at %d", i)
			break
		}
	}

	// After the copy, a fresh AppendEvent on the destination must
	// pick a seq strictly greater than the highest copied seq —
	// proves the setval(pg_get_serial_sequence(...)) call did its
	// job and future inserts won't collide.
	prevMaxSeq := transcript[len(transcript)-1].Seq
	if err := dst.AppendEvent(ctx, runA1.ID, "post-migration", []byte(`{}`)); err != nil {
		t.Fatalf("post-migration append: %v", err)
	}
	transcript2, _ := dst.GetTranscript(ctx, sessA.ID)
	newSeq := transcript2[len(transcript2)-1].Seq
	if newSeq <= prevMaxSeq {
		t.Errorf("post-migration seq did not advance: got %d, want > %d", newSeq, prevMaxSeq)
	}
}

// Missing --src / --dst flags → rc=2 + usage hint.
func TestMigrateSqliteToPostgres_MissingFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunMigrate([]string{"sqlite-to-postgres"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("stderr missing usage: %q", stderr.String())
	}
}

// Source file doesn't exist → rc=2 with a stat error.
func TestMigrateSqliteToPostgres_MissingSource(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunMigrate([]string{
		"sqlite-to-postgres",
		"--src", "/no/such/file.db",
		"--dst", "postgres://nowhere",
	}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "src:") {
		t.Errorf("stderr missing src error: %q", stderr.String())
	}
}

// Note: digest correctness is exercised by the end-to-end test above
// (verifyTranscriptSpotCheck reads back its own copy and digests both
// sides; if either side miscomputes, the test fails with "transcript
// mismatch"). A standalone digest test would need to reach into the
// storesqlite.Store's private *sql.DB field, which we deliberately
// don't expose — encapsulation matters more than test convenience
// here.
