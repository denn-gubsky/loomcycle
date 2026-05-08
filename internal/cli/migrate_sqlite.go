package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "modernc.org/sqlite"

	storepostgres "github.com/denn-gubsky/loomcycle/internal/store/postgres"
)

// runMigrateSqliteToPostgres is the data-migration arm of `loomcycle
// migrate`. It copies an existing SQLite store's contents into a
// freshly-migrated Postgres database, verifies row counts + sample
// transcripts byte-equal, then exits.
//
// Operator workflow:
//
//	# 1. stop loomcycle.
//	# 2. snapshot the SQLite DB.
//	cp /path/to/loomcycle.db /path/to/loomcycle.db.copy
//	# 3. ensure Postgres exists + schema is migrated.
//	loomcycle migrate up --dsn "postgres://..."
//	# 4. copy.
//	loomcycle migrate sqlite-to-postgres \
//	    --src /path/to/loomcycle.db.copy \
//	    --dst "postgres://..."
//	# 5. flip yaml storage.backend to postgres, restart.
//
// Why a *copy* of the SQLite DB and not the live file: SQLite's WAL +
// busy-timeout semantics mean a long-running read on the live DB
// races with any in-flight writes. The copy is a frozen snapshot —
// safer + faster + idempotent.
//
// Idempotency: every INSERT uses `ON CONFLICT DO NOTHING` so a re-run
// after partial failure resumes cleanly. The `events.seq` column has
// its sequence reset via `setval` after the copy so future inserts
// continue from `max(seq)+1`.
//
// Returns 0 on success, 1 on copy/verification failure, 2 on user
// error (missing flags, files don't exist).
func runMigrateSqliteToPostgres(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate sqlite-to-postgres", flag.ContinueOnError)
	fs.SetOutput(stderr)
	src := fs.String("src", "", "path to a SQLite database file (use a COPY of the live DB)")
	dst := fs.String("dst", "", "Postgres DSN")
	autoMigrate := fs.Bool("auto-migrate", false, "run `migrate up` against --dst before copying (default: false)")
	batchSize := fs.Int("batch", 1000, "rows per INSERT batch (events table only)")
	skipVerify := fs.Bool("skip-verify", false, "skip the row-count + transcript spot-check verification phase")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *src == "" || *dst == "" {
		fmt.Fprintln(stderr, "Usage: loomcycle migrate sqlite-to-postgres --src <path> --dst <dsn> [--auto-migrate]")
		return 2
	}
	if _, err := os.Stat(*src); err != nil {
		// User pointed at a non-existent file — user error.
		return fail(stderr, "src: %v", err)
	}

	ctx := context.Background()

	// Open SQLite read-only. The query-string flag tells modernc to
	// skip the journal-mode/foreign-key pragmas it normally applies
	// — we're a reader, not a writer.
	//
	// Open + Ping failures from here onward are infrastructure
	// problems (file corrupt, PG unreachable, schema missing) —
	// exit code 1, not 2.
	srcDB, err := sql.Open("sqlite", *src+"?_pragma=query_only(1)")
	if err != nil {
		return failOp(stderr, "open sqlite: %v", err)
	}
	defer srcDB.Close()
	if err := srcDB.Ping(); err != nil {
		return failOp(stderr, "ping sqlite: %v", err)
	}

	// Open Postgres pool. We don't go through the Store adapter
	// (which would require AutoMigrate=true to bootstrap; we want
	// the operator to control that explicitly via --auto-migrate).
	if *autoMigrate {
		fmt.Fprintln(stdout, "running `migrate up` against destination...")
		if err := storepostgres.MigrateUp(*dst); err != nil {
			return failOp(stderr, "migrate up: %v", err)
		}
	}
	dstPool, err := pgxpool.New(ctx, *dst)
	if err != nil {
		return failOp(stderr, "dial postgres: %v", err)
	}
	defer dstPool.Close()
	if err := dstPool.Ping(ctx); err != nil {
		return failOp(stderr, "ping postgres: %v", err)
	}

	// Verify the destination has the schema. golang-migrate's
	// schema_migrations table is the canary. This one IS arguably
	// user error (operator didn't run `migrate up` first), so 2
	// keeps the existing contract.
	if _, _, err := storepostgres.MigrateStatus(*dst); err != nil {
		return fail(stderr, "destination schema not initialised; pass --auto-migrate or run `loomcycle migrate up` first")
	}

	t0 := time.Now()
	fmt.Fprintf(stdout, "migrating %s -> postgres (batch=%d)\n", *src, *batchSize)

	srcSessions, err := copySessions(ctx, srcDB, dstPool, stdout)
	if err != nil {
		return failOp(stderr, "copy sessions: %v", err)
	}
	srcRuns, err := copyRuns(ctx, srcDB, dstPool, stdout)
	if err != nil {
		return failOp(stderr, "copy runs: %v", err)
	}
	srcEvents, err := copyEvents(ctx, srcDB, dstPool, stdout, *batchSize)
	if err != nil {
		return failOp(stderr, "copy events: %v", err)
	}

	// Reset the events seq sequence so future inserts continue from
	// max(seq)+1. Without this, the next AppendEvent's BIGSERIAL
	// would start at 1 and collide with the rows we just copied.
	if _, err := dstPool.Exec(ctx,
		`SELECT setval(pg_get_serial_sequence('events','seq'),
		               COALESCE((SELECT MAX(seq) FROM events), 1),
		               (SELECT MAX(seq) IS NOT NULL FROM events))`); err != nil {
		return failOp(stderr, "setval events.seq: %v", err)
	}

	if *skipVerify {
		fmt.Fprintf(stdout, "\nDONE in %s (verify skipped)\n", time.Since(t0).Round(time.Millisecond))
		return 0
	}

	// Verification phase. Mismatches are operational failures (a
	// row-count divergence means the copy didn't fully complete).
	if err := verifyRowCounts(ctx, srcDB, dstPool, srcSessions, srcRuns, srcEvents, stdout, stderr); err != nil {
		return 1
	}
	if err := verifyTranscriptSpotCheck(ctx, srcDB, dstPool, stdout, stderr); err != nil {
		return 1
	}
	fmt.Fprintf(stdout, "\nDONE in %s\n", time.Since(t0).Round(time.Millisecond))
	return 0
}

// copySessions streams the sessions table. SQLite stores created_at as
// unix-nano INTEGER; we convert to TIMESTAMPTZ for the Postgres column.
// Empty user_id values come back as NULL so the partial index on
// user_id IS NOT NULL stays small (mirrors what CreateSession writes).
func copySessions(ctx context.Context, src *sql.DB, dst *pgxpool.Pool, stdout io.Writer) (int, error) {
	rows, err := src.QueryContext(ctx, `SELECT id, tenant_id, agent, user_id, created_at FROM sessions`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var (
			id, tenantID, agent string
			userID              *string
			createdAtNs         int64
		)
		if err := rows.Scan(&id, &tenantID, &agent, &userID, &createdAtNs); err != nil {
			return count, fmt.Errorf("scan session: %w", err)
		}
		createdAt := time.Unix(0, createdAtNs).UTC()
		if _, err := dst.Exec(ctx,
			`INSERT INTO sessions (id, tenant_id, agent, user_id, created_at)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (id) DO NOTHING`,
			id, tenantID, agent, userID, createdAt,
		); err != nil {
			return count, fmt.Errorf("insert session %s: %w", id, err)
		}
		count++
		if count%1000 == 0 {
			fmt.Fprintf(stdout, "  sessions: %d copied\n", count)
		}
	}
	if err := rows.Err(); err != nil {
		return count, err
	}
	fmt.Fprintf(stdout, "sessions: %d copied (total)\n", count)
	return count, nil
}

// copyRuns streams the runs table with the v0.4 tracking columns.
// completed_at and last_heartbeat_at are nullable and converted to
// *time.Time so NULL round-trips cleanly.
func copyRuns(ctx context.Context, src *sql.DB, dst *pgxpool.Pool, stdout io.Writer) (int, error) {
	rows, err := src.QueryContext(ctx,
		`SELECT id, session_id, status, started_at, completed_at, stop_reason,
		        input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
		        model, error,
		        agent_id, parent_agent_id, parent_run_id, user_id, last_heartbeat_at
		 FROM runs`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var (
			id, sessionID, status                                    string
			startedAtNs                                              int64
			completedAtNs, lastHeartbeatAtNs                         *int64
			stopReason, model, errMsg                                *string
			inputTokens, outputTokens, cacheCreationTokens, cacheRead int64
			agentID, parentAgentID, parentRunID, userID              *string
		)
		if err := rows.Scan(
			&id, &sessionID, &status, &startedAtNs, &completedAtNs, &stopReason,
			&inputTokens, &outputTokens, &cacheCreationTokens, &cacheRead,
			&model, &errMsg,
			&agentID, &parentAgentID, &parentRunID, &userID, &lastHeartbeatAtNs,
		); err != nil {
			return count, fmt.Errorf("scan run: %w", err)
		}
		startedAt := time.Unix(0, startedAtNs).UTC()
		var completedAt, lastHB *time.Time
		if completedAtNs != nil {
			t := time.Unix(0, *completedAtNs).UTC()
			completedAt = &t
		}
		if lastHeartbeatAtNs != nil {
			t := time.Unix(0, *lastHeartbeatAtNs).UTC()
			lastHB = &t
		}
		if _, err := dst.Exec(ctx,
			`INSERT INTO runs (
				id, session_id, status, started_at, completed_at, stop_reason,
				input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
				model, error,
				agent_id, parent_agent_id, parent_run_id, user_id, last_heartbeat_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
			 ON CONFLICT (id) DO NOTHING`,
			id, sessionID, status, startedAt, completedAt, stopReason,
			inputTokens, outputTokens, cacheCreationTokens, cacheRead,
			model, errMsg,
			agentID, parentAgentID, parentRunID, userID, lastHB,
		); err != nil {
			return count, fmt.Errorf("insert run %s: %w", id, err)
		}
		count++
		if count%1000 == 0 {
			fmt.Fprintf(stdout, "  runs: %d copied\n", count)
		}
	}
	if err := rows.Err(); err != nil {
		return count, err
	}
	fmt.Fprintf(stdout, "runs: %d copied (total)\n", count)
	return count, nil
}

// copyEvents streams events with explicit `seq` values so the API's
// ordering surface stays stable across the migration. Postgres
// BIGSERIAL accepts explicit values; we reset the underlying sequence
// after the copy via the setval call in the caller.
//
// Events are batched into single multi-row INSERTs to amortise round-
// trip cost. ON CONFLICT (seq) is unusual but correct here — seq is
// the primary key, so re-running the migration mid-flight is
// idempotent.
func copyEvents(ctx context.Context, src *sql.DB, dst *pgxpool.Pool, stdout io.Writer, batchSize int) (int, error) {
	rows, err := src.QueryContext(ctx,
		`SELECT seq, session_id, run_id, ts, type, payload
		 FROM events ORDER BY seq ASC`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type ev struct {
		seq                  int64
		sessionID, runID     string
		ts                   time.Time
		typ                  string
		payload              []byte
	}
	batch := make([]ev, 0, batchSize)
	count := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		// Build a multi-row INSERT. pgx supports SendBatch for the
		// canonical batched-insert path; we use a single statement
		// here because (a) it keeps the prepared-statement count
		// bounded and (b) ON CONFLICT semantics are simpler.
		args := make([]any, 0, len(batch)*6)
		var sb stringWriter
		sb.WriteString(`INSERT INTO events (seq, session_id, run_id, ts, type, payload) VALUES `)
		for i, e := range batch {
			if i > 0 {
				sb.WriteString(",")
			}
			off := i*6 + 1
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d)", off, off+1, off+2, off+3, off+4, off+5)
			args = append(args, e.seq, e.sessionID, e.runID, e.ts, e.typ, e.payload)
		}
		sb.WriteString(` ON CONFLICT (seq) DO NOTHING`)
		if _, err := dst.Exec(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("insert events batch: %w", err)
		}
		batch = batch[:0]
		return nil
	}

	for rows.Next() {
		var (
			e    ev
			tsNs int64
		)
		if err := rows.Scan(&e.seq, &e.sessionID, &e.runID, &tsNs, &e.typ, &e.payload); err != nil {
			return count, fmt.Errorf("scan event: %w", err)
		}
		e.ts = time.Unix(0, tsNs).UTC()
		batch = append(batch, e)
		count++
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return count, err
			}
			fmt.Fprintf(stdout, "  events: %d copied\n", count)
		}
	}
	if err := rows.Err(); err != nil {
		return count, err
	}
	if err := flush(); err != nil {
		return count, err
	}
	fmt.Fprintf(stdout, "events: %d copied (total)\n", count)
	return count, nil
}

func verifyRowCounts(
	ctx context.Context, src *sql.DB, dst *pgxpool.Pool,
	srcSessions, srcRuns, srcEvents int,
	stdout, stderr io.Writer,
) error {
	fmt.Fprintln(stdout, "\nverifying row counts...")
	checks := []struct {
		table  string
		srcCnt int
	}{
		{"sessions", srcSessions},
		{"runs", srcRuns},
		{"events", srcEvents},
	}
	for _, c := range checks {
		var dstCnt int
		if err := dst.QueryRow(ctx, `SELECT COUNT(*) FROM `+c.table).Scan(&dstCnt); err != nil {
			fmt.Fprintf(stderr, "loomcycle: error: count %s on dst: %v\n", c.table, err)
			return err
		}
		mark := "OK"
		if dstCnt != c.srcCnt {
			mark = "MISMATCH"
		}
		fmt.Fprintf(stdout, "  %s: src=%d dst=%d %s\n", c.table, c.srcCnt, dstCnt, mark)
		if dstCnt != c.srcCnt {
			return fmt.Errorf("row-count mismatch on %s", c.table)
		}
	}
	_ = src // kept on signature for symmetry with verifyTranscriptSpotCheck
	return nil
}

// verifyTranscriptSpotCheck pulls 10 random session IDs from SQLite
// and asserts each one's transcript (events ORDER BY seq) is byte-
// equal between the two adapters. Catches the case where the ORDER BY
// path differs (Postgres returns events in seq order; SQLite likewise;
// any divergence here would indicate a bug in copyEvents).
//
// Limit of 10 sessions: keeps verification fast on large DBs while
// surfacing the most likely failure mode.
func verifyTranscriptSpotCheck(
	ctx context.Context, src *sql.DB, dst *pgxpool.Pool,
	stdout, stderr io.Writer,
) error {
	fmt.Fprintln(stdout, "spot-checking 10 random session transcripts...")
	rows, err := src.QueryContext(ctx, `SELECT id FROM sessions ORDER BY RANDOM() LIMIT 10`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var sessionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		sessionIDs = append(sessionIDs, id)
	}
	if len(sessionIDs) == 0 {
		fmt.Fprintln(stdout, "  (no sessions in source — skipping spot-check)")
		return nil
	}
	for _, sid := range sessionIDs {
		srcDigest, err := digestSqliteTranscript(ctx, src, sid)
		if err != nil {
			return fmt.Errorf("digest src %s: %w", sid, err)
		}
		dstDigest, err := digestPostgresTranscript(ctx, dst, sid)
		if err != nil {
			return fmt.Errorf("digest dst %s: %w", sid, err)
		}
		mark := "OK"
		if srcDigest != dstDigest {
			mark = "MISMATCH"
		}
		fmt.Fprintf(stdout, "  %s: %s\n", sid, mark)
		if srcDigest != dstDigest {
			fmt.Fprintf(stderr, "loomcycle: error: transcript mismatch on session %s\n", sid)
			return fmt.Errorf("transcript mismatch")
		}
	}
	return nil
}

func digestSqliteTranscript(ctx context.Context, db *sql.DB, sessionID string) (string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT seq, run_id, type, payload
		 FROM events WHERE session_id = ? ORDER BY seq ASC`, sessionID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var (
			seq          int64
			runID, typ   string
			payload      []byte
		)
		if err := rows.Scan(&seq, &runID, &typ, &payload); err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%d|%s|%s|", seq, runID, typ)
		h.Write(payload)
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil)), rows.Err()
}

func digestPostgresTranscript(ctx context.Context, pool *pgxpool.Pool, sessionID string) (string, error) {
	rows, err := pool.Query(ctx,
		`SELECT seq, run_id, type, payload
		 FROM events WHERE session_id = $1 ORDER BY seq ASC`, sessionID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var (
			seq        int64
			runID, typ string
			payload    []byte
		)
		if err := rows.Scan(&seq, &runID, &typ, &payload); err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%d|%s|%s|", seq, runID, typ)
		h.Write(payload)
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil)), rows.Err()
}

// stringWriter is a tiny strings.Builder shim usable with fmt.Fprintf.
// Avoids pulling strings.Builder + a dedicated %s helper for the one
// place that needs both `WriteString` and `Fprintf` together.
type stringWriter struct {
	buf []byte
}

func (w *stringWriter) Write(p []byte) (int, error)        { w.buf = append(w.buf, p...); return len(p), nil }
func (w *stringWriter) WriteString(s string) (int, error)  { w.buf = append(w.buf, s...); return len(s), nil }
func (w *stringWriter) String() string                     { return string(w.buf) }
