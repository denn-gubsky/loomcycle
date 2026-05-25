package coord

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestValidateTopic_AcceptsLoomcyclePrefix(t *testing.T) {
	for _, topic := range []string{
		"loomcycle.cancel",
		"loomcycle.pause",
		"loomcycle.test-1",
		"loomcycle.run_state",
	} {
		if err := validateTopic(topic); err != nil {
			t.Errorf("%q rejected: %v", topic, err)
		}
	}
}

func TestValidateTopic_Rejects(t *testing.T) {
	cases := []string{
		"",                    // empty
		"cancel",              // no prefix
		"loomcycle.",          // empty suffix
		"loomcycle.has space", // space in suffix
		"loomcycle.has/slash", // slash
		"OTHER.cancel",        // wrong prefix
	}
	for _, topic := range cases {
		if err := validateTopic(topic); err == nil {
			t.Errorf("invalid topic %q accepted", topic)
		}
	}
}

// ---- Postgres-gated tests ----

func TestPostgresBackplane_PublishSubscribeRoundtrip(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer pool.Close()

	// Two backplanes simulating two replicas sharing one Postgres.
	bpA, err := NewPostgresBackplane(PostgresBackplaneConfig{
		Pool: pool, DSN: dsn, ReplicaID: "test-A-" + time.Now().Format("150405.000"),
	})
	if err != nil {
		t.Fatalf("backplane A: %v", err)
	}
	defer bpA.Close()
	bpB, err := NewPostgresBackplane(PostgresBackplaneConfig{
		Pool: pool, DSN: dsn, ReplicaID: "test-B-" + time.Now().Format("150405.000"),
	})
	if err != nil {
		t.Fatalf("backplane B: %v", err)
	}
	defer bpB.Close()

	// Subscribe on B BEFORE A publishes — otherwise the NOTIFY misses
	// the listener entirely.
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	topic := "loomcycle.test." + time.Now().Format("150405.000")
	ch, err := bpB.Subscribe(subCtx, topic)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// Give the LISTEN connection a moment to actually issue the LISTEN
	// statement on its dedicated conn before we publish.
	time.Sleep(150 * time.Millisecond)

	payload := []byte("hello-from-A")
	if err := bpA.Publish(context.Background(), topic, payload); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case evt := <-ch:
		if !bytes.Equal(evt.Payload, payload) {
			t.Errorf("payload mismatch: got %q, want %q", evt.Payload, payload)
		}
		if evt.PublisherReplicaID != bpA.replicaID {
			t.Errorf("publisher = %q, want %q", evt.PublisherReplicaID, bpA.replicaID)
		}
		if evt.Topic != topic {
			t.Errorf("topic = %q, want %q", evt.Topic, topic)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive event on B's subscription within 3s")
	}
}

func TestPostgresBackplane_SelfMessageFiltered(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer pool.Close()
	bp, err := NewPostgresBackplane(PostgresBackplaneConfig{
		Pool: pool, DSN: dsn, ReplicaID: "test-self-" + time.Now().Format("150405.000"),
	})
	if err != nil {
		t.Fatalf("backplane: %v", err)
	}
	defer bp.Close()

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	topic := "loomcycle.test.self." + time.Now().Format("150405.000")
	ch, err := bp.Subscribe(subCtx, topic)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	if err := bp.Publish(context.Background(), topic, []byte("self")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case evt := <-ch:
		t.Errorf("unexpected self-event delivered: %+v", evt)
	case <-time.After(500 * time.Millisecond):
		// Expected — self-message filtered.
	}
}

func TestPostgresBackplane_PayloadTooLarge(t *testing.T) {
	// Pure unit: does not need the DB connection to actually round-trip
	// — the size check fires before pg_notify. Use a real pool to
	// satisfy the constructor's nil check, but we never publish enough
	// to reach the DB.
	dsn := pgDSNFromEnv(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer pool.Close()
	bp, err := NewPostgresBackplane(PostgresBackplaneConfig{
		Pool: pool, DSN: dsn, ReplicaID: "test-large-" + time.Now().Format("150405.000"),
	})
	if err != nil {
		t.Fatalf("backplane: %v", err)
	}
	defer bp.Close()

	big := bytes.Repeat([]byte("x"), MaxPayloadBytes+1)
	err = bp.Publish(context.Background(), "loomcycle.test.toolarge", big)
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Errorf("got %v, want ErrPayloadTooLarge", err)
	}
}

func TestPostgresBackplane_PublishAfterClose(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer pool.Close()
	bp, err := NewPostgresBackplane(PostgresBackplaneConfig{
		Pool: pool, DSN: dsn, ReplicaID: "test-closed-" + time.Now().Format("150405.000"),
	})
	if err != nil {
		t.Fatalf("backplane: %v", err)
	}
	if err := bp.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	err = bp.Publish(context.Background(), "loomcycle.test", []byte("nope"))
	if !errors.Is(err, ErrBackplaneClosed) {
		t.Errorf("got %v, want ErrBackplaneClosed", err)
	}
}

func TestPostgresBackplane_InvalidTopic(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer pool.Close()
	bp, err := NewPostgresBackplane(PostgresBackplaneConfig{
		Pool: pool, DSN: dsn, ReplicaID: "test-topic-" + time.Now().Format("150405.000"),
	})
	if err != nil {
		t.Fatalf("backplane: %v", err)
	}
	defer bp.Close()

	for _, topic := range []string{"no-prefix", "loomcycle.", "loomcycle.has space"} {
		err := bp.Publish(context.Background(), topic, []byte("x"))
		if !errors.Is(err, ErrInvalidTopic) {
			t.Errorf("topic %q: got %v, want ErrInvalidTopic", topic, err)
		}
	}
}

func TestNewPostgresBackplane_ValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  PostgresBackplaneConfig
		want string // substring of error message
	}{
		{"nil pool", PostgresBackplaneConfig{DSN: "x", ReplicaID: "x"}, "pgxpool is required"},
		{"empty DSN", PostgresBackplaneConfig{Pool: &pgxpool.Pool{}, ReplicaID: "x"}, "DSN is required"},
		{"bad replica id", PostgresBackplaneConfig{Pool: &pgxpool.Pool{}, DSN: "x", ReplicaID: "has space"}, "replica id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPostgresBackplane(tc.cfg)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}
