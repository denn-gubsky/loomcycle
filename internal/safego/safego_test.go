package safego

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestGo_PanicIsRecoveredLoggedAndCounted(t *testing.T) {
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	before := Panics()["test.recovered"]
	done := make(chan struct{})
	Go(context.Background(), "test.recovered", func() {
		defer close(done)
		panic("boom")
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine never ran")
	}
	// The count lands after fn's defers and the log line, in Run's own
	// deferred recover.
	deadline := time.Now().Add(5 * time.Second)
	for Panics()["test.recovered"] != before+1 {
		if time.Now().After(deadline) {
			t.Fatalf("panic count = %d, want %d", Panics()["test.recovered"], before+1)
		}
		time.Sleep(time.Millisecond)
	}
	out := buf.String()
	for _, want := range []string{"background goroutine panicked", "goroutine=test.recovered", "panic=boom", "stack="} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q: %s", want, out)
		}
	}
}

// TestRun_DeferredReleasesRunBeforeRecovery pins the contract converted sites
// rely on: a lock, a WaitGroup and a done channel released by defers inside fn
// are all released when fn panics.
func TestRun_DeferredReleasesRunBeforeRecovery(t *testing.T) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	Go(context.Background(), "test.release", func() {
		defer wg.Done()
		defer close(done)
		mu.Lock()
		defer mu.Unlock()
		panic("boom")
	})
	waited := make(chan struct{})
	go func() { wg.Wait(); <-done; mu.Lock(); mu.Unlock(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("a deferred release did not run after the panic")
	}
}

func TestRun_NoPanicIsNotCounted(t *testing.T) {
	before := Panics()["test.quiet"]
	ran := false
	Run(context.Background(), "test.quiet", func() { ran = true })
	if !ran || Panics()["test.quiet"] != before {
		t.Fatalf("ran=%v count=%d, want ran and no count", ran, Panics()["test.quiet"])
	}
}
