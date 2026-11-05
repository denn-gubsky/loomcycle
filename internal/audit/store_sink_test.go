package audit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

func sinkStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// slowAppender serializes appends behind one lock and makes each take a
// while, as a contended head lock on a busy database does.
type slowAppender struct {
	*sqlite.Store
	mu    sync.Mutex
	delay time.Duration
}

func (a *slowAppender) AuditAppend(ctx context.Context, rows []store.AuditEventRow, seal store.AuditSealFunc, opened *store.AuditEventRow) ([]store.AuditEventRow, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	time.Sleep(a.delay)
	return a.Store.AuditAppend(ctx, rows, seal, opened)
}

// TestStoreSink_AFloodOfFailedSignInsDoesNotBlockATokenMint: failed
// sign-ins are observations — queued, written by one writer, never waited
// for — so a flood of them neither slows its own callers nor holds up a
// state change (a token mint) recorded at the same time.
func TestStoreSink_AFloodOfFailedSignInsDoesNotBlockATokenMint(t *testing.T) {
	st := &slowAppender{Store: sinkStore(t), delay: 5 * time.Millisecond}
	sink, err := audit.NewStoreSink(st, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	sink.StartAsync(256)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var slowest time.Duration
	for g := 0; g < 40; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				start := time.Now()
				_ = sink.Record(audit.Event{Action: "auth_login_failed", AuthGate: "pw", SourceAddr: fmt.Sprintf("10.0.%d.%d", g, i)})
				if d := time.Since(start); d > 0 {
					mu.Lock()
					if d > slowest {
						slowest = d
					}
					mu.Unlock()
				}
			}
		}(g)
	}
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	if err := sink.Record(audit.Event{Action: "create", TargetTenant: "acme", TargetName: "ops-token"}); err != nil {
		t.Fatalf("the token mint's record: %v", err)
	}
	mint := time.Since(start)
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := sink.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// The mint waits for at most the writer's batch in flight (one append),
	// not for the flood: 40 callers queued at 5ms each would be ~200ms.
	if mint > 100*time.Millisecond {
		t.Errorf("the token mint's record took %s behind the flood, want under 100ms", mint)
	}
	if slowest > 50*time.Millisecond {
		t.Errorf("a failed sign-in's record took %s, want under 50ms (never waited for)", slowest)
	}
	rows, err := st.AuditEventsList(context.Background(), "acme", 0, 0)
	if err != nil || len(rows) != 1 || rows[0].Action != "create" {
		t.Errorf("acme's chain = %v, %v; want the mint", rows, err)
	}
}

// gatedAppender blocks every append until the gate opens.
type gatedAppender struct {
	*sqlite.Store
	gate chan struct{}
}

func (a *gatedAppender) AuditAppend(ctx context.Context, rows []store.AuditEventRow, seal store.AuditSealFunc, opened *store.AuditEventRow) ([]store.AuditEventRow, error) {
	<-a.gate
	return a.Store.AuditAppend(ctx, rows, seal, opened)
}

// TestStoreSink_OverflowIsCountedInADropSummaryRow: when the queue is full
// an observation is dropped, never waited for, and the drops are written as
// one signed summary row per action once the writer can.
func TestStoreSink_OverflowIsCountedInADropSummaryRow(t *testing.T) {
	st := &gatedAppender{Store: sinkStore(t), gate: make(chan struct{})}
	sink, err := audit.NewStoreSink(st, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	sink.StartAsync(4)
	const sent = 50
	returned := make(chan struct{})
	go func() {
		for i := 0; i < sent; i++ {
			_ = sink.Record(audit.Event{Action: "auth_login_failed", AuthGate: "pw", SourceAddr: fmt.Sprint(i)})
		}
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Error("recording observations blocked on a stalled store")
	}
	close(st.gate)
	<-returned
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := sink.Close(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := st.AuditEventsList(context.Background(), "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	stored, dropped := 0, 0
	for _, r := range rows {
		switch r.Action {
		case "auth_login_failed":
			stored++
		case audit.ActionEventsDropped:
			var ev audit.Event
			if err := json.Unmarshal([]byte(r.Payload), &ev); err != nil {
				t.Fatal(err)
			}
			dropped += ev.DroppedCounts["auth_login_failed"]
		}
	}
	if dropped == 0 || stored+dropped != sent {
		t.Errorf("stored %d and counted %d dropped of %d sent; want some dropped and every one accounted for", stored, dropped, sent)
	}
	if rep := verify(t, st.Store, testKeys); !rep.OK {
		t.Errorf("t1 after the overflow: %+v", rep)
	}
}

// TestStoreSink_ObservationsLandInOrderPerTenant: the single writer keeps
// each tenant's observations in the order they were recorded.
func TestStoreSink_ObservationsLandInOrderPerTenant(t *testing.T) {
	st := sinkStore(t)
	sink, err := audit.NewStoreSink(st, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	sink.StartAsync(1024)
	for i := 0; i < 200; i++ {
		tenant := []string{"t1", "t2"}[i%2]
		if err := sink.Record(audit.Event{Action: "acl_denied", ActorTenant: tenant, TargetName: fmt.Sprintf("doc:%03d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := sink.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"t1", "t2"} {
		rows, err := st.AuditEventsList(ctx, tenant, 0, 0)
		if err != nil || len(rows) != 100 {
			t.Fatalf("%s: %d rows, %v", tenant, len(rows), err)
		}
		for i := 1; i < len(rows); i++ {
			if rows[i].Subject <= rows[i-1].Subject {
				t.Fatalf("%s: row %d (%s) after %s: out of order", tenant, rows[i].Seq, rows[i].Subject, rows[i-1].Subject)
			}
		}
		if rep, err := audit.Verify(ctx, st, testKeys, tenant); err != nil || !rep.OK {
			t.Errorf("%s: %+v, %v", tenant, rep, err)
		}
	}
}
