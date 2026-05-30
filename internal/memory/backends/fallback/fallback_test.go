package fallback_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/fallback"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakeBackend is a memory.Backend test double whose every op returns a
// fixed error (when failErr != nil) or a canned success. Lets us drive
// the wrapper's primary-then-fallback path deterministically.
type fakeBackend struct {
	name     string
	failErr  error
	getValue json.RawMessage
}

func (f *fakeBackend) Get(_ context.Context, _ store.MemoryScope, _, key string) (store.MemoryEntry, error) {
	if f.failErr != nil {
		return store.MemoryEntry{}, f.failErr
	}
	return store.MemoryEntry{Key: key, Value: f.getValue}, nil
}
func (f *fakeBackend) Set(_ context.Context, _ store.MemoryScope, _, _ string, _ json.RawMessage, _ memory.SetOptions) (memory.SetResult, error) {
	if f.failErr != nil {
		return memory.SetResult{}, f.failErr
	}
	return memory.SetResult{Embedded: true}, nil
}
func (f *fakeBackend) Delete(_ context.Context, _ store.MemoryScope, _, _ string) (bool, error) {
	if f.failErr != nil {
		return false, f.failErr
	}
	return true, nil
}
func (f *fakeBackend) List(_ context.Context, _ store.MemoryScope, _, _ string, _ int) ([]store.MemoryEntry, bool, error) {
	if f.failErr != nil {
		return nil, false, f.failErr
	}
	return []store.MemoryEntry{{Key: "from-" + f.name}}, false, nil
}
func (f *fakeBackend) Search(_ context.Context, _ store.MemoryScope, _ string, _ memory.SearchQuery, _ memory.RankConfig) (memory.SearchResult, error) {
	if f.failErr != nil {
		return memory.SearchResult{}, f.failErr
	}
	return memory.SearchResult{QueryEmbeddingDim: 1}, nil
}
func (f *fakeBackend) Stats(_ context.Context, scope store.MemoryScope) (store.MemoryEmbedStats, error) {
	if f.failErr != nil {
		return store.MemoryEmbedStats{}, f.failErr
	}
	return store.MemoryEmbedStats{Scope: scope}, nil
}

// TestFallback_PrimaryErrorServesFromFallbackAndLogs pins the core
// graceful-degradation behavior: primary errors → fallback serves the op
// → success → a degradation log line is emitted (without any secret).
func TestFallback_PrimaryErrorServesFromFallbackAndLogs(t *testing.T) {
	primary := &fakeBackend{name: "primary", failErr: errors.New("mem9: request to GET: connection refused")}
	fb := &fakeBackend{name: "fallback"}

	var captured bytes.Buffer
	capLogf := func(format string, args ...any) {
		captured.WriteString(fmt.Sprintf(format, args...))
		captured.WriteByte('\n')
	}

	b := fallback.New(primary, fb, capLogf)
	entries, _, err := b.List(context.Background(), store.MemoryScopeAgent, "qa", "", 10)
	if err != nil {
		t.Fatalf("List: unexpected error after fallback: %v", err)
	}
	if len(entries) != 1 || entries[0].Key != "from-fallback" {
		t.Fatalf("List served from %+v, want fallback", entries)
	}
	if !strings.Contains(captured.String(), "falling back to in-process") {
		t.Errorf("expected degradation log, got: %q", captured.String())
	}
}

// TestFallback_PrimarySuccessDoesNotTouchFallback pins that a healthy
// primary serves directly — no fallback, no log.
func TestFallback_PrimarySuccessDoesNotTouchFallback(t *testing.T) {
	primary := &fakeBackend{name: "primary"}
	// A fallback that would fail loudly if reached.
	fb := &fakeBackend{name: "fallback", failErr: errors.New("fallback should not be called")}

	var captured bytes.Buffer
	b := fallback.New(primary, fb, func(f string, a ...any) { captured.WriteString(fmt.Sprintf(f, a...)) })

	res, err := b.Set(context.Background(), store.MemoryScopeAgent, "qa", "k", json.RawMessage(`1`), memory.SetOptions{})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !res.Embedded {
		t.Error("Set served by primary should report Embedded=true")
	}
	if captured.Len() != 0 {
		t.Errorf("healthy primary must not log a degradation, got: %q", captured.String())
	}
}

// TestFallback_GetNotFoundDoesNotDegrade pins that a real "absent key"
// from the primary is NOT treated as a backend failure — falling back on
// it would mask a deletion (gone from Mem9, lingering locally).
func TestFallback_GetNotFoundDoesNotDegrade(t *testing.T) {
	primary := &fakeBackend{name: "primary", failErr: &store.ErrNotFound{Kind: "memory", ID: "k"}}
	fb := &fakeBackend{name: "fallback", getValue: json.RawMessage(`"stale"`)}

	var captured bytes.Buffer
	b := fallback.New(primary, fb, func(f string, a ...any) { captured.WriteString(fmt.Sprintf(f, a...)) })

	_, err := b.Get(context.Background(), store.MemoryScopeAgent, "qa", "k")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("Get err = %v, want the primary's ErrNotFound passed through", err)
	}
	if captured.Len() != 0 {
		t.Errorf("NotFound must not trigger a fallback/log, got: %q", captured.String())
	}
}
