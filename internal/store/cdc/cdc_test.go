package cdc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// TestDocChunkPrefix_MatchesMemory pins the local constant against the
// canonical one so the classification can't silently drift.
func TestDocChunkPrefix_MatchesMemory(t *testing.T) {
	if docChunkPrefix != memory.DocumentChunkKeyPrefix {
		t.Fatalf("docChunkPrefix %q != memory.DocumentChunkKeyPrefix %q", docChunkPrefix, memory.DocumentChunkKeyPrefix)
	}
}

func newCDC(t *testing.T) (*Store, func()) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	return Wrap(st, nil), func() { _ = st.Close() }
}

func TestCDC_EmitsOnWrites(t *testing.T) {
	c, cleanup := newCDC(t)
	defer cleanup()
	ctx := context.Background()

	// A plain memory set + a document-chunk set + a delete.
	if err := c.MemorySet(ctx, "acme", store.MemoryScopeAgent, "a1", "k1", json.RawMessage(`1`), 0); err != nil {
		t.Fatalf("MemorySet: %v", err)
	}
	if err := c.MemorySet(ctx, "acme", store.MemoryScopeAgent, "a1", "doc.chunk:CID", json.RawMessage(`"body"`), 0); err != nil {
		t.Fatalf("MemorySet(doc): %v", err)
	}
	if _, err := c.MemoryDelete(ctx, "acme", store.MemoryScopeAgent, "a1", "k1"); err != nil {
		t.Fatalf("MemoryDelete: %v", err)
	}

	changes, err := c.GetMemoryChangesSince(ctx, "acme", 0, 100)
	if err != nil {
		t.Fatalf("GetMemoryChangesSince: %v", err)
	}
	if len(changes) != 3 {
		t.Fatalf("got %d changes, want 3: %+v", len(changes), changes)
	}
	// Ordered by seq ascending.
	if changes[0].Type != store.MemoryChangeSet || changes[0].Key != "k1" || changes[0].ChunkID != "" {
		t.Errorf("change[0] = %+v, want memory.set key=k1", changes[0])
	}
	if changes[1].Type != store.DocumentChangeUpdated || changes[1].ChunkID != "CID" || changes[1].Key != "" {
		t.Errorf("change[1] = %+v, want document.chunk.updated chunk_id=CID", changes[1])
	}
	if changes[2].Type != store.MemoryChangeDelete || changes[2].Key != "k1" {
		t.Errorf("change[2] = %+v, want memory.delete key=k1", changes[2])
	}
	// Seq is monotonic; At is stamped.
	if !(changes[0].Seq < changes[1].Seq && changes[1].Seq < changes[2].Seq) {
		t.Errorf("seq not monotonic: %d %d %d", changes[0].Seq, changes[1].Seq, changes[2].Seq)
	}
	if changes[0].At.IsZero() {
		t.Errorf("At not stamped")
	}
}

func TestCDC_TenantIsolation(t *testing.T) {
	c, cleanup := newCDC(t)
	defer cleanup()
	ctx := context.Background()

	_ = c.MemorySet(ctx, "acme", store.MemoryScopeAgent, "a1", "k", json.RawMessage(`1`), 0)
	_ = c.MemorySet(ctx, "globex", store.MemoryScopeAgent, "a1", "k", json.RawMessage(`1`), 0)

	acme, _ := c.GetMemoryChangesSince(ctx, "acme", 0, 100)
	if len(acme) != 1 || acme[0].TenantID != "acme" {
		t.Errorf("acme feed = %+v, want exactly its own 1 change", acme)
	}
	globex, _ := c.GetMemoryChangesSince(ctx, "globex", 0, 100)
	if len(globex) != 1 || globex[0].TenantID != "globex" {
		t.Errorf("globex feed = %+v, want exactly its own 1 change", globex)
	}
}

func TestCDC_CursorAndPrune(t *testing.T) {
	c, cleanup := newCDC(t)
	defer cleanup()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_ = c.MemorySet(ctx, "acme", store.MemoryScopeAgent, "a1", "k", json.RawMessage(`1`), 0)
	}
	all, _ := c.GetMemoryChangesSince(ctx, "acme", 0, 100)
	if len(all) != 3 {
		t.Fatalf("want 3 changes, got %d", len(all))
	}
	// Cursor: reading since the first seq returns only the later two.
	after := all[0].Seq
	rest, _ := c.GetMemoryChangesSince(ctx, "acme", after, 100)
	if len(rest) != 2 || rest[0].Seq != all[1].Seq {
		t.Errorf("since=%d returned %+v, want the last two", after, rest)
	}
	// Prune removes rows older than a future cutoff (all of them).
	n, err := c.PruneMemoryChanges(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("Prune removed %d (err %v), want 3", n, err)
	}
	if left, _ := c.GetMemoryChangesSince(ctx, "acme", 0, 100); len(left) != 0 {
		t.Errorf("after prune %d rows remain, want 0", len(left))
	}
}

// failingAppendStore fails every change-feed append; every other method is the
// nil embedded interface, so the test touches only AppendMemoryChange.
type failingAppendStore struct{ store.Store }

func (failingAppendStore) AppendMemoryChange(context.Context, store.MemoryChange) error {
	return errors.New("feed table locked")
}

// TestCDC_FailedAppendIsLoggedAsError pins that a change the feed could not
// record is reported at Error with its type, tenant and cause — the write it
// describes succeeded, so the log is the only trace of the lost feed row.
func TestCDC_FailedAppendIsLoggedAsError(t *testing.T) {
	var buf bytes.Buffer
	c := Wrap(failingAppendStore{}, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	c.emit(context.Background(), store.MemoryChange{TenantID: "acme", Type: store.MemoryChangeSet})

	line := strings.TrimSpace(buf.String())
	for _, want := range []string{
		"level=ERROR",
		"cdc: append change failed (feed lag, write succeeded)",
		"change_type=memory.set",
		"tenant=acme",
		`err="feed table locked"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q: %s", want, line)
		}
	}
}
